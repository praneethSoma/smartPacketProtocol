package gossip

import (
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"time"

	"smartpacket/metrics"
)

// ──────────────────────────────────────────────────────────────
// Gossip protocol constants
// ──────────────────────────────────────────────────────────────

const (
	// UDPMaxPayload is the maximum UDP datagram size.
	// Standard UDP payloads should not exceed 65535 bytes.
	UDPMaxPayload = 65535

	// DefaultGossipIntervalMs is the fast broadcast interval
	// used when topology changes are detected.
	DefaultGossipIntervalMs = 50

	// DefaultStableIntervalMs is the slow broadcast interval
	// used when the network topology is stable. Must be low enough
	// that multi-hop propagation completes within MaxStalenessMs.
	DefaultStableIntervalMs = 200

	// DefaultFullSyncIntervalMs is the interval for full-state
	// broadcasts as a safety net against topology drift.
	DefaultFullSyncIntervalMs = 2000

	// DefaultMaxStalenessMs is the age after which a link is pruned.
	// Must be large enough for multi-hop gossip propagation:
	// e.g., 3 hops × 200ms stable interval = 600ms, so 5000ms gives ample margin.
	DefaultMaxStalenessMs = 5000

	// DefaultWarnStalenessMs is the age at which a link is flagged
	// as unreliable and receives a penalty in routing.
	DefaultWarnStalenessMs = 300

	// DefaultReadDeadlineMs is the read timeout on the gossip
	// UDP socket per iteration.
	DefaultReadDeadlineMs = 100
)

// GossipMessage is what routers exchange to share topology state.
type GossipMessage struct {
	SenderName string
	States     []LinkState
	SentAt     time.Time
	IsDelta    bool // true = partial update, false = full state sync
}

// GossipConfig controls gossip behavior.
type GossipConfig struct {
	NodeName           string
	ListenAddr         string            // UDP address for gossip
	Neighbors          map[string]string  // neighbor name → gossip address
	IntervalMs         int               // Fast broadcast interval (default 50ms)
	StableIntervalMs   int               // Slow broadcast interval (default 500ms)
	FullSyncIntervalMs int               // Full-state sync interval (default 5000ms)
	MaxStalenessMs     int               // Max age before pruning (default 1000ms)
	WarnStalenessMs    int               // Age at which link is flagged (default 300ms)
	ReadDeadlineMs     int               // UDP read timeout per iteration (default 100ms)
}

// DefaultGossipConfig returns sensible defaults.
func DefaultGossipConfig() GossipConfig {
	return GossipConfig{
		IntervalMs:         DefaultGossipIntervalMs,
		StableIntervalMs:   DefaultStableIntervalMs,
		FullSyncIntervalMs: DefaultFullSyncIntervalMs,
		MaxStalenessMs:     DefaultMaxStalenessMs,
		WarnStalenessMs:    DefaultWarnStalenessMs,
		ReadDeadlineMs:     DefaultReadDeadlineMs,
	}
}

// applyDefaults fills zero-valued fields with defaults.
func (c *GossipConfig) applyDefaults() {
	defaults := DefaultGossipConfig()
	if c.IntervalMs == 0 {
		c.IntervalMs = defaults.IntervalMs
	}
	if c.StableIntervalMs == 0 {
		c.StableIntervalMs = defaults.StableIntervalMs
	}
	if c.FullSyncIntervalMs == 0 {
		c.FullSyncIntervalMs = defaults.FullSyncIntervalMs
	}
	if c.MaxStalenessMs == 0 {
		c.MaxStalenessMs = defaults.MaxStalenessMs
	}
	if c.WarnStalenessMs == 0 {
		c.WarnStalenessMs = defaults.WarnStalenessMs
	}
	if c.ReadDeadlineMs == 0 {
		c.ReadDeadlineMs = defaults.ReadDeadlineMs
	}
}

// ──────────────────────────────────────────────────────────────
// GossipNode — runs the gossip protocol for one router.
// ──────────────────────────────────────────────────────────────

// GossipNode manages gossip broadcasting and receiving for a single router.
type GossipNode struct {
	config        GossipConfig
	state         *TopologyState
	collector     *metrics.Collector
	conn          *net.UDPConn
	stopCh        chan struct{}
	lastFullSync  time.Time
	resolvedAddrs map[string]*net.UDPAddr // cached neighbor DNS resolutions
}

