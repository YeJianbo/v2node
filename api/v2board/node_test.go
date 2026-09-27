package panel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
)

func TestParseNodeInfoRavel(t *testing.T) {
	common := validRavelCommonNode()
	body, err := json.Marshal(common)
	if err != nil {
		t.Fatal(err)
	}

	node, err := parseNodeInfo(body, "https://panel.example", 7)
	if err != nil {
		t.Fatalf("parseNodeInfo() error = %v", err)
	}
	if node.Type != "ravel" {
		t.Fatalf("Type = %q, want ravel", node.Type)
	}
	if node.Security != Tls {
		t.Fatalf("Security = %d, want TLS", node.Security)
	}
	if node.PushInterval != 60*time.Second || node.PullInterval != 30*time.Second {
		t.Fatalf("intervals = %v/%v, want 60s/30s", node.PushInterval, node.PullInterval)
	}
	if node.Common.CertInfo.CertFile != "/cert/ravel.pem" ||
		node.Common.CertInfo.KeyFile != "/cert/ravel.key" {
		t.Fatalf("certificate paths were not taken from tls_settings: %+v", node.Common.CertInfo)
	}
	settings := node.Common.Ravel.Settings
	if settings.MaxFrameReceive != ravelDefaultMaxFrameReceive {
		t.Fatalf("MaxFrameReceive = %d, want default %d", settings.MaxFrameReceive, ravelDefaultMaxFrameReceive)
	}
	if settings.ClaimCapacity != ravelDefaultClaimCapacity {
		t.Fatalf("ClaimCapacity = %d, want default %d", settings.ClaimCapacity, ravelDefaultClaimCapacity)
	}
}

func TestParseNodeInfoRejectsInvalidRavelSettings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CommonNode)
	}{
		{
			name: "authority required",
			mutate: func(node *CommonNode) {
				node.Ravel.Authority = ""
			},
		},
		{
			name: "path starts with slash",
			mutate: func(node *CommonNode) {
				node.Ravel.Path = "ravel"
			},
		},
		{
			name: "gateway group length",
			mutate: func(node *CommonNode) {
				node.Ravel.GatewayGroup = "short"
			},
		},
		{
			name: "gateway group ascii",
			mutate: func(node *CommonNode) {
				node.Ravel.GatewayGroup = "123456中"
			},
		},
		{
			name: "port lower bound",
			mutate: func(node *CommonNode) {
				node.ServerPort = 0
			},
		},
		{
			name: "port upper bound",
			mutate: func(node *CommonNode) {
				node.ServerPort = 65536
			},
		},
		{
			name: "chunk exceeds message",
			mutate: func(node *CommonNode) {
				node.Ravel.Settings.MaxMessage = 4096
				node.Ravel.Settings.ChunkMax = 8192
			},
		},
		{
			name: "lifetime exceeds retention",
			mutate: func(node *CommonNode) {
				node.Ravel.Settings.ConnectionLifetime = 7201
				node.Ravel.Settings.ClaimRetention = 7200
			},
		},
		{
			name: "message hard limit",
			mutate: func(node *CommonNode) {
				node.Ravel.Settings.MaxMessage = ravelMaxMessageSize + 1
			},
		},
		{
			name: "frame hard limit",
			mutate: func(node *CommonNode) {
				node.Ravel.Settings.MaxFrameReceive = ravelMaxFrameReceive + 1
			},
		},
		{
			name: "stream count hard limit",
			mutate: func(node *CommonNode) {
				node.Ravel.Settings.MaxStreams = ravelMaxStreams + 1
			},
		},
		{
			name: "connection window hard limit",
			mutate: func(node *CommonNode) {
				node.Ravel.Settings.ConnectionWindow = ravelMaxConnectionWindow + 1
			},
		},
		{
			name: "stream window exceeds connection",
			mutate: func(node *CommonNode) {
				node.Ravel.Settings.StreamWindow = 2 << 20
				node.Ravel.Settings.ConnectionWindow = 1 << 20
			},
		},
		{
			name: "masquerade mode",
			mutate: func(node *CommonNode) {
				node.Ravel.Masquerade = "tcp"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			common := validRavelCommonNode()
			test.mutate(common)
			body, err := json.Marshal(common)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseNodeInfo(body, "https://panel.example", 7); err == nil {
				t.Fatal("parseNodeInfo() accepted invalid Ravel settings")
			}
		})
	}
}

