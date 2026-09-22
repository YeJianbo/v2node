package domaintraffic

import "testing"

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
