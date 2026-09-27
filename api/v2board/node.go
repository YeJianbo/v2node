package panel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	ravel "github.com/YeJianbo/ravel-go"
	"github.com/YeJianbo/ravel-go/session"
)

// Security type
const (
	None    = 0
	Tls     = 1
	Reality = 2
)

type NodeInfo struct {
	Id           int
	Type         string
	Security     int
	PushInterval time.Duration
	PullInterval time.Duration
	Tag          string
	Common       *CommonNode
}

type CommonNode struct {
	Protocol   string      `json:"protocol"`
	ListenIP   string      `json:"listen_ip"`
	ServerPort int         `json:"server_port"`
	Routes     []Route     `json:"routes"`
	BaseConfig *BaseConfig `json:"base_config"`
	//vless vmess trojan
	Tls                  int         `json:"tls"`
	TlsSettings          TlsSettings `json:"tls_settings"`
	CertInfo             *CertInfo
	Network              string          `json:"network"`
	NetworkSettings      json.RawMessage `json:"network_settings"`
	TrustedXForwardedFor []string        `json:"trusted_x_forwarded_for"`
	Encryption           string          `json:"encryption"`
	EncryptionSettings   EncSettings     `json:"encryption_settings"`
	ServerName           string          `json:"server_name"`
	Flow                 string          `json:"flow"`
	//shadowsocks
	Cipher    string `json:"cipher"`
	ServerKey string `json:"server_key"`
	//tuic
	CongestionControl string `json:"congestion_control"`
	ZeroRTTHandshake  bool   `json:"zero_rtt_handshake"`
	//anytls
	PaddingScheme []string `json:"padding_scheme,omitempty"`
	//hysteria hysteria2
	UpMbps                  int    `json:"up_mbps"`
	DownMbps                int    `json:"down_mbps"`
	Obfs                    string `json:"obfs"`
	ObfsPassword            string `json:"obfs_password"`
	Ignore_Client_Bandwidth bool   `json:"ignore_client_bandwidth"`
	// ravel
	Ravel *RavelSettings `json:"ravel,omitempty"`
}

type RavelSettings struct {
	Authority    string                `json:"authority"`
	Path         string                `json:"path"`
	GatewayGroup string                `json:"gateway_group"`
	Masquerade   string                `json:"masquerade"`
	Settings     RavelProtocolSettings `json:"settings"`
}

type RavelProtocolSettings struct {
	DrainTimeout       int    `json:"drain_timeout,omitempty"`
	SessionMaxBytes    uint64 `json:"session_max_bytes,omitempty"`
	ClaimCapacity      int    `json:"claim_capacity,omitempty"`
	ClaimRetention     int    `json:"claim_retention"`     // seconds
	ConnectionLifetime int    `json:"connection_lifetime"` // seconds
	HandshakeTimeout   int    `json:"handshake_timeout"`   // seconds
	IdleTimeout        int    `json:"idle_timeout"`        // seconds
	MaxMessage         int64  `json:"max_message"`
	MaxFrameReceive    uint32 `json:"max_frame_receive,omitempty"`
	MaxStreams         uint32 `json:"max_streams"`
	ConnectionWindow   uint64 `json:"connection_window"`
	StreamWindow       uint64 `json:"stream_window"`
	ChunkMin           int    `json:"chunk_min,omitempty"`
	ChunkMax           int    `json:"chunk_max"`
	ChunkMinDelay      int    `json:"chunk_min_delay,omitempty"` // milliseconds
	ChunkMaxDelay      int    `json:"chunk_max_delay,omitempty"` // milliseconds
}

const (
	ravelDefaultClaimCapacity   = 65536
	ravelDefaultMaxFrameReceive = ravel.DefaultMaxFrameSize
	ravelDefaultChunkMin        = 1024
	ravelDefaultChunkMinDelayMS = 2
	ravelDefaultChunkMaxDelayMS = 8

	ravelMaxAuthorityLength       = 255
	ravelMaxPathLength            = 2048
	ravelMaxClaimCapacity         = 1 << 20
	ravelMaxClaimRetentionSeconds = 7 * 24 * 60 * 60
	ravelMaxLifetimeSeconds       = 24 * 60 * 60
	ravelMaxHandshakeSeconds      = 60
	ravelMaxIdleSeconds           = int(session.MaxIdleTimeoutHardLimit / time.Second)
	ravelMinMessageSize           = ravel.MaxHandshakeSize
	ravelMaxMessageSize           = ravel.MaxCarrierMessageSize
	ravelMaxFrameReceive          = session.MaxFrameReceiveHardLimit
	ravelMaxStreams               = session.MaxStreamsHardLimit
	ravelMaxConnectionWindow      = session.MaxInitialConnectionWindowHardLimit
	ravelMaxStreamWindow          = session.MaxInitialStreamWindowHardLimit
	ravelMaxChunkSize             = ravel.MaxCarrierMessageSize
	ravelMaxChunkDelayMS          = 1000
)

