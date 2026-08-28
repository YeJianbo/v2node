package agent

import (
	"bufio"
	"os"
	"runtime"
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

var runtimeVersion = "unknown"

func SetRuntimeVersion(version string) {
	version = strings.TrimSpace(version)
	if version != "" {
		runtimeVersion = version
	}
}

func CollectStatus(nodeCount int) map[string]any {
	hostname, _ := os.Hostname()
	status := map[string]any{
		"version":       "ravel " + runtimeVersion,
		"ravel_version": runtimeVersion,
		"hostname":      hostname,
		"os":            readOSName(),
		"kernel":        readTrimmedFile("/proc/sys/kernel/osrelease"),
		"arch":          runtime.GOARCH,
		"cpu_cores":     runtime.NumCPU(),
		"cpu_model":     readCPUModel(),
		"managed_nodes": nodeCount,
		"v2node_status": "active",
		"process_count": readProcessCount(),
		"updated_at":    time.Now().Unix(),
	}
	status["cpu"] = readCPUPercent()
	readUptimeAndLoad(status)
	readMemory(status)
	readDisk(status)
	readNetwork(status)
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
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		return
	}
	defer file.Close()
	var received, transmitted uint64
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		separator := strings.IndexByte(line, ':')
		if separator < 0 || strings.TrimSpace(line[:separator]) == "lo" {
			continue
		}
		fields := strings.Fields(line[separator+1:])
		if len(fields) < 9 {
			continue
		}
		if value, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
			received += value
		}
		if value, err := strconv.ParseUint(fields[8], 10, 64); err == nil {
			transmitted += value
		}
	}
	status["net_rx"] = received
	status["net_tx"] = transmitted
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
