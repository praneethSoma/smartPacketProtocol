package gossip

import (
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"smartpacket/packet"
)

// ──────────────────────────────────────────────────────────────
// Staleness penalty defaults.
// ──────────────────────────────────────────────────────────────

const (
	// DefaultAgingLatencyPenalty controls how much latency increases
	// as a link ages past half its max lifetime. A value of 0.5 means
	// "add up to 50% extra latency for the oldest surviving link".
	DefaultAgingLatencyPenalty = 0.5

	// DefaultAgingLoadPenalty is the equivalent for load.
	DefaultAgingLoadPenalty = 0.3

	// DefaultWarnLatencyPenalty is applied to links in the warn zone
	// (between WarnStaleness and MaxStaleness).
	DefaultWarnLatencyPenalty = 1.5

	// DefaultWarnLoadPenalty is applied to link load in the warn zone.
	DefaultWarnLoadPenalty = 1.3
)

// StalenessConfig holds all penalty factors for aging links.
type StalenessConfig struct {
	AgingLatencyPenalty float64 // Factor for progressive latency penalty [0, 1]
	AgingLoadPenalty    float64 // Factor for progressive load penalty [0, 1]
	WarnLatencyPenalty  float64 // Multiplier for warn-zone latency (e.g., 1.5 = +50%)
	WarnLoadPenalty     float64 // Multiplier for warn-zone load (e.g., 1.3 = +30%)
}

// DefaultStalenessConfig returns the standard penalty configuration.
func DefaultStalenessConfig() StalenessConfig {
	return StalenessConfig{
		AgingLatencyPenalty: DefaultAgingLatencyPenalty,
		AgingLoadPenalty:    DefaultAgingLoadPenalty,
		WarnLatencyPenalty:  DefaultWarnLatencyPenalty,
		WarnLoadPenalty:     DefaultWarnLoadPenalty,
	}
}

// ──────────────────────────────────────────────────────────────
// LinkKey + LinkState — the atoms of topology state.
// ──────────────────────────────────────────────────────────────

// LinkKey uniquely identifies a directional link between two nodes.
type LinkKey struct {
	From string
	To   string
}

// LinkState holds the measured conditions of a single link.
type LinkState struct {
	From      string
	To        string
	LatencyMs float64
	LoadPct   float64
	LossPct   float64
	Timestamp time.Time // When this measurement was taken
	Origin    string    // Node that originally measured this link
	Sequence  uint64    // Monotonic counter to detect ordering
}

// ──────────────────────────────────────────────────────────────
// TopologyState — thread-safe topology knowledge base.
// ──────────────────────────────────────────────────────────────

// TopologyState is a thread-safe store of all known link states
// in the network. It supports local updates, remote merges,
// delta tracking for efficient gossip, and staleness management.
type TopologyState struct {
	mu             sync.RWMutex
	links          map[LinkKey]LinkState
	seqGen         uint64
	lastBroadcast  map[LinkKey]LinkState
	hasChanges     bool
	stalenessConfig StalenessConfig
}

// NewTopologyState creates an empty topology state with default config.
func NewTopologyState() *TopologyState {
	return NewTopologyStateWithConfig(DefaultStalenessConfig())
}

// NewTopologyStateWithConfig creates an empty topology state with
// the provided staleness penalty configuration.
func NewTopologyStateWithConfig(cfg StalenessConfig) *TopologyState {
	return &TopologyState{
		links:           make(map[LinkKey]LinkState),
		lastBroadcast:   make(map[LinkKey]LinkState),
		hasChanges:      true, // First broadcast sends everything.
		stalenessConfig: cfg,
	}
}

// UpdateLocal stores a locally measured link state with a new sequence number.
func (ts *TopologyState) UpdateLocal(from, to string, latencyMs, loadPct, lossPct float64) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	key := LinkKey{From: from, To: to}

	// Only flag changes if the new values differ significantly from
	// the previous ones (avoids float jitter keeping gossip in fast mode).
	prev, existed := ts.links[key]
	significant := !existed ||
		significantChange(latencyMs, prev.LatencyMs, 0.5) ||
		significantChange(loadPct, prev.LoadPct, 1.0) ||
		significantChange(lossPct, prev.LossPct, 1.0)

	ts.seqGen++
	ts.links[key] = LinkState{
		From:      from,
		To:        to,
		LatencyMs: latencyMs,
		LoadPct:   loadPct,
		LossPct:   lossPct,
		Timestamp: time.Now(),
		Origin:    from,
		Sequence:  ts.seqGen,
	}
	if significant {
		ts.hasChanges = true
	}
}

// MergeRemote merges link states received from a neighbor.
// Only updates if the incoming state has a newer sequence number
// or a newer timestamp from the same source. Returns the count
// of states that were accepted.
func (ts *TopologyState) MergeRemote(states []LinkState) int {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	updated := 0
	for _, incoming := range states {
		key := LinkKey{From: incoming.From, To: incoming.To}
		existing, exists := ts.links[key]

		// Accept if: new link, or same origin with newer sequence, or different origin with newer timestamp.
		accept := false
		if !exists {
			accept = true
		} else if incoming.Origin == existing.Origin {
			accept = incoming.Sequence > existing.Sequence
		} else {
			accept = incoming.Timestamp.After(existing.Timestamp)
		}
		if accept {
			ts.links[key] = incoming
			updated++
			ts.hasChanges = true
		}
	}
	return updated
}

// GetAllStates returns a copy of all link states (for full-sync gossip).
func (ts *TopologyState) GetAllStates() []LinkState {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	result := make([]LinkState, 0, len(ts.links))
	for _, state := range ts.links {
		result = append(result, state)
	}
	return result
}

