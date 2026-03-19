package packet

import (
	"fmt"
	"testing"
)

// ──────────────────────────────────────────────────────────────
// Packet fixtures — cover the three packet shapes routers see.
// ──────────────────────────────────────────────────────────────

// benchLightPacket returns a typical LightMode packet (no MiniMap):
// 4-hop path, 2 congestion log entries, 2 visited nodes.
// This is the common case for forwarding in LightMode deployments.
func benchLightPacket() *SmartPacket {
	p := NewLightPacket("receiver", IntentHeader{
		Reliability: 2, Latency: 2, Ordering: 0, Priority: 1,
	}, []string{"sender", "router_a", "router_f", "receiver"}, []byte("hello benchmark"))
	p.SourceNode = "sender"
	p.CreatedAtNs = 1710000000000000000
	p.LogHop("sender", 5.0, 0.2)
	p.LogHop("router_a", 12.3, 1.5)
	return p
}

// benchFullPacket returns a full SPP packet with embedded MiniMap:
// 14 links (7-router topology), 5-hop path, 3 congestion log entries.
func benchFullPacket() *SmartPacket {
	links := []Link{
		{"router_a", "router_b", 1.2, 15.0, 0.0},
		{"router_a", "router_c", 0.8, 10.0, 0.0},
		{"router_a", "router_f", 0.5, 8.0, 0.0},
		{"router_b", "router_a", 1.3, 14.0, 0.0},
		{"router_b", "router_d", 2.0, 20.0, 0.5},
		{"router_c", "router_a", 0.9, 11.0, 0.0},
		{"router_c", "router_e", 1.1, 12.0, 0.0},
		{"router_d", "router_b", 2.1, 19.0, 0.5},
		{"router_d", "router_g", 1.5, 16.0, 0.0},
		{"router_e", "router_c", 1.0, 13.0, 0.0},
		{"router_e", "router_g", 0.7, 9.0, 0.0},
		{"router_f", "router_a", 0.6, 7.0, 0.0},
		{"router_f", "receiver", 0.3, 5.0, 0.0},
		{"router_g", "receiver", 0.4, 6.0, 0.0},
	}
	p := NewSmartPacket("receiver", IntentHeader{
		Reliability: 2, Latency: 3, Ordering: 0, Priority: 2,
	}, links, []byte("full packet payload"))
	p.SourceNode = "sender"
	p.CreatedAtNs = 1710000000000000000
	p.PlannedPath = []string{"sender", "router_a", "router_c", "router_e", "router_g", "receiver"}
	p.LogHop("sender", 3.0, 0.1)
	p.LogHop("router_a", 10.0, 0.8)
	p.LogHop("router_c", 11.5, 1.1)
	return p
}

// benchCompactPacket returns a packet using compact uint16 IDs.
func benchCompactPacket() (*SmartPacket, *NodeIDTable) {
	nit := NewNodeIDTable(map[string]uint16{
		"sender":   1,
		"router_a": 2,
		"router_b": 3,
		"router_c": 4,
		"router_d": 5,
		"router_e": 6,
		"router_f": 7,
		"router_g": 8,
		"receiver": 9,
	})
	p := benchLightPacket()
	return p, nit
}

// ──────────────────────────────────────────────────────────────
// Wire format benchmarks — encode, decode, round-trip
// ──────────────────────────────────────────────────────────────

