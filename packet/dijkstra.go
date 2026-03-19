package packet

import (
	"math"
)

// ──────────────────────────────────────────────────────────────
// Weight configuration — the "brain" of intent-aware routing.
//
// These multipliers control how much latency, load, and loss
// influence the Dijkstra edge weights for each intent level.
// They are the core tuning knobs of the SPP routing engine.
// ──────────────────────────────────────────────────────────────

// WeightConfig holds the multipliers used by calculateWeight to convert
// raw link metrics (latency, load, loss) into a single composite edge
// weight for Dijkstra. Different intent levels apply different multipliers,
// making the same physical link appear "heavier" or "lighter" depending
// on what the packet needs.
type WeightConfig struct {
	// Latency multipliers per intent level (index 0=relaxed … 3=critical).
	// Higher values make Dijkstra penalize high-latency links more.
	LatencyMultiplier [4]float64

	// Load multipliers per intent level (index 0=relaxed … 3=critical).
	// Higher values make Dijkstra penalize high-load links more.
	LoadMultiplier [4]float64

	// LossMultiplier is applied to LossPct for reliability-critical packets.
	// For lower reliability levels, a base loss penalty is always applied
	// to avoid routing through completely broken links.
	LossMultiplier float64

	// BaseLossMultiplier is the minimum loss penalty applied when
	// reliability is below the threshold. Set to 0.0 to let
	// low-reliability packets fully tolerate loss (enabling intent
	// differentiation). Default: 0.5.
	BaseLossMultiplier float64

	// ReliabilityThreshold is the minimum Reliability level (inclusive)
	// at which the full LossMultiplier is applied. Default: 2 (guaranteed).
	ReliabilityThreshold uint8

	// StalePenaltyMultiplier is applied to edge weights when the
	// originating node is flagged as having stale data. Default: 1.5 (50% penalty).
	StalePenaltyMultiplier float64

	// ReliabilityLoadBoost multiplies the load component when
	// intent.Reliability >= ReliabilityThreshold, penalizing
	// overloaded links that are more likely to drop packets.
	// Default: 1.5.
	ReliabilityLoadBoost float64

	// HighPriorityLatencyBoost multiplies the latency component
	// when intent.Priority >= 2 (high/critical), giving a small
	// additional preference for lower-latency links. Default: 1.2.
	HighPriorityLatencyBoost float64

	// HopPenalty is a small constant added to every edge weight,
	// biasing Dijkstra toward paths with fewer hops. This reflects
	// the real-world per-hop processing overhead (~0.2ms per router).
	// Without this, SPP may choose a 4-hop path over a 2-hop path
	// when metrics are nearly equal, paying unnecessary overhead.
	// Default: 0.1.
	HopPenalty float64
}

// DefaultWeightConfig returns the standard SPP weight configuration.
//
// Weight formula per intent level:
//
//	critical (3): weight = latency×2.0 + load×0.5 + hopPenalty
//	low      (2): weight = latency×1.5 + load×1.0 + hopPenalty
//	normal   (1): weight = latency×1.0 + load×0.5 + hopPenalty
//	relaxed  (0): weight = load×0.3 + hopPenalty
//
// If reliability ≥ 2: weight += loss × 10.0
// If reliability < 2: weight += loss × 0.5  (tolerant — enables intent differentiation)
//
// LoadMultiplier[3] is intentionally low (0.5) so that latency-critical
// packets focus on latency, not load noise. Small Docker/container load
// fluctuations (1-5%) were causing SPP to pick longer paths that added
// real per-hop overhead (~0.24ms/hop) to avoid negligible load differences.
func DefaultWeightConfig() WeightConfig {
	return WeightConfig{
		LatencyMultiplier:      [4]float64{0.0, 1.0, 1.5, 2.0},
		LoadMultiplier:         [4]float64{0.3, 0.5, 1.0, 0.5},
		LossMultiplier:         10.0,
		BaseLossMultiplier:     0.5,
		ReliabilityThreshold:   2,
		StalePenaltyMultiplier:   1.5,
		ReliabilityLoadBoost:     1.5,
		HighPriorityLatencyBoost: 1.2,
		HopPenalty:               0.1,
	}
}

// defaultWeightCfg is the package-level default used when callers
// don't provide an explicit WeightConfig.
var defaultWeightCfg = DefaultWeightConfig()

// SetDefaultWeightConfig replaces the package-level weight configuration.
// Call this at program startup to tune routing globally.
func SetDefaultWeightConfig(cfg WeightConfig) {
	defaultWeightCfg = cfg
}

// ──────────────────────────────────────────────────────────────
// Graph types
// ──────────────────────────────────────────────────────────────

// Edge represents a weighted directed edge in the routing graph.
type Edge struct {
	To     string  // destination node name
	Weight float64 // composite cost (lower = better)
}

// ──────────────────────────────────────────────────────────────
// Graph construction
// ──────────────────────────────────────────────────────────────

