package packet

import (
	"fmt"
	"math"
)

// ──────────────────────────────────────────────────────────────
// Protocol defaults — all tunable, no hardcoded magic numbers.
// ──────────────────────────────────────────────────────────────

const (
	// DefaultMaxHops is the TTL applied to every new packet.
	// Prevents infinite routing loops even when loop detection fails.
	DefaultMaxHops uint8 = 16

	// DefaultRerouteThresholdPct is the percentage divergence between
	// the packet's map view and actual measured conditions that triggers
	// mid-flight rerouting. A value of 30 means "reroute if reality
	// diverges from the map by more than 30%".
	DefaultRerouteThresholdPct = 30.0

	// RerouteThreshold is a backward-compatible alias for DefaultRerouteThresholdPct.
	// Deprecated: use DefaultRerouteThresholdPct instead.
	RerouteThreshold = DefaultRerouteThresholdPct

	// DefaultSignificantLatencyMs is the absolute latency (in ms) that
	// is considered "significant" when the map predicts ~0ms latency.
	// If the map says a link has 0ms latency but the actual measurement
	// exceeds this value, a reroute is triggered without percentage math.
	DefaultSignificantLatencyMs = 10.0

	// DefaultMinDivergenceLoad is the minimum absolute load difference
	// (in percentage points) required to trigger a reroute. Prevents
	// spurious reroutes when both actual and expected values are small
	// (e.g., 0.8% vs 2.9% — 72% relative divergence but only 2.1 pp
	// absolute difference, which is insignificant).
	DefaultMinDivergenceLoad = 10.0

	// DefaultMinDivergenceLatencyMs is the minimum absolute latency
	// difference (in ms) required to trigger a reroute.
	DefaultMinDivergenceLatencyMs = 5.0

	// MaxReliability is the upper bound for IntentHeader.Reliability.
	MaxReliability uint8 = 2
	// MaxLatency is the upper bound for IntentHeader.Latency.
	MaxLatency uint8 = 3
	// MaxOrdering is the upper bound for IntentHeader.Ordering.
	MaxOrdering uint8 = 2
	// MaxPriority is the upper bound for IntentHeader.Priority.
	MaxPriority uint8 = 3
)

// ──────────────────────────────────────────────────────────────
// Logger — library code is silent by default.
// Callers inject their own logger; the default is a no-op.
// ──────────────────────────────────────────────────────────────

// Logger defines the logging interface used by the packet library.
// Library code NEVER writes to stdout directly — it calls this
// interface so the application can route logs wherever it wants.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// noopLogger discards all log messages (default for library code).
type noopLogger struct{}

func (noopLogger) Debug(string, ...any) {}
func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Warn(string, ...any)  {}

// log is the package-level logger. Replace with SetLogger().
var log Logger = noopLogger{}

// SetLogger replaces the package-level logger.
// Call this once at program startup to enable library logging.
//
//	packet.SetLogger(slog.Default())
func SetLogger(l Logger) {
	if l == nil {
		log = noopLogger{}
		return
	}
	log = l
}

// ──────────────────────────────────────────────────────────────
// IntentHeader — what the packet needs from the network.
// ──────────────────────────────────────────────────────────────

// IntentHeader describes the QoS requirements of this packet.
// Every field has a well-defined range; use Validate() to check bounds.
type IntentHeader struct {
	Reliability uint8 // 0=none  1=best-effort  2=guaranteed
	Latency     uint8 // 0=relaxed  1=normal  2=low  3=critical
	Ordering    uint8 // 0=none  1=partial  2=strict
	Priority    uint8 // 0=low  1=medium  2=high  3=critical
}

// Validate returns an error if any IntentHeader field is out of range.
func (h IntentHeader) Validate() error {
	if h.Reliability > MaxReliability {
		return fmt.Errorf("reliability %d exceeds max %d", h.Reliability, MaxReliability)
	}
	if h.Latency > MaxLatency {
		return fmt.Errorf("latency %d exceeds max %d", h.Latency, MaxLatency)
	}
	if h.Ordering > MaxOrdering {
		return fmt.Errorf("ordering %d exceeds max %d", h.Ordering, MaxOrdering)
	}
	if h.Priority > MaxPriority {
		return fmt.Errorf("priority %d exceeds max %d", h.Priority, MaxPriority)
	}
	return nil
}

