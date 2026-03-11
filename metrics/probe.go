package metrics

import (
	"fmt"
	"net"
	"sync"
	"time"
)

// ──────────────────────────────────────────────────────────────
// Probe result and configuration
// ──────────────────────────────────────────────────────────────

// ProbeResult holds the measured link quality to a neighbor.
type ProbeResult struct {
	LatencyMs float64   // Average RTT in milliseconds
	LossPct   float64   // Packet loss percentage (0–100)
	LastSeen  time.Time // Last successful probe response
	Alive     bool      // Whether neighbor responded recently
}

// ProbeConfig controls probe behavior.
type ProbeConfig struct {
	IntervalMs int // How often to probe each neighbor (default 100ms)
	TimeoutMs  int // Probe timeout before recording failure (default 50ms)
	WindowSize int // Sliding window size for loss calculation (default 20)
	ProbePort  int // Port to listen/send probes on (0 = use provided listen addr)
}

// DefaultProbeConfig returns sensible defaults for latency probing.
func DefaultProbeConfig() ProbeConfig {
	return ProbeConfig{
		IntervalMs: 100,
		TimeoutMs:  50,
		WindowSize: 20,
		ProbePort:  0, // Use whatever is passed in listen addr
	}
}

// ──────────────────────────────────────────────────────────────
// Prober — UDP ping/pong latency measurement.
// ──────────────────────────────────────────────────────────────

// Prober sends UDP pings to neighbors and measures RTT/loss.
type Prober struct {
	mu        sync.RWMutex
	config    ProbeConfig
	results   map[string]*probeState // neighbor name → state
	stopCh    chan struct{}
	conn      *net.UDPConn
	localName string
}

// probeState tracks the sliding window for one neighbor.
type probeState struct {
	addr     string
	history  []bool    // true = success, false = timeout (ring buffer)
	rtts     []float64 // RTT values for successful probes
	writeIdx int
	result   ProbeResult
}

// NewProber creates a prober that will measure latency to neighbors.
func NewProber(localName string, listenAddr string, config ProbeConfig) (*Prober, error) {
	addr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve probe listen addr %q: %w", listenAddr, err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen probe udp %q: %w", listenAddr, err)
	}

	return &Prober{
		config:    config,
		results:   make(map[string]*probeState),
		stopCh:    make(chan struct{}),
		conn:      conn,
		localName: localName,
	}, nil
}

// AddNeighbor registers a neighbor to probe.
func (p *Prober) AddNeighbor(name, addr string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.results[name] = &probeState{
		addr:    addr,
		history: make([]bool, p.config.WindowSize),
		rtts:    make([]float64, p.config.WindowSize),
	}
}

// Start begins probing all neighbors in the background.
func (p *Prober) Start() {
	go p.listenLoop()
	go p.probeLoop()
}

// Stop terminates all probing goroutines and closes the connection.
func (p *Prober) Stop() {
	close(p.stopCh)
	p.conn.Close()
}

// GetResults returns a snapshot of all probe results.
func (p *Prober) GetResults() map[string]ProbeResult {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make(map[string]ProbeResult, len(p.results))
	for name, state := range p.results {
		out[name] = state.result
	}
	return out
}

// GetResult returns the probe result for a specific neighbor.
func (p *Prober) GetResult(neighbor string) (ProbeResult, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	state, ok := p.results[neighbor]
	if !ok {
		return ProbeResult{}, false
	}
	return state.result, true
}

// ──────────────────────────────────────────────────────────────
// Internal loops
// ──────────────────────────────────────────────────────────────

// listenLoop handles incoming probe requests and sends immediate replies.
func (p *Prober) listenLoop() {
	buf := make([]byte, 128)
	readTimeout := time.Duration(p.config.TimeoutMs*2) * time.Millisecond
	if readTimeout < 100*time.Millisecond {
		readTimeout = 100 * time.Millisecond
	}

	for {
		select {
		case <-p.stopCh:
			return
		default:
		}

		p.conn.SetReadDeadline(time.Now().Add(readTimeout))
		n, remoteAddr, err := p.conn.ReadFromUDP(buf)
		if err != nil {
			continue // Timeout or closed — check stopCh next iteration.
		}

		msg := string(buf[:n])

		// If it's a PING, reply with PONG + original timestamp.
		if len(msg) > 5 && msg[:5] == "PING:" {
			reply := "PONG:" + msg[5:]
			p.conn.WriteToUDP([]byte(reply), remoteAddr)
		}

		// If it's a PONG, record the RTT.
		if len(msg) > 5 && msg[:5] == "PONG:" {
			sentTime, err := time.Parse(time.RFC3339Nano, msg[5:])
			if err != nil {
				continue
			}
			rtt := time.Since(sentTime)
			p.recordSuccess(remoteAddr.String(), rtt)
		}
	}
}

