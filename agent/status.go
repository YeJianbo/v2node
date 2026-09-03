package agent

import (
	"bufio"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type cpuCounters struct {
	total uint64
	idle  uint64
}

var cpuState struct {
	sync.Mutex
	previous cpuCounters
}

type networkInterfaceCounters struct {
	received    uint64
	transmitted uint64
}

type networkRateTracker struct {
	initialized  bool
	received     uint64
	transmitted  uint64
	capturedAt   time.Time
	interfaceKey string
	receiveRate  float64
	transmitRate float64
	receivePeak  float64
	transmitPeak float64
}

var networkRateState struct {
	sync.Mutex
	tracker networkRateTracker
}

var networkSamplerOnce sync.Once

type NodeHealth struct {
	NodeID     int    `json:"node_id"`
	Protocol   string `json:"protocol"`
	Status     string `json:"status"`
	ListenOK   bool   `json:"listen_ok"`
	Message    string `json:"message"`
	CheckedAt  int64  `json:"checked_at"`
	ServerPort int    `json:"server_port,omitempty"`
}

var nodeHealthState struct {
	sync.RWMutex
	records []NodeHealth
}

var (
	staticStatusOnce sync.Once
	staticStatus     map[string]any
)

var runtimeVersion = "unknown"

func SetRuntimeVersion(version string) {
	version = strings.TrimSpace(version)
	if version != "" {
		runtimeVersion = version
	}
}

func SetNodeHealth(records []NodeHealth) {
	nodeHealthState.Lock()
	nodeHealthState.records = append([]NodeHealth(nil), records...)
	nodeHealthState.Unlock()
}

func StartNetworkRateSampler() {
	if runtime.GOOS != "linux" {
		return
	}
	networkSamplerOnce.Do(func() {
		sampleNetworkRates(time.Now())
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for sampledAt := range ticker.C {
				sampleNetworkRates(sampledAt)
			}
		}()
	})
}

func CollectStatus(nodeCount int) map[string]any {
	status := collectStaticStatus()
	status["version"] = "ravel " + runtimeVersion
	status["ravel_version"] = runtimeVersion
	status["managed_nodes"] = nodeCount
	status["v2node_status"] = "active"
	nodeHealthState.RLock()
	status["node_health"] = append([]NodeHealth(nil), nodeHealthState.records...)
	nodeHealthState.RUnlock()
	status["process_count"] = readProcessCount()
	status["updated_at"] = time.Now().Unix()
	status["cpu"] = readCPUPercent()
	readUptimeAndLoad(status)
	readMemory(status)
	readDisk(status)
	readNetwork(status)
	return status
}

func collectStaticStatus() map[string]any {
	staticStatusOnce.Do(func() {
		hostname, _ := os.Hostname()
		staticStatus = map[string]any{
			"hostname":  hostname,
			"os":        readOSName(),
			"kernel":    readTrimmedFile("/proc/sys/kernel/osrelease"),
			"arch":      runtime.GOARCH,
			"cpu_cores": runtime.NumCPU(),
			"cpu_model": readCPUModel(),
		}
	})
	status := make(map[string]any, len(staticStatus)+16)
	for key, value := range staticStatus {
		status[key] = value
	}
	return status
}

func readCPUPercent() float64 {
	current, ok := readCPUCounters()
	if !ok {
		return 0
	}
	cpuState.Lock()
	previous := cpuState.previous
	cpuState.previous = current
	cpuState.Unlock()
	if previous.total == 0 || current.total <= previous.total {
		time.Sleep(100 * time.Millisecond)
		next, nextOK := readCPUCounters()
		if !nextOK || next.total <= current.total {
			return 0
		}
		previous = current
		current = next
		cpuState.Lock()
		cpuState.previous = next
		cpuState.Unlock()
	}
	totalDelta := current.total - previous.total
	idleDelta := current.idle - previous.idle
	if totalDelta == 0 || idleDelta > totalDelta {
		return 0
	}
	return float64(totalDelta-idleDelta) * 100 / float64(totalDelta)
}

func readCPUCounters() (cpuCounters, bool) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuCounters{}, false
	}
	fields := strings.Fields(strings.SplitN(string(raw), "\n", 2)[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuCounters{}, false
	}
	values := make([]uint64, 0, len(fields)-1)
	for _, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return cpuCounters{}, false
		}
		values = append(values, value)
	}
	var total uint64
	for _, value := range values {
		total += value
	}
	idle := values[3]
	if len(values) > 4 {
		idle += values[4]
	}
	return cpuCounters{total: total, idle: idle}, true
}

