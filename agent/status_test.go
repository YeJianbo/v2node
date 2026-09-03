package agent

import (
	"reflect"
	"testing"
	"time"
)

func TestParseNetworkDeviceCountersKeepsTotalsSeparateFromRateSelection(t *testing.T) {
	counters := parseNetworkDeviceCounters(`
Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 100 0 0 0 0 0 0 0 200 0 0 0 0 0 0 0
  eth0: 1000 0 0 0 0 0 0 0 2000 0 0 0 0 0 0 0
  tun0: 3000 0 0 0 0 0 0 0 4000 0 0 0 0 0 0 0
`)

	received, transmitted := sumNetworkCounters(counters, nil)
	if received != 4000 || transmitted != 6000 {
		t.Fatalf("unexpected total counters: rx=%d tx=%d", received, transmitted)
	}
	received, transmitted = sumNetworkCounters(counters, []string{"eth0"})
	if received != 1000 || transmitted != 2000 {
		t.Fatalf("unexpected selected counters: rx=%d tx=%d", received, transmitted)
	}
}

func TestConfiguredNetworkRateInterfacesTakePriority(t *testing.T) {
	t.Setenv("RAVEL_NETWORK_INTERFACES", "tun0, eth0,missing")
	counters := map[string]networkInterfaceCounters{
		"eth0": {},
		"tun0": {},
	}

	if got := selectNetworkRateInterfaces(counters); !reflect.DeepEqual(got, []string{"eth0", "tun0"}) {
		t.Fatalf("selected interfaces = %#v", got)
	}
}

func TestDefaultRouteInterfaceParsers(t *testing.T) {
	ipv4 := parseIPv4DefaultRouteInterfaces("Iface Destination Gateway Flags RefCnt Use Metric Mask\neth0 00000000 01020304 0003 0 0 100 00000000\n")
	if !reflect.DeepEqual(ipv4, []string{"eth0"}) {
		t.Fatalf("IPv4 defaults = %#v", ipv4)
	}
	ipv6 := parseIPv6DefaultRouteInterfaces("00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 00000064 00000000 00000000 00000001 eth1\n")
	if !reflect.DeepEqual(ipv6, []string{"eth1"}) {
		t.Fatalf("IPv6 defaults = %#v", ipv6)
	}
}

func TestNetworkRateTrackerSmoothsCurrentRateAndKeepsWindowPeak(t *testing.T) {
	var tracker networkRateTracker
	startedAt := time.Unix(100, 0)
	tracker.observe(1000, 2000, startedAt, "eth0")
	tracker.observe(2000, 4000, startedAt.Add(time.Second), "eth0")
	tracker.observe(2000, 4000, startedAt.Add(2*time.Second), "eth0")

	if tracker.receiveRate != 550 || tracker.transmitRate != 1100 {
		t.Fatalf("smoothed rates = %.2f/%.2f", tracker.receiveRate, tracker.transmitRate)
	}
	if tracker.receivePeak != 1000 || tracker.transmitPeak != 2000 {
		t.Fatalf("window peaks = %.2f/%.2f", tracker.receivePeak, tracker.transmitPeak)
	}
}
