package agent

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
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

func TestRelayListenersIncludeTargetEndpoint(t *testing.T) {
	listeners := relayListenersFromNodes([]string{"tcp://:10000/[2001:db8::1]:20000"})
	if len(listeners) != 1 {
		t.Fatalf("listeners = %#v", listeners)
	}
	listener := listeners[0]
	if listener.ListenHost != "0.0.0.0" || listener.ListenPort != 10000 || listener.TargetHost != "2001:db8::1" || listener.TargetPort != 20000 {
		t.Fatalf("unexpected listener = %#v", listener)
	}
}

func TestProbeRelayTargetReportsReachableTCPDestination(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = connection.Close()
		}
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	result := probeRelayTarget(relayListener{
		Protocol:   "tcp",
		ListenHost: "0.0.0.0",
		ListenPort: 10000,
		TargetHost: "127.0.0.1",
		TargetPort: port,
	}, time.Second)
	if result.Status != "reachable" || result.CheckedAt <= 0 {
		t.Fatalf("probe result = %#v", result)
	}
}
