package agent

import (
	"bufio"
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

const (
	gostVersion              = "2.11.2"
	relayTargetProbeInterval = 30 * time.Second
	relayTargetProbeTimeout  = 2 * time.Second
)

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

type relayListener struct {
	Protocol   string
	ListenHost string
	ListenPort int
	TargetHost string
	TargetPort int
}

type relayTargetHealth struct {
	Status    string
	Message   string
	CheckedAt int64
}

type RelayRuleHealth struct {
	Protocol        string `json:"protocol"`
	ListenHost      string `json:"listen_host"`
	ListenPort      int    `json:"listen_port"`
	Status          string `json:"status"`
	ListenOK        bool   `json:"listen_ok"`
	Message         string `json:"message"`
	CheckedAt       int64  `json:"checked_at"`
	TargetStatus    string `json:"target_status,omitempty"`
	TargetOK        *bool  `json:"target_ok,omitempty"`
	TargetMessage   string `json:"target_message,omitempty"`
	TargetCheckedAt int64  `json:"target_checked_at,omitempty"`
}

type RelayManager struct {
	BinaryPath string
	ConfigPath string

	mu                 sync.Mutex
	cmd                *exec.Cmd
	done               chan struct{}
	version            string
	ruleCount          int
	lastError          string
	migrated           bool
	listeners          []relayListener
	targetHealth       map[string]relayTargetHealth
	targetProbeAt      time.Time
	targetProbeRunning bool
}

func NewRelayManager(binaryPath, configPath string) *RelayManager {
	return &RelayManager{
		BinaryPath:   binaryPath,
		ConfigPath:   configPath,
		targetHealth: make(map[string]relayTargetHealth),
	}
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
		m.listeners = nil
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
	m.listeners = relayListenersFromNodes(nodes)
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
	m.listeners = relayListenersFromNodes(config.ServeNodes)
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
	m.scheduleTargetProbesLocked(status)
	return map[string]any{
		"gost_status":      status,
		"gost_version":     m.version,
		"gost_rule_count":  m.ruleCount,
		"gost_error":       m.lastError,
		"gost_rule_health": m.ruleHealthLocked(status),
	}
}

func (m *RelayManager) ruleHealthLocked(status string) []RelayRuleHealth {
	if len(m.listeners) == 0 {
		return []RelayRuleHealth{}
	}
	listening := map[string]bool{}
	if status == "active" && m.cmd != nil && m.cmd.Process != nil {
		listening = processListeningSockets(m.cmd.Process.Pid)
	}
	checkedAt := time.Now().Unix()
	health := make([]RelayRuleHealth, 0, len(m.listeners))
	for _, listener := range m.listeners {
		listenOK := listening[listener.Protocol+":"+strconv.Itoa(listener.ListenPort)]
		message := ""
		ruleStatus := "running"
		if status != "active" {
			ruleStatus = status
			message = m.lastError
		} else if !listenOK {
			ruleStatus = "failed"
			message = "GOST 进程未持有该监听端口"
		}
		record := RelayRuleHealth{
			Protocol:   listener.Protocol,
			ListenHost: listener.ListenHost,
			ListenPort: listener.ListenPort,
			Status:     ruleStatus,
			ListenOK:   listenOK,
			Message:    message,
			CheckedAt:  checkedAt,
		}
		if listener.Protocol != "tcp" || listener.TargetHost == "" || listener.TargetPort <= 0 {
			record.TargetStatus = "not_applicable"
		} else if target, ok := m.targetHealth[relayTargetHealthKey(listener)]; ok {
			targetOK := target.Status == "reachable"
			record.TargetStatus = target.Status
			record.TargetOK = &targetOK
			record.TargetMessage = target.Message
			record.TargetCheckedAt = target.CheckedAt
		} else {
			record.TargetStatus = "pending"
		}
		health = append(health, record)
	}
	return health
}

func (m *RelayManager) scheduleTargetProbesLocked(status string) {
	if status != "active" || m.targetProbeRunning {
		return
	}
	now := time.Now()
	if !m.targetProbeAt.IsZero() && now.Sub(m.targetProbeAt) < relayTargetProbeInterval {
		return
	}
	listeners := make([]relayListener, 0, len(m.listeners))
	for _, listener := range m.listeners {
		if listener.Protocol == "tcp" && listener.TargetHost != "" && listener.TargetPort > 0 {
			listeners = append(listeners, listener)
		}
	}
	if len(listeners) == 0 {
		return
	}
	m.targetProbeAt = now
	m.targetProbeRunning = true
	go func() {
		results := probeRelayTargets(listeners)
		m.mu.Lock()
		for key, result := range results {
			m.targetHealth[key] = result
		}
		m.targetProbeRunning = false
		m.mu.Unlock()
	}()
}

func probeRelayTargets(listeners []relayListener) map[string]relayTargetHealth {
	results := make(map[string]relayTargetHealth, len(listeners))
	var resultMu sync.Mutex
	var group sync.WaitGroup
	semaphore := make(chan struct{}, 8)
	for _, listener := range listeners {
		listener := listener
		group.Add(1)
		go func() {
			defer group.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			result := probeRelayTarget(listener, relayTargetProbeTimeout)
			resultMu.Lock()
			results[relayTargetHealthKey(listener)] = result
			resultMu.Unlock()
		}()
	}
	group.Wait()
	return results
}

func probeRelayTarget(listener relayListener, timeout time.Duration) relayTargetHealth {
	result := relayTargetHealth{Status: "unreachable", CheckedAt: time.Now().Unix()}
	connection, err := net.DialTimeout(
		"tcp",
		net.JoinHostPort(strings.Trim(listener.TargetHost, "[]"), strconv.Itoa(listener.TargetPort)),
		timeout,
	)
	if err != nil {
		result.Message = err.Error()
		return result
	}
	_ = connection.Close()
	result.Status = "reachable"
	result.CheckedAt = time.Now().Unix()
	return result
}

func relayTargetHealthKey(listener relayListener) string {
	return strings.Join([]string{
		listener.Protocol,
		strings.ToLower(strings.Trim(listener.ListenHost, "[]")),
		strconv.Itoa(listener.ListenPort),
		strings.ToLower(strings.Trim(listener.TargetHost, "[]")),
		strconv.Itoa(listener.TargetPort),
	}, "|")
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

func relayListenersFromNodes(nodes []string) []relayListener {
	listeners := make([]relayListener, 0, len(nodes))
	seen := map[string]bool{}
	for _, node := range nodes {
		parts := strings.SplitN(node, "://", 2)
		if len(parts) != 2 {
			continue
		}
		protocol := strings.ToLower(strings.TrimSpace(parts[0]))
		endpoints := strings.SplitN(parts[1], "/", 2)
		endpoint := endpoints[0]
		host, portText, err := net.SplitHostPort(endpoint)
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		host = strings.Trim(strings.TrimSpace(host), "[]")
		if host == "" {
			host = "0.0.0.0"
		}
		targetHost := ""
		targetPort := 0
		if len(endpoints) == 2 {
			parsedHost, parsedPort, targetErr := net.SplitHostPort(endpoints[1])
			if targetErr == nil {
				targetHost = strings.Trim(strings.TrimSpace(parsedHost), "[]")
				targetPort, _ = strconv.Atoi(parsedPort)
			}
		}
		key := protocol + "://" + host + ":" + strconv.Itoa(port)
		if seen[key] {
			continue
		}
		seen[key] = true
		listeners = append(listeners, relayListener{
			Protocol:   protocol,
			ListenHost: host,
			ListenPort: port,
			TargetHost: targetHost,
			TargetPort: targetPort,
		})
	}
	return listeners
}

func processListeningSockets(pid int) map[string]bool {
	result := map[string]bool{}
	if runtime.GOOS != "linux" || pid <= 0 {
		return result
	}
	inodes := processSocketInodes(pid)
	if len(inodes) == 0 {
		return result
	}
	for _, table := range []struct {
		path     string
		protocol string
		state    string
	}{
		{"/proc/net/tcp", "tcp", "0A"},
		{"/proc/net/tcp6", "tcp", "0A"},
		{"/proc/net/udp", "udp", "07"},
		{"/proc/net/udp6", "udp", "07"},
	} {
		file, err := os.Open(table.path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 10 || strings.ToUpper(fields[3]) != table.state || !inodes[fields[9]] {
				continue
			}
			address := strings.Split(fields[1], ":")
			if len(address) != 2 {
				continue
			}
			port, err := strconv.ParseInt(address[1], 16, 32)
			if err == nil {
				result[table.protocol+":"+strconv.Itoa(int(port))] = true
			}
		}
		_ = file.Close()
	}
	return result
}

func processSocketInodes(pid int) map[string]bool {
	inodes := map[string]bool{}
	entries, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "fd"))
	if err != nil {
		return inodes
	}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", entry.Name()))
		if err != nil || !strings.HasPrefix(target, "socket:[") || !strings.HasSuffix(target, "]") {
			continue
		}
		inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
		if inode != "" {
			inodes[inode] = true
		}
	}
	return inodes
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
