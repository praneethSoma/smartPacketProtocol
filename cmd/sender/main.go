package main

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"time"

	"smartpacket/gossip"
	"smartpacket/packet"
)

// ──────────────────────────────────────────────────────────────
// Configurable defaults
// ──────────────────────────────────────────────────────────────

const (
	// DefaultGossipTimeoutMs is how long to wait for a gossip response.
	DefaultGossipTimeoutMs = 500

	// UDPMaxPayload is the maximum UDP datagram size.
	UDPMaxPayload = 65535
)

// ──────────────────────────────────────────────────────────────
// Configuration — loaded from JSON.
// ──────────────────────────────────────────────────────────────

// SenderConfig holds sender configuration.
type SenderConfig struct {
	NodeName             string `json:"NodeName"`             // Source node name (default: "sender")
	RouterAddr           string `json:"RouterAddr"`
	GossipAddr           string `json:"GossipAddr"`
	Destination          string `json:"Destination"`
	Payload              string `json:"Payload"`
	FirstRouter          string `json:"FirstRouter"`          // Name of the entry-point router (auto-detect from gossip if empty)
	IntentLatency        uint8  `json:"IntentLatency"`
	IntentReliability    uint8  `json:"IntentReliability"`
	GossipTimeoutMs      int    `json:"GossipTimeoutMs"`      // Timeout for gossip query (default: 500ms)
	GossipRetries        int    `json:"GossipRetries"`        // Number of gossip query retries (default: 5)
	GossipRetryBackoffMs int    `json:"GossipRetryBackoffMs"` // Backoff between retries in ms (default: 500)
}

// applyDefaults fills zero-valued fields with production defaults.
func (c *SenderConfig) applyDefaults() {
	if c.NodeName == "" {
		c.NodeName = "sender"
	}
	if c.GossipTimeoutMs == 0 {
		c.GossipTimeoutMs = DefaultGossipTimeoutMs
	}
	if c.GossipRetries == 0 {
		c.GossipRetries = 5
	}
	if c.GossipRetryBackoffMs == 0 {
		c.GossipRetryBackoffMs = 500
	}
}