// BuildGraph converts a slice of Links into an adjacency list
// suitable for Dijkstra. Edge weights are computed using the
// package-level default WeightConfig.
func BuildGraph(links []Link, intent IntentHeader) map[string][]Edge {
	return BuildGraphWithConfig(links, intent, defaultWeightCfg)
}

// BuildGraphWithConfig converts Links into an adjacency list using
// the provided WeightConfig. Use this when you need non-default
// weight tuning (e.g., per-deployment or per-experiment).
func BuildGraphWithConfig(links []Link, intent IntentHeader, cfg WeightConfig) map[string][]Edge {
	graph := make(map[string][]Edge, len(links))

	for _, link := range links {
		weight := calculateWeightWithConfig(link, intent, cfg)

		graph[link.From] = append(graph[link.From], Edge{
			To:     link.To,
			Weight: weight,
		})

		// Ensure destination nodes exist in graph even if they
		// have no outgoing edges (required for Dijkstra termination).
		if _, exists := graph[link.To]; !exists {
			graph[link.To] = []Edge{}
		}
	}
	return graph
}

// BuildGraphForDest converts Links into an adjacency list, zeroing out
// LossPct for links whose To field matches destination. The receiver
// doesn't run probes, so its inbound links always show 100% loss in
// gossip — this prevents that unmeasurable loss from polluting weights.
func BuildGraphForDest(links []Link, intent IntentHeader, destination string) map[string][]Edge {
	return BuildGraphForDestWithConfig(links, intent, destination, defaultWeightCfg)
}

// BuildGraphForDestWithConfig is the configurable variant of BuildGraphForDest.
func BuildGraphForDestWithConfig(links []Link, intent IntentHeader, destination string, cfg WeightConfig) map[string][]Edge {
	graph := make(map[string][]Edge, len(links))

	for _, link := range links {
		l := link
		if l.To == destination {
			l.LossPct = 0
		}
		weight := calculateWeightWithConfig(l, intent, cfg)

		graph[link.From] = append(graph[link.From], Edge{
			To:     link.To,
			Weight: weight,
		})

		if _, exists := graph[link.To]; !exists {
			graph[link.To] = []Edge{}
		}
	}
	return graph
}

// BuildGraphWithPenalties returns a graph where links originating
// from stale nodes receive an additional weight penalty. This makes
// Dijkstra prefer paths through nodes with fresher data.
//
// staleNodes: set of node names whose outgoing links should be penalized.
func BuildGraphWithPenalties(links []Link, intent IntentHeader, staleNodes map[string]bool) map[string][]Edge {
	return BuildGraphWithPenaltiesConfig(links, intent, staleNodes, defaultWeightCfg)
}

// BuildGraphWithPenaltiesConfig is the configurable variant of
// BuildGraphWithPenalties, accepting an explicit WeightConfig.
func BuildGraphWithPenaltiesConfig(links []Link, intent IntentHeader, staleNodes map[string]bool, cfg WeightConfig) map[string][]Edge {
	graph := make(map[string][]Edge, len(links))

	for _, link := range links {
		weight := calculateWeightWithConfig(link, intent, cfg)

		// Penalize links from stale nodes.
		if staleNodes[link.From] {
			weight *= cfg.StalePenaltyMultiplier
		}

		graph[link.From] = append(graph[link.From], Edge{
			To:     link.To,
			Weight: weight,
		})

		if _, exists := graph[link.To]; !exists {
			graph[link.To] = []Edge{}
		}
	}
	return graph
}

// ──────────────────────────────────────────────────────────────
// Weight calculation — the core intelligence.
//
// The same link gets DIFFERENT weights depending on the packet's
// intent. This is the fundamental mechanism that makes SPP
// routing intent-aware.
// ──────────────────────────────────────────────────────────────

// calculateWeight computes the composite edge weight for a link
// using the package-level default WeightConfig.
func calculateWeight(link Link, intent IntentHeader) float64 {
	return calculateWeightWithConfig(link, intent, defaultWeightCfg)
}