// NewGossipNode creates a gossip node that will share topology with neighbors.
func NewGossipNode(config GossipConfig, state *TopologyState, collector *metrics.Collector) (*GossipNode, error) {
	config.applyDefaults()

	addr, err := net.ResolveUDPAddr("udp", config.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve gossip addr %q: %w", config.ListenAddr, err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen gossip %q: %w", config.ListenAddr, err)
	}

	// Pre-resolve all neighbor addresses to avoid DNS lookups on every broadcast.
	resolved := make(map[string]*net.UDPAddr, len(config.Neighbors))
	for name, addrStr := range config.Neighbors {
		nAddr, err := net.ResolveUDPAddr("udp", addrStr)
		if err != nil {
			return nil, fmt.Errorf("resolve neighbor %s addr %q: %w", name, addrStr, err)
		}
		resolved[name] = nAddr
	}

	return &GossipNode{
		config:        config,
		state:         state,
		collector:     collector,
		conn:          conn,
		stopCh:        make(chan struct{}),
		lastFullSync:  time.Time{}, // Zero → first broadcast is full-sync.
		resolvedAddrs: resolved,
	}, nil
}

// Start begins gossiping — broadcasting and receiving link states.
func (g *GossipNode) Start() {
	go g.receiveLoop()
	go g.broadcastLoop()
	go g.metricsLoop()
	go g.pruneLoop()

	log.Info("gossip started",
		"node", g.config.NodeName,
		"addr", g.config.ListenAddr,
		"interval_ms", g.config.IntervalMs,
	)
}

// Stop terminates all gossip goroutines and closes the socket.
func (g *GossipNode) Stop() {
	close(g.stopCh)
	g.conn.Close()
}

// GetState returns the topology state for building mini maps.
func (g *GossipNode) GetState() *TopologyState {
	return g.state
}

// ──────────────────────────────────────────────────────────────
// Metrics loop — reads local probes and updates topology.
// ──────────────────────────────────────────────────────────────

func (g *GossipNode) metricsLoop() {
	ticker := time.NewTicker(time.Duration(g.config.IntervalMs) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-g.stopCh:
			return
		case <-ticker.C:
			g.updateFromMetrics()
		}
	}
}

func (g *GossipNode) updateFromMetrics() {
	if g.collector == nil {
		return
	}

	m := g.collector.GetMetrics()
	for neighborName, probeResult := range m.Neighbors {
		g.state.UpdateLocal(
			g.config.NodeName,
			neighborName,
			probeResult.LatencyMs,
			m.LoadPct,
			probeResult.LossPct,
		)
	}
}

// ──────────────────────────────────────────────────────────────
// Broadcast loop — adaptive frequency gossip.
// ──────────────────────────────────────────────────────────────

func (g *GossipNode) broadcastLoop() {
	fastInterval := time.Duration(g.config.IntervalMs) * time.Millisecond
	stableInterval := time.Duration(g.config.StableIntervalMs) * time.Millisecond

	currentInterval := fastInterval // Start fast for quick convergence.
	timer := time.NewTimer(currentInterval)
	defer timer.Stop()

	for {
		select {
		case <-g.stopCh:
			return
		case <-timer.C:
			g.broadcast()

			if g.state.HasChanges() {
				currentInterval = fastInterval
			} else {
				currentInterval = stableInterval
			}
			timer.Reset(currentInterval)
		}
	}
}

func (g *GossipNode) broadcast() {
	fullSyncAge := time.Duration(g.config.FullSyncIntervalMs) * time.Millisecond
	needFullSync := time.Since(g.lastFullSync) >= fullSyncAge

	var states []LinkState
	isDelta := false

	if needFullSync {
		states = g.state.GetAllStates()
		g.lastFullSync = time.Now()
	} else {
		states = g.state.GetChangedStates()
		isDelta = true
	}

	if len(states) == 0 {
		return
	}

	msg := GossipMessage{
		SenderName: g.config.NodeName,
		States:     states,
		SentAt:     time.Now(),
		IsDelta:    isDelta,
	}

	data, err := EncodeGossipMessage(msg)
	if err != nil {
		log.Warn("gossip encode failed", "err", err)
		return
	}

	for name, addr := range g.resolvedAddrs {
		if _, err := g.conn.WriteToUDP(data, addr); err != nil {
			// Re-resolve on send error (address may have changed).
			if newAddr, resolveErr := net.ResolveUDPAddr("udp", g.config.Neighbors[name]); resolveErr == nil {
				g.resolvedAddrs[name] = newAddr
			}
		}
	}

	g.state.MarkBroadcast()
}

// ──────────────────────────────────────────────────────────────
// Receive loop — handles incoming gossip messages.
// ──────────────────────────────────────────────────────────────