// String returns a human-readable representation of the intent.
func (h IntentHeader) String() string {
	latLabels := [4]string{"relaxed", "normal", "low", "critical"}
	relLabels := [3]string{"none", "best-effort", "guaranteed"}
	priLabels := [4]string{"low", "medium", "high", "critical"}

	lat := "unknown"
	if int(h.Latency) < len(latLabels) {
		lat = latLabels[h.Latency]
	}
	rel := "unknown"
	if int(h.Reliability) < len(relLabels) {
		rel = relLabels[h.Reliability]
	}
	pri := "unknown"
	if int(h.Priority) < len(priLabels) {
		pri = priLabels[h.Priority]
	}
	return fmt.Sprintf("Intent{latency=%s reliability=%s priority=%s}", lat, rel, pri)
}

// ──────────────────────────────────────────────────────────────
// Link — one edge on the packet's mini-map.
// ──────────────────────────────────────────────────────────────

// Link represents one directional connection between two nodes.
type Link struct {
	From      string  // source node name
	To        string  // destination node name
	LatencyMs float64 // current latency in milliseconds
	LoadPct   float64 // current load as a percentage (0–100)
	LossPct   float64 // current packet loss as a percentage (0–100)
}

// ──────────────────────────────────────────────────────────────
// HopRecord — congestion audit trail entry.
// ──────────────────────────────────────────────────────────────

// HopRecord is written by each router the packet passes through,
// recording the real-time conditions observed at that hop.
type HopRecord struct {
	NodeName  string  // which router wrote this
	LoadPct   float64 // how loaded that router was (0–100)
	LatencyMs float64 // actual latency measured at this hop (ms)
}

// ──────────────────────────────────────────────────────────────
// SmartPacket — the complete intelligent packet.
// ──────────────────────────────────────────────────────────────

// SmartPacket carries its own identity, map, routing engine,
// and audit trail. The router's job is reduced to: stamp, read, forward.
type SmartPacket struct {
	// Protocol metadata
	Version    uint8 // Protocol version (currently 1)
	PacketType uint8 // One of PacketType* constants from wire.go

	// Origin and destination
	SourceNode  string // Node that created this packet (for ACK routing)
	Destination string // Final destination node name

	// Intent tags — what this packet needs from the network
	Intent IntentHeader

	// Mini-map — the packet's view of the network topology
	MiniMap []Link

	// Current routing state
	CurrentNode string   // Where the packet is right now
	PlannedPath []string // Computed shortest path
	HopIndex    int      // Current position within PlannedPath

	// TTL — prevents infinite routing loops
	MaxHops  uint8 // Maximum allowed hops (default 16)
	HopCount uint8 // Incremented at each hop

	// Routing flags
	Degraded     bool            // True if forced to take a suboptimal path
	Rerouted     bool            // True if path was recalculated mid-flight
	LightMode    bool            // True if packet carries no MiniMap (routers use local gossip)
	VisitedNodes map[string]bool // Loop detection — tracks every node visited

	// Congestion audit trail — live data written by routers
	CongestionLog []HopRecord

	// Timing — set by the sender for end-to-end latency measurement
	CreatedAtNs int64 // Unix nanosecond timestamp when the packet was created

	// Application payload
	Payload []byte
}

// NewSmartPacket creates a new intelligent packet ready for routing.
// The source node name defaults to empty; set it explicitly when the
// originating node name is known.
func NewSmartPacket(destination string, intent IntentHeader, miniMap []Link, payload []byte) *SmartPacket {
	return &SmartPacket{
		Version:       ProtocolVersion,
		PacketType:    PacketTypeData,
		Destination:   destination,
		Intent:        intent,
		MiniMap:       miniMap,
		CurrentNode:   "",
		PlannedPath:   []string{},
		HopIndex:      0,
		MaxHops:       DefaultMaxHops,
		HopCount:      0,
		Degraded:      false,
		Rerouted:      false,
		VisitedNodes:  make(map[string]bool),
		CongestionLog: []HopRecord{},
		Payload:       payload,
	}
}

// ──────────────────────────────────────────────────────────────
// Hop management
// ──────────────────────────────────────────────────────────────

