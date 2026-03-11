package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"smartpacket/gossip"
	"smartpacket/metrics"
	"smartpacket/packet"
)

// NeighborConfig holds the multi-port address for a neighbor
type NeighborConfig struct {
	DataAddr   string `json:"DataAddr"`
	GossipAddr string `json:"GossipAddr"`
	ProbeAddr  string `json:"ProbeAddr"`
}

// NodeConfig is the router configuration loaded from JSON
type NodeConfig struct {
	Name             string                    `json:"Name"`
	ListenAddr       string                    `json:"ListenAddr"`
	GossipPort       int                       `json:"GossipPort"`
	ProbeIntervalMs  int                       `json:"ProbeIntervalMs"`
	GossipIntervalMs int                       `json:"GossipIntervalMs"`
	Neighbors        map[string]NeighborConfig `json:"Neighbors"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: router <config.json>")
		os.Exit(1)
	}

	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Printf("Error reading config: %v\n", err)
		os.Exit(1)
	}

	var config NodeConfig
	if err := json.Unmarshal(data, &config); err != nil {
		fmt.Printf("Error parsing config: %v\n", err)
		os.Exit(1)
	}

	// Set defaults
	if config.ProbeIntervalMs == 0 {
		config.ProbeIntervalMs = 100
	}
	if config.GossipIntervalMs == 0 {
		config.GossipIntervalMs = 50
	}
	if config.GossipPort == 0 {
		config.GossipPort = 7000
	}

	fmt.Printf("[%s] ═══════════════════════════════════════\n", config.Name)
	fmt.Printf("[%s] Smart Packet Protocol Router v1.0\n", config.Name)
	fmt.Printf("[%s] ═══════════════════════════════════════\n", config.Name)

	// ──────────────────────────────────────
	// Phase 1: Start metrics collector
	// ──────────────────────────────────────
	probeNeighbors := make(map[string]string)
	for name, neighbor := range config.Neighbors {
		probeNeighbors[name] = neighbor.ProbeAddr
	}

	probeListenAddr := fmt.Sprintf("0.0.0.0:%d", config.GossipPort-1000) // probe = gossip-1000
	metricsCollector, err := metrics.NewCollector(metrics.CollectorConfig{
		NodeName:    config.Name,
		ListenAddr:  probeListenAddr,
		Neighbors:   probeNeighbors,
		IntervalMs:  config.ProbeIntervalMs,
		ProbeConfig: metrics.DefaultProbeConfig(),
	})
	if err != nil {
		fmt.Printf("[%s] Warning: metrics collector failed: %v (using defaults)\n", config.Name, err)
		metricsCollector = nil
	} else {
		metricsCollector.Start()
		fmt.Printf("[%s] Metrics collector active (probe=%s)\n", config.Name, probeListenAddr)
	}

	// ──────────────────────────────────────
	// Phase 2: Start gossip protocol
	// ──────────────────────────────────────
	topoState := gossip.NewTopologyState()
	gossipNeighbors := make(map[string]string)
	for name, neighbor := range config.Neighbors {
		gossipNeighbors[name] = neighbor.GossipAddr
	}

	gossipListenAddr := fmt.Sprintf("0.0.0.0:%d", config.GossipPort)
	gossipNode, err := gossip.NewGossipNode(
		gossip.GossipConfig{
			NodeName:        config.Name,
			ListenAddr:      gossipListenAddr,
			Neighbors:       gossipNeighbors,
			IntervalMs:      config.GossipIntervalMs,
			MaxStalenessMs:  1000,
			WarnStalenessMs: 300,
		},
		topoState,
		metricsCollector,
	)
	if err != nil {
		fmt.Printf("[%s] Warning: gossip failed: %v\n", config.Name, err)
	} else {
		gossipNode.Start()
		fmt.Printf("[%s] Gossip active on %s\n", config.Name, gossipListenAddr)
	}

	// ──────────────────────────────────────
	// Data plane: listen for smart packets
	// ──────────────────────────────────────
	fmt.Printf("[%s] Listening for data on %s\n", config.Name, config.ListenAddr)

	addr, _ := net.ResolveUDPAddr("udp", config.ListenAddr)
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		fmt.Printf("[%s] Failed to listen: %v\n", config.Name, err)
		os.Exit(1)
	}
	defer conn.Close()

	buf := make([]byte, 65535)

	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}

		p, err := packet.Decode(buf[:n])
		if err != nil {
			fmt.Printf("[%s] Decode error: %v\n", config.Name, err)
			continue
		}

		fmt.Printf("\n[%s] ── Packet received ──────────────\n", config.Name)
		fmt.Printf("[%s] Destination: %s\n", config.Name, p.Destination)
		fmt.Printf("[%s] Planned path: %v\n", config.Name, p.PlannedPath)
		fmt.Printf("[%s] Hop %d/%d\n", config.Name, p.HopCount, p.MaxHops)

		// ── Step 1: Check TTL ──
		if p.IsExpired() {
			fmt.Printf("[%s] ✗ Packet expired (TTL exceeded). Dropping.\n", config.Name)
			continue
		}

		// ── Step 2: Stamp real congestion from metrics ──
		currentLoad := 0.0
		currentLatency := 0.0
		if metricsCollector != nil {
			m := metricsCollector.GetMetrics()
			currentLoad = m.LoadPct
			// Use average neighbor latency as representative
			for _, pr := range m.Neighbors {
				currentLatency = pr.LatencyMs
				break
			}
		}
		p.LogHop(config.Name, currentLoad, currentLatency)
		fmt.Printf("[%s] Stamped: load=%.1f%% latency=%.2fms\n", config.Name, currentLoad, currentLatency)

		// ── Step 3: Check for reroute ──
		if p.ShouldReroute(config.Name, currentLoad, currentLatency, packet.RerouteThreshold) {
			fmt.Printf("[%s] ⚡ Conditions diverged — rerouting packet!\n", config.Name)

			// Get fresh topology from gossip
			freshLinks := topoState.GetFreshLinks(500 * time.Millisecond)
			if len(freshLinks) > 0 {
				p.Reroute(config.Name, freshLinks)
				fmt.Printf("[%s] New path: %v\n", config.Name, p.PlannedPath)
			} else {
				fmt.Printf("[%s] No fresh gossip data available for reroute\n", config.Name)
			}
		}

		// ── Step 4: Determine next hop ──
		nextHop := p.NextHop()

		// ── Step 5: Loop detection ──
		if nextHop != "" && p.DetectLoop(nextHop) {
			fmt.Printf("[%s] ⚠ Loop detected! %s already visited. Force-forwarding.\n", config.Name, nextHop)
			nextHop = p.ForceForward(config.Name)
			p.Degraded = true
			fmt.Printf("[%s] Force-forward to: %s\n", config.Name, nextHop)
		}

		if nextHop == "" {
			nextHop = p.Destination
		}

		// ── Step 6: Find neighbor address and forward ──
		neighbor, ok := config.Neighbors[nextHop]
		if !ok {
			fmt.Printf("[%s] ✗ Cannot find neighbor: %s\n", config.Name, nextHop)
			continue
		}

		encoded, err := p.Encode()
		if err != nil {
			fmt.Printf("[%s] Encode error: %v\n", config.Name, err)
			continue
		}

		udpAddr, _ := net.ResolveUDPAddr("udp", neighbor.DataAddr)
		out, err := net.DialUDP("udp", nil, udpAddr)
		if err != nil {
			fmt.Printf("[%s] Dial error: %v\n", config.Name, err)
			continue
		}
		out.Write(encoded)
		out.Close()
		fmt.Printf("[%s] → Forwarded to %s (%s)\n", config.Name, nextHop, neighbor.DataAddr)
	}
}
