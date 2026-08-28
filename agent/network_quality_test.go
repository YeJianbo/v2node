package agent

import "testing"

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