func readUptimeAndLoad(status map[string]any) {
	if raw, err := os.ReadFile("/proc/uptime"); err == nil {
		if fields := strings.Fields(string(raw)); len(fields) > 0 {
			if value, err := strconv.ParseFloat(fields[0], 64); err == nil {
				status["uptime"] = int64(value)
			}
		}
	}
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(raw))
		for index, key := range []string{"load1", "load5", "load15"} {
			if len(fields) <= index {
				break
			}
			if value, err := strconv.ParseFloat(fields[index], 64); err == nil {
				status[key] = value
			}
		}
	}
}

func readMemory(status map[string]any) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer file.Close()
	values := map[string]uint64{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err == nil {
			values[strings.TrimSuffix(fields[0], ":")] = value * 1024
		}
	}
	memTotal := values["MemTotal"]
	memAvailable := values["MemAvailable"]
	memUsed := uint64(0)
	if memTotal >= memAvailable {
		memUsed = memTotal - memAvailable
	}
	status["mem_total"] = memTotal
	status["mem_used"] = memUsed
	status["memory_total"] = memTotal
	status["memory_used"] = memUsed
	status["mem"] = percent(memUsed, memTotal)
	swapTotal := values["SwapTotal"]
	swapFree := values["SwapFree"]
	swapUsed := uint64(0)
	if swapTotal >= swapFree {
		swapUsed = swapTotal - swapFree
	}
	status["swap_total"] = swapTotal
	status["swap_used"] = swapUsed
	status["swap_percent"] = percent(swapUsed, swapTotal)
}

func readNetwork(status map[string]any) {
	counters, err := readNetworkDeviceCounters()
	if err != nil {
		return
	}
	received, transmitted := sumNetworkCounters(counters, nil)
	status["net_rx"] = received
	status["net_tx"] = transmitted

	networkRateState.Lock()
	tracker := &networkRateState.tracker
	if tracker.initialized && time.Since(tracker.capturedAt) <= 10*time.Second {
		status["net_rx_rate"] = tracker.receiveRate
		status["net_tx_rate"] = tracker.transmitRate
		status["net_rx_peak_rate"] = max(tracker.receivePeak, tracker.receiveRate)
		status["net_tx_peak_rate"] = max(tracker.transmitPeak, tracker.transmitRate)
		status["network_rate_interfaces"] = tracker.interfaceKey
		tracker.receivePeak = 0
		tracker.transmitPeak = 0
	}
	networkRateState.Unlock()
}

func readNetworkDeviceCounters() (map[string]networkInterfaceCounters, error) {
	raw, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	return parseNetworkDeviceCounters(string(raw)), nil
}

func parseNetworkDeviceCounters(raw string) map[string]networkInterfaceCounters {
	result := make(map[string]networkInterfaceCounters)
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Text()
		separator := strings.IndexByte(line, ':')
		if separator < 0 {
			continue
		}
		name := strings.TrimSpace(line[:separator])
		fields := strings.Fields(line[separator+1:])
		if name == "" || len(fields) < 9 {
			continue
		}
		received, receiveErr := strconv.ParseUint(fields[0], 10, 64)
		transmitted, transmitErr := strconv.ParseUint(fields[8], 10, 64)
		if receiveErr != nil || transmitErr != nil {
			continue
		}
		result[name] = networkInterfaceCounters{received: received, transmitted: transmitted}
	}
	return result
}

func sumNetworkCounters(counters map[string]networkInterfaceCounters, interfaces []string) (uint64, uint64) {
	var received, transmitted uint64
	if len(interfaces) == 0 {
		for name, counter := range counters {
			if name == "lo" {
				continue
			}
			received += counter.received
			transmitted += counter.transmitted
		}
		return received, transmitted
	}
	for _, name := range interfaces {
		counter, ok := counters[name]
		if !ok || name == "lo" {
			continue
		}
		received += counter.received
		transmitted += counter.transmitted
	}
	return received, transmitted
}

func sampleNetworkRates(sampledAt time.Time) {
	counters, err := readNetworkDeviceCounters()
	if err != nil {
		return
	}
	interfaces := selectNetworkRateInterfaces(counters)
	received, transmitted := sumNetworkCounters(counters, interfaces)
	interfaceKey := strings.Join(interfaces, ",")
	networkRateState.Lock()
	networkRateState.tracker.observe(received, transmitted, sampledAt, interfaceKey)
	networkRateState.Unlock()
}

