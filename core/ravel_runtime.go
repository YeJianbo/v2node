package core

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	ravel "github.com/YeJianbo/ravel-go"
	"github.com/YeJianbo/ravel-go/carrier"
	"github.com/YeJianbo/ravel-go/gateway"
	"github.com/YeJianbo/ravel-go/session"
	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/common/rate"
	"github.com/wyx2685/v2node/limiter"
)

const ravelTrafficFlushInterval = time.Second

type ravelRuntimeConfig struct {
	tag          string
	listen       string
	tlsConfig    *tls.Config
	authority    string
	path         string
	gatewayGroup [8]byte
	fallback     http.Handler
	settings     panel.RavelProtocolSettings
}

type ravelRuntime struct {
	config  ravelRuntimeConfig
	dialer  *net.Dialer
	limiter *limiter.Limiter

	opMu        sync.Mutex
	users       map[string]panel.UserInfo
	server      *gateway.Server
	listener    net.Listener
	serveDone   chan error
	initialized bool
	closed      bool

	authMu            sync.RWMutex
	activeAuth        map[ravel.AuthContext]int
	activeUUID        map[ravel.AuthContext]string
	activeCredentials map[ravel.AuthContext]ravel.Credential
	authUID           map[ravel.AuthContext]int

	traffic sync.Map
}

type ravelTrafficCounter struct {
	upload   atomic.Uint64
	download atomic.Uint64
}

type ravelCredentialSnapshot struct {
	credentials []ravel.Credential
	activeAuth  map[ravel.AuthContext]int
	activeUUID  map[ravel.AuthContext]string
	users       map[string]panel.UserInfo
}

func (core *V2Core) lookupRavelRuntime(tag string) (*ravelRuntime, bool) {
	core.ravelMu.RLock()
	runtime, ok := core.ravel[tag]
	core.ravelMu.RUnlock()
	return runtime, ok
}

func (core *V2Core) getRavelRuntime(tag string) (*ravelRuntime, error) {
	runtime, ok := core.lookupRavelRuntime(tag)
	if !ok {
		return nil, fmt.Errorf("no such ravel node: %s", tag)
	}
	return runtime, nil
}

func (core *V2Core) ReplaceRavelUsers(tag string, users []panel.UserInfo) error {
	runtime, err := core.getRavelRuntime(tag)
	if err != nil {
		return err
	}
	return runtime.replaceUsers(users)
}

func newRavelRuntime(tag string, info *panel.NodeInfo) (*ravelRuntime, error) {
	if !carrier.ExtendedConnectEnabled() {
		return nil, errors.New("RFC 8441 is disabled: set GODEBUG=http2xconnect=1 before process start")
	}
	if info == nil || info.Common == nil || info.Common.Ravel == nil {
		return nil, errors.New("ravel node settings are incomplete")
	}
	if info.Common.CertInfo == nil {
		return nil, errors.New("ravel TLS certificate settings are missing")
	}
	certificate, err := tls.LoadX509KeyPair(
		info.Common.CertInfo.CertFile,
		info.Common.CertInfo.KeyFile,
	)
	if err != nil {
		return nil, fmt.Errorf("load ravel TLS certificate: %w", err)
	}
	fallback, err := newRavelFallback(info.Common.Ravel.Masquerade)
	if err != nil {
		return nil, fmt.Errorf("build ravel masquerade handler: %w", err)
	}
	authority, err := ravel.NormalizeAuthority(info.Common.Ravel.Authority)
	if err != nil {
		return nil, fmt.Errorf("normalize ravel authority: %w", err)
	}
	gatewayGroup, err := ravelGatewayGroup(info.Common.Ravel.GatewayGroup)
	if err != nil {
		return nil, err
	}
	listen := net.JoinHostPort(
		info.Common.ListenIP,
		strconv.Itoa(info.Common.ServerPort),
	)
	dialTimeout := time.Duration(info.Common.Ravel.Settings.HandshakeTimeout) * time.Second
	if dialTimeout <= 0 {
		dialTimeout = 10 * time.Second
	}
	userLimiter, _ := limiter.GetLimiter(tag)
	return &ravelRuntime{
		limiter: userLimiter,
		config: ravelRuntimeConfig{
			tag:    tag,
			listen: listen,
			tlsConfig: &tls.Config{Certificates: []tls.Certificate{certificate}, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				if info.Common.CertInfo.RejectUnknownSni && !strings.EqualFold(hello.ServerName, info.Common.CertInfo.CertDomain) {
					return nil, errors.New("unrecognized TLS server name")
				}
				current, err := tls.LoadX509KeyPair(info.Common.CertInfo.CertFile, info.Common.CertInfo.KeyFile)
				return &current, err
			}},
			authority:    authority,
			path:         info.Common.Ravel.Path,
			gatewayGroup: gatewayGroup,
			fallback:     fallback,
			settings:     info.Common.Ravel.Settings,
		},
		dialer:     &net.Dialer{Timeout: dialTimeout},
		users:      make(map[string]panel.UserInfo),
		activeAuth: make(map[ravel.AuthContext]int),
		authUID:    make(map[ravel.AuthContext]int),
	}, nil
}

