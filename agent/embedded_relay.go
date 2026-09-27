package agent

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

const embeddedRelayMaxConnections = 32

type embeddedRelay struct {
	mu          sync.Mutex
	listeners   []io.Closer
	connections map[net.Conn]struct{}
	udpPeers    map[string]*embeddedUDPPeer
	listening   map[string]bool
	active      chan struct{}
	closed      bool
}

type embeddedUDPPeer struct {
	conn net.Conn
}

func startEmbeddedRelay(nodes []string) (*embeddedRelay, error) {
	listeners := relayListenersFromNodes(nodes)
	relay := &embeddedRelay{
		connections: make(map[net.Conn]struct{}),
		udpPeers:    make(map[string]*embeddedUDPPeer),
		listening:   make(map[string]bool, len(listeners)),
		active:      make(chan struct{}, embeddedRelayMaxConnections),
	}
	for _, rule := range listeners {
		address := net.JoinHostPort(rule.ListenHost, strconv.Itoa(rule.ListenPort))
		if rule.ListenHost == "0.0.0.0" || rule.ListenHost == "::" {
			address = ":" + strconv.Itoa(rule.ListenPort)
		}
		key := rule.Protocol + ":" + strconv.Itoa(rule.ListenPort)
		switch rule.Protocol {
		case "tcp":
			listener, err := net.Listen("tcp", address)
			if err != nil {
				relay.Close()
				return nil, fmt.Errorf("listen %s: %w", address, err)
			}
			relay.listeners = append(relay.listeners, listener)
			relay.listening[key] = true
		case "udp":
			listener, err := net.ListenPacket("udp", address)
			if err != nil {
				relay.Close()
				return nil, fmt.Errorf("listen UDP %s: %w", address, err)
			}
			relay.listeners = append(relay.listeners, listener)
			relay.listening[key] = true
		default:
			relay.Close()
			return nil, fmt.Errorf("unsupported embedded relay protocol %q", rule.Protocol)
		}
	}
	for index, rule := range listeners {
		switch listener := relay.listeners[index].(type) {
		case net.Listener:
			go relay.serveTCP(listener, rule)
		case net.PacketConn:
			go relay.serveUDP(listener, rule, index)
		}
	}
	return relay, nil
}

func (r *embeddedRelay) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	for _, listener := range r.listeners {
		_ = listener.Close()
	}
	for conn := range r.connections {
		_ = conn.Close()
	}
	for _, peer := range r.udpPeers {
		_ = peer.conn.Close()
	}
}

func (r *embeddedRelay) serveTCP(listener net.Listener, rule relayListener) {
	target := net.JoinHostPort(rule.TargetHost, strconv.Itoa(rule.TargetPort))
	for {
		client, err := listener.Accept()
		if err != nil {
			return
		}
		select {
		case r.active <- struct{}{}:
			go func() {
				defer func() { <-r.active }()
				r.forwardTCP(client, target)
			}()
		default:
			_ = client.Close()
		}
	}
}

func (r *embeddedRelay) forwardTCP(client net.Conn, target string) {
	defer client.Close()
	upstream, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.connections[client] = struct{}{}
	r.connections[upstream] = struct{}{}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.connections, client)
		delete(r.connections, upstream)
		r.mu.Unlock()
	}()
	done := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(upstream, client)
		if conn, ok := upstream.(*net.TCPConn); ok {
			_ = conn.CloseWrite()
		}
		done <- struct{}{}
	}()
	_, _ = io.Copy(client, upstream)
	if conn, ok := client.(*net.TCPConn); ok {
		_ = conn.CloseWrite()
	}
	<-done
}

func (r *embeddedRelay) serveUDP(listener net.PacketConn, rule relayListener, index int) {
	target := net.JoinHostPort(rule.TargetHost, strconv.Itoa(rule.TargetPort))
	buffer := make([]byte, 65535)
	for {
		count, address, err := listener.ReadFrom(buffer)
		if err != nil {
			return
		}
		key := strconv.Itoa(index) + ":" + address.String()
		r.mu.Lock()
		peer := r.udpPeers[key]
		if peer == nil && !r.closed && len(r.udpPeers) < embeddedRelayMaxConnections {
			conn, dialErr := net.DialTimeout("udp", target, 3*time.Second)
			if dialErr == nil {
				peer = &embeddedUDPPeer{conn: conn}
				r.udpPeers[key] = peer
				go r.forwardUDPReplies(listener, address, key, peer)
			}
		}
		if peer != nil {
			_, _ = peer.conn.Write(buffer[:count])
			_ = peer.conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
		}
		r.mu.Unlock()
	}
}

func (r *embeddedRelay) forwardUDPReplies(listener net.PacketConn, address net.Addr, key string, peer *embeddedUDPPeer) {
	defer func() {
		r.mu.Lock()
		if r.udpPeers[key] == peer {
			delete(r.udpPeers, key)
		}
		r.mu.Unlock()
		_ = peer.conn.Close()
	}()
	buffer := make([]byte, 65535)
	for {
		count, err := peer.conn.Read(buffer)
		if err != nil {
			return
		}
		if _, err := listener.WriteTo(buffer[:count], address); err != nil {
			return
		}
	}
}
