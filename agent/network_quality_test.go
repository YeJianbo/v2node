package agent

import (
	"reflect"
	"testing"
)

func TestParsePingOutput(t *testing.T) {
	output := `5 packets transmitted, 4 received, 20% packet loss, time 4004ms
rtt min/avg/max/mdev = 24.111/25.222/28.333/1.100 ms`
	sample := parsePingOutput("aliyun", 5, output)
	if sample.Sent != 5 || sample.Received != 4 || sample.PacketLoss != 20 {
		t.Fatalf("unexpected packet summary: %+v", sample)
	}
	if sample.LatencyAvg == nil || *sample.LatencyAvg != 25.222 {
		t.Fatalf("unexpected latency summary: %+v", sample)
	}
}

func TestParsePingOutputForTotalLoss(t *testing.T) {
	output := `5 packets transmitted, 0 received, 100% packet loss, time 4090ms`
	sample := parsePingOutput("baidu", 5, output)
	if sample.PacketLoss != 100 || sample.LatencyAvg != nil {
		t.Fatalf("unexpected total-loss sample: %+v", sample)
	}
}

func TestParseBusyBoxPingOutput(t *testing.T) {
	output := `5 packets transmitted, 5 packets received, 0% packet loss
round-trip min/avg/max = 1.407/1.931/3.805 ms`
	sample := parsePingOutput("dnspod", 5, output)
	if sample.Sent != 5 || sample.Received != 5 || sample.PacketLoss != 0 {
		t.Fatalf("unexpected BusyBox packet summary: %+v", sample)
	}
	if sample.LatencyAvg == nil || *sample.LatencyAvg != 1.931 {
		t.Fatalf("unexpected BusyBox latency summary: %+v", sample)
	}
}

func TestNormalizeNetworkQualityIPVersion(t *testing.T) {
	config := normalizeNetworkQualityConfig(NetworkQualityConfig{
		Enabled: true,
		Targets: []NetworkQualityTarget{
			{Key: "auto", Host: "dual.example.com"},
			{Key: "v4", Host: "dual.example.com", IPVersion: "4"},
			{Key: "invalid", Host: "dual.example.com", IPVersion: "ipv6"},
		},
	})

	want := []string{"auto", "4", "auto"}
	for index, target := range config.Targets {
		if target.IPVersion != want[index] {
			t.Fatalf("target %d IP version = %q, want %q", index, target.IPVersion, want[index])
		}
	}
}

func TestNetworkQualityPingArgs(t *testing.T) {
	tests := []struct {
		name   string
		goos   string
		target NetworkQualityTarget
		want   []string
	}{
		{
			name:   "linux ipv4",
			goos:   "linux",
			target: NetworkQualityTarget{Host: "dual.example.com", IPVersion: "4"},
			want:   []string{"-n", "-4", "-c", "5", "-W", "2", "dual.example.com"},
		},
		{
			name:   "linux auto",
			goos:   "linux",
			target: NetworkQualityTarget{Host: "dual.example.com", IPVersion: "auto"},
			want:   []string{"-n", "-c", "5", "-W", "2", "dual.example.com"},
		},
		{
			name:   "windows ipv6",
			goos:   "windows",
			target: NetworkQualityTarget{Host: "dual.example.com", IPVersion: "6"},
			want:   []string{"-6", "-n", "5", "-w", "2000", "dual.example.com"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := networkQualityPingArgs(test.target, 5, 2, test.goos)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("args = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestNetworkQualityFingerprintIncludesIPVersion(t *testing.T) {
	base := NetworkQualityConfig{Targets: []NetworkQualityTarget{{Key: "target", Host: "dual.example.com", IPVersion: "4"}}}
	v6 := base
	v6.Targets = []NetworkQualityTarget{{Key: "target", Host: "dual.example.com", IPVersion: "6"}}
	if NetworkQualityFingerprint(base) == NetworkQualityFingerprint(v6) {
		t.Fatal("fingerprint must change when the requested IP version changes")
	}
}