func ravelGatewayGroup(raw string) ([8]byte, error) {
	var group [8]byte
	if len(raw) != len(group) {
		return group, errors.New("ravel gateway group must be exactly 8 raw bytes")
	}
	copy(group[:], raw)
	return group, nil
}

func (runtime *ravelRuntime) replaceUsers(users []panel.UserInfo) error {
	snapshot, err := buildRavelCredentialSnapshot(users, runtime.config.gatewayGroup)
	if err != nil {
		return err
	}

	runtime.opMu.Lock()
	defer runtime.opMu.Unlock()
	return runtime.applySnapshotLocked(snapshot)
}

func (runtime *ravelRuntime) applySnapshotLocked(snapshot ravelCredentialSnapshot) error {
	if runtime.closed {
		return errors.New("ravel runtime is closed")
	}
	if err := runtime.checkHistoricalOwners(snapshot.activeAuth); err != nil {
		return err
	}
	if runtime.server == nil {
		if err := runtime.startGatewayLocked(snapshot); err != nil {
			return err
		}
		runtime.users = snapshot.users
		runtime.initialized = true
		return nil
	}

	runtime.authMu.Lock()
	if err := runtime.server.UpdateCredentials(snapshot.credentials); err != nil {
		runtime.authMu.Unlock()
		return fmt.Errorf("update ravel credentials: %w", err)
	}
	runtime.activeAuth = snapshot.activeAuth
	runtime.activeUUID = snapshot.activeUUID
	runtime.setActiveCredentialsLocked(snapshot.credentials)
	runtime.mergeHistoricalOwnersLocked(snapshot.activeAuth)
	runtime.authMu.Unlock()
	runtime.users = snapshot.users
	runtime.initialized = true
	return nil
}

func (runtime *ravelRuntime) addUsers(users []panel.UserInfo) error {
	runtime.opMu.Lock()
	defer runtime.opMu.Unlock()
	proposed := cloneRavelUsers(runtime.users)
	for i := range users {
		if users[i].Uuid == "" {
			return fmt.Errorf("ravel user %d has an empty UUID", users[i].Id)
		}
		proposed[users[i].Uuid] = cloneRavelUser(users[i])
	}
	snapshot, err := buildRavelCredentialSnapshot(
		ravelUsersFromMap(proposed),
		runtime.config.gatewayGroup,
	)
	if err != nil {
		return err
	}
	return runtime.applySnapshotLocked(snapshot)
}

func (runtime *ravelRuntime) delUsers(users []panel.UserInfo) error {
	runtime.opMu.Lock()
	defer runtime.opMu.Unlock()
	proposed := cloneRavelUsers(runtime.users)
	for i := range users {
		delete(proposed, users[i].Uuid)
	}
	snapshot, err := buildRavelCredentialSnapshot(
		ravelUsersFromMap(proposed),
		runtime.config.gatewayGroup,
	)
	if err != nil {
		return err
	}
	return runtime.applySnapshotLocked(snapshot)
}