func TestParseNodeInfoNonRavelRegression(t *testing.T) {
	common := &CommonNode{
		Protocol:   "vmess",
		ServerPort: 443,
		Tls:        Reality,
		BaseConfig: &BaseConfig{
			PushInterval: 60,
			PullInterval: 60,
		},
	}
	body, err := json.Marshal(common)
	if err != nil {
		t.Fatal(err)
	}

	node, err := parseNodeInfo(body, "https://panel.example", 9)
	if err != nil {
		t.Fatalf("parseNodeInfo() error = %v", err)
	}
	if node.Type != "vmess" || node.Security != Reality || node.Common.Ravel != nil {
		t.Fatalf("non-Ravel node changed unexpectedly: %+v", node)
	}
}

func TestGetNodeInfoBadBodyDoesNotCommitHashOrETag(t *testing.T) {
	goodBody, err := json.Marshal(validRavelCommonNode())
	if err != nil {
		t.Fatal(err)
	}
	semanticBadNode := validRavelCommonNode()
	semanticBadNode.Ravel.Path = "missing-leading-slash"
	semanticBadBody, err := json.Marshal(semanticBadNode)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		badBody []byte
	}{
		{name: "malformed JSON", badBody: []byte(`{"protocol":"ravel"`)},
		{name: "semantic validation", badBody: semanticBadBody},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requestCount := 0
			var secondIfNoneMatch string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCount++
				if requestCount == 1 {
					w.Header().Set("ETag", `"good"`)
					_, _ = w.Write(goodBody)
					return
				}
				secondIfNoneMatch = r.Header.Get("If-None-Match")
				w.Header().Set("ETag", `"bad"`)
				_, _ = w.Write(test.badBody)
			}))
			defer server.Close()

			client := &Client{
				client:  resty.New().SetBaseURL(server.URL),
				APIHost: server.URL,
				NodeId:  1,
			}
			if _, err := client.GetNodeInfo(context.Background()); err != nil {
				t.Fatalf("first GetNodeInfo() error = %v", err)
			}
			goodHash := client.responseBodyHash
			if goodHash == "" || client.nodeEtag != `"good"` {
				t.Fatalf("valid response validators not committed: hash=%q etag=%q", goodHash, client.nodeEtag)
			}

			if _, err := client.GetNodeInfo(context.Background()); err == nil {
				t.Fatal("second GetNodeInfo() accepted invalid body")
			}
			if secondIfNoneMatch != `"good"` {
				t.Fatalf("If-None-Match = %q, want previous valid ETag", secondIfNoneMatch)
			}
			if client.responseBodyHash != goodHash || client.nodeEtag != `"good"` {
				t.Fatalf("bad response poisoned validators: hash=%q etag=%q", client.responseBodyHash, client.nodeEtag)
			}
			badHash := sha256.Sum256(test.badBody)
			if client.responseBodyHash == hex.EncodeToString(badHash[:]) {
				t.Fatal("bad body hash was committed")
			}
		})
	}
}

func validRavelCommonNode() *CommonNode {
	return &CommonNode{
		Protocol:   "ravel",
		ListenIP:   "0.0.0.0",
		ServerPort: 443,
		Tls:        None,
		TlsSettings: TlsSettings{
			ServerName: "ravel.example.com",
			CertMode:   "file",
			CertFile:   "/cert/ravel.pem",
			KeyFile:    "/cert/ravel.key",
		},
		BaseConfig: &BaseConfig{
			PushInterval: 60,
			PullInterval: 30,
		},
		Ravel: &RavelSettings{
			Authority:    "ravel.example.com:443",
			Path:         "/ravel/v1",
			GatewayGroup: "gw000001",
			Masquerade:   "https://origin.example/cover",
			Settings: RavelProtocolSettings{
				ClaimRetention:     7200,
				ConnectionLifetime: 3600,
				HandshakeTimeout:   10,
				IdleTimeout:        300,
				MaxMessage:         1 << 20,
				MaxStreams:         256,
				ConnectionWindow:   4 << 20,
				StreamWindow:       1 << 20,
				ChunkMax:           65536,
			},
		},
	}
}
