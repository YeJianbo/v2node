package domaintraffic

import (
	"net"
	"strings"
	"sync"
	"sync/atomic"
)

type Record struct {
	Domain   string `json:"domain"`
	Upload   int64  `json:"upload"`
	Download int64  `json:"download"`
}
type Counter struct {
	up, down atomic.Int64
	refs     int
}
type bucket struct {
	enabled bool
	entries map[string]*Counter
}

var mu sync.Mutex
var nodes = map[string]*bucket{}

func Remove(tag string) { mu.Lock(); delete(nodes, tag); mu.Unlock() }

func Enable(tag string, enabled bool) {
	mu.Lock()
	defer mu.Unlock()
	b := nodes[tag]
	if b == nil {
		b = &bucket{entries: map[string]*Counter{}}
		nodes[tag] = b
	}
	b.enabled = enabled
}

// Only domain names are collected: never URLs, user identifiers or IP addresses.
func Acquire(tag, domain string) (*Counter, func()) {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	if len(domain) > 253 || !strings.Contains(domain, ".") || net.ParseIP(domain) != nil {
		return nil, func() {}
	}
	for _, c := range domain {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return nil, func() {}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	b := nodes[tag]
	if b == nil || !b.enabled {
		return nil, func() {}
	}
	c := b.entries[domain]
	if c == nil {
		if len(b.entries) >= 4096 {
			return nil, func() {}
		}
		c = &Counter{}
		b.entries[domain] = c
	}
	c.refs++
	return c, func() { mu.Lock(); c.refs--; mu.Unlock() }
}
func (c *Counter) Add(up, down int64) {
	if c != nil {
		c.up.Add(up)
		c.down.Add(down)
	}
}

func Drain(tag string) []Record {
	mu.Lock()
	defer mu.Unlock()
	b := nodes[tag]
	if b == nil {
		return nil
	}
	var result []Record
	for domain, c := range b.entries {
		up, down := c.up.Swap(0), c.down.Swap(0)
		if up > 0 || down > 0 {
			result = append(result, Record{domain, up, down})
		}
		if c.refs == 0 {
			delete(b.entries, domain)
		}
	}
	return result
}
