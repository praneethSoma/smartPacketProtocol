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
// Test: Path Flapping / Oscillation
//
// Topology:
//   sender → router_a → router_b → receiver   (primary path)
//                      ↘ router_c → receiver   (alternate path)
//
// Congestion on router_b alternates: slow 5s, clean 5s, slow 5s.
// We measure whether SPP ping-pongs between paths (flapping) and
// how many path switches occur.
// ──────────────────────────────────────────────────────────────

func TestPathFlapping(t *testing.T) {
	// --- Receiver ---
	receiverAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	receiverConn, _ := net.ListenUDP("udp", receiverAddr)
	defer receiverConn.Close()
	receiverListenAddr := receiverConn.LocalAddr().String()

	// --- Routers ---
	routerA := newMiniRouter(t, "router_a", 5.0, 1.0)
	routerB := newMiniRouter(t, "router_b", 5.0, 1.0)
	routerC := newMiniRouter(t, "router_c", 5.0, 1.0)

	// Enable 2-second reroute cooldown on router_b (anti-flap).
	routerB.rerouteCooldown = 2 * time.Second

	// Wire topology: A→B, A→C, B→receiver, B→C, C→receiver
	routerA.neighbors["router_b"] = routerB.listenAddr
	routerA.neighbors["router_c"] = routerC.listenAddr
	routerB.neighbors["receiver"] = receiverListenAddr
	routerB.neighbors["router_c"] = routerC.listenAddr // B can reroute to C
	routerC.neighbors["receiver"] = receiverListenAddr

	// Pre-seed topology on all routers (both paths healthy).
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
	miniMap := []packet.Link{
		{From: "sender", To: "router_a", LatencyMs: 1, LoadPct: 5},
		{From: "router_a", To: "router_b", LatencyMs: 1, LoadPct: 5},
		{From: "router_a", To: "router_c", LatencyMs: 1, LoadPct: 5},
		{From: "router_b", To: "receiver", LatencyMs: 1, LoadPct: 5},
		{From: "router_b", To: "router_c", LatencyMs: 1, LoadPct: 5},
		{From: "router_c", To: "receiver", LatencyMs: 1, LoadPct: 5},
	}
	intent := packet.IntentHeader{Latency: 3, Reliability: 1}
	primaryPath := []string{"sender", "router_a", "router_b", "receiver"}

	// --- Receiver goroutine ---
	type receivedPacket struct {
		seqNum     int32
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

	// --- Send packets continuously while toggling congestion ---
	var packetSeq atomic.Int32
	sendInterval := 200 * time.Millisecond

	sendPacket := func() {
		seq := packetSeq.Add(1)
		p := packet.NewSmartPacket("receiver", intent, miniMap, []byte(fmt.Sprintf("flap_%d", seq)))
		p.UpdatePath(primaryPath)
		p.SourceNode = "sender"

		encoded, err := p.EncodeWire()
		if err != nil {
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

	injectCongestion := func() {
		routerB.loadPct = 95.0
		routerB.latencyMs = 200.0
		// Update gossip so rerouting has fresh alternate path info
		for _, r := range []*miniRouter{routerA, routerB, routerC} {
			r.topoState.UpdateLocal("router_b", "receiver", 200.0, 95.0, 5.0)
			r.topoState.UpdateLocal("router_c", "receiver", 1.0, 5.0, 0.0)
		}
	}

	clearCongestion := func() {
		routerB.loadPct = 5.0
		routerB.latencyMs = 1.0
		// Update gossip to reflect B is healthy again
		for _, r := range []*miniRouter{routerA, routerB, routerC} {
			r.topoState.UpdateLocal("router_b", "receiver", 1.0, 5.0, 0.0)
		}
	}

	// Oscillation phases: slow → clean → slow → clean → slow
	phases := []struct {
		label    string
		duration time.Duration
		congested bool
	}{
		{"Phase 1: baseline (healthy)", 2 * time.Second, false},
		{"Phase 2: congestion ON", 5 * time.Second, true},
		{"Phase 3: congestion OFF", 5 * time.Second, false},
		{"Phase 4: congestion ON", 5 * time.Second, true},
		{"Phase 5: congestion OFF (settle)", 3 * time.Second, false},
	}

	for _, phase := range phases {
		t.Logf("%s", phase.label)
		if phase.congested {
			injectCongestion()
		} else {
			clearCongestion()
		}

		deadline := time.Now().Add(phase.duration)
		for time.Now().Before(deadline) {
			sendPacket()
			time.Sleep(sendInterval)
		}
	}

	// Drain
	time.Sleep(500 * time.Millisecond)
	close(stopRecv)
	recvDone.Wait()

	// --- Analyze ---
	receivedMu.Lock()
	defer receivedMu.Unlock()

	totalSent := int(packetSeq.Load())
	t.Logf("Total packets: %d sent, %d received", totalSent, len(received))

	// Classify each packet's path and count transitions
	type pathRecord struct {
		viaB       bool
		rerouted   bool
		receivedAt time.Time
	}
	var records []pathRecord

	for _, rp := range received {
		// If router_c appears in the planned path, the packet was rerouted via C.
		viaC := false
		for _, hop := range rp.pkt.PlannedPath {
			if hop == "router_c" {
				viaC = true
				break
			}
		}
		records = append(records, pathRecord{
			viaB:       !viaC, // primary path (no reroute through C)
			rerouted:   rp.pkt.Rerouted,
			receivedAt: rp.receivedAt,
		})
	}

	// Count path switches (flaps)
	flaps := 0
	viaBCount := 0
	viaCCount := 0
	var flapTimes []time.Time

	for i, rec := range records {
		if rec.viaB {
			viaBCount++
		} else {
			viaCCount++
		}
		if i > 0 && records[i-1].viaB != rec.viaB {
			flaps++
			flapTimes = append(flapTimes, rec.receivedAt)
		}
	}

	t.Logf("Path usage: %d via B (primary), %d via C (alternate)", viaBCount, viaCCount)
	t.Logf("Path switches (flaps): %d", flaps)

	for i, ft := range flapTimes {
		direction := "B→C"
		if i < len(records) {
			// Find the record at this flap time
			for _, rec := range records {
				if rec.receivedAt == ft && rec.viaB {
					direction = "C→B"
					break
				}
			}
		}
		if i == 0 {
			t.Logf("  Flap %d: %s at %v", i+1, direction, ft.Format("15:04:05.000"))
		} else {
			delta := ft.Sub(flapTimes[i-1])
			t.Logf("  Flap %d: %s at %v (%.1fs after previous)", i+1, direction, ft.Format("15:04:05.000"), delta.Seconds())
		}
	}

	// --- Assertions ---

	// We expect at least some rerouted packets (congestion was injected).
	reroutedCount := 0
	for _, rec := range records {
		if rec.rerouted {
			reroutedCount++
		}
	}
	if reroutedCount == 0 {
		t.Fatal("FAIL: No packets were rerouted despite congestion injection")
	}
	t.Logf("Rerouted packets: %d / %d", reroutedCount, len(records))

	// With cooldown + hysteresis, we expect significantly fewer reroutes
	// than without damping (was 50 reroutes out of 100 packets).
	// The cooldown suppresses rapid re-rerouting within the 2s window.
	t.Logf("\n--- Flapping Analysis ---")
	t.Logf("Reroute ratio: %d/%d (%.0f%%)", reroutedCount, len(records), float64(reroutedCount)/float64(len(records))*100)

	if reroutedCount <= 15 {
		t.Logf("RESULT: Anti-flap working. Cooldown reduced reroutes to %d (was 50 without damping).", reroutedCount)
	} else if reroutedCount <= 30 {
		t.Logf("RESULT: Partial damping. %d reroutes — cooldown helps but oscillation still visible.", reroutedCount)
	} else {
		t.Logf("RESULT: High flapping (%d reroutes). Anti-flap measures may need tuning.", reroutedCount)
	}

	// Inter-flap timing analysis
	if len(flapTimes) >= 2 {
		var minGap, maxGap time.Duration
		for i := 1; i < len(flapTimes); i++ {
			gap := flapTimes[i].Sub(flapTimes[i-1])
			if i == 1 || gap < minGap {
				minGap = gap
			}
			if gap > maxGap {
				maxGap = gap
			}
		}
		t.Logf("Inter-flap timing: min=%.1fs, max=%.1fs", minGap.Seconds(), maxGap.Seconds())

		// If flaps happen faster than every 1s, that's rapid oscillation.
		if minGap < 1*time.Second {
			t.Logf("WARNING: Rapid oscillation detected (gap < 1s). Consider adding hysteresis/damping.")
		}
	}
}
