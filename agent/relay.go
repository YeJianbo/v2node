package agent

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const gostVersion = "2.11.2"

type flexiblePort int

func (p *flexiblePort) UnmarshalJSON(raw []byte) error {
	var number int
	if json.Unmarshal(raw, &number) == nil {
		*p = flexiblePort(number)
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	value, err := strconv.Atoi(text)
	if err != nil {
		return err
	}
	*p = flexiblePort(value)
	return nil
}

type flexibleStrings []string

func (s *flexibleStrings) UnmarshalJSON(raw []byte) error {
	var values []string
	if json.Unmarshal(raw, &values) == nil {
		*s = values
		return nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*s = strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == '/' || r == '|' || r == '+' || r == ' '
	})
	return nil
}

type RelayConfig struct {
	Rules []RelayRule `json:"rules"`
}

type RelayRule struct {
	ListenHost    string          `json:"listen_host"`
	ListenHostAlt string          `json:"listenHost"`
	LocalHost     string          `json:"local_host"`
	LocalHostAlt  string          `json:"localHost"`
	ListenPort    flexiblePort    `json:"listen_port"`
	ListenPortAlt flexiblePort    `json:"listenPort"`
	LocalPort     flexiblePort    `json:"local_port"`
	LocalPortAlt  flexiblePort    `json:"localPort"`
	TargetHost    string          `json:"target_host"`
	TargetHostAlt string          `json:"targetHost"`
	RemoteHost    string          `json:"remote_host"`
	RemoteHostAlt string          `json:"remoteHost"`
	Host          string          `json:"host"`
	TargetPort    flexiblePort    `json:"target_port"`
	TargetPortAlt flexiblePort    `json:"targetPort"`
	RemotePort    flexiblePort    `json:"remote_port"`
	RemotePortAlt flexiblePort    `json:"remotePort"`
	Port          flexiblePort    `json:"port"`
	Protocols     flexibleStrings `json:"protocols"`
	Protocol      string          `json:"protocol"`
	Type          string          `json:"type"`
}

type gostConfig struct {
	Debug      bool     `json:"Debug"`
	Retries    int      `json:"Retries"`
	ServeNodes []string `json:"ServeNodes"`
}

type RelayManager struct {
	BinaryPath string
	ConfigPath string

	mu        sync.Mutex
	cmd       *exec.Cmd
	done      chan struct{}
	version   string
	ruleCount int
	lastError string
	migrated  bool
}

func NewRelayManager(binaryPath, configPath string) *RelayManager {
	return &RelayManager{BinaryPath: binaryPath, ConfigPath: configPath}
}

func (m *RelayManager) Sync(config RelayConfig) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if runtime.GOOS != "linux" {
		m.lastError = "relay management is only supported on Linux"
		return false, fmt.Errorf("%s", m.lastError)
	}
	if !m.migrated {
		m.cleanupLegacyLocked()
		m.migrated = true
	}

	nodes := normalizeRelayRules(config.Rules)
	if len(nodes) == 0 {
		changed := m.cmd != nil
		m.stopLocked()
		if err := os.Remove(m.ConfigPath); err == nil {
			changed = true
		}
		m.ruleCount = 0
		m.lastError = ""
		return changed, nil
	}
	if err := m.ensureBinaryLocked(); err != nil {
		m.lastError = err.Error()
		return false, err
	}

	raw, err := json.MarshalIndent(gostConfig{ServeNodes: nodes}, "", "  ")
	if err != nil {
		m.lastError = err.Error()
		return false, err
	}
	raw = append(raw, '\n')
	current, _ := os.ReadFile(m.ConfigPath)
	changed := !bytes.Equal(current, raw)
	if changed {
		if err := atomicWrite(m.ConfigPath, raw, 0o600); err != nil {
			m.lastError = err.Error()
			return false, err
		}
		m.stopLocked()
	}
	if m.cmd == nil {
		if err := m.startAndVerifyLocked(); err != nil {
			if changed {
				m.restoreRelayConfigLocked(current)
			}
			m.lastError = err.Error()
			return changed, err
		}
	}
	m.ruleCount = len(nodes)
	m.lastError = ""
	return changed, nil
}

