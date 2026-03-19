//go:build integration

package test

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"smartpacket/packet"
)

// ──────────────────────────────────────────────────────────────
// Test: Gossip Detection Lag — measures how quickly SPP reroutes
// after congestion appears mid-stream.
//
// Topology:
//   sender → router_a → router_b → receiver   (primary path)
//                      ↘ router_c → receiver   (alternate path)
//
// Phase 1: Send packets every 100ms on the primary path (no congestion).
// Phase 2: Inject congestion on router_b, continue sending.
// Phase 3: Measure which packet is the first to be rerouted via router_c.
//
// The delta between congestion injection and first rerouted packet
// is the gossip detection lag.
// ──────────────────────────────────────────────────────────────

func TestGossipDetectionLag(t *testing.T) {
	// --- Set up receiver ---
	receiverAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	receiverConn, _ := net.ListenUDP("udp", receiverAddr)
	defer receiverConn.Close()
	receiverListenAddr := receiverConn.LocalAddr().String()

	// --- Create routers ---
	routerA := newMiniRouter(t, "router_a", 5.0, 1.0)
	routerB := newMiniRouter(t, "router_b", 5.0, 1.0)  // starts healthy
	routerC := newMiniRouter(t, "router_c", 5.0, 1.0)   // alternate path

	// Wire topology: A→B, A→C, B→C (for reroute), B→receiver, C→receiver.
	routerA.neighbors["router_b"] = routerB.listenAddr
	routerA.neighbors["router_c"] = routerC.listenAddr
	routerB.neighbors["receiver"] = receiverListenAddr
	routerB.neighbors["router_c"] = routerC.listenAddr // B can reroute to C
	routerC.neighbors["receiver"] = receiverListenAddr

	// Pre-seed topology on all routers (healthy view).
	for _, r := range []*miniRouter{routerA, routerB, routerC} {
		r.topoState.UpdateLocal("router_a", "router_b", 1.0, 5.0, 0.0)
		r.topoState.UpdateLocal("router_a", "router_c", 1.0, 5.0, 0.0)
		r.topoState.UpdateLocal("router_b", "receiver", 1.0, 5.0, 0.0)
		r.topoState.UpdateLocal("router_b", "router_c", 1.0, 5.0, 0.0)
		r.topoState.UpdateLocal("router_c", "receiver", 1.0, 5.0, 0.0)
	}

	routerA.start(t)
	routerB.start(t)
	routerC.start(t)
	defer routerA.stop()
	defer routerB.stop()
	defer routerC.stop()

	// --- Packet template ---
	// Map shows both paths are healthy → Dijkstra picks primary path through B.
	miniMap := []packet.Link{
		{From: "sender", To: "router_a", LatencyMs: 1, LoadPct: 5},
		{From: "router_a", To: "router_b", LatencyMs: 1, LoadPct: 5},
		{From: "router_a", To: "router_c", LatencyMs: 1, LoadPct: 5},
		{From: "router_b", To: "receiver", LatencyMs: 1, LoadPct: 5},
		{From: "router_b", To: "router_c", LatencyMs: 1, LoadPct: 5},
		{From: "router_c", To: "receiver", LatencyMs: 1, LoadPct: 5},
	}
	intent := packet.IntentHeader{Latency: 3, Reliability: 1}
	forcedPath := []string{"sender", "router_a", "router_b", "receiver"}

	// --- Receiver goroutine: collect all received packets ---
	type receivedPacket struct {
		pkt        *packet.SmartPacket
		receivedAt time.Time
	}
	var (
		received   []receivedPacket
		receivedMu sync.Mutex
		stopRecv   = make(chan struct{})
		recvDone   sync.WaitGroup
	)
	recvDone.Add(1)
	go func() {
		defer recvDone.Done()
		buf := make([]byte, 65535)
		for {
			select {
			case <-stopRecv:
				return
			default:
			}
			receiverConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			n, _, err := receiverConn.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			p, err := packet.DecodeWire(buf[:n])
			if err != nil {
				continue
			}
			receivedMu.Lock()
			received = append(received, receivedPacket{pkt: p, receivedAt: time.Now()})
			receivedMu.Unlock()
		}
	}()

	// --- Send packets every 100ms ---
	const (
		sendInterval         = 100 * time.Millisecond
		preCongestPackets    = 10  // 1s of normal traffic
		postCongestPackets   = 30  // 3s after congestion injection
	)

	var congestionInjectedAt time.Time
	var packetSeq atomic.Int32

	sendPacket := func() {
		seq := packetSeq.Add(1)
		payload := fmt.Sprintf("pkt_%d", seq)
		p := packet.NewSmartPacket("receiver", intent, miniMap, []byte(payload))
		p.UpdatePath(forcedPath)
		p.SourceNode = "sender"

		encoded, err := p.EncodeWire()
		if err != nil {
			t.Logf("encode error for %s: %v", payload, err)
			return
		}

		udpAddr, _ := net.ResolveUDPAddr("udp", routerA.listenAddr)
		conn, err := net.DialUDP("udp", nil, udpAddr)
		if err != nil {
			return
		}
		conn.Write(encoded)
		conn.Close()
	}

	// Phase 1: Send packets with no congestion.
	t.Log("Phase 1: Sending packets on healthy network...")
	for i := 0; i < preCongestPackets; i++ {
		sendPacket()
		time.Sleep(sendInterval)
	}

	// Phase 2: Inject congestion on router_b path.
	t.Log("Phase 2: Injecting congestion on router_b...")
	congestionInjectedAt = time.Now()

	// Make router_b actually congested (ShouldReroute checks router's own metrics).
	routerB.loadPct = 95.0
	routerB.latencyMs = 200.0

	// Simulate gossip propagation: update topology to reflect B's congestion.
	// B→receiver is the congested link (high latency + loss).
	// B→C remains healthy (the alternate path).
	routerA.topoState.UpdateLocal("router_a", "router_b", 200.0, 95.0, 0.0)
	routerB.topoState.UpdateLocal("router_b", "receiver", 200.0, 95.0, 30.0)
	routerB.topoState.UpdateLocal("router_b", "router_c", 1.0, 5.0, 0.0)
	routerB.topoState.UpdateLocal("router_c", "receiver", 1.0, 5.0, 0.0)

	// Phase 3: Continue sending packets and observe rerouting.
	t.Log("Phase 3: Sending packets post-congestion, measuring detection lag...")
	for i := 0; i < postCongestPackets; i++ {
		sendPacket()
		time.Sleep(sendInterval)
	}

	// Wait for in-flight packets to arrive.
	time.Sleep(500 * time.Millisecond)
	close(stopRecv)
	recvDone.Wait()

	// --- Analyze results ---
	receivedMu.Lock()
	defer receivedMu.Unlock()

	t.Logf("Total packets received: %d / %d sent", len(received), preCongestPackets+postCongestPackets)

	var firstRerouteTime time.Time
	normalCount := 0
	reroutedCount := 0

	for _, rp := range received {
		if rp.pkt.Rerouted {
			reroutedCount++
			if firstRerouteTime.IsZero() {
				firstRerouteTime = rp.receivedAt
			}
		} else {
			normalCount++
		}
	}

	t.Logf("Normal (via B): %d, Rerouted (via C): %d", normalCount, reroutedCount)

	if reroutedCount == 0 {
		t.Fatal("FAIL: No packets were rerouted after congestion injection. " +
			"Gossip detection did not trigger rerouting.")
	}

	detectionLag := firstRerouteTime.Sub(congestionInjectedAt)
	t.Logf("Gossip detection lag: %v (time from congestion injection to first rerouted packet received)", detectionLag)

	// Log hop details for the first rerouted packet.
	for _, rp := range received {
		if rp.pkt.Rerouted {
			t.Logf("First rerouted packet: path=%v, hops=%d, payload=%q",
				rp.pkt.PlannedPath, rp.pkt.HopCount, rp.pkt.Payload)
			break
		}
	}

	// Sanity check: detection lag should be under 2s for in-process test.
	// (No real network delay, gossip is pre-seeded — should be near-instant.)
	if detectionLag > 2*time.Second {
		t.Errorf("Detection lag too high: %v (expected < 2s for in-process test)", detectionLag)
	}

	// Report: pre-congestion packets should NOT be rerouted.
	t.Logf("Summary: %d normal packets before congestion, %d rerouted after. Lag = %v",
		normalCount, reroutedCount, detectionLag)
}

