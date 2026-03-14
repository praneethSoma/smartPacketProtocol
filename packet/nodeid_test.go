package packet

import (
	"testing"
)

func TestNodeIDTableBidirectional(t *testing.T) {
	nit := NewNodeIDTable(map[string]uint16{
		"sender":   1,
		"router_a": 2,
		"receiver": 3,
	})

	id, ok := nit.ToID("router_a")
	if !ok || id != 2 {
		t.Fatalf("ToID(router_a) = %d, %v; want 2, true", id, ok)
	}

	name, ok := nit.ToName(2)
	if !ok || name != "router_a" {
		t.Fatalf("ToName(2) = %q, %v; want router_a, true", name, ok)
	}
}

func TestNodeIDTableUnknown(t *testing.T) {
	nit := NewNodeIDTable(map[string]uint16{"sender": 1})

	if _, ok := nit.ToID("unknown"); ok {
		t.Fatal("expected ToID(unknown) to return false")
	}
	if _, ok := nit.ToName(999); ok {
		t.Fatal("expected ToName(999) to return false")
	}
}

func TestNodeIDTableNil(t *testing.T) {
	var nit *NodeIDTable
	if _, ok := nit.ToID("sender"); ok {
		t.Fatal("nil table ToID should return false")
	}
	if _, ok := nit.ToName(1); ok {
		t.Fatal("nil table ToName should return false")
	}
}

func TestLoadNodeIDTableFromJSON(t *testing.T) {
	data := []byte(`{"sender": 1, "router_a": 2, "receiver": 3}`)
	nit, err := LoadNodeIDTableFromJSON(data)
	if err != nil {
		t.Fatal(err)
	}

	id, ok := nit.ToID("receiver")
	if !ok || id != 3 {
		t.Fatalf("got %d, %v; want 3, true", id, ok)
	}
}

func TestLoadNodeIDTableFromJSONInvalid(t *testing.T) {
	_, err := LoadNodeIDTableFromJSON([]byte(`not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestCompactIDsEmptyCurrentNode(t *testing.T) {
	nit := NewNodeIDTable(map[string]uint16{
		"sender":   1,
		"router_a": 2,
		"router_f": 3,
		"receiver": 4,
	})

	p := NewLightPacket("receiver", IntentHeader{Latency: 3}, []string{"sender", "router_a", "router_f", "receiver"}, []byte("test"))
	// CurrentNode is "" from NewLightPacket

	encoded, err := p.EncodeWireWithIDs(nit)
	if err != nil {
		t.Fatalf("encode with empty CurrentNode failed: %v", err)
	}

	decoded, err := DecodeWireWithIDs(encoded, nit)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if decoded.CurrentNode != "" {
		t.Fatalf("CurrentNode = %q, want empty", decoded.CurrentNode)
	}
	if decoded.Destination != "receiver" {
		t.Fatalf("Destination = %q, want receiver", decoded.Destination)
	}
}

func TestWireRoundtripWithCompactIDs(t *testing.T) {
	nit := NewNodeIDTable(map[string]uint16{
		"sender":   1,
		"router_a": 2,
		"router_b": 3,
		"receiver": 4,
	})

	p := NewSmartPacket("receiver", IntentHeader{Latency: 3, Reliability: 1}, []Link{
		{From: "sender", To: "router_a", LatencyMs: 1.0, LoadPct: 5.0, LossPct: 0.1},
		{From: "router_a", To: "router_b", LatencyMs: 2.0, LoadPct: 10.0, LossPct: 0.0},
		{From: "router_b", To: "receiver", LatencyMs: 1.5, LoadPct: 3.0, LossPct: 0.0},
	}, []byte("test_payload"))
	p.UpdatePath([]string{"sender", "router_a", "router_b", "receiver"})
	p.CurrentNode = "sender"
	p.LogHop("sender", 5.0, 1.0)

	encoded, err := p.EncodeWireWithIDs(nit)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded, err := DecodeWireWithIDs(encoded, nit)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if decoded.Destination != "receiver" {
		t.Fatalf("destination = %q, want receiver", decoded.Destination)
	}
	if len(decoded.PlannedPath) != 4 {
		t.Fatalf("path len = %d, want 4", len(decoded.PlannedPath))
	}
	if string(decoded.Payload) != "test_payload" {
		t.Fatalf("payload = %q", decoded.Payload)
	}
	if len(decoded.MiniMap) != 3 {
		t.Fatalf("minimap len = %d, want 3", len(decoded.MiniMap))
	}
	if len(decoded.CongestionLog) != 1 {
		t.Fatalf("congestion log len = %d, want 1", len(decoded.CongestionLog))
	}
	if decoded.CongestionLog[0].NodeName != "sender" {
		t.Fatalf("congestion log node = %q, want sender", decoded.CongestionLog[0].NodeName)
	}
}

func TestCompactIDsSmallerThanStrings(t *testing.T) {
	nit := NewNodeIDTable(map[string]uint16{
		"sender":   1,
		"router_a": 2,
		"router_b": 3,
		"receiver": 4,
	})

	p := NewSmartPacket("receiver", IntentHeader{Latency: 3}, []Link{
		{From: "sender", To: "router_a", LatencyMs: 1.0, LoadPct: 5.0},
		{From: "router_a", To: "router_b", LatencyMs: 2.0, LoadPct: 10.0},
		{From: "router_b", To: "receiver", LatencyMs: 1.5, LoadPct: 3.0},
	}, []byte("payload"))
	p.UpdatePath([]string{"sender", "router_a", "router_b", "receiver"})
	p.CurrentNode = "sender"

	stringEncoded, err := p.EncodeWire()
	if err != nil {
		t.Fatalf("string encode: %v", err)
	}

	compactEncoded, err := p.EncodeWireWithIDs(nit)
	if err != nil {
		t.Fatalf("compact encode: %v", err)
	}

	if len(compactEncoded) >= len(stringEncoded) {
		t.Fatalf("compact (%d bytes) should be smaller than string (%d bytes)",
			len(compactEncoded), len(stringEncoded))
	}

	t.Logf("string: %d bytes, compact: %d bytes (saved %d bytes, %.0f%%)",
		len(stringEncoded), len(compactEncoded),
		len(stringEncoded)-len(compactEncoded),
		float64(len(stringEncoded)-len(compactEncoded))/float64(len(stringEncoded))*100)
}

func TestDecodeWireWithIDsFallsBackForNonCompact(t *testing.T) {
	p := NewSmartPacket("receiver", IntentHeader{Latency: 2}, nil, []byte("hello"))
	p.CurrentNode = "sender"

	encoded, err := p.EncodeWire()
	if err != nil {
		t.Fatal(err)
	}

	// DecodeWireWithIDs should handle non-compact packets even with a nit.
	nit := NewNodeIDTable(map[string]uint16{"sender": 1, "receiver": 2})
	decoded, err := DecodeWireWithIDs(encoded, nit)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Destination != "receiver" {
		t.Fatalf("destination = %q", decoded.Destination)
	}
}

func TestEncodeWireWithIDsNilTableFallback(t *testing.T) {
	p := NewSmartPacket("receiver", IntentHeader{}, nil, []byte("test"))
	p.CurrentNode = "sender"

	encoded, err := p.EncodeWireWithIDs(nil)
	if err != nil {
		t.Fatal(err)
	}

	// Should be decodable as regular wire format.
	decoded, err := DecodeWire(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Destination != "receiver" {
		t.Fatalf("destination = %q", decoded.Destination)
	}
}
