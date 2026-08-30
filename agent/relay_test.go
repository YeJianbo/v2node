package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestStartExistingKeepsRunningRelayProcess(t *testing.T) {
	manager := NewRelayManager("unused", filepath.Join(t.TempDir(), "missing.json"))
	manager.cmd = &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}
	manager.done = make(chan struct{})

	if err := manager.StartExisting(); err != nil {
		t.Fatal(err)
	}
	if manager.cmd == nil {
		t.Fatal("running relay process was discarded")
	}
}

func TestNormalizeRelayRulesDeduplicatesListeners(t *testing.T) {
	rules := []RelayRule{
		{ListenPort: 10000, TargetHost: "192.0.2.1", TargetPort: 20000, Protocol: "tcp"},
		{ListenPort: 10000, TargetHost: "192.0.2.2", TargetPort: 30000, Protocol: "tcp"},
		{ListenPort: 10000, TargetHost: "192.0.2.2", TargetPort: 30000, Protocol: "udp"},
	}
	nodes := normalizeRelayRules(rules)
	if len(nodes) != 2 {
		t.Fatalf("expected one TCP and one UDP listener, got %v", nodes)
	}
}
