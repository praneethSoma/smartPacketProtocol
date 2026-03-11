package gossip

import (
	"bytes"
	"testing"
	"time"

	"smartpacket/packet"
)

func TestTopologyUpdateAndRetrieve(t *testing.T) {
	ts := NewTopologyState()

	ts.UpdateLocal("A", "B", 5.0, 20.0, 0.0)
	ts.UpdateLocal("B", "C", 3.0, 10.0, 1.0)

	if ts.Size() != 2 {
		t.Fatalf("Expected 2 links, got %d", ts.Size())
	}

	links := ts.GetFreshLinks(1 * time.Second)
	if len(links) != 2 {
		t.Fatalf("Expected 2 fresh links, got %d", len(links))
	}
	t.Logf("Topology has %d links ✓", ts.Size())
}

func TestMergeRemoteNewerWins(t *testing.T) {
	ts := NewTopologyState()

	ts.UpdateLocal("A", "B", 5.0, 20.0, 0.0)

	remote := []LinkState{
		{From: "A", To: "B", LatencyMs: 10.0, LoadPct: 50.0, LossPct: 2.0,
			Timestamp: time.Now(), Sequence: 100},
	}

	updated := ts.MergeRemote(remote)
	if updated != 1 {
		t.Fatalf("Expected 1 update, got %d", updated)
	}

	links := ts.GetFreshLinks(1 * time.Second)
	for _, l := range links {
		if l.From == "A" && l.To == "B" {
			if l.LoadPct != 50.0 {
				t.Fatalf("Remote (newer seq) should win: got load=%.0f%%, want 50%%", l.LoadPct)
			}
		}
	}
	t.Log("Remote with higher sequence wins ✓")
}

func TestMergeRemoteOlderLoses(t *testing.T) {
	ts := NewTopologyState()

	remote1 := []LinkState{
		{From: "A", To: "B", LatencyMs: 10.0, LoadPct: 50.0, Sequence: 100, Timestamp: time.Now()},
	}
	ts.MergeRemote(remote1)

	remote2 := []LinkState{
		{From: "A", To: "B", LatencyMs: 1.0, LoadPct: 5.0, Sequence: 50, Timestamp: time.Now().Add(-1 * time.Second)},
	}
	updated := ts.MergeRemote(remote2)
	if updated != 0 {
		t.Fatal("Older sequence should not overwrite newer")
	}
	t.Log("Old sequence correctly rejected ✓")
}

func TestStalenessExpiry(t *testing.T) {
	ts := NewTopologyState()

	ts.MergeRemote([]LinkState{
		{From: "A", To: "B", LatencyMs: 5, LoadPct: 10,
			Timestamp: time.Now().Add(-2 * time.Second), Sequence: 1},
	})

	if !ts.IsStale("A", "B", 1*time.Second) {
		t.Fatal("Link older than 1s should be stale")
	}

	pruned := ts.PruneStale(1 * time.Second)
	if pruned != 1 {
		t.Fatalf("Should prune 1 stale link, pruned %d", pruned)
	}
	if ts.Size() != 0 {
		t.Fatal("Topology should be empty after pruning")
	}
	t.Log("Stale entries pruned correctly ✓")
}

func TestGetFreshLinksConvertsToPacketLink(t *testing.T) {
	ts := NewTopologyState()
	ts.UpdateLocal("sender", "router_a", 1.5, 15.0, 0.5)

	links := ts.GetFreshLinks(1 * time.Second)
	if len(links) != 1 {
		t.Fatalf("Expected 1 link, got %d", len(links))
	}

	link := links[0]
	if link.From != "sender" || link.To != "router_a" {
		t.Fatalf("Link names wrong: %s→%s", link.From, link.To)
	}

	var _ packet.Link = link
	t.Logf("Converted to packet.Link: %s→%s lat=%.1f load=%.0f%% ✓",
		link.From, link.To, link.LatencyMs, link.LoadPct)
}

func TestFreshCountVsStaleCount(t *testing.T) {
	ts := NewTopologyState()

	ts.UpdateLocal("A", "B", 5, 10, 0)

	ts.MergeRemote([]LinkState{
		{From: "C", To: "D", LatencyMs: 5, LoadPct: 10,
			Timestamp: time.Now().Add(-5 * time.Second), Sequence: 1},
	})

	threshold := 1 * time.Second
	if ts.CountFresh(threshold) != 1 {
		t.Fatalf("Expected 1 fresh, got %d", ts.CountFresh(threshold))
	}
	if ts.CountStale(threshold) != 1 {
		t.Fatalf("Expected 1 stale, got %d", ts.CountStale(threshold))
	}
	t.Log("Fresh/stale counting correct ✓")
}

// ── Delta-based gossip tests ──

func TestDeltaTracking(t *testing.T) {
	ts := NewTopologyState()

	ts.UpdateLocal("A", "B", 5.0, 20.0, 0.0)
	ts.UpdateLocal("B", "C", 3.0, 10.0, 1.0)

	changed := ts.GetChangedStates()
	if len(changed) != 2 {
		t.Fatalf("Expected 2 changed states before first broadcast, got %d", len(changed))
	}

	ts.MarkBroadcast()

	changed = ts.GetChangedStates()
	if len(changed) != 0 {
		t.Fatalf("Expected 0 changed states after MarkBroadcast, got %d", len(changed))
	}

	ts.UpdateLocal("A", "B", 10.0, 50.0, 2.0)

	changed = ts.GetChangedStates()
	if len(changed) != 1 {
		t.Fatalf("Expected 1 changed state after partial update, got %d", len(changed))
	}
	if changed[0].From != "A" || changed[0].To != "B" {
		t.Fatalf("Changed link should be A→B, got %s→%s", changed[0].From, changed[0].To)
	}
	t.Log("Delta tracking returns only changed states ✓")
}