func (runtime *ravelRuntime) startGatewayLocked(snapshot ravelCredentialSnapshot) error {
	settings := runtime.config.settings
	server, err := gateway.New(gateway.Config{
		TLSConfig:              runtime.config.tlsConfig,
		Authority:              runtime.config.authority,
		Path:                   runtime.config.path,
		Credentials:            snapshot.credentials,
		GatewayGroup:           runtime.config.gatewayGroup,
		ClaimCapacity:          settings.ClaimCapacity,
		ClaimRetention:         time.Duration(settings.ClaimRetention) * time.Second,
		ConnectionLifetime:     time.Duration(settings.ConnectionLifetime) * time.Second,
		DrainTimeout:           time.Duration(settings.DrainTimeout) * time.Second,
		SessionMaxBytes:        settings.SessionMaxBytes,
		HandshakeTimeout:       time.Duration(settings.HandshakeTimeout) * time.Second,
		MaxMessageSize:         settings.MaxMessage,
		H2MaxConcurrentStreams: settings.MaxStreams,
		TrafficFlushInterval:   ravelTrafficFlushInterval,
		Handshake: session.ServerHandshakeConfig{
			MaxFrameReceive:         settings.MaxFrameReceive,
			MaxStreams:              settings.MaxStreams,
			InitialConnectionWindow: settings.ConnectionWindow,
			InitialStreamWindow:     settings.StreamWindow,
			IdleTimeout:             time.Duration(settings.IdleTimeout) * time.Second,
			Authorize:               runtime.authorize,
		},
		Chunk: session.ChunkConfig{
			MinSize:       settings.ChunkMin,
			MaxSize:       settings.ChunkMax,
			MinFlushDelay: time.Duration(settings.ChunkMinDelay) * time.Millisecond,
			MaxFlushDelay: time.Duration(settings.ChunkMaxDelay) * time.Millisecond,
		},
		OnOpen:    runtime.onOpen,
		OnTraffic: runtime.onTraffic,
		OnSession: runtime.watchSession,
		Fallback:  runtime.config.fallback,
		Decoy:     ravelSinkDecoy,
	})
	if err != nil {
		return fmt.Errorf("create ravel gateway: %w", err)
	}
	listener, err := net.Listen("tcp", runtime.config.listen)
	if err != nil {
		_ = server.Close()
		return fmt.Errorf("listen for ravel gateway on %s: %w", runtime.config.listen, err)
	}

	runtime.authMu.Lock()
	runtime.activeAuth = snapshot.activeAuth
	runtime.activeUUID = snapshot.activeUUID
	runtime.setActiveCredentialsLocked(snapshot.credentials)
	runtime.mergeHistoricalOwnersLocked(snapshot.activeAuth)
	runtime.authMu.Unlock()
	runtime.server = server
	runtime.listener = listener
	runtime.serveDone = make(chan error, 1)
	done := runtime.serveDone
	go func() {
		err := server.Serve(listener)
		done <- err
		if err != nil {
			log.WithFields(log.Fields{
				"tag": runtime.config.tag,
				"err": err,
			}).Error("Ravel gateway stopped")
		}
	}()
	log.WithFields(log.Fields{
		"tag":    runtime.config.tag,
		"listen": listener.Addr().String(),
	}).Info("Ravel gateway started")
	return nil
}

func (runtime *ravelRuntime) closeGatewayLocked() error {
	server := runtime.server
	done := runtime.serveDone
	runtime.server = nil
	runtime.listener = nil
	runtime.serveDone = nil
	if server == nil {
		return nil
	}
	closeErr := server.Close()
	var serveErr error
	if done != nil {
		serveErr = <-done
	}
	if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
		return errors.Join(closeErr, serveErr)
	}
	return closeErr
}

func (runtime *ravelRuntime) close() error {
	runtime.opMu.Lock()
	defer runtime.opMu.Unlock()
	if runtime.closed {
		return nil
	}
	runtime.closed = true
	runtime.authMu.Lock()
	runtime.activeAuth = make(map[ravel.AuthContext]int)
	runtime.authMu.Unlock()
	return runtime.closeGatewayLocked()
}

func (runtime *ravelRuntime) authorize(auth ravel.AuthContext) bool {
	runtime.authMu.RLock()
	_, ok := runtime.activeAuth[auth]
	credential, exists := runtime.activeCredentials[auth]
	runtime.authMu.RUnlock()
	now := time.Now()
	return ok && exists && now.Unix() < int64(credential.NotAfter) && credential.ValidateAt(now, runtime.config.gatewayGroup) == nil
}

