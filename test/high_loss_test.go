//go:build integration

package test

import (
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"smartpacket/packet"
)

// lossyRouter wraps miniRouter with simulated packet loss.
// On each incoming packet, it drops it with probability lossPct/100.
type lossyRouter struct {
	name       string
	listenAddr string
	conn       *net.UDPConn
	neighbors  map[string]string
	stopCh     chan struct{}
	wg         sync.WaitGroup

	loadPct   float64
	latencyMs float64
	lossPct   float64 // 0–100, probability of dropping each packet

	dropped atomic.Int64
	forwarded atomic.Int64
}

func newLossyRouter(t *testing.T, name string, lossPct float64) *lossyRouter {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return &lossyRouter{
		name:       name,
		listenAddr: conn.LocalAddr().String(),
		conn:       conn,
		neighbors:  make(map[string]string),
		stopCh:     make(chan struct{}),
		loadPct:    5.0,
		latencyMs:  1.0,
		lossPct:    lossPct,
	}
}

func (r *lossyRouter) start(t *testing.T) {
	t.Helper()
	r.wg.Add(1)
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	go func() {
		defer r.wg.Done()
		buf := make([]byte, 65535)
		for {
			select {
			case <-r.stopCh:
				return
			default:
			}

			r.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			n, _, err := r.conn.ReadFromUDP(buf)
			if err != nil {
				continue
			}

			// Simulate packet loss.
			if rng.Float64()*100 < r.lossPct {
				r.dropped.Add(1)
				continue
			}

			p, err := packet.DecodeWire(buf[:n])
			if err != nil {
				continue
			}
			if p.IsExpired() {
				continue
			}

			p.LogHop(r.name, r.loadPct, r.latencyMs)

			nextHop := p.NextHop()
			if nextHop == "" {
				nextHop = p.Destination
			}

			neighborAddr, ok := r.neighbors[nextHop]
			if !ok {
				continue
			}

			encoded, err := p.EncodeWire()
			if err != nil {
				continue
			}

			udpAddr, _ := net.ResolveUDPAddr("udp", neighborAddr)
			fwdConn, err := net.DialUDP("udp", nil, udpAddr)
			if err != nil {
				continue
			}
			fwdConn.Write(encoded)
			fwdConn.Close()
			r.forwarded.Add(1)
		}
	}()
}

func (r *lossyRouter) stop() {
	close(r.stopCh)
	r.conn.Close()
	r.wg.Wait()
}

// ──────────────────────────────────────────────────────────────
// Test: High Packet Loss Delivery Rate
//
// Sends N packets through a 3-router chain where each router
// drops packets at the configured loss rate. Measures actual
// delivery rate vs theoretical expectation.
//
// Theoretical delivery probability for a chain of k routers
// each with independent loss rate p:
//   P(delivery) = (1 - p)^k
//
// At 30% loss × 3 hops: (0.7)^3 = 34.3%
// At 50% loss × 3 hops: (0.5)^3 = 12.5%
// At 70% loss × 3 hops: (0.3)^3 =  2.7%
// ──────────────────────────────────────────────────────────────