func TestDeltaNoChangeReturnsEmpty(t *testing.T) {
	ts := NewTopologyState()

	ts.UpdateLocal("X", "Y", 1.0, 5.0, 0.0)
	ts.MarkBroadcast()

	changed := ts.GetChangedStates()
	if len(changed) != 0 {
		t.Fatalf("Expected 0 deltas when nothing changed, got %d", len(changed))
	}
	t.Log("No-change delta correctly returns empty ✓")
}

func TestAdaptiveInterval(t *testing.T) {
	ts := NewTopologyState()

	if !ts.HasChanges() {
		t.Fatal("New TopologyState should have changes=true")
	}

	ts.MarkBroadcast()
	if ts.HasChanges() {
		t.Fatal("HasChanges should be false after MarkBroadcast with no new updates")
	}

	ts.UpdateLocal("A", "B", 5.0, 20.0, 0.0)
	if !ts.HasChanges() {
		t.Fatal("HasChanges should be true after UpdateLocal")
	}

	ts.MarkBroadcast()
	ts.MergeRemote([]LinkState{
		{From: "C", To: "D", LatencyMs: 3, LoadPct: 10, Sequence: 999, Timestamp: time.Now()},
	})
	if !ts.HasChanges() {
		t.Fatal("HasChanges should be true after MergeRemote with new data")
	}
	t.Log("HasChanges flag tracks state mutations correctly ✓")
}

func TestFullSyncAfterDeltaCycle(t *testing.T) {
	ts := NewTopologyState()

	ts.UpdateLocal("A", "B", 5.0, 20.0, 0.0)
	ts.UpdateLocal("B", "C", 3.0, 10.0, 1.0)
	ts.UpdateLocal("C", "D", 7.0, 30.0, 0.5)

	ts.MarkBroadcast()

	allStates := ts.GetAllStates()
	if len(allStates) != 3 {
		t.Fatalf("Full sync (GetAllStates) should return all 3 links, got %d", len(allStates))
	}

	changed := ts.GetChangedStates()
	if len(changed) != 0 {
		t.Fatalf("Delta should be 0 after broadcast, got %d", len(changed))
	}
	t.Log("Full-sync returns complete state independent of delta tracking ✓")
}

// ── New tests for production features ──

func TestDumpToWriter(t *testing.T) {
	ts := NewTopologyState()
	ts.UpdateLocal("A", "B", 5.0, 20.0, 0.0)

	var buf bytes.Buffer
	ts.Dump("test", &buf)

	if buf.Len() == 0 {
		t.Fatal("Dump should write non-empty output")
	}
	t.Logf("Dump output: %s", buf.String())
}

func TestStalenessConfigCustom(t *testing.T) {
	cfg := StalenessConfig{
		AgingLatencyPenalty: 1.0, // 100% penalty — very aggressive
		AgingLoadPenalty:    1.0,
		WarnLatencyPenalty:  2.0,
		WarnLoadPenalty:     2.0,
	}
	ts := NewTopologyStateWithConfig(cfg)
	ts.UpdateLocal("A", "B", 10.0, 50.0, 0.0)

	links := ts.GetLinksWithStaleness(0, 10*time.Second)
	if len(links) != 1 {
		t.Fatalf("Expected 1 link, got %d", len(links))
	}
	// With WarnLatencyPenalty=2.0 and link being in warn zone (warnAge=0),
	// latency should be 10.0 * 2.0 = 20.0
	if links[0].LatencyMs != 20.0 {
		t.Fatalf("Custom warn penalty: expected latency 20.0, got %.1f", links[0].LatencyMs)
	}
	t.Log("Custom StalenessConfig applied correctly ✓")
}

func TestDefaultGossipConfig(t *testing.T) {
	cfg := DefaultGossipConfig()
	if cfg.IntervalMs != DefaultGossipIntervalMs {
		t.Fatalf("IntervalMs should be %d, got %d", DefaultGossipIntervalMs, cfg.IntervalMs)
	}
	if cfg.ReadDeadlineMs != DefaultReadDeadlineMs {
		t.Fatalf("ReadDeadlineMs should be %d, got %d", DefaultReadDeadlineMs, cfg.ReadDeadlineMs)
	}
}

func TestGossipMessageEncodeDecode(t *testing.T) {
	msg := GossipMessage{
		SenderName: "router_a",
		States: []LinkState{
			{From: "A", To: "B", LatencyMs: 5, LoadPct: 20, Sequence: 42, Timestamp: time.Now()},
		},
		SentAt:  time.Now(),
		IsDelta: true,
	}

	data, err := encodeGossipMessage(msg)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded, err := decodeGossipMessage(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if decoded.SenderName != msg.SenderName {
		t.Fatalf("SenderName: got %s, want %s", decoded.SenderName, msg.SenderName)
	}
	if len(decoded.States) != 1 {
		t.Fatalf("States: got %d, want 1", len(decoded.States))
	}
	if !decoded.IsDelta {
		t.Fatal("IsDelta should be true")
	}
	t.Logf("GossipMessage round-trip: %d bytes ✓", len(data))
}