func (runtime *ravelRuntime) setActiveCredentialsLocked(credentials []ravel.Credential) {
	runtime.activeCredentials = make(map[ravel.AuthContext]ravel.Credential, len(credentials))
	for _, credential := range credentials {
		runtime.activeCredentials[credential.AuthContext()] = credential
	}
}

// Re-check current authorization for existing carriers as well as new OPENs.
// This also enforces an overlap expiry shortened by a credential update.
func (runtime *ravelRuntime) watchSession(active *session.Session) {
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			if !runtime.authorize(active.AuthContext()) {
				_ = active.Close()
				return
			}
			select {
			case <-active.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (runtime *ravelRuntime) onTraffic(auth ravel.AuthContext, upload, download uint64) {
	runtime.authMu.RLock()
	uid, ok := runtime.authUID[auth]
	runtime.authMu.RUnlock()
	if !ok || uid <= 0 {
		return
	}
	value, _ := runtime.traffic.LoadOrStore(uid, &ravelTrafficCounter{})
	counter := value.(*ravelTrafficCounter)
	if upload != 0 {
		counter.upload.Add(upload)
	}
	if download != 0 {
		counter.download.Add(download)
	}
}

func (runtime *ravelRuntime) trafficSlice(minTraffic int) []panel.UserTraffic {
	threshold := uint64(0)
	if minTraffic > 0 {
		threshold = uint64(minTraffic) * 1000
	}
	traffic := make([]panel.UserTraffic, 0)
	runtime.traffic.Range(func(key, value any) bool {
		uid := key.(int)
		counter := value.(*ravelTrafficCounter)
		upload := counter.upload.Load()
		download := counter.download.Load()
		if !ravelTrafficAboveThreshold(upload, download, threshold) {
			return true
		}
		upload = counter.upload.Swap(0)
		download = counter.download.Swap(0)
		if upload == 0 && download == 0 {
			return true
		}
		traffic = append(traffic, panel.UserTraffic{
			UID:      uid,
			Upload:   ravelTrafficInt64(upload),
			Download: ravelTrafficInt64(download),
		})
		return true
	})
	sort.Slice(traffic, func(i, j int) bool {
		return traffic[i].UID < traffic[j].UID
	})
	if len(traffic) == 0 {
		return nil
	}
	return traffic
}

func ravelTrafficAboveThreshold(upload, download, threshold uint64) bool {
	if upload > threshold {
		return true
	}
	return download > threshold-upload
}

func ravelTrafficInt64(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}

func (runtime *ravelRuntime) checkHistoricalOwners(active map[ravel.AuthContext]int) error {
	runtime.authMu.RLock()
	defer runtime.authMu.RUnlock()
	for auth, uid := range active {
		if previous, exists := runtime.authUID[auth]; exists && previous != uid {
			return fmt.Errorf(
				"ravel credential identity cannot move from user %d to user %d",
				previous,
				uid,
			)
		}
	}
	return nil
}

func (runtime *ravelRuntime) mergeHistoricalOwnersLocked(active map[ravel.AuthContext]int) {
	for auth, uid := range active {
		runtime.authUID[auth] = uid
	}
}

func buildRavelCredentialSnapshot(
	users []panel.UserInfo,
	gatewayGroup [8]byte,
) (ravelCredentialSnapshot, error) {
	snapshot := ravelCredentialSnapshot{
		activeAuth: make(map[ravel.AuthContext]int),
		activeUUID: make(map[ravel.AuthContext]string),
		users:      make(map[string]panel.UserInfo, len(users)),
	}
	owners := make(map[int]bool, len(users))
	for userIndex := range users {
		user := users[userIndex]
		if user.Id <= 0 {
			return ravelCredentialSnapshot{}, fmt.Errorf("ravel user at index %d has an invalid UID", userIndex)
		}
		if user.Uuid == "" {
			return ravelCredentialSnapshot{}, fmt.Errorf("ravel user %d has an empty UUID", user.Id)
		}
		if owners[user.Id] {
			return ravelCredentialSnapshot{}, fmt.Errorf("duplicate ravel user ID %d", user.Id)
		}
		owners[user.Id] = true
		if _, exists := snapshot.users[user.Uuid]; exists {
			return ravelCredentialSnapshot{}, fmt.Errorf("duplicate ravel user UUID %q", user.Uuid)
		}
		snapshot.users[user.Uuid] = cloneRavelUser(user)
		for credentialIndex := range user.RavelCredentials {
			encoded := user.RavelCredentials[credentialIndex]
			credential, err := ravel.ParseCredential(
				encoded.CredentialID,
				encoded.CapabilityKey,
				encoded.KeyVersion,
				encoded.PolicyID,
				encoded.GatewayGroup,
				encoded.NotBefore,
				encoded.NotAfter,
			)
			if err != nil {
				return ravelCredentialSnapshot{}, fmt.Errorf(
					"parse ravel credential for user %d at index %d: %w",
					user.Id,
					credentialIndex,
					err,
				)
			}
			if credential.GatewayGroup != gatewayGroup {
				return ravelCredentialSnapshot{}, fmt.Errorf(
					"ravel credential gateway group mismatch for user %d at index %d",
					user.Id,
					credentialIndex,
				)
			}
			auth := credential.AuthContext()
			if previous, exists := snapshot.activeAuth[auth]; exists {
				return ravelCredentialSnapshot{}, fmt.Errorf(
					"duplicate ravel credential identity for users %d and %d",
					previous,
					user.Id,
				)
			}
			snapshot.activeAuth[auth] = user.Id
			snapshot.activeUUID[auth] = user.Uuid
			snapshot.credentials = append(snapshot.credentials, credential)
		}
	}
	return snapshot, nil
}

func cloneRavelUser(user panel.UserInfo) panel.UserInfo {
	user.RavelCredentials = append(
		[]panel.RavelCredential(nil),
		user.RavelCredentials...,
	)
	return user
}

func cloneRavelUsers(users map[string]panel.UserInfo) map[string]panel.UserInfo {
	cloned := make(map[string]panel.UserInfo, len(users))
	for uuid, user := range users {
		cloned[uuid] = cloneRavelUser(user)
	}
	return cloned
}

func ravelUsersFromMap(users map[string]panel.UserInfo) []panel.UserInfo {
	uuids := make([]string, 0, len(users))
	for uuid := range users {
		uuids = append(uuids, uuid)
	}
	sort.Strings(uuids)
	result := make([]panel.UserInfo, 0, len(uuids))
	for _, uuid := range uuids {
		result = append(result, cloneRavelUser(users[uuid]))
	}
	return result
}

func (runtime *ravelRuntime) onOpen(
	ctx context.Context,
	stream *session.Stream,
	request ravel.OpenFrame,
) {
	defer stream.Close()
	if !runtime.authorize(stream.AuthContext()) {
		_ = stream.Reject(ravel.OpenResultInternalError)
		return
	}
	if request.StreamKind != ravel.StreamKindTCP {
		_ = stream.Reject(ravel.OpenResultUnsupportedKind)
		return
	}
	destination, err := ravelOpenDestination(request)
	if err != nil {
		_ = stream.Reject(ravel.OpenResultInvalidAddress)
		return
	}
	var streamConnection net.Conn = stream
	if runtime.limiter != nil {
		runtime.authMu.RLock()
		uuid := runtime.activeUUID[stream.AuthContext()]
		runtime.authMu.RUnlock()
		ip, _, err := net.SplitHostPort(stream.RemoteAddr().String())
		if err != nil {
			_ = stream.Reject(ravel.OpenResultInvalidAddress)
			return
		}
		bucket, rejected := runtime.limiter.CheckLimit(format.UserTag(runtime.config.tag, uuid), ip, true)
		if rejected {
			_ = stream.Reject(ravel.OpenResultResourceLimit)
			return
		}
		if bucket != nil {
			streamConnection = &ravelLimitedStream{Stream: stream, bucket: bucket}
		}
	}
	outbound, err := runtime.dialer.DialContext(ctx, "tcp", destination)
	if err != nil {
		_ = stream.Reject(ravelMapOpenError(err))
		return
	}
	if err := stream.Accept(); err != nil {
		_ = outbound.Close()
		return
	}
	_ = ravelRelayTCP(streamConnection, outbound)
}

type ravelLimitedStream struct {
	*session.Stream
	bucket *rate.DynamicBucket
}

func (s *ravelLimitedStream) waitBytes(size int) error {
	bucket := s.bucket.Get()
	if bucket == nil {
		return nil
	}
	wait := bucket.Take(int64(size))
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-s.Context().Done():
		return context.Cause(s.Context())
	}
}

func (s *ravelLimitedStream) Read(payload []byte) (int, error) {
	n, err := s.Stream.Read(payload)
	if n > 0 {
		if limitErr := s.waitBytes(n); limitErr != nil {
			return 0, limitErr
		}
	}
	return n, err
}

func (s *ravelLimitedStream) Write(payload []byte) (int, error) {
	if err := s.waitBytes(len(payload)); err != nil {
		return 0, err
	}
	return s.Stream.Write(payload)
}

func ravelOpenDestination(request ravel.OpenFrame) (string, error) {
	if request.Port == 0 {
		return "", errors.New("zero port")
	}
	var host string
	switch request.AddressType {
	case ravel.AddressTypeIPv4:
		if len(request.Address) != net.IPv4len {
			return "", errors.New("invalid IPv4 length")
		}
		host = net.IP(request.Address).String()
	case ravel.AddressTypeIPv6:
		if len(request.Address) != net.IPv6len || net.IP(request.Address).To4() != nil {
			return "", errors.New("invalid IPv6 address")
		}
		host = net.IP(request.Address).String()
	case ravel.AddressTypeDNS:
		host = string(request.Address)
		if !ravelValidDNSName(host) {
			return "", errors.New("invalid DNS name")
		}
	default:
		return "", errors.New("unsupported address type")
	}
	return net.JoinHostPort(host, strconv.Itoa(int(request.Port))), nil
}

func ravelValidDNSName(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range []byte(label) {
			if (character >= 'a' && character <= 'z') ||
				(character >= 'A' && character <= 'Z') ||
				(character >= '0' && character <= '9') ||
				character == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func ravelMapOpenError(err error) ravel.OpenResultCode {
	switch {
	case err == nil:
		return ravel.OpenResultOK
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return ravel.OpenResultConnectTimeout
	case ravelIsTimeout(err):
		return ravel.OpenResultConnectTimeout
	case errors.Is(err, syscall.EMFILE),
		errors.Is(err, syscall.ENFILE),
		errors.Is(err, syscall.ENOBUFS),
		errors.Is(err, syscall.ENOMEM):
		return ravel.OpenResultResourceLimit
	case errors.Is(err, syscall.ENETUNREACH),
		errors.Is(err, syscall.EHOSTUNREACH),
		errors.Is(err, syscall.ECONNREFUSED),
		errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, syscall.EADDRNOTAVAIL):
		return ravel.OpenResultNetworkUnreachable
	default:
		var dnsError *net.DNSError
		if errors.As(err, &dnsError) {
			return ravel.OpenResultNetworkUnreachable
		}
		return ravel.OpenResultInternalError
	}
}

func ravelIsTimeout(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

type ravelCloseWriter interface {
	CloseWrite() error
}

type ravelCloseReader interface {
	CloseRead() error
}

func ravelRelayTCP(left, right net.Conn) error {
	results := make(chan error, 2)
	var abortOnce sync.Once
	abort := func() {
		abortOnce.Do(func() {
			_ = left.Close()
			_ = right.Close()
		})
	}
	copyOne := func(destination, source net.Conn) {
		_, err := io.Copy(destination, source)
		if writer, ok := destination.(ravelCloseWriter); ok {
			err = errors.Join(err, writer.CloseWrite())
		}
		if reader, ok := source.(ravelCloseReader); ok {
			err = errors.Join(err, reader.CloseRead())
		}
		if err != nil &&
			!errors.Is(err, net.ErrClosed) &&
			!errors.Is(err, io.ErrClosedPipe) {
			abort()
		}
		results <- err
	}
	go copyOne(right, left)
	go copyOne(left, right)
	first := <-results
	second := <-results
	abort()
	return errors.Join(ravelNormalizeRelayError(first), ravelNormalizeRelayError(second))
}

func ravelNormalizeRelayError(err error) error {
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return err
}

func ravelSinkDecoy(
	socket *carrier.WebSocket,
	_ carrier.Message,
	_ *http.Request,
) {
	defer socket.Abort()
	for {
		if _, err := socket.ReadMessage(); err != nil {
			return
		}
	}
}