// LogHop is called by each router to stamp real-time conditions
// into the packet's congestion log. It also advances the hop index
// and records the node as visited for loop detection.
func (p *SmartPacket) LogHop(nodeName string, loadPct float64, latencyMs float64) {
	p.CongestionLog = append(p.CongestionLog, HopRecord{
		NodeName:  nodeName,
		LoadPct:   loadPct,
		LatencyMs: latencyMs,
	})
	p.CurrentNode = nodeName
	p.HopCount++

	// Track visited nodes for loop detection
	if p.VisitedNodes == nil {
		p.VisitedNodes = make(map[string]bool)
	}
	p.VisitedNodes[nodeName] = true

	// Advance hop index so NextHop() stays correct
	for i, node := range p.PlannedPath {
		if node == nodeName {
			p.HopIndex = i
			break
		}
	}
}

// NextHop returns the node this packet should be forwarded to next.
// Returns "" if the packet has reached or passed its destination.
func (p *SmartPacket) NextHop() string {
	if p.HopIndex+1 < len(p.PlannedPath) {
		return p.PlannedPath[p.HopIndex+1]
	}
	return "" // arrived at destination
}

// UpdatePath replaces the planned path (e.g., after Dijkstra runs).
func (p *SmartPacket) UpdatePath(newPath []string) {
	p.PlannedPath = newPath
	p.HopIndex = 0
}

// ──────────────────────────────────────────────────────────────
// Live rerouting — mid-flight path recalculation.
// ──────────────────────────────────────────────────────────────

// ShouldReroute returns true if live conditions at currentNode diverge
// from the packet's mini-map by more than threshold percent.
//
// Parameters:
//   - currentNode:    name of the node checking conditions
//   - currentLoad:    real measured load % at this router  (0–100)
//   - currentLatency: real measured latency ms at this link
//   - threshold:      divergence percentage to trigger reroute (e.g., 30.0)
func (p *SmartPacket) ShouldReroute(currentNode string, currentLoad, currentLatency, threshold float64) bool {
	for _, link := range p.MiniMap {
		if link.From != currentNode {
			continue
		}

		// Check load divergence (with absolute floor to avoid spurious triggers)
		if divergesWithFloor(currentLoad, link.LoadPct, threshold, DefaultMinDivergenceLoad) {
			log.Info("load divergence detected",
				"node", currentNode,
				"map_load", link.LoadPct,
				"actual_load", currentLoad,
			)
			return true
		}

		// Check latency divergence (with absolute floor)
		if divergesWithFloor(currentLatency, link.LatencyMs, threshold, DefaultMinDivergenceLatencyMs) {
			log.Info("latency divergence detected",
				"node", currentNode,
				"map_latency_ms", link.LatencyMs,
				"actual_latency_ms", currentLatency,
			)
			return true
		}
	}
	return false
}

// diverges returns true if actual diverges from expected by more than
// thresholdPct AND the absolute difference exceeds minAbsolute.
// The dual threshold prevents spurious reroutes when both values are
// small (e.g., 0.8% vs 2.9% load — 72% relative but only 2.1 pp absolute).
// When expected is zero, we use DefaultSignificantLatencyMs as an absolute
// threshold to avoid division by zero.
func diverges(actual, expected, thresholdPct float64) bool {
	return divergesWithFloor(actual, expected, thresholdPct, 0)
}

// divergesWithFloor is like diverges but with an explicit minimum absolute
// difference floor. If the absolute difference is below minAbsolute, no
// divergence is reported regardless of percentage.
func divergesWithFloor(actual, expected, thresholdPct, minAbsolute float64) bool {
	absDiff := math.Abs(actual - expected)
	if minAbsolute > 0 && absDiff < minAbsolute {
		return false
	}
	if expected > 0 {
		pct := (absDiff / expected) * 100
		return pct > thresholdPct
	}
	// Map says 0 — use absolute significance threshold.
	return actual > DefaultSignificantLatencyMs
}

