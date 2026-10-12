package core

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/YeJianbo/ravel-go"
	"github.com/YeJianbo/ravel-go/carrier"
	"github.com/YeJianbo/ravel-go/client"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/limiter"
)

func TestRavelRuntimeRotationRevocationAndMetering(t *testing.T) {
	if !carrier.ExtendedConnectEnabled() {
		t.Fatal("run with GODEBUG=http2xconnect=1")
	}
	certificate := httptest.NewTLSServer(http.NotFoundHandler())
	tlsConfig := certificate.TLS.Clone()
	certificate.Close()
	var group [8]byte
	copy(group[:], "group-01")
	runtime := &ravelRuntime{
		config: ravelRuntimeConfig{listen: "127.0.0.1:0", tlsConfig: tlsConfig, authority: "example.com", path: "/socket", gatewayGroup: group,
			fallback: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("cover")) }),
			settings: panel.RavelProtocolSettings{ClaimRetention: 60, ConnectionLifetime: 60, DrainTimeout: 1, HandshakeTimeout: 1}},
		dialer: &net.Dialer{Timeout: time.Second}, users: make(map[string]panel.UserInfo),
		activeAuth: make(map[ravel.AuthContext]int), authUID: make(map[ravel.AuthContext]int),
	}
	if err := runtime.replaceUsers(nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtime.close(); err != nil {
			t.Error(err)
		}
	})
	address := runtime.listener.Addr().String()
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Timeout: 2 * time.Second}
	t.Cleanup(httpClient.CloseIdleConnections)
	response, err := httpClient.Get("https://" + address + "/")
	if err != nil {
		t.Fatal(err)
	}
	cover, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(cover) != "cover" {
		t.Fatal("empty-user node lost its cover")
	}
	now := time.Now().Unix()
	makeCredential := func(version uint32) panel.RavelCredential {
		return panel.RavelCredential{CredentialID: hex.EncodeToString(bytes.Repeat([]byte{byte(version)}, 16)),
			CapabilityKey: hex.EncodeToString(bytes.Repeat([]byte{byte(version + 10)}, 32)), KeyVersion: version,
			GatewayGroup: "group-01", NotBefore: now - 1, NotAfter: now + 3600}
	}
	first, second := makeCredential(1), makeCredential(2)
	user := panel.UserInfo{Id: 42, Uuid: "user-42", RavelCredentials: []panel.RavelCredential{first}}
	limiter.Init()
	runtime.limiter = limiter.AddLimiter("ravel", "rotation-test", []panel.UserInfo{user}, map[int]int{})
	runtime.config.tag = "rotation-test"
	if err := runtime.replaceUsers([]panel.UserInfo{user}); err != nil {
		t.Fatal(err)
	}
	newClient := func(encoded panel.RavelCredential) *client.Client {
		credential, err := ravel.ParseCredential(encoded.CredentialID, encoded.CapabilityKey, encoded.KeyVersion, 0, "group-01", encoded.NotBefore, encoded.NotAfter)
		if err != nil {
			t.Fatal(err)
		}
		dial, err := client.NewStandardTLSDialer(client.StandardTLSDialerConfig{ServerAddress: address,
			TLSConfig: &tls.Config{ServerName: "example.com", InsecureSkipVerify: true}, DialContext: (&net.Dialer{}).DialContext})
		if err != nil {
			t.Fatal(err)
		}
		c, err := client.New(client.Config{Authority: "example.com", Path: "/socket", DialTLSContext: dial, Credential: credential, HandshakeTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	target := netip.MustParseAddrPort(echo.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	oldClient := newClient(first)
	conn, err := oldClient.Open(ctx, target.Addr().String(), target.Port())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	payload := bytes.Repeat([]byte("metered"), 20000)
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, reply) {
		t.Fatal("payload changed")
	}
	overlap := first
	overlap.NotAfter = time.Now().Unix() + 3
	user.Uuid = "rotated-user-42"
	user.RavelCredentials = []panel.RavelCredential{second, overlap}
	if err := runtime.replaceUsers([]panel.UserInfo{user}); err != nil {
		t.Fatal(err)
	}
	runtime.limiter.ReplaceRavelUsers(runtime.config.tag, []panel.UserInfo{user})
	newer := newClient(second)
	next, err := newer.Open(ctx, target.Addr().String(), target.Port())
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
	var up, down int64
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, traffic := range runtime.trafficSlice(0) {
			if traffic.UID != 42 {
				t.Fatalf("wrong traffic owner %d", traffic.UID)
			}
			up += traffic.Upload
			down += traffic.Download
		}
		if up == int64(len(payload)) && down == up {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if up != int64(len(payload)) || down != up {
		t.Fatalf("traffic = %d/%d, want %d each", up, down, len(payload))
	}
	// No further panel update: local expiry must close an already-open old carrier.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("expired carrier was not closed")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("local credential expiry timed out")
	}
	if _, err := oldClient.Open(ctx, target.Addr().String(), target.Port()); err == nil {
		t.Fatal("expired credential still connected")
	}
	fresh, err := newer.Open(ctx, target.Addr().String(), target.Port())
	if err != nil {
		t.Fatal("new credential expired with the old one", err)
	}
	fresh.Close()
	user.RavelCredentials = []panel.RavelCredential{second}
	if err := runtime.replaceUsers([]panel.UserInfo{user}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.replaceUsers(nil); err != nil {
		t.Fatal(err)
	}
	if runtime.listener.Addr().String() != address {
		t.Fatal("empty update restarted listener")
	}
	if err := runtime.replaceUsers([]panel.UserInfo{user}); err != nil {
		t.Fatal(err)
	}
	last, err := newClient(second).Open(ctx, target.Addr().String(), target.Port())
	if err != nil {
		t.Fatal(err)
	}
	last.Close()
}