// ──────────────────────────────────────────────────────────────
// Test: Detection Lag with Gradual Congestion — simulates
// congestion building over time (not instant), measuring when
// the 30% divergence threshold is first crossed.
// ──────────────────────────────────────────────────────────────

func TestGossipDetectionLagGradual(t *testing.T) {
	// Same topology as above.
	receiverAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	receiverConn, _ := net.ListenUDP("udp", receiverAddr)
	defer receiverConn.Close()
	receiverListenAddr := receiverConn.LocalAddr().String()

	routerA := newMiniRouter(t, "router_a", 5.0, 1.0)
	routerB := newMiniRouter(t, "router_b", 5.0, 1.0)
	routerC := newMiniRouter(t, "router_c", 5.0, 1.0)

	routerA.neighbors["router_b"] = routerB.listenAddr
	routerA.neighbors["router_c"] = routerC.listenAddr
	routerB.neighbors["receiver"] = receiverListenAddr
	routerB.neighbors["router_c"] = routerC.listenAddr
	routerC.neighbors["receiver"] = receiverListenAddr

	for _, r := range []*miniRouter{routerA, routerB, routerC} {
		r.topoState.UpdateLocal("router_a", "router_b", 1.0, 5.0, 0.0)
		r.topoState.UpdateLocal("router_a", "router_c", 1.0, 5.0, 0.0)
		r.topoState.UpdateLocal("router_b", "receiver", 1.0, 5.0, 0.0)
		r.topoState.UpdateLocal("router_b", "router_c", 1.0, 5.0, 0.0)
		r.topoState.UpdateLocal("router_c", "receiver", 1.0, 5.0, 0.0)
	}

	routerA.start(t)
	routerB.start(t)
	routerC.start(t)
	defer routerA.stop()
	defer routerB.stop()
	defer routerC.stop()

	miniMap := []packet.Link{
		{From: "sender", To: "router_a", LatencyMs: 1, LoadPct: 5},
		{From: "router_a", To: "router_b", LatencyMs: 1, LoadPct: 5},
		{From: "router_a", To: "router_c", LatencyMs: 1, LoadPct: 5},
		{From: "router_b", To: "receiver", LatencyMs: 1, LoadPct: 5},
		{From: "router_b", To: "router_c", LatencyMs: 1, LoadPct: 5},
		{From: "router_c", To: "receiver", LatencyMs: 1, LoadPct: 5},
	}
	intent := packet.IntentHeader{Latency: 3, Reliability: 1}
	forcedPath := []string{"sender", "router_a", "router_b", "receiver"}

	type result struct {
		seq        int
		rerouted   bool
		path       []string
		sentAt     time.Time
		receivedAt time.Time
	}
	var (
		results   []result
		resultsMu sync.Mutex
		stopRecv  = make(chan struct{})
		recvDone  sync.WaitGroup
	)

	recvDone.Add(1)
	go func() {
		defer recvDone.Done()
		buf := make([]byte, 65535)
		for {
			select {
			case <-stopRecv:
				return
			default:
			}
			receiverConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			n, _, err := receiverConn.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			p, err := packet.DecodeWire(buf[:n])
			if err != nil {
				continue
			}
			resultsMu.Lock()
			results = append(results, result{
				rerouted:   p.Rerouted,
				path:       p.PlannedPath,
				receivedAt: time.Now(),
			})
			resultsMu.Unlock()
		}
	}()

	// Congestion steps: gradually increase load on B.
	// Map says 5% load. Reroute triggers at 30% divergence + 10pp floor.
	// So reroute triggers when actual load > 5 + max(5*0.3, 10) = 15%.
	congestionSteps := []struct {
		loadPct   float64
		latencyMs float64
		label     string
	}{
		{5.0, 1.0, "baseline (5% load)"},
		{10.0, 3.0, "light (10% load)"},
		{20.0, 8.0, "moderate (20% load)"},     // Below threshold: 20-5=15, but latency 8-1=7 > 5ms floor, (7/1)*100=700% > 30%
		{40.0, 15.0, "heavy (40% load)"},        // Clearly above threshold
		{80.0, 100.0, "severe (80% load)"},       // Way above threshold
		{95.0, 200.0, "critical (95% load)"},
	}

	var congestionStartAt time.Time
	sendInterval := 100 * time.Millisecond

	for stepIdx, step := range congestionSteps {
		// Update router_b's actual conditions (ShouldReroute checks router's own metrics).
		routerB.loadPct = step.loadPct
		routerB.latencyMs = step.latencyMs

		// Simulate gossip propagation: A learns about B's congestion.
		// B→receiver gets the full congestion penalty; B→C stays healthy.
		lossPct := 0.0
		if step.loadPct >= 40 {
			lossPct = step.loadPct * 0.3 // loss correlates with severe load
		}
		routerA.topoState.UpdateLocal("router_a", "router_b", step.latencyMs, step.loadPct, 0.0)
		routerB.topoState.UpdateLocal("router_b", "receiver", step.latencyMs, step.loadPct, lossPct)
		routerB.topoState.UpdateLocal("router_b", "router_c", 1.0, 5.0, 0.0)

		if stepIdx == 2 { // Mark the first "meaningful" congestion step.
			congestionStartAt = time.Now()
		}

		t.Logf("  Step %d: %s", stepIdx, step.label)

		// Send 5 packets at this congestion level.
		for i := 0; i < 5; i++ {
			p := packet.NewSmartPacket("receiver", intent, miniMap, []byte(fmt.Sprintf("grad_%d_%d", stepIdx, i)))
			p.UpdatePath(forcedPath)
			p.SourceNode = "sender"

			encoded, _ := p.EncodeWire()
			udpAddr, _ := net.ResolveUDPAddr("udp", routerA.listenAddr)
			conn, _ := net.DialUDP("udp", nil, udpAddr)
			conn.Write(encoded)
			conn.Close()

			time.Sleep(sendInterval)
		}
	}

	time.Sleep(500 * time.Millisecond)
	close(stopRecv)
	recvDone.Wait()

	// --- Analyze ---
	resultsMu.Lock()
	defer resultsMu.Unlock()

	t.Logf("Total packets received: %d / %d sent", len(results), len(congestionSteps)*5)

	firstRerouteIdx := -1
	for i, r := range results {
		if r.rerouted {
			firstRerouteIdx = i
			break
		}
	}

	if firstRerouteIdx == -1 {
		t.Fatal("FAIL: No packets were rerouted despite gradual congestion increase")
	}

	t.Logf("First reroute at packet #%d (0-indexed), path=%v", firstRerouteIdx, results[firstRerouteIdx].path)

	if !congestionStartAt.IsZero() {
		lag := results[firstRerouteIdx].receivedAt.Sub(congestionStartAt)
		t.Logf("Detection lag from moderate congestion: %v", lag)
	}

	// Summary table.
	normalCount := 0
	rerouteCount := 0
	for _, r := range results {
		if r.rerouted {
			rerouteCount++
		} else {
			normalCount++
		}
	}
	t.Logf("Summary: %d normal, %d rerouted out of %d received", normalCount, rerouteCount, len(results))
}
