package agent

import (
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const networkQualityPath = "/api/v1/server/machine/networkQuality"

var (
	packetSummaryPattern = regexp.MustCompile(`(?m)(\d+)\s+packets transmitted,\s+(\d+)\s+(?:packets )?received`)
	packetLossPattern    = regexp.MustCompile(`(?m)([0-9]+(?:\.[0-9]+)?)%\s+packet loss`)
	rttPattern           = regexp.MustCompile(`(?m)=\s*([0-9.]+)/([0-9.]+)/([0-9.]+)(?:/[0-9.]+)?\s*ms`)
)

type NetworkQualityTarget struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Host      string `json:"host"`
	IPVersion string `json:"ip_version"`
	Enabled   *bool  `json:"enabled"`
}

type NetworkQualityConfig struct {
	Enabled         bool                   `json:"enabled"`
	IntervalSeconds int                    `json:"interval_seconds"`
	PacketCount     int                    `json:"packet_count"`
	TimeoutSeconds  int                    `json:"timeout_seconds"`
	Targets         []NetworkQualityTarget `json:"targets"`
}

type NetworkQualitySample struct {
	Key        string   `json:"key"`
	Sent       int      `json:"sent"`
	Received   int      `json:"received"`
	PacketLoss float64  `json:"packet_loss"`
	LatencyMin *float64 `json:"latency_min"`
	LatencyAvg *float64 `json:"latency_avg"`
	LatencyMax *float64 `json:"latency_max"`
}

func normalizeNetworkQualityConfig(config NetworkQualityConfig) NetworkQualityConfig {
	if config.IntervalSeconds < 60 || config.IntervalSeconds > 3600 {
		config.IntervalSeconds = 300
	}
	if config.PacketCount < 1 || config.PacketCount > 20 {
		config.PacketCount = 5
	}
	if config.TimeoutSeconds < 1 || config.TimeoutSeconds > 10 {
		config.TimeoutSeconds = 2
	}
	if len(config.Targets) < 1 || len(config.Targets) > 8 || len(enabledNetworkQualityTargets(config.Targets)) < 1 {
		config.Enabled = false
	}
	for index := range config.Targets {
		if config.Targets[index].IPVersion != "4" && config.Targets[index].IPVersion != "6" {
			config.Targets[index].IPVersion = "auto"
		}
	}
	return config
}

func (c *Controller) ProbeAndPushNetworkQuality() error {
	config := c.NetworkQualityConfig()
	if !config.Enabled {
		return nil
	}
	samples := collectNetworkQuality(config)
	var response map[string]any
	return c.Client.Post(networkQualityPath, map[string]any{"samples": samples}, &response)
}

func collectNetworkQuality(config NetworkQualityConfig) []NetworkQualitySample {
	targets := enabledNetworkQualityTargets(config.Targets)
	samples := make([]NetworkQualitySample, len(targets))
	var group sync.WaitGroup
	for index, target := range targets {
		group.Add(1)
		go func() {
			defer group.Done()
			samples[index] = pingNetworkQualityTarget(target, config.PacketCount, config.TimeoutSeconds)
		}()
	}
	group.Wait()
	return samples
}

func enabledNetworkQualityTargets(targets []NetworkQualityTarget) []NetworkQualityTarget {
	enabled := make([]NetworkQualityTarget, 0, len(targets))
	for _, target := range targets {
		if target.Enabled == nil || *target.Enabled {
			enabled = append(enabled, target)
		}
	}
	return enabled
}

func pingNetworkQualityTarget(target NetworkQualityTarget, count, timeoutSeconds int) NetworkQualitySample {
	args := networkQualityPingArgs(target, count, timeoutSeconds, runtime.GOOS)
	command := exec.Command("ping", args...)
	command.Env = append(command.Environ(), "LC_ALL=C", "LANG=C")
	output, _ := command.CombinedOutput()
	return parsePingOutput(target.Key, count, string(output))
}

func networkQualityPingArgs(target NetworkQualityTarget, count, timeoutSeconds int, goos string) []string {
	args := []string{"-n"}
	if target.IPVersion == "4" || target.IPVersion == "6" {
		args = append(args, "-"+target.IPVersion)
	}
	args = append(args, "-c", strconv.Itoa(count), "-W", strconv.Itoa(timeoutSeconds), target.Host)
	if goos == "windows" {
		args = []string{}
		if target.IPVersion == "4" || target.IPVersion == "6" {
			args = append(args, "-"+target.IPVersion)
		}
		args = append(args, "-n", strconv.Itoa(count), "-w", strconv.Itoa(timeoutSeconds*1000), target.Host)
	}
	return args
}

func parsePingOutput(key string, requested int, output string) NetworkQualitySample {
	sample := NetworkQualitySample{Key: key, Sent: requested, PacketLoss: 100}
	if match := packetSummaryPattern.FindStringSubmatch(output); len(match) == 3 {
		sample.Sent, _ = strconv.Atoi(match[1])
		sample.Received, _ = strconv.Atoi(match[2])
	}
	if match := packetLossPattern.FindStringSubmatch(output); len(match) == 2 {
		sample.PacketLoss, _ = strconv.ParseFloat(match[1], 64)
	} else if sample.Sent > 0 {
		sample.PacketLoss = float64(sample.Sent-sample.Received) * 100 / float64(sample.Sent)
	}
	if match := rttPattern.FindStringSubmatch(output); len(match) == 4 {
		minimum, minErr := strconv.ParseFloat(match[1], 64)
		average, avgErr := strconv.ParseFloat(match[2], 64)
		maximum, maxErr := strconv.ParseFloat(match[3], 64)
		if minErr == nil && avgErr == nil && maxErr == nil {
			sample.LatencyMin = &minimum
			sample.LatencyAvg = &average
			sample.LatencyMax = &maximum
		}
	}
	if sample.Sent < 1 {
		sample.Sent = requested
	}
	if sample.Received > sample.Sent {
		sample.Received = sample.Sent
	}
	return sample
}

func NetworkQualityFingerprint(config NetworkQualityConfig) string {
	parts := []string{
		strconv.FormatBool(config.Enabled),
		strconv.Itoa(config.IntervalSeconds),
		strconv.Itoa(config.PacketCount),
		strconv.Itoa(config.TimeoutSeconds),
	}
	for _, target := range config.Targets {
		parts = append(parts, target.Key, target.Host, target.IPVersion, strconv.FormatBool(target.Enabled == nil || *target.Enabled))
	}
	return strings.Join(parts, "|")
}

func NetworkQualityInterval(config NetworkQualityConfig) time.Duration {
	return time.Duration(config.IntervalSeconds) * time.Second
}
