package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wyx2685/v2node/node"
)

func TestReloadKeepsCurrentRuntimeWhenNewConfigCannotBePrepared(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"Log":{"Level":"warning","Output":"","Access":"none"},"Nodes":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reloadCh := make(chan struct{}, 1)
	bundle, _, err := startRuntime(configPath, reloadCh, node.RuntimeSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = bundle.nodes.Close()
		_ = closeCoreSafely(bundle.core)
	}()
	originalCore := bundle.core.Server

	if err := os.WriteFile(configPath, []byte(`{"Nodes":`), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := reloadRuntime(configPath, "", reloadCh, node.RuntimeSnapshot{}, &bundle)
	if err == nil {
		t.Fatal("invalid configuration was accepted")
	}
	if report.Status != "failed" {
		t.Fatalf("unexpected apply report: %+v", report)
	}
	if bundle.core.Server != originalCore {
		t.Fatal("current runtime was replaced before the new configuration was prepared")
	}
}
