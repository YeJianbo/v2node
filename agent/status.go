package agent

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func CollectStatus(nodeCount int) map[string]any {
	hostname, _ := os.Hostname()
	status := map[string]any{
		"version":       "ravel-native",
		"hostname":      hostname,
		"os":            runtime.GOOS,
		"arch":          runtime.GOARCH,
		"cpu_cores":     runtime.NumCPU(),
		"managed_nodes": nodeCount,
		"v2node_status": "active",
		"updated_at":    time.Now().Unix(),
	}
	if raw, err := os.ReadFile("/proc/uptime"); err == nil {
		if fields := strings.Fields(string(raw)); len(fields) > 0 {
			if value, err := strconv.ParseFloat(fields[0], 64); err == nil {
				status["uptime"] = int64(value)
			}
		}
	}
	if file, err := os.Open("/proc/meminfo"); err == nil {
		defer file.Close()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 2 {
				continue
			}
			value, _ := strconv.ParseInt(fields[1], 10, 64)
			switch strings.TrimSuffix(fields[0], ":") {
			case "MemTotal":
				status["memory_total"] = value * 1024
			case "MemAvailable":
				status["memory_available"] = value * 1024
			}
		}
	}
	return status
}