func (g *GossipNode) receiveLoop() {
	buf := make([]byte, UDPMaxPayload)
	readDeadline := time.Duration(g.config.ReadDeadlineMs) * time.Millisecond

	for {
		select {
		case <-g.stopCh:
			return
		default:
		}

		g.conn.SetReadDeadline(time.Now().Add(readDeadline))
		n, remoteAddr, err := g.conn.ReadFromUDP(buf)
		if err != nil {
			continue // Timeout or closed — check stopCh next iteration.
		}

		// Handle MAP_REQUEST from senders querying for topology.
		if n == len("MAP_REQUEST") && string(buf[:n]) == "MAP_REQUEST" {
			log.Info("MAP_REQUEST received", "node", g.config.NodeName, "from", remoteAddr)
			g.respondToMapRequest(remoteAddr)
			continue
		}

		msg, err := DecodeGossipMessage(buf[:n])
		if err != nil {
			log.Warn("gossip decode failed", "err", err)
			continue
		}

		updated := g.state.MergeRemote(msg.States)
		if updated > 0 {
			log.Info("gossip merged",
				"node", g.config.NodeName,
				"updates", updated,
				"from", msg.SenderName,
			)
		}
	}
}

// respondToMapRequest sends a full gossip state message back to the requester.
func (g *GossipNode) respondToMapRequest(addr *net.UDPAddr) {
	states := g.state.GetAllStates()
	if len(states) == 0 {
		log.Warn("MAP_REQUEST: no topology data to send", "node", g.config.NodeName)
		return
	}

	msg := GossipMessage{
		SenderName: g.config.NodeName,
		States:     states,
		SentAt:     time.Now(),
		IsDelta:    false,
	}

	data, err := EncodeGossipMessage(msg)
	if err != nil {
		log.Warn("MAP_REQUEST: encode failed", "err", err)
		return
	}

	if _, err := g.conn.WriteToUDP(data, addr); err != nil {
		log.Warn("MAP_REQUEST: send failed", "addr", addr, "err", err)
		return
	}

	log.Info("MAP_REQUEST: responded",
		"node", g.config.NodeName,
		"links", len(states),
		"to", addr,
	)
}

// ──────────────────────────────────────────────────────────────
// Prune loop — removes stale entries.
// ──────────────────────────────────────────────────────────────

func (g *GossipNode) pruneLoop() {
	maxAge := time.Duration(g.config.MaxStalenessMs) * time.Millisecond
	ticker := time.NewTicker(maxAge / 2)
	defer ticker.Stop()

	for {
		select {
		case <-g.stopCh:
			return
		case <-ticker.C:
			g.state.PruneStale(maxAge)
		}
	}
}

// ──────────────────────────────────────────────────────────────
// Compact binary encoding for gossip messages.
//
// Wire format (all multi-byte values are big-endian):
//
//   [magic: 2B] [flags: 1B] [sender_name_len: 1B] [sender_name: NB]
//   [sent_at_nanos: 8B] [state_count: 2B]
//   Per LinkState:
//     [from_len: 1B] [from: NB] [to_len: 1B] [to: NB]
//     [origin_len: 1B] [origin: NB]
//     [latency_ms: 8B float64] [load_pct: 8B float64] [loss_pct: 8B float64]
//     [timestamp_nanos: 8B] [sequence: 8B]
//
// ~50 bytes per LinkState vs ~150+ with gob. Zero reflection overhead.
// ──────────────────────────────────────────────────────────────

const (
	gossipMagic1 = 0x53 // 'S' for SPP
	gossipMagic2 = 0x47 // 'G' for Gossip
	flagIsDelta  = 0x01
)

