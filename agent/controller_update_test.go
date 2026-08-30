package agent

import (
	"strings"
	"testing"
)

func TestClearUpdateRequestOnlyClearsMatchingRequest(t *testing.T) {
	controller := &Controller{autoUpdate: AutoUpdateConfig{
		RequestID:     "request-1",
		TargetVersion: "v0.5.4-ravel4",
		RequestedAt:   123,
	}}

	controller.ClearUpdateRequest("request-2")
	if controller.AutoUpdateConfig().RequestID != "request-1" {
		t.Fatal("a different update request was cleared")
	}

	controller.ClearUpdateRequest("request-1")
	config := controller.AutoUpdateConfig()
	if config.RequestID != "" || config.TargetVersion != "" || config.RequestedAt != 0 {
		t.Fatalf("matching update request was not cleared: %+v", config)
	}
}

func TestTruncateStatusMessageKeepsValidUTF8(t *testing.T) {
	message := strings.Repeat("错", 300)
	truncated := truncateStatusMessage(message, 255)
	if len([]rune(truncated)) != 255 {
		t.Fatalf("unexpected truncated length: %d", len([]rune(truncated)))
	}
}
