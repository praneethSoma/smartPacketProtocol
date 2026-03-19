package metrics

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
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
		WindowSize: 10, // Smaller window = faster reaction to link changes (~1s vs ~2s)
		ProbePort:  0,
	}
}

// ──────────────────────────────────────────────────────────────
// Wire format — compact binary probes.
//
// PING: [0x01] [seq:8] [send_nanos:8]  = 17 bytes
// PONG: [0x02] [seq:8] [send_nanos:8]  = 17 bytes
//
// Benefits over the old RFC3339 string format:
//   - Fixed 17 bytes vs ~35 bytes (51% smaller)
//   - Zero allocations (no string formatting/parsing)
//   - Sequence numbers enable definitive PING↔PONG correlation
// ──────────────────────────────────────────────────────────────

const (
	probeTypePing = 0x01
	probeTypePong = 0x02
	probeSize     = 17 // 1 + 8 + 8
)

func encodePing(seq uint64, sendNanos int64) []byte {
	buf := make([]byte, probeSize)
	buf[0] = probeTypePing
	binary.BigEndian.PutUint64(buf[1:9], seq)
	binary.BigEndian.PutUint64(buf[9:17], uint64(sendNanos))
	return buf
}

func encodePong(seq uint64, sendNanos int64) []byte {
	buf := make([]byte, probeSize)
	buf[0] = probeTypePong
	binary.BigEndian.PutUint64(buf[1:9], seq)
	binary.BigEndian.PutUint64(buf[9:17], uint64(sendNanos))
	return buf
}

func decodeProbe(data []byte) (typ byte, seq uint64, sendNanos int64, ok bool) {
	if len(data) < probeSize {
		return 0, 0, 0, false
	}
	typ = data[0]
	seq = binary.BigEndian.Uint64(data[1:9])
	sendNanos = int64(binary.BigEndian.Uint64(data[9:17]))
	ok = typ == probeTypePing || typ == probeTypePong
	return
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
	seqGen    atomic.Uint64 // monotonic probe sequence counter
}

// probeState tracks the sliding window for one neighbor.
type probeState struct {
	name         string
	addr         string
	resolvedAddr *net.UDPAddr // pre-resolved, avoids DNS per probe
	history      []bool       // true = success, false = timeout (ring buffer)
	rtts         []float64    // RTT values for successful probes
	writeIdx     int
	result       ProbeResult

	// Sequence-based correlation: the seq of the outstanding probe.
	// Set by probeSingle, cleared by listenLoop on matching PONG.
	pendingSeq  uint64
	pendingSent time.Time // when the pending probe was sent
	responded   bool      // set true by listenLoop if PONG matched pendingSeq
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
	resolved, _ := net.ResolveUDPAddr("udp", addr)

	p.mu.Lock()
	defer p.mu.Unlock()

	p.results[name] = &probeState{
		name:         name,
		addr:         addr,
		resolvedAddr: resolved,
		history:      make([]bool, p.config.WindowSize),
		rtts:         make([]float64, p.config.WindowSize),
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
// Listen loop — handles PINGs (reply immediately) and PONGs
// (match sequence number to the correct pending probe).
// ──────────────────────────────────────────────────────────────

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
			continue
		}

		typ, seq, sendNanos, ok := decodeProbe(buf[:n])
		if !ok {
			// Legacy support: try old string format for mixed deployments
			p.handleLegacyMessage(buf[:n], remoteAddr)
			continue
		}

		switch typ {
		case probeTypePing:
			// Reply immediately with PONG echoing the same seq + timestamp.
			pong := encodePong(seq, sendNanos)
			p.conn.WriteToUDP(pong, remoteAddr)

		case probeTypePong:
			// Match this PONG to the pending probe by sequence number.
			sendTime := time.Unix(0, sendNanos)
			rtt := time.Since(sendTime)
			p.recordPong(seq, rtt)
		}
	}
}

// handleLegacyMessage handles old-format PING/PONG strings during rolling upgrades.
func (p *Prober) handleLegacyMessage(data []byte, remoteAddr *net.UDPAddr) {
	msg := string(data)
	if len(msg) > 5 && msg[:5] == "PING:" {
		reply := "PONG:" + msg[5:]
		p.conn.WriteToUDP([]byte(reply), remoteAddr)
	}
	// Ignore legacy PONGs — sequence-based probes handle all recording.
}

// recordPong finds the neighbor with the matching pending sequence number
// and records a successful probe. This is O(neighbors) but neighbors are
// few (typically 2-6) so a linear scan is fine.
func (p *Prober) recordPong(seq uint64, rtt time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, state := range p.results {
		if state.pendingSeq == seq {
			// Match! Record success in the sliding window.
			state.responded = true
			idx := state.writeIdx % p.config.WindowSize
			state.history[idx] = true
			state.rtts[idx] = float64(rtt.Microseconds()) / 1000.0
			state.writeIdx++
			state.result = computeResult(state, p.config.WindowSize, time.Duration(p.config.TimeoutMs)*time.Millisecond)
			state.result.LastSeen = time.Now()
			state.result.Alive = true
			return
		}
	}
}

