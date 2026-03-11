package gossip

import (
	"time"
)

// PruneStale removes link states older than maxAge from the topology.
// Returns the number of pruned entries.
func (ts *TopologyState) PruneStale(maxAge time.Duration) int {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	now := time.Now()
	pruned := 0

	for key, state := range ts.links {
		if now.Sub(state.Timestamp) > maxAge {
			delete(ts.links, key)
			pruned++
		}
	}
	return pruned
}

// IsStale returns true if the specified link is older than maxAge.
// Unknown links are considered stale.
func (ts *TopologyState) IsStale(from, to string, maxAge time.Duration) bool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	key := LinkKey{From: from, To: to}
	state, exists := ts.links[key]
	if !exists {
		return true // Unknown links are stale.
	}
	return time.Since(state.Timestamp) > maxAge
}

// GetAge returns how old a specific link state is.
// Returns math.MaxInt64 duration for unknown links.
func (ts *TopologyState) GetAge(from, to string) time.Duration {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	key := LinkKey{From: from, To: to}
	state, exists := ts.links[key]
	if !exists {
		return time.Duration(1<<63 - 1) // Max duration for unknown.
	}
	return time.Since(state.Timestamp)
}

// CountFresh returns how many links are fresher than maxAge.
func (ts *TopologyState) CountFresh(maxAge time.Duration) int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	now := time.Now()
	count := 0
	for _, state := range ts.links {
		if now.Sub(state.Timestamp) <= maxAge {
			count++
		}
	}
	return count
}

// CountStale returns how many links are older than maxAge.
func (ts *TopologyState) CountStale(maxAge time.Duration) int {
	return ts.Size() - ts.CountFresh(maxAge)
}