func (tracker *networkRateTracker) observe(received, transmitted uint64, sampledAt time.Time, interfaceKey string) {
	if !tracker.initialized || tracker.interfaceKey != interfaceKey || received < tracker.received || transmitted < tracker.transmitted {
		tracker.initialized = true
		tracker.received = received
		tracker.transmitted = transmitted
		tracker.capturedAt = sampledAt
		tracker.interfaceKey = interfaceKey
		tracker.receiveRate = 0
		tracker.transmitRate = 0
		tracker.receivePeak = 0
		tracker.transmitPeak = 0
		return
	}

	elapsed := sampledAt.Sub(tracker.capturedAt).Seconds()
	if elapsed < 0.25 || elapsed > 10 {
		tracker.received = received
		tracker.transmitted = transmitted
		tracker.capturedAt = sampledAt
		return
	}
	receiveRate := float64(received-tracker.received) / elapsed
	transmitRate := float64(transmitted-tracker.transmitted) / elapsed
	if tracker.receiveRate == 0 {
		tracker.receiveRate = receiveRate
	} else {
		tracker.receiveRate = receiveRate*0.45 + tracker.receiveRate*0.55
	}
	if tracker.transmitRate == 0 {
		tracker.transmitRate = transmitRate
	} else {
		tracker.transmitRate = transmitRate*0.45 + tracker.transmitRate*0.55
	}
	tracker.receivePeak = max(tracker.receivePeak, receiveRate)
	tracker.transmitPeak = max(tracker.transmitPeak, transmitRate)
	tracker.received = received
	tracker.transmitted = transmitted
	tracker.capturedAt = sampledAt
}

func selectNetworkRateInterfaces(counters map[string]networkInterfaceCounters) []string {
	configured := strings.FieldsFunc(os.Getenv("RAVEL_NETWORK_INTERFACES"), func(value rune) bool {
		return value == ',' || value == ';' || value == ' ' || value == '\t'
	})
	if selected := existingNetworkInterfaces(configured, counters); len(selected) > 0 {
		return selected
	}
	if selected := existingNetworkInterfaces(readDefaultNetworkInterfaces(), counters); len(selected) > 0 {
		return selected
	}

	physical := make([]string, 0, len(counters))
	for name := range counters {
		if name != "lo" && !isLikelyVirtualNetworkInterface(name) {
			physical = append(physical, name)
		}
	}
	if len(physical) > 0 {
		sort.Strings(physical)
		return physical
	}

	fallback := make([]string, 0, len(counters))
	for name := range counters {
		if name != "lo" {
			fallback = append(fallback, name)
		}
	}
	sort.Strings(fallback)
	return fallback
}

func existingNetworkInterfaces(values []string, counters map[string]networkInterfaceCounters) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		name := strings.TrimSpace(value)
		if name == "" || name == "lo" || seen[name] {
			continue
		}
		if _, ok := counters[name]; !ok {
			continue
		}
		seen[name] = true
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func readDefaultNetworkInterfaces() []string {
	result := make([]string, 0, 2)
	if raw, err := os.ReadFile("/proc/net/route"); err == nil {
		result = append(result, parseIPv4DefaultRouteInterfaces(string(raw))...)
	}
	if raw, err := os.ReadFile("/proc/net/ipv6_route"); err == nil {
		result = append(result, parseIPv6DefaultRouteInterfaces(string(raw))...)
	}
	return result
}

func parseIPv4DefaultRouteInterfaces(raw string) []string {
	result := []string{}
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 || fields[1] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err == nil && flags&1 != 0 {
			result = append(result, fields[0])
		}
	}
	return result
}

func parseIPv6DefaultRouteInterfaces(raw string) []string {
	result := []string{}
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 || strings.Trim(fields[0], "0") != "" || fields[1] != "00" {
			continue
		}
		result = append(result, fields[len(fields)-1])
	}
	return result
}

func isLikelyVirtualNetworkInterface(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	for _, prefix := range []string{"br-", "cni", "docker", "dummy", "flannel", "kube", "tap", "tailscale", "tun", "veth", "virbr", "wg", "zt"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func percent(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) * 100 / float64(total)
}

func readCPUModel() string {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if separator := strings.IndexByte(line, ':'); separator >= 0 {
			key := strings.TrimSpace(line[:separator])
			if key == "model name" || key == "Hardware" {
				return strings.TrimSpace(line[separator+1:])
			}
		}
	}
	return ""
}

func readOSName() string {
	file, err := os.Open("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), "\"")
		}
	}
	return runtime.GOOS
}

func readProcessCount() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			if _, err := strconv.Atoi(entry.Name()); err == nil {
				count++
			}
		}
	}
	return count
}

func readTrimmedFile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