type Route struct {
	Id          int      `json:"id"`
	Match       []string `json:"match"`
	Action      string   `json:"action"`
	ActionValue *string  `json:"action_value"`
}

type BaseConfig struct {
	DomainTrafficEnable    bool `json:"domain_traffic_enable"`
	PushInterval           any  `json:"push_interval"`
	PullInterval           any  `json:"pull_interval"`
	DeviceOnlineMinTraffic int  `json:"device_online_min_traffic"`
	NodeReportMinTraffic   int  `json:"node_report_min_traffic"`
}

type TlsSettings struct {
	ServerName       string   `json:"server_name"`
	ServerNames      []string `json:"server_names"`
	Dest             string   `json:"dest"`
	ServerPort       string   `json:"server_port"`
	ShortId          string   `json:"short_id"`
	ShortIds         []string `json:"short_ids"`
	PrivateKey       string   `json:"private_key"`
	Mldsa65Seed      string   `json:"mldsa65Seed"`
	Xver             uint64   `json:"xver,string"`
	CertMode         string   `json:"cert_mode"`
	CertFile         string   `json:"cert_file"`
	KeyFile          string   `json:"key_file"`
	TlsCert          string   `json:"tls_cert"`
	TlsKey           string   `json:"tls_key"`
	Provider         string   `json:"provider"`
	DNSEnv           string   `json:"dns_env"`
	RejectUnknownSni string   `json:"reject_unknown_sni"`
}

type CertInfo struct {
	CertMode         string
	CertFile         string
	KeyFile          string
	TlsCert          string
	TlsKey           string
	Email            string
	CertDomain       string
	DNSEnv           map[string]string
	Provider         string
	RejectUnknownSni bool
}

type EncSettings struct {
	Mode          string `json:"mode"`
	Ticket        string `json:"ticket"`
	ServerPadding string `json:"server_padding"`
	PrivateKey    string `json:"private_key"`
}

func (c *Client) GetNodeInfo(ctx context.Context) (node *NodeInfo, err error) {
	const path = "/api/v2/server/config"
	r, err := c.client.
		R().
		SetContext(ctx).
		SetHeader("If-None-Match", c.nodeEtag).
		ForceContentType("application/json").
		Get(path)
	if err != nil {
		return nil, c.sanitizeRequestError(err)
	}
	if r == nil {
		return nil, fmt.Errorf("received nil response")
	}

	if r.StatusCode() == 304 {
		return nil, nil
	}
	defer func() {
		if r.RawBody() != nil {
			r.RawBody().Close()
		}
	}()
	if r.StatusCode() < 200 || r.StatusCode() >= 300 {
		return nil, fmt.Errorf("get node config returned HTTP %d", r.StatusCode())
	}

	body := r.Body()
	hash := sha256.Sum256(body)
	newBodyHash := hex.EncodeToString(hash[:])
	if c.responseBodyHash == newBodyHash {
		c.nodeEtag = r.Header().Get("ETag")
		return nil, nil
	}

	node, err = parseNodeInfo(body, c.APIHost, c.NodeId)
	if err != nil {
		return nil, err
	}

	// Commit validators only after this exact body has been fully decoded and
	// semantically validated. A bad panel response must not poison the cache.
	c.responseBodyHash = newBodyHash
	c.nodeEtag = r.Header().Get("ETag")
	return node, nil
}

