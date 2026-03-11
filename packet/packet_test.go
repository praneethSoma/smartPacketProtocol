package packet

import (
	"strings"
	"testing"
)

// ══════════════════════════════════════
// IntentHeader Validation + String
// ══════════════════════════════════════

func TestIntentHeaderValidate(t *testing.T) {
	tests := []struct {
		name    string
		intent  IntentHeader
		wantErr bool
	}{
		{"valid_critical", IntentHeader{Latency: 3, Reliability: 2, Ordering: 2, Priority: 3}, false},
		{"valid_relaxed", IntentHeader{Latency: 0, Reliability: 0, Ordering: 0, Priority: 0}, false},
		{"latency_out_of_range", IntentHeader{Latency: 4}, true},
		{"reliability_out_of_range", IntentHeader{Reliability: 3}, true},
		{"ordering_out_of_range", IntentHeader{Ordering: 3}, true},
		{"priority_out_of_range", IntentHeader{Priority: 4}, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.intent.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestIntentHeaderString(t *testing.T) {
	h := IntentHeader{Latency: 3, Reliability: 2, Priority: 3}
	s := h.String()
	if !strings.Contains(s, "critical") {
		t.Fatalf("String() should contain 'critical', got: %s", s)
	}
	if !strings.Contains(s, "guaranteed") {
		t.Fatalf("String() should contain 'guaranteed', got: %s", s)
	}
	t.Logf("IntentHeader.String() = %s ✓", s)
}

// ══════════════════════════════════════
// Dijkstra + Weight Tests
// ══════════════════════════════════════

func TestDijkstraBasicPath(t *testing.T) {
	miniMap := []Link{
		{From: "A", To: "B", LatencyMs: 1, LoadPct: 5, LossPct: 0},
		{From: "B", To: "C", LatencyMs: 1, LoadPct: 5, LossPct: 0},
	}
	intent := IntentHeader{Latency: 3, Reliability: 1}
	graph := BuildGraph(miniMap, intent)
	path := Dijkstra(graph, "A", "C")

	if len(path) != 3 || path[0] != "A" || path[1] != "B" || path[2] != "C" {
		t.Fatalf("Expected [A B C], got %v", path)
	}
}

func TestDijkstraAvoidsCongestion(t *testing.T) {
	miniMap := []Link{
		{From: "sender", To: "router_a", LatencyMs: 1, LoadPct: 5, LossPct: 0},
		{From: "router_a", To: "router_b", LatencyMs: 5, LoadPct: 90, LossPct: 8},
		{From: "router_a", To: "router_c", LatencyMs: 5, LoadPct: 10, LossPct: 0},
		{From: "router_b", To: "receiver", LatencyMs: 100, LoadPct: 90, LossPct: 8},
		{From: "router_c", To: "receiver", LatencyMs: 5, LoadPct: 10, LossPct: 0},
	}

	intent := IntentHeader{Latency: 3, Reliability: 1, Priority: 3}
	graph := BuildGraph(miniMap, intent)
	path := Dijkstra(graph, "sender", "receiver")

	// Should avoid router_b due to high load and latency
	for _, p := range path {
		if p == "router_b" {
			t.Fatalf("Latency-critical packet should avoid congested router_b, path: %v", path)
		}
	}
	t.Logf("Path correctly avoids congestion: %v", path)
}

func TestDijkstraExcludingNodes(t *testing.T) {
	miniMap := []Link{
		{From: "A", To: "B", LatencyMs: 1, LoadPct: 5, LossPct: 0},
		{From: "A", To: "C", LatencyMs: 10, LoadPct: 50, LossPct: 0},
		{From: "B", To: "D", LatencyMs: 1, LoadPct: 5, LossPct: 0},
		{From: "C", To: "D", LatencyMs: 1, LoadPct: 5, LossPct: 0},
	}
	intent := IntentHeader{Latency: 3}
	graph := BuildGraph(miniMap, intent)

	// Exclude B — should force path through C
	exclude := map[string]bool{"B": true}
	path := DijkstraExcluding(graph, "A", "D", exclude)

	for _, p := range path {
		if p == "B" {
			t.Fatalf("Path should not include excluded node B, got: %v", path)
		}
	}
	t.Logf("Path correctly excludes B: %v", path)
}

func TestDijkstraNoPath(t *testing.T) {
	miniMap := []Link{
		{From: "A", To: "B", LatencyMs: 1, LoadPct: 5},
	}
	graph := BuildGraph(miniMap, IntentHeader{Latency: 1})
	path := Dijkstra(graph, "A", "Z") // Z doesn't exist

	if path != nil {
		t.Fatalf("Expected nil for unreachable destination, got: %v", path)
	}
}

func TestIntentAffectsWeight(t *testing.T) {
	link := Link{From: "A", To: "B", LatencyMs: 50, LoadPct: 50, LossPct: 5}

	criticalIntent := IntentHeader{Latency: 3, Reliability: 2}
	relaxedIntent := IntentHeader{Latency: 0, Reliability: 0}

	criticalWeight := calculateWeight(link, criticalIntent)
	relaxedWeight := calculateWeight(link, relaxedIntent)

	if criticalWeight <= relaxedWeight {
		t.Fatalf("Critical intent should assign higher weight (penalize congestion more), critical=%.1f relaxed=%.1f",
			criticalWeight, relaxedWeight)
	}
	t.Logf("Critical weight=%.1f > Relaxed weight=%.1f ✓", criticalWeight, relaxedWeight)
}

func TestCustomWeightConfig(t *testing.T) {
	cfg := WeightConfig{
		LatencyMultiplier:      [4]float64{0, 0, 0, 100.0}, // Extreme latency sensitivity
		LoadMultiplier:         [4]float64{0, 0, 0, 0.0},   // Ignore load entirely
		LossMultiplier:         0,
		ReliabilityThreshold:   2,
		StalePenaltyMultiplier: 1.5,
	}
	link := Link{From: "A", To: "B", LatencyMs: 10, LoadPct: 99, LossPct: 50}
	intent := IntentHeader{Latency: 3}

	weight := calculateWeightWithConfig(link, intent, cfg)
	expected := 10.0 * 100.0 // latency × multiplier, load ignored
	if weight != expected {
		t.Fatalf("Custom config weight: got %.1f, want %.1f", weight, expected)
	}
	t.Logf("Custom WeightConfig: weight=%.1f (load correctly ignored) ✓", weight)
}

// ══════════════════════════════════════
// Reroute Logic Tests
// ══════════════════════════════════════

func TestShouldReroute(t *testing.T) {
	miniMap := []Link{
		{From: "router_a", To: "router_b", LatencyMs: 5, LoadPct: 10, LossPct: 0},
		{From: "router_a", To: "router_c", LatencyMs: 5, LoadPct: 10, LossPct: 0},
		{From: "router_b", To: "receiver", LatencyMs: 5, LoadPct: 10, LossPct: 0},
		{From: "router_c", To: "receiver", LatencyMs: 5, LoadPct: 10, LossPct: 0},
	}

	p := NewSmartPacket("receiver", IntentHeader{Latency: 3}, miniMap, []byte("test"))

	// Map says load=10%, actual load=80% → should reroute (700% divergence)
	if !p.ShouldReroute("router_a", 80.0, 5.0, 30.0) {
		t.Fatal("Should reroute when load diverges significantly")
	}

	// Map says load=10%, actual load=12% → should NOT reroute (20% divergence < 30% threshold)
	if p.ShouldReroute("router_a", 12.0, 5.0, 30.0) {
		t.Fatal("Should not reroute when conditions are close to map")
	}
}

func TestShouldRerouteZeroMapValues(t *testing.T) {
	miniMap := []Link{
		{From: "A", To: "B", LatencyMs: 0, LoadPct: 0, LossPct: 0},
	}
	p := NewSmartPacket("B", IntentHeader{Latency: 3}, miniMap, []byte("test"))

	// Map says 0ms latency, actual is 50ms → should reroute (exceeds DefaultSignificantLatencyMs)
	if !p.ShouldReroute("A", 0.0, 50.0, 30.0) {
		t.Fatal("Should reroute when actual latency is significant but map says 0")
	}

	// Map says 0ms latency, actual is 5ms → should NOT reroute (below significance threshold)
	if p.ShouldReroute("A", 0.0, 5.0, 30.0) {
		t.Fatal("Should not reroute when actual latency is below significance threshold")
	}
}

func TestRerouteChangesPath(t *testing.T) {
	miniMap := []Link{
		{From: "router_a", To: "router_b", LatencyMs: 5, LoadPct: 10, LossPct: 0},
		{From: "router_a", To: "router_c", LatencyMs: 5, LoadPct: 10, LossPct: 0},
		{From: "router_b", To: "receiver", LatencyMs: 5, LoadPct: 10, LossPct: 0},
		{From: "router_c", To: "receiver", LatencyMs: 5, LoadPct: 10, LossPct: 0},
	}

	intent := IntentHeader{Latency: 3}
	p := NewSmartPacket("receiver", intent, miniMap, []byte("test"))
	p.UpdatePath([]string{"router_a", "router_b", "receiver"})

	// Fresh data says router_b is now congested
	freshLinks := []Link{
		{From: "router_a", To: "router_b", LatencyMs: 100, LoadPct: 95, LossPct: 10},
		{From: "router_a", To: "router_c", LatencyMs: 3, LoadPct: 5, LossPct: 0},
		{From: "router_b", To: "receiver", LatencyMs: 100, LoadPct: 95, LossPct: 10},
		{From: "router_c", To: "receiver", LatencyMs: 3, LoadPct: 5, LossPct: 0},
	}

	p.Reroute("router_a", freshLinks)

	// Should now avoid router_b
	for _, node := range p.PlannedPath {
		if node == "router_b" {
			t.Fatalf("After reroute, path should avoid congested router_b, got: %v", p.PlannedPath)
		}
	}
	if !p.Rerouted {
		t.Fatal("Rerouted flag should be true")
	}
	t.Logf("Rerouted path: %v ✓", p.PlannedPath)
}

func TestLoopDetection(t *testing.T) {
	p := NewSmartPacket("D", IntentHeader{Latency: 3}, nil, []byte("test"))
	p.LogHop("A", 10, 1)
	p.LogHop("B", 10, 1)

	// B already visited — loop!
	if !p.DetectLoop("B") {
		t.Fatal("Should detect loop when revisiting node B")
	}

	// C not visited — no loop
	if p.DetectLoop("C") {
		t.Fatal("Should not detect loop for unvisited node C")
	}
}

func TestTTLExpiry(t *testing.T) {
	p := NewSmartPacket("Z", IntentHeader{}, nil, []byte("test"))
	p.MaxHops = 3

	if p.IsExpired() {
		t.Fatal("Fresh packet should not be expired")
	}

	p.HopCount = 3
	if !p.IsExpired() {
		t.Fatal("Packet at MaxHops should be expired")
	}
}

func TestForceForward(t *testing.T) {
	miniMap := []Link{
		{From: "A", To: "B", LatencyMs: 1, LoadPct: 90, LossPct: 0},
		{From: "A", To: "C", LatencyMs: 1, LoadPct: 10, LossPct: 0},
	}

	p := NewSmartPacket("D", IntentHeader{Latency: 3}, miniMap, []byte("test"))
	p.VisitedNodes["B"] = true // B already visited

	// Should pick C (unvisited, lower weight)
	next := p.ForceForward("A")
	if next != "C" {
		t.Fatalf("ForceForward should prefer unvisited node C, got: %s", next)
	}
}

func TestForceForwardDirectDestination(t *testing.T) {
	miniMap := []Link{
		{From: "A", To: "Z", LatencyMs: 100, LoadPct: 99, LossPct: 0},
		{From: "A", To: "B", LatencyMs: 1, LoadPct: 1, LossPct: 0},
	}

	p := NewSmartPacket("Z", IntentHeader{Latency: 3}, miniMap, []byte("test"))

	// Should always prefer direct link to destination, even if it's worse
	next := p.ForceForward("A")
	if next != "Z" {
		t.Fatalf("ForceForward should prefer direct destination link, got: %s", next)
	}
}

// ══════════════════════════════════════
// NewSmartPacket defaults
// ══════════════════════════════════════

func TestNewSmartPacketDefaults(t *testing.T) {
	p := NewSmartPacket("dst", IntentHeader{Latency: 1}, nil, []byte("data"))

	if p.Version != ProtocolVersion {
		t.Errorf("Version should be %d, got %d", ProtocolVersion, p.Version)
	}
	if p.PacketType != PacketTypeData {
		t.Errorf("PacketType should be %d (DATA), got %d", PacketTypeData, p.PacketType)
	}
	if p.MaxHops != DefaultMaxHops {
		t.Errorf("MaxHops should be %d, got %d", DefaultMaxHops, p.MaxHops)
	}
	if p.HopCount != 0 {
		t.Errorf("HopCount should be 0, got %d", p.HopCount)
	}
	if p.VisitedNodes == nil {
		t.Error("VisitedNodes map should be initialized")
	}
}

// ══════════════════════════════════════
// Wire Format Tests
// ══════════════════════════════════════

func TestWireEncodeDecodeRoundTrip(t *testing.T) {
	p := NewSmartPacket("receiver", IntentHeader{
		Reliability: 2,
		Latency:     3,
		Ordering:    1,
		Priority:    3,
	}, []Link{
		{From: "sender", To: "router_a", LatencyMs: 1.5, LoadPct: 15, LossPct: 0.5},
		{From: "router_a", To: "receiver", LatencyMs: 3.2, LoadPct: 25, LossPct: 1.0},
	}, []byte("game_data:x=42,y=99"))

	p.UpdatePath([]string{"sender", "router_a", "receiver"})
	p.LogHop("router_a", 15.0, 1.5)
	p.Rerouted = true

	encoded, err := p.EncodeWire()
	if err != nil {
		t.Fatalf("EncodeWire failed: %v", err)
	}

	decoded, err := DecodeWire(encoded)
	if err != nil {
		t.Fatalf("DecodeWire failed: %v", err)
	}

	// Verify all fields
	if decoded.Destination != p.Destination {
		t.Errorf("Destination: got %s, want %s", decoded.Destination, p.Destination)
	}
	if decoded.Intent.Latency != p.Intent.Latency {
		t.Errorf("Intent.Latency: got %d, want %d", decoded.Intent.Latency, p.Intent.Latency)
	}
	if decoded.Intent.Reliability != p.Intent.Reliability {
		t.Errorf("Intent.Reliability: got %d, want %d", decoded.Intent.Reliability, p.Intent.Reliability)
	}
	if len(decoded.PlannedPath) != len(p.PlannedPath) {
		t.Errorf("PlannedPath length: got %d, want %d", len(decoded.PlannedPath), len(p.PlannedPath))
	}
	if len(decoded.MiniMap) != len(p.MiniMap) {
		t.Errorf("MiniMap length: got %d, want %d", len(decoded.MiniMap), len(p.MiniMap))
	}
	if len(decoded.CongestionLog) != len(p.CongestionLog) {
		t.Errorf("CongestionLog length: got %d, want %d", len(decoded.CongestionLog), len(p.CongestionLog))
	}
	if string(decoded.Payload) != string(p.Payload) {
		t.Errorf("Payload: got %s, want %s", decoded.Payload, p.Payload)
	}
	if !decoded.Rerouted {
		t.Error("Rerouted flag should be true")
	}
	if decoded.MaxHops != p.MaxHops {
		t.Errorf("MaxHops: got %d, want %d", decoded.MaxHops, p.MaxHops)
	}

	t.Logf("Wire format round-trip: %d bytes ✓", len(encoded))
}

func TestWireMagicValidation(t *testing.T) {
	data := []byte{0xFF, 0xFF, 0xFF, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	_, err := DecodeWire(data)
	if err == nil {
		t.Fatal("Should reject packet with invalid magic bytes")
	}
}

func TestWireChecksumValidation(t *testing.T) {
	p := NewSmartPacket("dst", IntentHeader{Latency: 1}, nil, []byte("data"))
	encoded, _ := p.EncodeWire()

	// Corrupt a byte in the payload area
	if len(encoded) > WireHeaderSize+5 {
		encoded[WireHeaderSize+3] ^= 0xFF
	}

	_, err := DecodeWire(encoded)
	if err == nil {
		t.Fatal("Should reject packet with corrupted checksum")
	}
}

func TestWireTooShort(t *testing.T) {
	_, err := DecodeWire([]byte{0x53, 0x50})
	if err == nil {
		t.Fatal("Should reject packet shorter than header size")
	}
}

func TestWireVersionMismatch(t *testing.T) {
	p := NewSmartPacket("dst", IntentHeader{}, nil, []byte("x"))
	encoded, _ := p.EncodeWire()

	// Tamper version byte
	encoded[3] = 99
	// Fix CRC so only version check fails
	_, err := DecodeWire(encoded)
	if err == nil {
		t.Fatal("Should reject packet with unsupported version")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Fatalf("Error should mention version, got: %v", err)
	}
}

// ══════════════════════════════════════
// Serialization (gob) backward compat
// ══════════════════════════════════════

func TestGobEncodeDecodeRoundTrip(t *testing.T) {
	p := NewSmartPacket("receiver", IntentHeader{Latency: 3}, []Link{
		{From: "A", To: "B", LatencyMs: 5, LoadPct: 20, LossPct: 1},
	}, []byte("hello_world"))
	p.UpdatePath([]string{"A", "B", "receiver"})

	encoded, err := p.Encode()
	if err != nil {
		t.Fatalf("Encode failed: %v", err)
	}

	decoded, err := Decode(encoded)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.Destination != "receiver" {
		t.Errorf("Destination mismatch: %s", decoded.Destination)
	}
	if string(decoded.Payload) != "hello_world" {
		t.Errorf("Payload mismatch: %s", decoded.Payload)
	}
	t.Logf("Gob round-trip: %d bytes ✓", len(encoded))
}

func TestGobDecodeTooLarge(t *testing.T) {
	// Create data exceeding MaxGobDecodeSize
	huge := make([]byte, MaxGobDecodeSize+1)
	_, err := Decode(huge)
	if err == nil {
		t.Fatal("Should reject gob payload exceeding MaxGobDecodeSize")
	}
}

// ══════════════════════════════════════
// Summary + RemainingPath
// ══════════════════════════════════════

func TestSummary(t *testing.T) {
	p := NewSmartPacket("dst", IntentHeader{Latency: 3}, nil, []byte("data"))
	p.SourceNode = "src"
	s := p.Summary()
	if !strings.Contains(s, "dst") || !strings.Contains(s, "src") {
		t.Fatalf("Summary should contain src and dst, got: %s", s)
	}
}

func TestRemainingPath(t *testing.T) {
	p := NewSmartPacket("D", IntentHeader{}, nil, nil)
	p.UpdatePath([]string{"A", "B", "C", "D"})
	p.HopIndex = 1 // At B

	remaining := p.RemainingPath()
	if len(remaining) != 3 || remaining[0] != "B" {
		t.Fatalf("RemainingPath from B should be [B C D], got: %v", remaining)
	}
}