// GetFreshLinks converts non-stale link states to packet.Link format.
// Links older than maxAge are excluded. Links older than maxAge/2
// receive progressive penalty to express reduced confidence.
func (ts *TopologyState) GetFreshLinks(maxAge time.Duration) []packet.Link {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	now := time.Now()
	cfg := ts.stalenessConfig
	result := make([]packet.Link, 0, len(ts.links))

	for _, state := range ts.links {
		age := now.Sub(state.Timestamp)
		if age > maxAge {
			continue // Prune stale.
		}

		link := packet.Link{
			From:      state.From,
			To:        state.To,
			LatencyMs: state.LatencyMs,
			LoadPct:   state.LoadPct,
			LossPct:   state.LossPct,
		}

		// Progressive penalty for aging links.
		if age > maxAge/2 {
			ageFactor := float64(age) / float64(maxAge)
			link.LatencyMs *= (1.0 + ageFactor*cfg.AgingLatencyPenalty)
			link.LoadPct = min(100, link.LoadPct*(1.0+ageFactor*cfg.AgingLoadPenalty))
		}

		result = append(result, link)
	}
	return result
}

// GetLinksWithStaleness returns all links with staleness-based penalties.
// Links between warnAge and maxAge receive a configurable penalty.
// Links older than maxAge are pruned entirely.
func (ts *TopologyState) GetLinksWithStaleness(warnAge, maxAge time.Duration) []packet.Link {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	now := time.Now()
	cfg := ts.stalenessConfig
	result := make([]packet.Link, 0, len(ts.links))

	for _, state := range ts.links {
		age := now.Sub(state.Timestamp)
		if age > maxAge {
			continue
		}

		link := packet.Link{
			From:      state.From,
			To:        state.To,
			LatencyMs: state.LatencyMs,
			LoadPct:   state.LoadPct,
			LossPct:   state.LossPct,
		}

		// Apply penalty in the warn zone.
		if age > warnAge {
			link.LatencyMs *= cfg.WarnLatencyPenalty
			link.LoadPct = min(100, link.LoadPct*cfg.WarnLoadPenalty)
		}

		result = append(result, link)
	}
	return result
}

// CheckDivergence checks if any link from currentNode has conditions that
// diverge from the given actual values by more than threshold percent.
// Returns true if rerouting may be needed. This is a read-locked O(neighbors)
// check with zero allocations — use it as a fast pre-filter before GetFreshLinks.
func (ts *TopologyState) CheckDivergence(currentNode string, actualLoad, actualLatency, threshold, minLoadDiff, minLatencyDiff float64) bool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	for key, state := range ts.links {
		if key.From != currentNode {
			continue
		}

		// Check load divergence with absolute floor.
		loadDiff := math.Abs(actualLoad - state.LoadPct)
		if loadDiff >= minLoadDiff {
			if state.LoadPct > 0 {
				if (loadDiff/state.LoadPct)*100 > threshold {
					return true
				}
			} else if actualLoad > 10.0 { // DefaultSignificantLatencyMs equivalent
				return true
			}
		}

		// Check latency divergence with absolute floor.
		latDiff := math.Abs(actualLatency - state.LatencyMs)
		if latDiff >= minLatencyDiff {
			if state.LatencyMs > 0 {
				if (latDiff/state.LatencyMs)*100 > threshold {
					return true
				}
			} else if actualLatency > 10.0 {
				return true
			}
		}
	}
	return false
}

// Size returns the number of known links.
func (ts *TopologyState) Size() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return len(ts.links)
}

// Dump writes all link states to w for debugging.
// Pass os.Stdout for console output or a *bytes.Buffer for testing.
func (ts *TopologyState) Dump(prefix string, w io.Writer) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	for key, state := range ts.links {
		age := time.Since(state.Timestamp)
		fmt.Fprintf(w, "[%s] %s → %s  lat=%.1fms load=%.0f%% loss=%.0f%% age=%v seq=%d\n",
			prefix, key.From, key.To, state.LatencyMs, state.LoadPct, state.LossPct,
			age.Round(time.Millisecond), state.Sequence)
	}
}

// ──────────────────────────────────────────────────────────────
// Delta-based gossip — bandwidth-efficient state sharing.
// ──────────────────────────────────────────────────────────────

// GetChangedStates returns only link states that changed since the
// last MarkBroadcast. This is the core of delta-based gossip —
// bandwidth drops from O(E) to O(changed).
func (ts *TopologyState) GetChangedStates() []LinkState {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	var changed []LinkState
	for key, current := range ts.links {
		prev, existed := ts.lastBroadcast[key]
		if !existed ||
			current.Sequence != prev.Sequence ||
			significantChange(current.LatencyMs, prev.LatencyMs, 0.5) ||
			significantChange(current.LoadPct, prev.LoadPct, 1.0) ||
			significantChange(current.LossPct, prev.LossPct, 1.0) {
			changed = append(changed, current)
		}
	}
	return changed
}

// MarkBroadcast snapshots the current topology as "last broadcast".
// Call this after a successful gossip round to reset the delta.
func (ts *TopologyState) MarkBroadcast() {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	ts.lastBroadcast = make(map[LinkKey]LinkState, len(ts.links))
	for key, state := range ts.links {
		ts.lastBroadcast[key] = state
	}
	ts.hasChanges = false
}

// HasChanges returns true if any link state changed since the
// last MarkBroadcast. O(1) check used by adaptive interval logic.
func (ts *TopologyState) HasChanges() bool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.hasChanges
}

// significantChange returns true if the absolute difference between
// a and b exceeds the given threshold. Used to avoid treating tiny
// float jitter as meaningful topology changes.
func significantChange(a, b, threshold float64) bool {
	return math.Abs(a-b) > threshold
}