// Reroute recalculates the planned path from currentNode using freshLinks.
// Fresh link data is merged into the packet's mini-map (fresher wins),
// then Dijkstra is re-run from the current position.
func (p *SmartPacket) Reroute(currentNode string, freshLinks []Link) {
	// Merge fresh data into the existing mini-map.
	// Build a From→To→Link index so fresh data overwrites stale entries.
	mergedMap := make(map[string]map[string]Link, len(p.MiniMap))
	for _, link := range p.MiniMap {
		if mergedMap[link.From] == nil {
			mergedMap[link.From] = make(map[string]Link)
		}
		mergedMap[link.From][link.To] = link
	}
	for _, link := range freshLinks {
		if mergedMap[link.From] == nil {
			mergedMap[link.From] = make(map[string]Link)
		}
		mergedMap[link.From][link.To] = link
	}

	// Flatten back to a slice.
	updatedMap := make([]Link, 0, len(p.MiniMap)+len(freshLinks))
	for _, dests := range mergedMap {
		for _, link := range dests {
			updatedMap = append(updatedMap, link)
		}
	}
	p.MiniMap = updatedMap

	// Recalculate path from current position.
	graph := BuildGraph(updatedMap, p.Intent)
	newPath := Dijkstra(graph, currentNode, p.Destination)

	if len(newPath) > 0 {
		p.PlannedPath = newPath
		p.HopIndex = 0
		p.Rerouted = true
		log.Info("path recalculated",
			"from", currentNode,
			"new_path", fmt.Sprintf("%v", newPath),
		)
	} else {
		// No path found — mark as degraded so the router can ForceForward.
		p.Degraded = true
		log.Warn("no path found after reroute",
			"from", currentNode,
			"destination", p.Destination,
		)
	}
}

// ──────────────────────────────────────────────────────────────
// Loop detection and TTL
// ──────────────────────────────────────────────────────────────

// DetectLoop returns true if nextNode has already been visited.
func (p *SmartPacket) DetectLoop(nextNode string) bool {
	if p.VisitedNodes == nil {
		return false
	}
	return p.VisitedNodes[nextNode]
}

// IsExpired returns true if the packet has exhausted its hop budget (TTL).
func (p *SmartPacket) IsExpired() bool {
	return p.HopCount >= p.MaxHops
}

// ──────────────────────────────────────────────────────────────
// Force-forward — last-resort routing when loops are detected.
// ──────────────────────────────────────────────────────────────

// ForceForward selects the best direct neighbor to forward to when
// normal routing fails (e.g., loop detected). Selection uses the same
// intent-aware weight function as Dijkstra so that the fallback path
// still respects the packet's QoS requirements.
//
// Priority order:
//  1. Direct link to destination (if available)
//  2. Least-weighted unvisited neighbor
//  3. Least-weighted neighbor overall (all visited — emergency)
func (p *SmartPacket) ForceForward(currentNode string) string {
	// 1. Direct link to destination
	for _, link := range p.MiniMap {
		if link.From == currentNode && link.To == p.Destination {
			return p.Destination
		}
	}

	// 2. Least-weighted unvisited neighbor (intent-aware)
	bestNode := ""
	bestWeight := math.Inf(1)
	for _, link := range p.MiniMap {
		if link.From == currentNode && !p.VisitedNodes[link.To] {
			weight := calculateWeight(link, p.Intent)
			if weight < bestWeight {
				bestWeight = weight
				bestNode = link.To
			}
		}
	}
	if bestNode != "" {
		return bestNode
	}

	// 3. Emergency — all neighbors visited, pick lowest weight anyway.
	for _, link := range p.MiniMap {
		if link.From == currentNode {
			weight := calculateWeight(link, p.Intent)
			if weight < bestWeight {
				bestWeight = weight
				bestNode = link.To
			}
		}
	}
	return bestNode
}

// ──────────────────────────────────────────────────────────────
// Introspection helpers
// ──────────────────────────────────────────────────────────────

// RemainingPath returns the portion of the planned path not yet traversed.
// Returns nil if the packet has reached or passed its destination.
func (p *SmartPacket) RemainingPath() []string {
	if p.HopIndex+1 < len(p.PlannedPath) {
		return p.PlannedPath[p.HopIndex:]
	}
	return nil
}