// calculateWeightWithConfig computes the composite edge weight using
// an explicit WeightConfig.
//
// Weight formula:
//
//	weight = latency × latencyMultiplier[intent.Latency]
//	       + load    × loadMultiplier[intent.Latency]
//	       + (if reliability ≥ threshold) loss × lossMultiplier
func calculateWeightWithConfig(link Link, intent IntentHeader, cfg WeightConfig) float64 {
	// Clamp latency level to valid index range.
	latLevel := intent.Latency
	if latLevel > MaxLatency {
		latLevel = MaxLatency
	}

	latencyTerm := link.LatencyMs * cfg.LatencyMultiplier[latLevel]
	loadTerm := link.LoadPct * cfg.LoadMultiplier[latLevel]

	// High-priority packets (Priority >= 2) get additional latency sensitivity.
	if intent.Priority >= 2 && cfg.HighPriorityLatencyBoost > 0 {
		latencyTerm *= cfg.HighPriorityLatencyBoost
	}

	// Reliability-critical packets penalize overloaded links more heavily,
	// since high load correlates with packet drops.
	if intent.Reliability >= cfg.ReliabilityThreshold && cfg.ReliabilityLoadBoost > 0 {
		loadTerm *= cfg.ReliabilityLoadBoost
	}

	weight := latencyTerm + loadTerm + cfg.HopPenalty

	// Always apply a base loss penalty so Dijkstra avoids broken links
	// (e.g., 100% loss). Higher reliability levels get a stronger penalty.
	if intent.Reliability >= cfg.ReliabilityThreshold {
		weight += link.LossPct * cfg.LossMultiplier
	} else if cfg.BaseLossMultiplier > 0 {
		weight += link.LossPct * cfg.BaseLossMultiplier
	}

	return weight
}

// ──────────────────────────────────────────────────────────────
// Dijkstra implementation.
//
// Complexity: O(V²) with linear scan for minimum extraction.
// This is intentional — SPP mini-maps are small (typically 5–50
// nodes). A binary-heap version would add code complexity with
// negligible benefit at this scale. If SPP is deployed on networks
// with >500 nodes in the mini-map, switch to container/heap.
// ──────────────────────────────────────────────────────────────

// Dijkstra finds the shortest weighted path from start to end.
// Returns the path as a slice of node names, or nil if no path exists.
func Dijkstra(graph map[string][]Edge, start, end string) []string {
	return dijkstraCore(graph, start, end, nil)
}

// DijkstraExcluding finds the shortest path while avoiding the
// specified nodes. Used for rerouting around detected loops.
// The start and end nodes are never excluded regardless of the set.
func DijkstraExcluding(graph map[string][]Edge, start, end string, exclude map[string]bool) []string {
	return dijkstraCore(graph, start, end, exclude)
}

// ──────────────────────────────────────────────────────────────
// OSPF-style graph — single metric, no intent awareness.
//
// Real OSPF uses cost = reference_bandwidth / interface_bandwidth.
// Since SPP measures latency rather than bandwidth, we use latency
// as the sole metric. This matches OSPF's fundamental design:
// one metric per link, same cost for all traffic classes.
// ──────────────────────────────────────────────────────────────

// BuildOSPFGraph converts links into an adjacency list using OSPF-style
// single-metric weights. Unlike BuildGraph, this ignores:
//   - Intent headers (all packets get the same path)
//   - Load metrics (OSPF doesn't consider load)
//   - Loss metrics (OSPF doesn't consider packet loss)
//
// Only latency is used, with a minimum cost of 1.0 to avoid zero-weight edges.
func BuildOSPFGraph(links []Link) map[string][]Edge {
	graph := make(map[string][]Edge, len(links))
	for _, link := range links {
		weight := link.LatencyMs
		if weight < 1.0 {
			weight = 1.0 // OSPF minimum cost
		}
		graph[link.From] = append(graph[link.From], Edge{
			To:     link.To,
			Weight: weight,
		})
		if _, exists := graph[link.To]; !exists {
			graph[link.To] = []Edge{}
		}
	}
	return graph
}

// dijkstraCore is the shared Dijkstra implementation.
func dijkstraCore(graph map[string][]Edge, start, end string, exclude map[string]bool) []string {
	dist := make(map[string]float64, len(graph))
	prev := make(map[string]string, len(graph))
	unvisited := make(map[string]bool, len(graph))

	// Initialize all nodes (excluding blacklisted, but never start/end).
	for node := range graph {
		if exclude != nil && exclude[node] && node != start && node != end {
			continue
		}
		dist[node] = math.Inf(1)
		unvisited[node] = true
	}
	dist[start] = 0

	for len(unvisited) > 0 {
		// Extract the unvisited node with the smallest distance.
		// O(V) scan — acceptable for small mini-maps.
		current := ""
		for node := range unvisited {
			if current == "" || dist[node] < dist[current] {
				current = node
			}
		}

		// Early termination: reached destination or no reachable nodes.
		if current == end {
			break
		}
		if math.IsInf(dist[current], 1) {
			break
		}

		// Relax all outgoing edges.
		for _, edge := range graph[current] {
			if exclude != nil && exclude[edge.To] && edge.To != end {
				continue
			}
			newDist := dist[current] + edge.Weight
			if newDist < dist[edge.To] {
				dist[edge.To] = newDist
				prev[edge.To] = current
			}
		}

		delete(unvisited, current)
	}

	// Reconstruct path by walking backward from end to start.
	// Build in reverse order then flip — O(n) instead of O(n²) prepend.
	path := make([]string, 0, 8)
	current := end
	for current != "" {
		path = append(path, current)
		current = prev[current]
	}

	// Reverse in-place.
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}

	if len(path) > 0 && path[0] == start {
		return path
	}
	return nil
}