func TestHighPacketLossDelivery(t *testing.T) {
	scenarios := []struct {
		name    string
		lossPct float64
		packets int
	}{
		{"30pct_loss", 30.0, 200},
		{"50pct_loss", 50.0, 300},
		{"70pct_loss", 70.0, 500},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			// Set up receiver.
			receiverAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
			receiverConn, _ := net.ListenUDP("udp", receiverAddr)
			defer receiverConn.Close()
			receiverListenAddr := receiverConn.LocalAddr().String()

			// 3-router chain, each with the same loss rate.
			rA := newLossyRouter(t, "router_a", sc.lossPct)
			rB := newLossyRouter(t, "router_b", sc.lossPct)
			rC := newLossyRouter(t, "router_c", sc.lossPct)

			rA.neighbors["router_b"] = rB.listenAddr
			rB.neighbors["router_c"] = rC.listenAddr
			rC.neighbors["receiver"] = receiverListenAddr

			rA.start(t)
			rB.start(t)
			rC.start(t)
			defer rA.stop()
			defer rB.stop()
			defer rC.stop()

			miniMap := []packet.Link{
				{From: "sender", To: "router_a", LatencyMs: 1, LoadPct: 5, LossPct: sc.lossPct},
				{From: "router_a", To: "router_b", LatencyMs: 1, LoadPct: 5, LossPct: sc.lossPct},
				{From: "router_b", To: "router_c", LatencyMs: 1, LoadPct: 5, LossPct: sc.lossPct},
				{From: "router_c", To: "receiver", LatencyMs: 1, LoadPct: 5, LossPct: sc.lossPct},
			}
			intent := packet.IntentHeader{Latency: 3, Reliability: 2} // max reliability
			graph := packet.BuildGraph(miniMap, intent)
			path := packet.Dijkstra(graph, "sender", "receiver")

			// Send packets.
			for i := 0; i < sc.packets; i++ {
				payload := fmt.Sprintf("loss_test_%d", i)
				p := packet.NewSmartPacket("receiver", intent, miniMap, []byte(payload))
				p.UpdatePath(path)
				p.SourceNode = "sender"

				encoded, err := p.EncodeWire()
				if err != nil {
					t.Fatalf("encode: %v", err)
				}

				udpAddr, _ := net.ResolveUDPAddr("udp", rA.listenAddr)
				sendConn, err := net.DialUDP("udp", nil, udpAddr)
				if err != nil {
					t.Fatalf("dial: %v", err)
				}
				sendConn.Write(encoded)
				sendConn.Close()

				// Small delay to avoid overwhelming UDP buffers.
				time.Sleep(1 * time.Millisecond)
			}

			// Collect delivered packets.
			delivered := 0
			receiverConn.SetReadDeadline(time.Now().Add(3 * time.Second))
			buf := make([]byte, 65535)
			for {
				n, _, err := receiverConn.ReadFromUDP(buf)
				if err != nil {
					break // timeout = done collecting
				}
				_, err = packet.DecodeWire(buf[:n])
				if err != nil {
					continue
				}
				delivered++
			}

			sent := sc.packets
			deliveryRate := float64(delivered) / float64(sent) * 100.0
			survivalProb := 1.0
			for i := 0; i < 3; i++ {
				survivalProb *= (1.0 - sc.lossPct/100.0)
			}
			theoreticalRate := survivalProb * 100.0

			t.Logf("═══════════════════════════════════════════════════")
			t.Logf("  Loss Rate Per Hop : %.0f%%", sc.lossPct)
			t.Logf("  Packets Sent      : %d", sent)
			t.Logf("  Packets Delivered : %d", delivered)
			t.Logf("  Delivery Rate     : %.1f%%", deliveryRate)
			t.Logf("  Theoretical Rate  : %.1f%% ((1-%.2f)^3)", theoreticalRate, sc.lossPct/100.0)
			t.Logf("  Router A drops    : %d fwd: %d", rA.dropped.Load(), rA.forwarded.Load())
			t.Logf("  Router B drops    : %d fwd: %d", rB.dropped.Load(), rB.forwarded.Load())
			t.Logf("  Router C drops    : %d fwd: %d", rC.dropped.Load(), rC.forwarded.Load())
			t.Logf("═══════════════════════════════════════════════════")

			// Sanity: at least some packets should be delivered (statistical, not strict).
			if sc.lossPct <= 50 && delivered == 0 {
				t.Errorf("Expected at least some deliveries at %.0f%% loss, got 0", sc.lossPct)
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────
// Test: Reliability-Aware Routing Avoids Lossy Paths
//
// Two paths exist: a lossy direct path and a clean alternate.
// With high reliability intent, Dijkstra should prefer the
// clean path. We verify delivery rate is much higher than
// if the lossy path were used.
// ──────────────────────────────────────────────────────────────

func TestReliabilityAwareRoutingUnderLoss(t *testing.T) {
	receiverAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	receiverConn, _ := net.ListenUDP("udp", receiverAddr)
	defer receiverConn.Close()
	receiverListenAddr := receiverConn.LocalAddr().String()

	// Path 1: A → B (50% loss) → receiver
	// Path 2: A → C (0% loss) → receiver
	rA := newLossyRouter(t, "router_a", 0)
	rB := newLossyRouter(t, "router_b", 50) // lossy path
	rC := newLossyRouter(t, "router_c", 0)  // clean path

	rA.neighbors["router_b"] = rB.listenAddr
	rA.neighbors["router_c"] = rC.listenAddr
	rB.neighbors["receiver"] = receiverListenAddr
	rC.neighbors["receiver"] = receiverListenAddr

	rA.start(t)
	rB.start(t)
	rC.start(t)
	defer rA.stop()
	defer rB.stop()
	defer rC.stop()

	miniMap := []packet.Link{
		{From: "sender", To: "router_a", LatencyMs: 1, LoadPct: 5, LossPct: 0},
		{From: "router_a", To: "router_b", LatencyMs: 1, LoadPct: 5, LossPct: 50},
		{From: "router_a", To: "router_c", LatencyMs: 5, LoadPct: 5, LossPct: 0},
		{From: "router_b", To: "receiver", LatencyMs: 1, LoadPct: 5, LossPct: 50},
		{From: "router_c", To: "receiver", LatencyMs: 5, LoadPct: 5, LossPct: 0},
	}

	// High reliability intent — Dijkstra should route through C.
	intent := packet.IntentHeader{Latency: 1, Reliability: 2}
	graph := packet.BuildGraph(miniMap, intent)
	path := packet.Dijkstra(graph, "sender", "receiver")

	t.Logf("Dijkstra chose path: %v", path)

	numPackets := 100
	for i := 0; i < numPackets; i++ {
		p := packet.NewSmartPacket("receiver", intent, miniMap, []byte(fmt.Sprintf("rel_%d", i)))
		p.UpdatePath(path)
		p.SourceNode = "sender"

		encoded, _ := p.EncodeWire()
		udpAddr, _ := net.ResolveUDPAddr("udp", rA.listenAddr)
		sendConn, _ := net.DialUDP("udp", nil, udpAddr)
		sendConn.Write(encoded)
		sendConn.Close()
		time.Sleep(1 * time.Millisecond)
	}

	delivered := 0
	receiverConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 65535)
	for {
		n, _, err := receiverConn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if _, err := packet.DecodeWire(buf[:n]); err == nil {
			delivered++
		}
	}

	deliveryRate := float64(delivered) / float64(numPackets) * 100.0

	t.Logf("═══════════════════════════════════════════════════")
	t.Logf("  Reliability-Aware Routing Test")
	t.Logf("  Path chosen       : %v", path)
	t.Logf("  Packets Sent      : %d", numPackets)
	t.Logf("  Packets Delivered : %d", delivered)
	t.Logf("  Delivery Rate     : %.1f%%", deliveryRate)
	t.Logf("  Router B (lossy) fwd: %d drops: %d", rB.forwarded.Load(), rB.dropped.Load())
	t.Logf("  Router C (clean) fwd: %d drops: %d", rC.forwarded.Load(), rC.dropped.Load())
	t.Logf("═══════════════════════════════════════════════════")

	// If Dijkstra correctly avoids the lossy path, delivery should be high.
	if deliveryRate < 80 {
		t.Errorf("Reliability-aware routing should achieve >80%% delivery, got %.1f%%", deliveryRate)
	}
}