// probeLoop sends periodic probes to all neighbors.
func (p *Prober) probeLoop() {
	ticker := time.NewTicker(time.Duration(p.config.IntervalMs) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.probeAll()
		}
	}
}

// probeAll sends a single probe to every registered neighbor.
func (p *Prober) probeAll() {
	p.mu.RLock()
	neighbors := make(map[string]string, len(p.results))
	for name, state := range p.results {
		neighbors[name] = state.addr
	}
	p.mu.RUnlock()

	for name, addrStr := range neighbors {
		go p.probeSingle(name, addrStr)
	}
}

// probeSingle sends one probe to a neighbor. Failure is recorded
// after a timeout using a timer instead of time.Sleep to prevent
// goroutine accumulation under load.
func (p *Prober) probeSingle(name, addrStr string) {
	addr, err := net.ResolveUDPAddr("udp", addrStr)
	if err != nil {
		p.recordFailure(name)
		return
	}

	// Send PING with current timestamp.
	msg := "PING:" + time.Now().Format(time.RFC3339Nano)
	if _, err = p.conn.WriteToUDP([]byte(msg), addr); err != nil {
		p.recordFailure(name)
		return
	}

	// Schedule a timeout check. The timer is GC'd if it fires or
	// the prober is stopped — no goroutine leak.
	timeout := time.Duration(p.config.TimeoutMs) * time.Millisecond
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-p.stopCh:
		return
	case <-timer.C:
		p.mu.RLock()
		state, ok := p.results[name]
		p.mu.RUnlock()
		if !ok {
			return
		}
		// If last seen is older than 2× timeout, record as failure.
		if time.Since(state.result.LastSeen) > timeout*2 {
			p.recordFailure(name)
		}
	}
}

// ──────────────────────────────────────────────────────────────
// Recording
// ──────────────────────────────────────────────────────────────

// recordSuccess records a successful probe with the given RTT.
func (p *Prober) recordSuccess(remoteAddr string, rtt time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, state := range p.results {
		if state.addr == remoteAddr || matchAddr(state.addr, remoteAddr) {
			idx := state.writeIdx % p.config.WindowSize
			state.history[idx] = true
			state.rtts[idx] = float64(rtt.Microseconds()) / 1000.0 // convert to ms
			state.writeIdx++
			state.result = computeResult(state, p.config.WindowSize)
			state.result.LastSeen = time.Now()
			state.result.Alive = true
			return
		}
	}
}

// recordFailure records a failed probe for a neighbor.
func (p *Prober) recordFailure(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	state, ok := p.results[name]
	if !ok {
		return
	}

	idx := state.writeIdx % p.config.WindowSize
	state.history[idx] = false
	state.rtts[idx] = 0
	state.writeIdx++
	state.result = computeResult(state, p.config.WindowSize)
}

// computeResult calculates average RTT and loss from the sliding window.
func computeResult(state *probeState, windowSize int) ProbeResult {
	total := windowSize
	if state.writeIdx < windowSize {
		total = state.writeIdx
	}
	if total == 0 {
		return ProbeResult{}
	}

	successes := 0
	var rttSum float64

	for i := 0; i < total; i++ {
		idx := i
		if state.writeIdx > windowSize {
			idx = (state.writeIdx - windowSize + i) % windowSize
		}
		if state.history[idx] {
			successes++
			rttSum += state.rtts[idx]
		}
	}

	result := state.result // preserve LastSeen and Alive
	if successes > 0 {
		result.LatencyMs = rttSum / float64(successes)
	}
	result.LossPct = float64(total-successes) / float64(total) * 100.0
	return result
}

// ──────────────────────────────────────────────────────────────
// Address matching
// ──────────────────────────────────────────────────────────────

// matchAddr checks if two address strings refer to the same endpoint.
// Handles cases where resolved addresses differ in format (e.g., IPv4 vs IPv6).
func matchAddr(configured, actual string) bool {
	confHost, confPort, err1 := net.SplitHostPort(configured)
	actHost, actPort, err2 := net.SplitHostPort(actual)
	if err1 != nil || err2 != nil {
		return false
	}

	if confPort != actPort {
		return false
	}

	confIP := net.ParseIP(confHost)
	actIP := net.ParseIP(actHost)
	if confIP != nil && actIP != nil {
		return confIP.Equal(actIP)
	}

	return confHost == actHost
}