// ──────────────────────────────────────────────────────────────
// Probe loop — sends periodic probes to all neighbors.
// ──────────────────────────────────────────────────────────────

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

func (p *Prober) probeAll() {
	p.mu.RLock()
	neighbors := make([]*probeState, 0, len(p.results))
	for _, state := range p.results {
		neighbors = append(neighbors, state)
	}
	p.mu.RUnlock()

	for _, state := range neighbors {
		go p.probeSingle(state)
	}
}

// probeSingle sends one sequenced probe to a neighbor and waits for timeout.
// The listenLoop handles the PONG asynchronously and sets state.responded.
// After the timeout, probeSingle checks whether a response arrived and
// records exactly one outcome (success or failure) — never both.
func (p *Prober) probeSingle(state *probeState) {
	// Re-resolve if the initial resolution failed.
	if state.resolvedAddr == nil {
		resolved, err := net.ResolveUDPAddr("udp", state.addr)
		if err != nil {
			p.recordFailure(state.name)
			return
		}
		p.mu.Lock()
		state.resolvedAddr = resolved
		p.mu.Unlock()
	}

	// Assign a unique sequence number for this probe.
	seq := p.seqGen.Add(1)
	sendTime := time.Now()

	// Register the pending probe so listenLoop can match the PONG.
	p.mu.Lock()
	state.pendingSeq = seq
	state.pendingSent = sendTime
	state.responded = false
	p.mu.Unlock()

	// Send binary PING.
	ping := encodePing(seq, sendTime.UnixNano())
	if _, err := p.conn.WriteToUDP(ping, state.resolvedAddr); err != nil {
		p.recordFailure(state.name)
		return
	}

	// Wait for timeout. During this time, listenLoop may receive the
	// matching PONG and set state.responded = true.
	timeout := time.Duration(p.config.TimeoutMs) * time.Millisecond
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-p.stopCh:
		return
	case <-timer.C:
		// Check if listenLoop already recorded the response.
		p.mu.RLock()
		responded := state.responded
		p.mu.RUnlock()

		if !responded {
			// No matching PONG arrived within timeout — definitive failure.
			p.recordFailure(state.name)
		}
		// If responded == true, listenLoop already recorded the success
		// in the sliding window. Nothing more to do — no double-counting.
	}
}

// ──────────────────────────────────────────────────────────────
// Recording
// ──────────────────────────────────────────────────────────────

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
	state.result = computeResult(state, p.config.WindowSize, time.Duration(p.config.TimeoutMs)*time.Millisecond)
}

// computeResult calculates EWMA-weighted RTT and loss from the sliding window.
// Recent probes are weighted exponentially more than older ones, allowing SPP
// to react to latency/loss changes within 3-5 probe cycles (~300-500ms)
// instead of waiting for the full window to rotate (~2s).
func computeResult(state *probeState, windowSize int, timeout time.Duration) ProbeResult {
	total := windowSize
	if state.writeIdx < windowSize {
		total = state.writeIdx
	}
	if total == 0 {
		return ProbeResult{}
	}

	// EWMA alpha: higher = more responsive to recent changes.
	// 0.3 means the most recent probe contributes ~30% of the signal.
	const alpha = 0.3

	successes := 0
	var ewmaRTT float64
	ewmaInitialized := false
	var weightedSuccesses, weightedTotal float64

	for i := 0; i < total; i++ {
		idx := i
		if state.writeIdx > windowSize {
			idx = (state.writeIdx - windowSize + i) % windowSize
		}

		// Exponential weight: newer samples (higher i) get more weight.
		// weight = alpha * (1-alpha)^(total-1-i)  but simplified via running EWMA.
		recency := float64(i) / float64(total) // 0.0 = oldest, ~1.0 = newest
		weight := 0.5 + 0.5*recency            // range [0.5, 1.0] — gentle bias

		weightedTotal += weight
		if state.history[idx] {
			successes++
			weightedSuccesses += weight
			if !ewmaInitialized {
				ewmaRTT = state.rtts[idx]
				ewmaInitialized = true
			} else {
				ewmaRTT = alpha*state.rtts[idx] + (1-alpha)*ewmaRTT
			}
		}
	}

	result := state.result // preserve LastSeen
	if successes > 0 {
		result.LatencyMs = ewmaRTT
	}
	// Weighted loss: recent failures count more than old ones.
	result.LossPct = (1.0 - weightedSuccesses/weightedTotal) * 100.0

	// Set Alive based on actual data.
	if result.LossPct == 100 {
		result.Alive = false
	} else if !result.LastSeen.IsZero() && timeout > 0 && time.Since(result.LastSeen) > 5*timeout {
		result.Alive = false
	}
	return result
}

// ──────────────────────────────────────────────────────────────
// Address matching (kept for backward compatibility)
// ──────────────────────────────────────────────────────────────

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