func parseNodeInfo(body []byte, apiHost string, nodeID int) (*NodeInfo, error) {
	cm := &CommonNode{}
	if err := json.Unmarshal(body, cm); err != nil {
		return nil, fmt.Errorf("decode node params error: %s", err)
	}
	if cm.BaseConfig == nil {
		return nil, fmt.Errorf("invalid node settings: base_config is required")
	}
	node := &NodeInfo{Id: nodeID}
	switch cm.Protocol {
	case "vmess", "trojan", "hysteria2", "tuic", "anytls", "vless":
		node.Type = cm.Protocol
		node.Security = cm.Tls
	case "shadowsocks":
		node.Type = cm.Protocol
		node.Security = None
	case "ravel":
		if err := validateRavelNode(cm); err != nil {
			return nil, fmt.Errorf("invalid ravel node settings: %w", err)
		}
		node.Type = cm.Protocol
		node.Security = Tls
	default:
		return nil, fmt.Errorf("unsupport protocol: %s", cm.Protocol)
	}
	node.Tag = fmt.Sprintf("[%s]-%s:%d", apiHost, node.Type, node.Id)
	cf := cm.TlsSettings.CertFile
	kf := cm.TlsSettings.KeyFile
	if cf == "" {
		cf = filepath.Join("/etc/v2node/", cm.Protocol+strconv.Itoa(nodeID)+".cer")
	}
	if kf == "" {
		kf = filepath.Join("/etc/v2node/", cm.Protocol+strconv.Itoa(nodeID)+".key")
	}
	cm.CertInfo = &CertInfo{
		CertMode:         cm.TlsSettings.CertMode,
		CertFile:         cf,
		KeyFile:          kf,
		Email:            "node@v2board.com",
		CertDomain:       cm.TlsSettings.PrimaryServerName(),
		DNSEnv:           make(map[string]string),
		Provider:         cm.TlsSettings.Provider,
		RejectUnknownSni: cm.TlsSettings.RejectUnknownSni == "1",
	}
	if cm.CertInfo.CertMode == "dns" && cm.TlsSettings.DNSEnv != "" {
		envs := strings.Split(cm.TlsSettings.DNSEnv, ",")
		for _, env := range envs {
			kv := strings.SplitN(env, "=", 2)
			if len(kv) == 2 {
				cm.CertInfo.DNSEnv[kv[0]] = kv[1]
			}
		}
	}
	if cm.CertInfo.CertMode == "remote" {
		cm.CertInfo.TlsCert = cm.TlsSettings.TlsCert
		cm.CertInfo.TlsKey = cm.TlsSettings.TlsKey
	}
	// set interval
	node.PushInterval = intervalToTime(cm.BaseConfig.PushInterval)
	node.PullInterval = intervalToTime(cm.BaseConfig.PullInterval)

	node.Common = cm

	return node, nil
}

func validateRavelNode(cm *CommonNode) error {
	if len(cm.Routes) != 0 {
		return fmt.Errorf("ravel direct TCP runtime does not support route rules")
	}
	if cm.ServerPort < 1 || cm.ServerPort > 65535 {
		return fmt.Errorf("server_port must be between 1 and 65535")
	}
	if cm.Ravel == nil {
		return fmt.Errorf("ravel settings are required")
	}
	ravel := cm.Ravel
	if err := validateRavelAuthority(ravel.Authority); err != nil {
		return err
	}
	if ravel.Path == "" || !strings.HasPrefix(ravel.Path, "/") {
		return fmt.Errorf("path must start with /")
	}
	if len(ravel.Path) > ravelMaxPathLength || strings.ContainsAny(ravel.Path, "\r\n") {
		return fmt.Errorf("path is invalid")
	}
	if !isPrintableASCII(ravel.GatewayGroup, 8) {
		return fmt.Errorf("gateway_group must be exactly 8 printable ASCII bytes")
	}
	if err := validateRavelMasqueradeURL(ravel.Masquerade); err != nil {
		return err
	}
	if err := ravel.Settings.validateAndNormalize(); err != nil {
		return err
	}
	return nil
}

func validateRavelAuthority(authority string) error {
	if authority == "" || len(authority) > ravelMaxAuthorityLength {
		return fmt.Errorf("authority is required and must not exceed %d bytes", ravelMaxAuthorityLength)
	}
	if _, err := ravel.NormalizeAuthority(authority); err != nil {
		return fmt.Errorf("authority is invalid: %w", err)
	}
	return nil
}

func validateRavelMasqueradeURL(rawURL string) error {
	origin, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("masquerade is not a valid URL: %w", err)
	}
	if origin.Opaque != "" || origin.Fragment != "" || origin.User != nil {
		return fmt.Errorf("masquerade URL must not contain opaque data, userinfo, or a fragment")
	}
	switch origin.Scheme {
	case "file":
		if origin.Host != "" && origin.Host != "localhost" {
			return fmt.Errorf("file masquerade URL host must be empty or localhost")
		}
		if origin.RawQuery != "" || origin.Path == "" || !strings.HasPrefix(origin.Path, "/") {
			return fmt.Errorf("file masquerade URL requires an absolute path without a query")
		}
	case "http", "https":
		if origin.Host == "" {
			return fmt.Errorf("http(s) masquerade URL requires an origin host")
		}
	default:
		return fmt.Errorf("masquerade URL scheme must be file, http, or https")
	}
	return nil
}