func (m *RelayManager) StartExisting() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cmd != nil && m.cmd.Process != nil {
		if m.done == nil {
			return nil
		}
		select {
		case <-m.done:
			m.cmd = nil
			m.done = nil
		default:
			return nil
		}
	}

	raw, err := os.ReadFile(m.ConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var config gostConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return fmt.Errorf("decode existing relay configuration: %w", err)
	}
	if len(config.ServeNodes) == 0 {
		return nil
	}
	if err := m.ensureBinaryLocked(); err != nil {
		m.lastError = err.Error()
		return err
	}
	if err := m.startAndVerifyLocked(); err != nil {
		m.lastError = err.Error()
		return err
	}
	m.ruleCount = len(config.ServeNodes)
	m.lastError = ""
	return nil
}

func (m *RelayManager) Status() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := "inactive"
	if m.cmd != nil && m.cmd.Process != nil {
		status = "active"
	} else if m.lastError != "" {
		status = "failed"
	}
	return map[string]any{
		"gost_status":     status,
		"gost_version":    m.version,
		"gost_rule_count": m.ruleCount,
		"gost_error":      m.lastError,
	}
}

func (m *RelayManager) ensureBinaryLocked() error {
	if info, err := os.Stat(m.BinaryPath); err == nil && info.Mode()&0o111 != 0 {
		m.readVersionLocked()
		return nil
	}
	arch := map[string]string{"amd64": "amd64", "arm64": "arm64", "arm": "armv7", "386": "386"}[runtime.GOARCH]
	if arch == "" {
		return fmt.Errorf("unsupported GOST architecture %s", runtime.GOARCH)
	}
	url := fmt.Sprintf("https://github.com/ginuerzh/gost/releases/download/v%s/gost-linux-%s-%s.gz", gostVersion, arch, gostVersion)
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("download GOST: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download GOST: HTTP %d", resp.StatusCode)
	}
	reader, err := gzip.NewReader(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("open GOST archive: %w", err)
	}
	defer reader.Close()
	if err := os.MkdirAll(filepath.Dir(m.BinaryPath), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(m.BinaryPath), ".gost-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = io.Copy(tmp, io.LimitReader(reader, 64<<20)); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, m.BinaryPath); err != nil {
		return err
	}
	m.readVersionLocked()
	return nil
}

func (m *RelayManager) readVersionLocked() {
	output, err := exec.Command(m.BinaryPath, "-V").CombinedOutput()
	if err == nil {
		m.version = strings.TrimSpace(string(output))
	}
}

func (m *RelayManager) startLocked() error {
	cmd := exec.Command(m.BinaryPath, "-C", m.ConfigPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start GOST: %w", err)
	}
	done := make(chan struct{})
	m.cmd = cmd
	m.done = done
	go func() {
		err := cmd.Wait()
		close(done)
		m.mu.Lock()
		if m.cmd == cmd {
			m.cmd = nil
			m.done = nil
			if err != nil {
				m.lastError = err.Error()
			}
		}
		m.mu.Unlock()
	}()
	return nil
}

func (m *RelayManager) startAndVerifyLocked() error {
	if err := m.startLocked(); err != nil {
		return err
	}
	done := m.done
	if done == nil {
		return fmt.Errorf("GOST did not expose a process handle")
	}
	select {
	case <-done:
		m.cmd = nil
		m.done = nil
		return fmt.Errorf("GOST exited immediately; check relay ports and configuration")
	case <-time.After(500 * time.Millisecond):
		return nil
	}
}

