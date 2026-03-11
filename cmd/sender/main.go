package main

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"smartpacket/packet"
)

// SenderConfig holds sender configuration
type SenderConfig struct {
	RouterAddr        string `json:"RouterAddr"`
	GossipAddr        string `json:"GossipAddr"`
	Destination       string `json:"Destination"`
	Payload           string `json:"Payload"`
	IntentLatency     uint8  `json:"IntentLatency"`
	IntentReliability uint8  `json:"IntentReliability"`
}

// GossipMessage mirrors the gossip package structure for decoding
type GossipMessage struct {
	SenderName string
	States     []LinkState
	SentAt     time.Time
}

// LinkState mirrors gossip.LinkState
type LinkState struct {
	From      string
	To        string
	LatencyMs float64
	LoadPct   float64
	LossPct   float64
	Timestamp time.Time
	Sequence  uint64
}

func main() {
	fmt.Println("═══════════════════════════════════════")
	fmt.Println("  Smart Packet Protocol — Sender v1.0")
	fmt.Println("═══════════════════════════════════════")

	// Default config
	config := SenderConfig{
		RouterAddr:        "10.0.1.2:8001",
		GossipAddr:        "10.0.1.2:7001",
		Destination:       "receiver",
		Payload:           "player_position:x=100,y=200",
		IntentLatency:     3,
		IntentReliability: 1,
	}

	// Load config from file if provided
	if len(os.Args) >= 2 {
		data, err := os.ReadFile(os.Args[1])
		if err == nil {
			json.Unmarshal(data, &config)
		}
	}

	// ── Step 1: Try to get fresh topology from gossip ──
	var miniMap []packet.Link
	fmt.Printf("[sender] Querying gossip from %s...\n", config.GossipAddr)

	gossipMap := queryGossipState(config.GossipAddr)
	if len(gossipMap) > 0 {
		miniMap = gossipMap
		fmt.Printf("[sender] Got live topology: %d links\n", len(miniMap))
		for _, l := range miniMap {
			fmt.Printf("[sender]   %s → %s  lat=%.1fms load=%.0f%% loss=%.0f%%\n",
				l.From, l.To, l.LatencyMs, l.LoadPct, l.LossPct)
		}
	} else {
		// Fallback: hardcoded map (backward compatibility)
		fmt.Println("[sender] No gossip data — using fallback map")
		miniMap = []packet.Link{
			{From: "sender", To: "router_a", LatencyMs: 1, LoadPct: 5, LossPct: 0},
			{From: "router_a", To: "router_b", LatencyMs: 5, LoadPct: 90, LossPct: 8},
			{From: "router_a", To: "router_c", LatencyMs: 5, LoadPct: 10, LossPct: 0},
			{From: "router_b", To: "receiver", LatencyMs: 100, LoadPct: 90, LossPct: 8},
			{From: "router_c", To: "receiver", LatencyMs: 5, LoadPct: 10, LossPct: 0},
		}
	}

	// ── Step 2: Build the smart packet ──
	intent := packet.IntentHeader{
		Reliability: config.IntentReliability,
		Latency:     config.IntentLatency,
		Ordering:    0,
		Priority:    config.IntentLatency,
	}

	p := packet.NewSmartPacket(config.Destination, intent, miniMap, []byte(config.Payload))

	// ── Step 3: Packet calculates its own path ──
	graph := packet.BuildGraph(miniMap, intent)
	path := packet.Dijkstra(graph, "sender", config.Destination)
	p.UpdatePath(path)

	fmt.Printf("\n[sender] ── Smart Packet Created ──────\n")
	fmt.Printf("[sender] Destination: %s\n", config.Destination)
	fmt.Printf("[sender] Intent:      latency=%d reliability=%d\n", intent.Latency, intent.Reliability)
	fmt.Printf("[sender] Chosen path: %v\n", path)
	fmt.Printf("[sender] MaxHops:     %d\n", p.MaxHops)
	fmt.Printf("[sender] Payload:     %s\n", string(p.Payload))

	// ── Step 4: Transmit over UDP ──
	encoded, err := p.Encode()
	if err != nil {
		fmt.Printf("[sender] Encode error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("[sender] Packet size: %d bytes\n", len(encoded))

	addr, _ := net.ResolveUDPAddr("udp", config.RouterAddr)
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		fmt.Printf("[sender] Dial error: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	conn.Write(encoded)
	fmt.Printf("[sender] Packet sent at %s to %s\n", time.Now().Format("15:04:05.000"), config.RouterAddr)
}

// queryGossipState listens for a gossip broadcast to get live topology
func queryGossipState(gossipAddr string) []packet.Link {
	listenAddr, err := net.ResolveUDPAddr("udp", "0.0.0.0:0")
	if err != nil {
		return nil
	}

	conn, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		return nil
	}
	defer conn.Close()

	// Nudge the gossip address
	targetAddr, _ := net.ResolveUDPAddr("udp", gossipAddr)
	conn.WriteToUDP([]byte("MAP_REQUEST"), targetAddr)

	// Wait for response
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 65535)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return nil
	}

	// Decode gossip message
	var msg GossipMessage
	dec := gob.NewDecoder(bytes.NewBuffer(buf[:n]))
	if err := dec.Decode(&msg); err != nil {
		return nil
	}

	links := make([]packet.Link, len(msg.States))
	for i, s := range msg.States {
		links[i] = packet.Link{
			From:      s.From,
			To:        s.To,
			LatencyMs: s.LatencyMs,
			LoadPct:   s.LoadPct,
			LossPct:   s.LossPct,
		}
	}
	return links
}