func isPrintableASCII(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func (s *RavelProtocolSettings) validateAndNormalize() error {
	if s.DrainTimeout == 0 {
		s.DrainTimeout = 30
	}
	if s.ClaimCapacity == 0 {
		s.ClaimCapacity = ravelDefaultClaimCapacity
	}
	if s.MaxFrameReceive == 0 {
		s.MaxFrameReceive = ravelDefaultMaxFrameReceive
	}
	if s.ChunkMin == 0 {
		s.ChunkMin = ravelDefaultChunkMin
	}
	if s.ChunkMinDelay == 0 {
		s.ChunkMinDelay = ravelDefaultChunkMinDelayMS
	}
	if s.ChunkMaxDelay == 0 {
		s.ChunkMaxDelay = ravelDefaultChunkMaxDelayMS
	}

	switch {
	case s.DrainTimeout < 1 || s.DrainTimeout >= s.ConnectionLifetime:
		return fmt.Errorf("drain_timeout must be shorter than connection_lifetime")
	case s.ClaimCapacity < 1 || s.ClaimCapacity > ravelMaxClaimCapacity:
		return fmt.Errorf("claim_capacity is outside supported bounds")
	case s.ClaimRetention < 60 || s.ClaimRetention > ravelMaxClaimRetentionSeconds:
		return fmt.Errorf("claim_retention is outside supported bounds")
	case s.ConnectionLifetime < 60 || s.ConnectionLifetime > ravelMaxLifetimeSeconds:
		return fmt.Errorf("connection_lifetime is outside supported bounds")
	case s.ConnectionLifetime > s.ClaimRetention:
		return fmt.Errorf("connection_lifetime must not exceed claim_retention")
	case s.HandshakeTimeout < 1 || s.HandshakeTimeout > ravelMaxHandshakeSeconds:
		return fmt.Errorf("handshake_timeout is outside supported bounds")
	case s.IdleTimeout < 5 || s.IdleTimeout > ravelMaxIdleSeconds:
		return fmt.Errorf("idle_timeout is outside supported bounds")
	case s.MaxMessage < ravelMinMessageSize || s.MaxMessage > ravelMaxMessageSize:
		return fmt.Errorf("max_message is outside supported bounds")
	case s.MaxFrameReceive < 9 || s.MaxFrameReceive > ravelMaxFrameReceive:
		return fmt.Errorf("max_frame_receive is outside supported bounds")
	case s.MaxStreams < 1 || s.MaxStreams > ravelMaxStreams:
		return fmt.Errorf("max_streams is outside supported bounds")
	case s.ConnectionWindow < 65536 || s.ConnectionWindow > ravelMaxConnectionWindow:
		return fmt.Errorf("connection_window is outside supported bounds")
	case s.StreamWindow < 65536 || s.StreamWindow > ravelMaxStreamWindow:
		return fmt.Errorf("stream_window is outside supported bounds")
	case s.StreamWindow > s.ConnectionWindow:
		return fmt.Errorf("stream_window must not exceed connection_window")
	case s.ChunkMin < 256 || s.ChunkMin > ravelMaxChunkSize:
		return fmt.Errorf("chunk_min is outside supported bounds")
	case s.ChunkMax < 256 || s.ChunkMax > ravelMaxChunkSize:
		return fmt.Errorf("chunk_max is outside supported bounds")
	case s.ChunkMin > s.ChunkMax:
		return fmt.Errorf("chunk_min must not exceed chunk_max")
	case int64(s.ChunkMax) > s.MaxMessage:
		return fmt.Errorf("chunk_max must not exceed max_message")
	case s.ChunkMinDelay < 1 || s.ChunkMinDelay > ravelMaxChunkDelayMS:
		return fmt.Errorf("chunk_min_delay is outside supported bounds")
	case s.ChunkMaxDelay < 1 || s.ChunkMaxDelay > ravelMaxChunkDelayMS:
		return fmt.Errorf("chunk_max_delay is outside supported bounds")
	case s.ChunkMinDelay > s.ChunkMaxDelay:
		return fmt.Errorf("chunk_min_delay must not exceed chunk_max_delay")
	}
	return nil
}

func intervalToTime(i interface{}) time.Duration {
	switch value := i.(type) {
	case nil:
		return 0
	case int:
		return time.Duration(value) * time.Second
	case int64:
		return time.Duration(value) * time.Second
	case string:
		seconds, _ := strconv.Atoi(value)
		return time.Duration(seconds) * time.Second
	case float64:
		return time.Duration(value) * time.Second
	default:
		return 0
	}
}

func (t TlsSettings) EffectiveServerNames() []string {
	if len(t.ServerNames) > 0 {
		return t.ServerNames
	}
	if t.ServerName == "" {
		return nil
	}
	return []string{t.ServerName}
}

func (t TlsSettings) EffectiveShortIds() []string {
	if len(t.ShortIds) > 0 {
		return t.ShortIds
	}
	if t.ShortId == "" {
		return nil
	}
	return []string{t.ShortId}
}

func (t TlsSettings) PrimaryServerName() string {
	serverNames := t.EffectiveServerNames()
	if len(serverNames) == 0 {
		return ""
	}
	return serverNames[0]
}
