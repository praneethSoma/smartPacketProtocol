package connpool

import (
	"fmt"
	"net"
	"sync"
)

// UDPPool is a thread-safe pool of persistent UDP connections,
// keyed by destination address string. Connections are created
// lazily on first Send() and reused for subsequent writes.
// If a write fails the stale connection is discarded and re-dialed once.
type UDPPool struct {
	mu    sync.RWMutex
	conns map[string]*net.UDPConn
}

// New creates an empty connection pool.
func New() *UDPPool {
	return &UDPPool{
		conns: make(map[string]*net.UDPConn),
	}
}

// Send writes data to the given UDP address, reusing an existing
// connection if available. On the first call for a given address
// the connection is dialed lazily. If the write fails (broken conn),
// the pool discards the old connection, re-dials, and retries once.
func (p *UDPPool) Send(addr string, data []byte) error {
	conn, err := p.getOrDial(addr)
	if err != nil {
		return fmt.Errorf("connpool dial %s: %w", addr, err)
	}

	_, err = conn.Write(data)
	if err == nil {
		return nil
	}

	// Write failed — evict stale connection, redial, retry once.
	p.evict(addr)
	conn, err = p.dial(addr)
	if err != nil {
		return fmt.Errorf("connpool redial %s: %w", addr, err)
	}
	p.put(addr, conn)

	if _, err = conn.Write(data); err != nil {
		return fmt.Errorf("connpool retry write %s: %w", addr, err)
	}
	return nil
}

// Close closes all cached connections and empties the pool.
func (p *UDPPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for addr, conn := range p.conns {
		conn.Close()
		delete(p.conns, addr)
	}
}

// getOrDial returns a cached connection or dials a new one.
// Uses double-checked locking so only one goroutine dials per address.
func (p *UDPPool) getOrDial(addr string) (*net.UDPConn, error) {
	p.mu.RLock()
	conn, ok := p.conns[addr]
	p.mu.RUnlock()
	if ok {
		return conn, nil
	}

	// Not cached — take write lock and double-check.
	p.mu.Lock()
	conn, ok = p.conns[addr]
	if ok {
		p.mu.Unlock()
		return conn, nil
	}

	// Still not cached — dial under the write lock.
	newConn, err := p.dial(addr)
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}
	p.conns[addr] = newConn
	p.mu.Unlock()
	return newConn, nil
}

// dial resolves and dials a UDP address.
func (p *UDPPool) dial(addr string) (*net.UDPConn, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", addr, err)
	}
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return conn, nil
}

// put stores a connection in the pool, closing any previously cached one.
func (p *UDPPool) put(addr string, conn *net.UDPConn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if old, ok := p.conns[addr]; ok {
		old.Close()
	}
	p.conns[addr] = conn
}

// evict removes and closes the cached connection for addr.
func (p *UDPPool) evict(addr string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if conn, ok := p.conns[addr]; ok {
		conn.Close()
		delete(p.conns, addr)
	}
}

// Size returns the number of cached connections (for testing).
func (p *UDPPool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.conns)
}
