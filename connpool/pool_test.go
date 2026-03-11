package connpool

import (
	"net"
	"sync"
	"testing"
)

func TestSendToLocalUDP(t *testing.T) {
	// Start a local UDP listener.
	laddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUDP("udp", laddr)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	addr := listener.LocalAddr().String()
	pool := New()
	defer pool.Close()

	payload := []byte("hello_connpool")
	if err := pool.Send(addr, payload); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	buf := make([]byte, 1024)
	n, _, err := listener.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if string(buf[:n]) != string(payload) {
		t.Fatalf("Payload mismatch: got %q, want %q", buf[:n], payload)
	}

	// Pool should have exactly 1 cached connection.
	if pool.Size() != 1 {
		t.Fatalf("Pool size: got %d, want 1", pool.Size())
	}
	t.Logf("Send+receive via pool: %q ✓", payload)
}

func TestConnectionReuse(t *testing.T) {
	laddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	listener, _ := net.ListenUDP("udp", laddr)
	defer listener.Close()

	addr := listener.LocalAddr().String()
	pool := New()
	defer pool.Close()

	// Send twice — pool should reuse the same connection.
	for i := 0; i < 5; i++ {
		if err := pool.Send(addr, []byte("msg")); err != nil {
			t.Fatalf("Send #%d failed: %v", i, err)
		}
	}

	if pool.Size() != 1 {
		t.Fatalf("Pool should reuse conn: size=%d, want 1", pool.Size())
	}
	t.Log("Connection reused across 5 sends ✓")
}

func TestConcurrentSends(t *testing.T) {
	laddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	listener, _ := net.ListenUDP("udp", laddr)
	defer listener.Close()

	addr := listener.LocalAddr().String()
	pool := New()
	defer pool.Close()

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	errCh := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			if err := pool.Send(addr, []byte("concurrent")); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("Concurrent send error: %v", err)
	}
	t.Logf("Concurrent sends from %d goroutines: no errors ✓", goroutines)
}

func TestCloseAll(t *testing.T) {
	// Create connections to two different listeners.
	laddr1, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	l1, _ := net.ListenUDP("udp", laddr1)
	defer l1.Close()

	laddr2, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	l2, _ := net.ListenUDP("udp", laddr2)
	defer l2.Close()

	pool := New()
	pool.Send(l1.LocalAddr().String(), []byte("a"))
	pool.Send(l2.LocalAddr().String(), []byte("b"))

	if pool.Size() != 2 {
		t.Fatalf("Pool should have 2 conns, got %d", pool.Size())
	}

	pool.Close()

	if pool.Size() != 0 {
		t.Fatalf("Pool should be empty after Close, got %d", pool.Size())
	}
	t.Log("Close() cleaned up all connections ✓")
}

func TestMultipleDestinations(t *testing.T) {
	listeners := make([]*net.UDPConn, 3)
	for i := range listeners {
		laddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
		l, _ := net.ListenUDP("udp", laddr)
		listeners[i] = l
		defer l.Close()
	}

	pool := New()
	defer pool.Close()

	for _, l := range listeners {
		if err := pool.Send(l.LocalAddr().String(), []byte("test")); err != nil {
			t.Fatalf("Send failed: %v", err)
		}
	}

	if pool.Size() != 3 {
		t.Fatalf("Pool should have 3 conns, got %d", pool.Size())
	}
	t.Log("3 different destinations cached independently ✓")
}
