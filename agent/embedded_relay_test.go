package agent

import (
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

func reserveRelayPort(t *testing.T, network string) int {
	t.Helper()
	if network == "tcp" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		return listener.Addr().(*net.TCPAddr).Port
	}
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.LocalAddr().(*net.UDPAddr).Port
}

func TestEmbeddedRelayTCPAndShutdown(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		conn, acceptErr := target.Accept()
		if acceptErr == nil {
			defer conn.Close()
			_, _ = io.Copy(conn, conn)
		}
	}()
	port := reserveRelayPort(t, "tcp")
	node := "tcp://127.0.0.1:" + strconv.Itoa(port) + "/" + target.Addr().String()
	relay, err := startEmbeddedRelay([]string{node})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 5)
	if _, err := io.ReadFull(conn, reply); err != nil || string(reply) != "hello" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	relay.Close()
	if _, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond); err == nil {
		t.Fatal("relay listener remained open after shutdown")
	}
}

func TestEmbeddedRelayUDPKeepsClientRepliesSeparate(t *testing.T) {
	target, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		buffer := make([]byte, 128)
		for {
			count, address, readErr := target.ReadFrom(buffer)
			if readErr != nil {
				return
			}
			_, _ = target.WriteTo(buffer[:count], address)
		}
	}()
	port := reserveRelayPort(t, "udp")
	node := "udp://127.0.0.1:" + strconv.Itoa(port) + "/" + target.LocalAddr().String()
	relay, err := startEmbeddedRelay([]string{node})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	for _, message := range []string{"first", "second"} {
		conn, dialErr := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(port))
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = conn.Write([]byte(message))
		buffer := make([]byte, 16)
		count, readErr := conn.Read(buffer)
		_ = conn.Close()
		if readErr != nil || string(buffer[:count]) != message {
			t.Fatalf("reply=%q err=%v", buffer[:count], readErr)
		}
	}
}
