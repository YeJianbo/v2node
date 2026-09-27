package domaintraffic

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestLimitedBatchesPreserveUnsentCounters(t *testing.T) {
	Enable("bounded", true)
	defer Remove("bounded")
	for i := 0; i < 400; i++ {
		c, release := Acquire("bounded", fmt.Sprintf("site%d.example.com", i))
		c.Add(10, 20)
		release()
	}
	total := int64(0)
	for i := 0; i < 10; i++ {
		records := DrainLimited("bounded", 15*1024)
		body, _ := json.Marshal(records)
		if len(body) > 15*1024 {
			t.Fatalf("oversized batch: %d", len(body))
		}
		for _, r := range records {
			total += r.Upload + r.Download
		}
	}
	if total != 12000 {
		t.Fatalf("lost counters: %d", total)
	}
}

func TestDisableStopsExistingConnectionsAndDropsBacklog(t *testing.T) {
	Enable("toggle", true)
	defer Remove("toggle")
	c, release := Acquire("toggle", "example.com")
	defer release()
	c.Add(10, 20)
	Enable("toggle", false)
	c.Add(40, 50)
	if len(Drain("toggle")) != 0 {
		t.Fatal("disabled batch")
	}
	Enable("toggle", true)
	if len(Drain("toggle")) != 0 {
		t.Fatal("stale counters")
	}
	c.Add(1, 2)
	records := Drain("toggle")
	if len(records) != 1 || records[0].Download != 2 {
		t.Fatal("resume failed")
	}
}

func TestAggregateAndLongLivedConnection(t *testing.T) {
	Enable("test", true)
	c, release := Acquire("test", "EXAMPLE.com.")
	if c == nil {
		t.Fatal("domain rejected")
	}
	c.Add(10, 20)
	r := Drain("test")
	if len(r) != 1 || r[0].Domain != "example.com" || r[0].Upload != 10 || r[0].Download != 20 {
		t.Fatalf("%+v", r)
	}
	if len(Drain("test")) != 0 {
		t.Fatal("duplicate bytes")
	}
	c.Add(30, 40)
	release()
	r = Drain("test")
	if len(r) != 1 || r[0].Upload != 30 {
		t.Fatalf("long lived connection lost: %+v", r)
	}
	mu.Lock()
	size := len(nodes["test"].entries)
	mu.Unlock()
	if size != 0 {
		t.Fatal("inactive entry retained")
	}
}

func TestDoesNotCollectURLsOrIPs(t *testing.T) {
	Enable("private", true)
	for _, value := range []string{"1.1.1.1", "https://example.com/path", "example.com?secret=1", "localhost", "example.com/user@email"} {
		if c, _ := Acquire("private", value); c != nil {
			t.Fatalf("accepted %s", value)
		}
	}
	Enable("off", false)
	if c, _ := Acquire("off", "example.com"); c != nil {
		t.Fatal("disabled collection")
	}
}