func BenchmarkEncodeWire(b *testing.B) {
	b.Run("light", func(b *testing.B) {
		p := benchLightPacket()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := p.EncodeWire(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("full_map", func(b *testing.B) {
		p := benchFullPacket()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := p.EncodeWire(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("compact_ids", func(b *testing.B) {
		p, nit := benchCompactPacket()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := p.EncodeWireWithIDs(nit); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkDecodeWire(b *testing.B) {
	b.Run("light", func(b *testing.B) {
		data, _ := benchLightPacket().EncodeWire()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := DecodeWire(data); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("full_map", func(b *testing.B) {
		data, _ := benchFullPacket().EncodeWire()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := DecodeWire(data); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("compact_ids", func(b *testing.B) {
		p, nit := benchCompactPacket()
		data, _ := p.EncodeWireWithIDs(nit)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := DecodeWireWithIDs(data, nit); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkRoundTrip(b *testing.B) {
	b.Run("light", func(b *testing.B) {
		p := benchLightPacket()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			data, err := p.EncodeWire()
			if err != nil {
				b.Fatal(err)
			}
			if _, err = DecodeWire(data); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("full_map", func(b *testing.B) {
		p := benchFullPacket()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			data, err := p.EncodeWire()
			if err != nil {
				b.Fatal(err)
			}
			if _, err = DecodeWire(data); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("compact_ids", func(b *testing.B) {
		p, nit := benchCompactPacket()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			data, err := p.EncodeWireWithIDs(nit)
			if err != nil {
				b.Fatal(err)
			}
			if _, err = DecodeWireWithIDs(data, nit); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// ──────────────────────────────────────────────────────────────
// Dijkstra benchmarks — the routing brain
// ──────────────────────────────────────────────────────────────

// bench7RouterLinks returns the 14-link topology used in the 7-router test setup.
func bench7RouterLinks() []Link {
	return []Link{
		{"sender", "router_a", 0.1, 2.0, 0.0},
		{"router_a", "router_b", 1.2, 15.0, 0.0},
		{"router_a", "router_c", 0.8, 10.0, 0.0},
		{"router_a", "router_f", 0.5, 8.0, 0.0},
		{"router_b", "router_d", 2.0, 20.0, 0.5},
		{"router_c", "router_e", 1.1, 12.0, 0.0},
		{"router_d", "router_g", 1.5, 16.0, 0.0},
		{"router_e", "router_g", 0.7, 9.0, 0.0},
		{"router_f", "receiver", 0.3, 5.0, 0.0},
		{"router_g", "receiver", 0.4, 6.0, 0.0},
		// Reverse links
		{"router_b", "router_a", 1.3, 14.0, 0.0},
		{"router_c", "router_a", 0.9, 11.0, 0.0},
		{"router_d", "router_b", 2.1, 19.0, 0.5},
		{"router_e", "router_c", 1.0, 13.0, 0.0},
	}
}

func BenchmarkBuildGraph(b *testing.B) {
	links := bench7RouterLinks()
	intent := IntentHeader{Reliability: 2, Latency: 3, Priority: 2}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		BuildGraph(links, intent)
	}
}

func BenchmarkBuildGraphForDest(b *testing.B) {
	links := bench7RouterLinks()
	intent := IntentHeader{Reliability: 2, Latency: 3, Priority: 2}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		BuildGraphForDest(links, intent, "receiver")
	}
}

func BenchmarkDijkstra(b *testing.B) {
	links := bench7RouterLinks()
	intent := IntentHeader{Reliability: 2, Latency: 3, Priority: 2}
	graph := BuildGraph(links, intent)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Dijkstra(graph, "sender", "receiver")
	}
}

func BenchmarkDijkstraExcluding(b *testing.B) {
	links := bench7RouterLinks()
	intent := IntentHeader{Reliability: 2, Latency: 3, Priority: 2}
	graph := BuildGraph(links, intent)
	exclude := map[string]bool{"router_f": true}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		DijkstraExcluding(graph, "sender", "receiver", exclude)
	}
}

func BenchmarkBuildOSPFGraph(b *testing.B) {
	links := bench7RouterLinks()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		BuildOSPFGraph(links)
	}
}

// BenchmarkFullRoutingPipeline measures the complete cost of what a router
// does per packet: decode → build graph → dijkstra → encode.
func BenchmarkFullRoutingPipeline(b *testing.B) {
	p := benchFullPacket()
	wireData, _ := p.EncodeWire()
	freshLinks := bench7RouterLinks()

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// Decode incoming packet
		pkt, err := DecodeWire(wireData)
		if err != nil {
			b.Fatal(err)
		}
		// Build graph and run Dijkstra (simulates reroute check)
		graph := BuildGraphForDest(freshLinks, pkt.Intent, pkt.Destination)
		path := Dijkstra(graph, "router_a", pkt.Destination)
		if path == nil {
			b.Fatal("no path")
		}
		// Encode outgoing packet
		if _, err := pkt.EncodeWire(); err != nil {
			b.Fatal(err)
		}
	}
}

// ──────────────────────────────────────────────────────────────
// Packet operations benchmarks — LogHop, ShouldReroute, etc.
// ──────────────────────────────────────────────────────────────

func BenchmarkLogHop(b *testing.B) {
	p := benchFullPacket()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p.HopIndex = 0
		p.HopCount = 0
		p.VisitedNodes = make(map[string]bool)
		p.CongestionLog = p.CongestionLog[:0]
		p.LogHop("router_a", 10.0, 0.8)
	}
}

func BenchmarkShouldReroute(b *testing.B) {
	p := benchFullPacket()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p.ShouldReroute("router_a", 15.0, 2.0, RerouteThreshold)
	}
}

func BenchmarkShouldReroute_NoReroute(b *testing.B) {
	p := benchFullPacket()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p.ShouldReroute("router_a", 10.1, 0.85, RerouteThreshold)
	}
}

func BenchmarkDetectLoop(b *testing.B) {
	p := benchFullPacket()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p.DetectLoop("router_e")
	}
}

func BenchmarkForceForward(b *testing.B) {
	p := benchFullPacket()
	p.CurrentNode = "router_a"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p.ForceForward("router_a")
	}
}

// ──────────────────────────────────────────────────────────────
// Weight calculation benchmarks
// ──────────────────────────────────────────────────────────────

func BenchmarkCalculateWeight(b *testing.B) {
	link := Link{"router_a", "router_b", 5.0, 30.0, 2.0}
	intents := []struct {
		name   string
		intent IntentHeader
	}{
		{"relaxed", IntentHeader{Reliability: 0, Latency: 0, Priority: 0}},
		{"normal", IntentHeader{Reliability: 1, Latency: 1, Priority: 1}},
		{"critical", IntentHeader{Reliability: 2, Latency: 3, Priority: 3}},
	}
	for _, tc := range intents {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				calculateWeight(link, tc.intent)
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────
// Packet size reporting (not a speed benchmark, but useful)
// ──────────────────────────────────────────────────────────────

func BenchmarkPacketSizes(b *testing.B) {
	cases := []struct {
		name string
		fn   func() ([]byte, error)
	}{
		{"light", func() ([]byte, error) { return benchLightPacket().EncodeWire() }},
		{"full_map", func() ([]byte, error) { return benchFullPacket().EncodeWire() }},
		{"compact_ids", func() ([]byte, error) {
			p, nit := benchCompactPacket()
			return p.EncodeWireWithIDs(nit)
		}},
	}
	for _, tc := range cases {
		data, err := tc.fn()
		if err != nil {
			b.Fatal(err)
		}
		b.Run(fmt.Sprintf("%s_%dB", tc.name, len(data)), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				tc.fn()
			}
		})
	}
}
