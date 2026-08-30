package agent

import (
	"path/filepath"
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

func TestConfigApplyStatePreservesPartialAndFailedRevisions(t *testing.T) {
	managedPath := filepath.Join(t.TempDir(), "managed-nodes.json")
	controller := &Controller{ManagedFile: managedPath}
	controller.setDesiredRevision("revision-1")
	controller.MarkConfigApply("partial", "one node failed")

	state := controller.currentApplyState()
	if state.AppliedRevision != "revision-1" || state.Status != "partial" || state.Error == "" {
		t.Fatalf("unexpected partial apply state: %+v", state)
	}

	reloaded := &Controller{ManagedFile: managedPath}
	reloaded.LoadApplyState()
	state = reloaded.currentApplyState()
	if state.AppliedRevision != "revision-1" || state.Status != "partial" {
		t.Fatalf("apply state was not persisted: %+v", state)
	}

	reloaded.setDesiredRevision("revision-2")
	reloaded.MarkConfigApply("failed", "new revision failed")
	state = reloaded.currentApplyState()
	if state.AppliedRevision != "revision-1" || state.DesiredRevision != "revision-2" || state.Status != "failed" {
		t.Fatalf("failed revision replaced the last applied revision: %+v", state)
	}
}
