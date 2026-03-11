package gossip

import (
	"bytes"
	"encoding/gob"
	"fmt"
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
	// used when the network topology is stable.
	DefaultStableIntervalMs = 500

	// DefaultFullSyncIntervalMs is the interval for full-state
	// broadcasts as a safety net against topology drift.
	DefaultFullSyncIntervalMs = 5000

	// DefaultMaxStalenessMs is the age after which a link is pruned.
	DefaultMaxStalenessMs = 1000

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
	config       GossipConfig
	state        *TopologyState
	collector    *metrics.Collector
	conn         *net.UDPConn
	stopCh       chan struct{}
	lastFullSync time.Time
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

	return &GossipNode{
		config:       config,
		state:        state,
		collector:    collector,
		conn:         conn,
		stopCh:       make(chan struct{}),
		lastFullSync: time.Time{}, // Zero → first broadcast is full-sync.
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

	data, err := encodeGossipMessage(msg)
	if err != nil {
		log.Warn("gossip encode failed", "err", err)
		return
	}

	for _, addrStr := range g.config.Neighbors {
		addr, err := net.ResolveUDPAddr("udp", addrStr)
		if err != nil {
			continue
		}
		g.conn.WriteToUDP(data, addr)
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

		msg, err := decodeGossipMessage(buf[:n])
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

	data, err := encodeGossipMessage(msg)
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
// Gob encoding for gossip messages.
// ──────────────────────────────────────────────────────────────

func encodeGossipMessage(msg GossipMessage) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(msg); err != nil {
		return nil, fmt.Errorf("gob encode gossip: %w", err)
	}
	return buf.Bytes(), nil
}

func decodeGossipMessage(data []byte) (GossipMessage, error) {
	var msg GossipMessage
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&msg); err != nil {
		return GossipMessage{}, fmt.Errorf("gob decode gossip: %w", err)
	}
	return msg, nil
}