func (m *RelayManager) restoreRelayConfigLocked(previous []byte) {
	m.stopLocked()
	if len(previous) == 0 {
		_ = os.Remove(m.ConfigPath)
		return
	}
	if err := atomicWrite(m.ConfigPath, previous, 0o600); err != nil {
		m.lastError = "restore previous relay configuration: " + err.Error()
		return
	}
	if err := m.startAndVerifyLocked(); err != nil {
		m.lastError = "restore previous relay process: " + err.Error()
	}
}

func (m *RelayManager) stopLocked() {
	if m.cmd == nil || m.cmd.Process == nil {
		return
	}
	done := m.done
	_ = m.cmd.Process.Kill()
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	m.cmd = nil
	m.done = nil
}

func (m *RelayManager) cleanupLegacyLocked() {
	legacy := false
	for _, path := range []string{"/etc/systemd/system/gost.service", "/etc/init.d/gost", "/etc/gost/config.json"} {
		if _, err := os.Stat(path); err == nil {
			legacy = true
			break
		}
	}
	if !legacy {
		return
	}
	_ = exec.Command("systemctl", "disable", "--now", "gost.service").Run()
	_ = exec.Command("rc-service", "gost", "stop").Run()
	_ = exec.Command("rc-update", "del", "gost", "default").Run()
	_ = exec.Command("pkill", "-x", "gost").Run()
	_ = os.Remove("/etc/systemd/system/gost.service")
	_ = os.Remove("/etc/init.d/gost")
	_ = os.Remove("/etc/gost/config.json")
	_ = os.Remove("/etc/gost/config.json.last-good")
	_ = os.Remove("/usr/bin/gost")
	_ = os.Remove("/etc/gost")
	_ = exec.Command("systemctl", "daemon-reload").Run()
}

func normalizeRelayRules(rules []RelayRule) []string {
	nodes := make([]string, 0, len(rules)*2)
	seenListeners := map[string]bool{}
	for _, rule := range rules {
		listenHost := firstString(rule.ListenHost, rule.ListenHostAlt, rule.LocalHost, rule.LocalHostAlt, "0.0.0.0")
		targetHost := firstString(rule.TargetHost, rule.TargetHostAlt, rule.RemoteHost, rule.RemoteHostAlt, rule.Host)
		listenPort := firstPort(rule.ListenPort, rule.ListenPortAlt, rule.LocalPort, rule.LocalPortAlt)
		targetPort := firstPort(rule.TargetPort, rule.TargetPortAlt, rule.RemotePort, rule.RemotePortAlt, rule.Port)
		if listenPort < 1 || listenPort > 65535 || targetPort < 1 || targetPort > 65535 || targetHost == "" {
			continue
		}
		protocols := []string(rule.Protocols)
		if len(protocols) == 0 {
			protocols = []string{firstString(rule.Protocol, rule.Type)}
		}
		for _, protocol := range normalizeProtocols(protocols) {
			listenAddress := ":" + strconv.Itoa(listenPort)
			if listenHost != "" && listenHost != "0.0.0.0" && listenHost != "::" {
				listenAddress = net.JoinHostPort(strings.Trim(listenHost, "[]"), strconv.Itoa(listenPort))
			}
			listener := protocol + "://" + listenAddress
			node := listener + "/" + net.JoinHostPort(strings.Trim(targetHost, "[]"), strconv.Itoa(targetPort))
			if !seenListeners[listener] {
				seenListeners[listener] = true
				nodes = append(nodes, node)
			}
		}
	}
	return nodes
}

func normalizeProtocols(values []string) []string {
	result := make([]string, 0, 2)
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		items := []string{value}
		if value == "all" || value == "both" || value == "tcpudp" {
			items = []string{"tcp", "udp"}
		}
		for _, item := range items {
			if (item == "tcp" || item == "udp") && !seen[item] {
				seen[item] = true
				result = append(result, item)
			}
		}
	}
	return result
}

func firstString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstPort(values ...flexiblePort) int {
	for _, value := range values {
		if value > 0 {
			return int(value)
		}
	}
	return 0
}
