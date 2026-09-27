package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/wyx2685/v2node/conf"
)

func TestPanelAuthCompatibilityAndCapability(t *testing.T) {
	body, err := json.Marshal(validRavelCommonNode())
	if err != nil {
		t.Fatal(err)
	}
	const token = "panel-token"
	var authorization string
	var queryToken string
	var userAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		queryToken = r.URL.Query().Get("token")
		userAgent = r.Header.Get("User-Agent")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	retryCount := 0
	client, err := New(&conf.NodeConfig{
		APIHost:    server.URL,
		NodeID:     1,
		Key:        token,
		RetryCount: &retryCount,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := client.GetNodeInfo(context.Background()); err != nil {
		t.Fatalf("GetNodeInfo() error = %v", err)
	}
	if authorization != "Bearer "+token {
		t.Fatalf("Authorization = %q", authorization)
	}
	if queryToken != token {
		t.Fatalf("legacy query token = %q, want %q", queryToken, token)
	}
	if !strings.Contains(userAgent, ravelServerCapability) {
		t.Fatalf("User-Agent %q does not declare %s", userAgent, ravelServerCapability)
	}
	if !strings.Contains(panelUserAgent(), ravelServerCapability) {
		t.Fatalf("panelUserAgent() does not declare %s", ravelServerCapability)
	}
}

func TestRedactPanelError(t *testing.T) {
	token := "secret token/with-symbols"
	message := fmt.Errorf(
		"request failed: token=%s raw=%s",
		url.QueryEscape(token),
		token,
	)
	redacted := redactPanelError(message, token)
	if strings.Contains(redacted, token) || strings.Contains(redacted, url.QueryEscape(token)) {
		t.Fatalf("redacted error still contains token: %q", redacted)
	}
	if !strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("redacted error = %q", redacted)
	}

	cause := errors.New("transport failed for token=" + token)
	client := &Client{Token: token}
	wrapped := client.sanitizeRequestError(cause)
	if !errors.Is(wrapped, cause) {
		t.Fatal("sanitized error did not retain its cause")
	}
	if strings.Contains(wrapped.Error(), token) {
		t.Fatalf("sanitized request error still contains token: %q", wrapped)
	}
}
