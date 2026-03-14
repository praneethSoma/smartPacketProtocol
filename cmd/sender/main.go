package main

import (
	"bytes"
	"encoding/binary"
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
	NodeName             string   `json:"NodeName"`             // Source node name (default: "sender")
	RouterAddr           string   `json:"RouterAddr"`
	GossipAddr           string   `json:"GossipAddr"`
	Destination          string   `json:"Destination"`
	Payload              string   `json:"Payload"`
	FirstRouter          string   `json:"FirstRouter"`          // Name of the entry-point router (auto-detect from gossip if empty)
	IntentLatency        uint8    `json:"IntentLatency"`
	IntentReliability    uint8    `json:"IntentReliability"`
	GossipTimeoutMs      int      `json:"GossipTimeoutMs"`      // Timeout for gossip query (default: 500ms)
	GossipRetries        int      `json:"GossipRetries"`        // Number of gossip query retries (default: 5)
	GossipRetryBackoffMs int      `json:"GossipRetryBackoffMs"` // Backoff between retries in ms (default: 500)
	PacketCount          int      `json:"PacketCount"`          // Number of packets to send (default: 1)
	IntervalMs           int      `json:"IntervalMs"`           // Delay between packets in ms (default: 0)
	StaticPath           []string          `json:"StaticPath"`           // If set, skip gossip and use this exact path (baseline mode)
	RawMode              bool              `json:"RawMode"`              // If true, send raw 8-byte timestamp (no SPP framing) for overhead measurement
	LightMode            bool              `json:"LightMode"`            // If true, don't embed MiniMap — routers use local gossip for rerouting
	NodeIDMap            map[string]uint16 `json:"NodeIDMap"`            // Compact node ID mapping for wire compression
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
	if c.PacketCount == 0 {
		c.PacketCount = 1
	}
}

func main() {
	logger := slog.Default()

	logger.Info("═══════════════════════════════════════")
	logger.Info("Smart Packet Protocol — Sender v2.1")
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

	// ── Build intent header (used in both static and dynamic modes) ──
	intent := packet.IntentHeader{
		Reliability: config.IntentReliability,
		Latency:     config.IntentLatency,
		Ordering:    0,
		Priority:    config.IntentLatency,
	}

	var miniMap []packet.Link
	var path []string

	if config.RawMode {
		// ── Raw probe mode: skip all SPP path computation ──
		logger.Info("raw probe mode — SPP skipped, will send 8-byte timestamps")
	} else if len(config.StaticPath) > 0 {
		// ── Static baseline mode: skip gossip, use hardcoded path ──
		// Empty MiniMap ensures routers will NOT reroute this packet,
		// so it always follows the fixed path regardless of congestion.
		path = config.StaticPath
		logger.Info("static baseline mode — gossip skipped", "path", path)
	} else {
		// ── Dynamic mode: query gossip and run Dijkstra ──
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

		// ── Inject sender→first-router link ──
		firstRouter := config.FirstRouter
		if firstRouter == "" {
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

		// ── Compute path via Dijkstra ──
		graph := packet.BuildGraph(miniMap, intent)
		path = packet.Dijkstra(graph, config.NodeName, config.Destination)
		if len(path) == 0 {
			logger.Error("no path found to destination",
				"from", config.NodeName,
				"to", config.Destination,
				"links", len(miniMap),
			)
			os.Exit(1)
		}
	}

	// ── Step 5: Resolve target address ──
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

	// ── Step 6: Send packet(s) ──
	interval := time.Duration(config.IntervalMs) * time.Millisecond

	var nodeIDTable *packet.NodeIDTable
	if len(config.NodeIDMap) > 0 {
		nodeIDTable = packet.NewNodeIDTable(config.NodeIDMap)
	}

	for i := 0; i < config.PacketCount; i++ {
		if config.RawMode {
			var buf [8]byte
			binary.BigEndian.PutUint64(buf[:], uint64(time.Now().UnixNano()))
			if _, err := conn.Write(buf[:]); err != nil {
				logger.Error("raw probe write failed", "err", err)
				os.Exit(1)
			}
			logger.Info("raw probe sent", "packet", i+1, "of", config.PacketCount)
			if i < config.PacketCount-1 && interval > 0 {
				time.Sleep(interval)
			}
			continue
		}

		var p *packet.SmartPacket
		if config.LightMode {
			p = packet.NewLightPacket(config.Destination, intent, path, []byte(config.Payload))
		} else {
			p = packet.NewSmartPacket(config.Destination, intent, miniMap, []byte(config.Payload))
			p.UpdatePath(path)
		}
		p.CreatedAtNs = time.Now().UnixNano()

		var encoded []byte
		var encErr error
		if nodeIDTable != nil {
			encoded, encErr = p.EncodeWireWithIDs(nodeIDTable)
		} else {
			encoded, encErr = p.EncodeWire()
		}
		if encErr != nil {
			logger.Error("encode failed", "err", encErr, "packet", i+1)
			os.Exit(1)
		}

		sendTime := time.Now()
		if _, err := conn.Write(encoded); err != nil {
			logger.Error("write failed", "err", err, "packet", i+1)
			os.Exit(1)
		}

		logger.Info("packet sent",
			"packet", i+1,
			"of", config.PacketCount,
			"time", sendTime.Format("15:04:05.000000"),
			"target", config.RouterAddr,
			"path", path,
			"bytes", len(encoded),
		)

		if i < config.PacketCount-1 && interval > 0 {
			time.Sleep(interval)
		}
	}

	logger.Info("all packets sent", "total", config.PacketCount)
}

// queryGossipState listens for a gossip broadcast to get live topology.
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

	targetAddr, err := net.ResolveUDPAddr("udp", gossipAddr)
	if err != nil {
		return nil
	}
	if _, err := conn.WriteToUDP([]byte("MAP_REQUEST"), targetAddr); err != nil {
		return nil
	}

	conn.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, UDPMaxPayload)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return nil
	}

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