func main() {
	logger := slog.Default()

	logger.Info("═══════════════════════════════════════")
	logger.Info("Smart Packet Protocol — Sender v2.0")
	logger.Info("═══════════════════════════════════════")

	// Default config.
	config := SenderConfig{
		RouterAddr:        "10.0.1.2:8001",
		GossipAddr:        "10.0.1.2:7001",
		Destination:       "receiver",
		Payload:           "player_position:x=100,y=200",
		IntentLatency:     3,
		IntentReliability: 1,
	}

	// Load config from file if provided.
	if len(os.Args) >= 2 {
		data, err := os.ReadFile(os.Args[1])
		if err != nil {
			logger.Error("failed to read config", "path", os.Args[1], "err", err)
			os.Exit(1)
		}
		if err := json.Unmarshal(data, &config); err != nil {
			logger.Error("failed to parse config", "path", os.Args[1], "err", err)
			os.Exit(1)
		}
	}
	config.applyDefaults()

	logger = slog.With("node", config.NodeName)

	var miniMap []packet.Link
	logger.Info("querying gossip", "addr", config.GossipAddr)

	gossipTimeout := time.Duration(config.GossipTimeoutMs) * time.Millisecond
	backoff := time.Duration(config.GossipRetryBackoffMs) * time.Millisecond

	for attempt := 1; attempt <= config.GossipRetries; attempt++ {
		gossipMap := queryGossipState(config.GossipAddr, gossipTimeout)
		if len(gossipMap) > 0 {
			miniMap = gossipMap
			logger.Info("got live topology", "links", len(miniMap), "attempt", attempt)
			for _, l := range miniMap {
				logger.Info("  link",
					"from", l.From, "to", l.To,
					"lat_ms", l.LatencyMs, "load", l.LoadPct, "loss", l.LossPct,
				)
			}
			break
		}
		logger.Warn("gossip attempt failed",
			"attempt", attempt,
			"max", config.GossipRetries,
			"retry_in", backoff,
		)
		if attempt < config.GossipRetries {
			time.Sleep(backoff)
		}
	}

	if len(miniMap) == 0 {
		logger.Error("gossip unavailable after all retries — cannot send",
			"retries", config.GossipRetries,
			"addr", config.GossipAddr,
		)
		os.Exit(1)
	}

	// ── Step 2: Inject sender→first-router link ──
	// Gossip only has router-to-router links. The sender needs to add
	// its own edge so Dijkstra can find a path from sender → destination.
	firstRouter := config.FirstRouter
	if firstRouter == "" {
		// Auto-detect: pick the router name that appears most as a From node.
		nameCount := make(map[string]int)
		for _, l := range miniMap {
			nameCount[l.From]++
		}
		bestCount := 0
		for name, count := range nameCount {
			if count > bestCount {
				bestCount = count
				firstRouter = name
			}
		}
		logger.Info("auto-detected first router", "name", firstRouter)
	}

	if firstRouter != "" {
		miniMap = append(miniMap, packet.Link{
			From: config.NodeName, To: firstRouter,
			LatencyMs: 0.1, LoadPct: 0, LossPct: 0,
		})
	}

	// ── Step 3: Build the smart packet ──
	intent := packet.IntentHeader{
		Reliability: config.IntentReliability,
		Latency:     config.IntentLatency,
		Ordering:    0,
		Priority:    config.IntentLatency,
	}

	p := packet.NewSmartPacket(config.Destination, intent, miniMap, []byte(config.Payload))

	// ── Step 4: Packet calculates its own path ──
	graph := packet.BuildGraph(miniMap, intent)
	path := packet.Dijkstra(graph, config.NodeName, config.Destination)
	if len(path) == 0 {
		logger.Error("no path found to destination",
			"from", config.NodeName,
			"to", config.Destination,
			"links", len(miniMap),
		)
		os.Exit(1)
	}
	p.UpdatePath(path)

	logger.Info("smart packet created",
		"dest", config.Destination,
		"intent", intent.String(),
		"path", path,
		"max_hops", p.MaxHops,
		"payload_len", len(p.Payload),
	)

	// ── Step 4: Transmit over UDP ──
	encoded, err := p.Encode()
	if err != nil {
		logger.Error("encode failed", "err", err)
		os.Exit(1)
	}

	logger.Info("packet encoded", "bytes", len(encoded))

	addr, err := net.ResolveUDPAddr("udp", config.RouterAddr)
	if err != nil {
		logger.Error("failed to resolve router addr", "addr", config.RouterAddr, "err", err)
		os.Exit(1)
	}

	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		logger.Error("dial failed", "addr", config.RouterAddr, "err", err)
		os.Exit(1)
	}
	defer conn.Close()

	if _, err := conn.Write(encoded); err != nil {
		logger.Error("write failed", "err", err)
		os.Exit(1)
	}

	logger.Info("packet sent",
		"time", time.Now().Format("15:04:05.000"),
		"target", config.RouterAddr,
	)
}

// queryGossipState listens for a gossip broadcast to get live topology.
// Uses gossip.GossipMessage and gossip.LinkState directly to avoid struct duplication.
func queryGossipState(gossipAddr string, timeout time.Duration) []packet.Link {
	listenAddr, err := net.ResolveUDPAddr("udp", "0.0.0.0:0")
	if err != nil {
		return nil
	}

	conn, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		return nil
	}
	defer conn.Close()

	// Nudge the gossip address to trigger a response.
	targetAddr, err := net.ResolveUDPAddr("udp", gossipAddr)
	if err != nil {
		return nil
	}
	if _, err := conn.WriteToUDP([]byte("MAP_REQUEST"), targetAddr); err != nil {
		return nil
	}

	// Wait for response with configurable timeout.
	conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, UDPMaxPayload)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return nil
	}

	// Decode gossip message using the gossip package types.
	var msg gossip.GossipMessage
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