// Summary returns a one-line human-readable summary of the packet state.
func (p *SmartPacket) Summary() string {
	status := "normal"
	if p.Degraded {
		status = "DEGRADED"
	}
	if p.Rerouted {
		status += " (rerouted)"
	}

	return fmt.Sprintf("SPP[v%d src=%s dst=%s hops=%d/%d path=%v status=%s payload=%dB]",
		p.Version, p.SourceNode, p.Destination, p.HopCount, p.MaxHops,
		p.PlannedPath, status, len(p.Payload))
}

// ──────────────────────────────────────────────────────────────
// LightMode — packets without embedded topology.
// ──────────────────────────────────────────────────────────────

// NewLightPacket creates a packet that carries only path + intent,
// no MiniMap. Routers use their own gossip-derived topology for
// rerouting decisions, keeping packet size minimal (~100 bytes overhead).
func NewLightPacket(destination string, intent IntentHeader, path []string, payload []byte) *SmartPacket {
	return &SmartPacket{
		Version:       ProtocolVersion,
		PacketType:    PacketTypeData,
		Destination:   destination,
		Intent:        intent,
		MiniMap:       nil,
		LightMode:     true,
		CurrentNode:   "",
		PlannedPath:   path,
		HopIndex:      0,
		MaxHops:       DefaultMaxHops,
		HopCount:      0,
		Degraded:      false,
		Rerouted:      false,
		VisitedNodes:  make(map[string]bool),
		CongestionLog: []HopRecord{},
		Payload:       payload,
	}
}

// ShouldRerouteFromTopology checks whether live conditions at currentNode
// diverge from the router's local topology view. Used for LightMode packets
// that carry no MiniMap.
func (p *SmartPacket) ShouldRerouteFromTopology(currentNode string, currentLoad, currentLatency, threshold float64, localMap []Link) bool {
	for _, link := range localMap {
		if link.From != currentNode {
			continue
		}
		if divergesWithFloor(currentLoad, link.LoadPct, threshold, DefaultMinDivergenceLoad) {
			log.Info("load divergence detected (light)",
				"node", currentNode,
				"map_load", link.LoadPct,
				"actual_load", currentLoad,
			)
			return true
		}
		if divergesWithFloor(currentLatency, link.LatencyMs, threshold, DefaultMinDivergenceLatencyMs) {
			log.Info("latency divergence detected (light)",
				"node", currentNode,
				"map_latency_ms", link.LatencyMs,
				"actual_latency_ms", currentLatency,
			)
			return true
		}
	}
	return false
}

// RerouteFromTopology recalculates the path using the router's local
// gossip topology instead of the packet's MiniMap. The packet's MiniMap
// remains empty — only the PlannedPath is updated.
func (p *SmartPacket) RerouteFromTopology(currentNode string, topology []Link) {
	graph := BuildGraph(topology, p.Intent)
	newPath := Dijkstra(graph, currentNode, p.Destination)

	if len(newPath) > 0 {
		p.PlannedPath = newPath
		p.HopIndex = 0
		p.Rerouted = true
		log.Info("path recalculated (light)",
			"from", currentNode,
			"new_path", fmt.Sprintf("%v", newPath),
		)
	} else {
		p.Degraded = true
		log.Warn("no path found after reroute (light)",
			"from", currentNode,
			"destination", p.Destination,
		)
	}
}

// ForceForwardFromTopology selects the best next hop using the router's
// local topology instead of the packet's MiniMap. Used for LightMode
// packets during loop recovery.
func (p *SmartPacket) ForceForwardFromTopology(currentNode string, topology []Link) string {
	// 1. Direct link to destination
	for _, link := range topology {
		if link.From == currentNode && link.To == p.Destination {
			return p.Destination
		}
	}

	// 2. Least-weighted unvisited neighbor
	bestNode := ""
	bestWeight := math.Inf(1)
	for _, link := range topology {
		if link.From == currentNode && !p.VisitedNodes[link.To] {
			weight := calculateWeight(link, p.Intent)
			if weight < bestWeight {
				bestWeight = weight
				bestNode = link.To
			}
		}
	}
	if bestNode != "" {
		return bestNode
	}

	// 3. Emergency — all visited
	for _, link := range topology {
		if link.From == currentNode {
			weight := calculateWeight(link, p.Intent)
			if weight < bestWeight {
				bestWeight = weight
				bestNode = link.To
			}
		}
	}
	return bestNode
}