// EncodeGossipMessage encodes a GossipMessage to compact binary format.
func EncodeGossipMessage(msg GossipMessage) ([]byte, error) {
	// Estimate size: header ~12B + sender + states * ~60B each.
	est := 12 + len(msg.SenderName)
	for _, s := range msg.States {
		est += 3 + len(s.From) + len(s.To) + len(s.Origin) + 40
	}
	buf := make([]byte, 0, est)

	// Magic
	buf = append(buf, gossipMagic1, gossipMagic2)

	// Flags
	var flags byte
	if msg.IsDelta {
		flags |= flagIsDelta
	}
	buf = append(buf, flags)

	// Sender name
	if len(msg.SenderName) > 255 {
		return nil, fmt.Errorf("sender name too long: %d", len(msg.SenderName))
	}
	buf = append(buf, byte(len(msg.SenderName)))
	buf = append(buf, msg.SenderName...)

	// SentAt
	buf = binary.BigEndian.AppendUint64(buf, uint64(msg.SentAt.UnixNano()))

	// State count
	if len(msg.States) > 65535 {
		return nil, fmt.Errorf("too many states: %d", len(msg.States))
	}
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(msg.States)))

	// Each LinkState
	for _, s := range msg.States {
		if len(s.From) > 255 || len(s.To) > 255 || len(s.Origin) > 255 {
			return nil, fmt.Errorf("node name too long")
		}
		buf = append(buf, byte(len(s.From)))
		buf = append(buf, s.From...)
		buf = append(buf, byte(len(s.To)))
		buf = append(buf, s.To...)
		buf = append(buf, byte(len(s.Origin)))
		buf = append(buf, s.Origin...)
		buf = binary.BigEndian.AppendUint64(buf, math.Float64bits(s.LatencyMs))
		buf = binary.BigEndian.AppendUint64(buf, math.Float64bits(s.LoadPct))
		buf = binary.BigEndian.AppendUint64(buf, math.Float64bits(s.LossPct))
		buf = binary.BigEndian.AppendUint64(buf, uint64(s.Timestamp.UnixNano()))
		buf = binary.BigEndian.AppendUint64(buf, s.Sequence)
	}

	return buf, nil
}

// DecodeGossipMessage decodes a GossipMessage from compact binary format.
func DecodeGossipMessage(data []byte) (GossipMessage, error) {
	if len(data) < 4 {
		return GossipMessage{}, fmt.Errorf("gossip message too short: %d bytes", len(data))
	}

	if data[0] != gossipMagic1 || data[1] != gossipMagic2 {
		return GossipMessage{}, fmt.Errorf("invalid gossip magic: %x%x", data[0], data[1])
	}

	off := 2
	flags := data[off]
	off++

	// Sender name
	senderLen := int(data[off])
	off++
	if off+senderLen > len(data) {
		return GossipMessage{}, fmt.Errorf("truncated sender name")
	}
	senderName := string(data[off : off+senderLen])
	off += senderLen

	// SentAt
	if off+8 > len(data) {
		return GossipMessage{}, fmt.Errorf("truncated sent_at")
	}
	sentAtNanos := int64(binary.BigEndian.Uint64(data[off : off+8]))
	off += 8

	// State count
	if off+2 > len(data) {
		return GossipMessage{}, fmt.Errorf("truncated state count")
	}
	stateCount := int(binary.BigEndian.Uint16(data[off : off+2]))
	off += 2

	states := make([]LinkState, stateCount)
	for i := 0; i < stateCount; i++ {
		// Read 3 length-prefixed strings
		readStr := func() (string, error) {
			if off >= len(data) {
				return "", fmt.Errorf("truncated string length at state %d", i)
			}
			sLen := int(data[off])
			off++
			if off+sLen > len(data) {
				return "", fmt.Errorf("truncated string data at state %d", i)
			}
			s := string(data[off : off+sLen])
			off += sLen
			return s, nil
		}

		from, err := readStr()
		if err != nil {
			return GossipMessage{}, err
		}
		to, err := readStr()
		if err != nil {
			return GossipMessage{}, err
		}
		origin, err := readStr()
		if err != nil {
			return GossipMessage{}, err
		}

		// 5 × uint64 = 40 bytes
		if off+40 > len(data) {
			return GossipMessage{}, fmt.Errorf("truncated state %d metrics", i)
		}
		latencyMs := math.Float64frombits(binary.BigEndian.Uint64(data[off : off+8]))
		off += 8
		loadPct := math.Float64frombits(binary.BigEndian.Uint64(data[off : off+8]))
		off += 8
		lossPct := math.Float64frombits(binary.BigEndian.Uint64(data[off : off+8]))
		off += 8
		tsNanos := int64(binary.BigEndian.Uint64(data[off : off+8]))
		off += 8
		seq := binary.BigEndian.Uint64(data[off : off+8])
		off += 8

		states[i] = LinkState{
			From:      from,
			To:        to,
			Origin:    origin,
			LatencyMs: latencyMs,
			LoadPct:   loadPct,
			LossPct:   lossPct,
			Timestamp: time.Unix(0, tsNanos),
			Sequence:  seq,
		}
	}

	return GossipMessage{
		SenderName: senderName,
		States:     states,
		SentAt:     time.Unix(0, sentAtNanos),
		IsDelta:    flags&flagIsDelta != 0,
	}, nil
}
