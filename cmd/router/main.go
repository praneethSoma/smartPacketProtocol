package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"smartpacket/connpool"
	"smartpacket/gossip"
	"smartpacket/metrics"
	"smartpacket/packet"
	"smartpacket/prommetrics"
)

// ──────────────────────────────────────────────────────────────
// Configurable defaults — no hardcoded magic numbers.
// ──────────────────────────────────────────────────────────────

const (
	// DefaultProbeIntervalMs is the fallback probe interval.
	DefaultProbeIntervalMs = 100

	// DefaultGossipIntervalMs is the fallback gossip broadcast interval.
	DefaultGossipIntervalMs = 50

	// DefaultGossipPort is the fallback gossip listen port.
	DefaultGossipPort = 7000

	// DefaultMaxStalenessMs is the fallback max link age before pruning.
	DefaultMaxStalenessMs = 1000

	// DefaultWarnStalenessMs is the fallback age at which links are penalized.
	DefaultWarnStalenessMs = 300

	// DefaultFreshnessMs is the fallback freshness threshold for
	// GetFreshLinks() when deciding whether gossip data is usable.
	DefaultFreshnessMs = 500

	// DefaultMetricsPort is the fallback HTTP port for /metrics,
	// /healthz, and /readyz endpoints.
	DefaultMetricsPort = 9090

	// UDPMaxPayload is the maximum UDP datagram size.
	UDPMaxPayload = 65535
)

// ──────────────────────────────────────────────────────────────
// Configuration — loaded from JSON.
// ──────────────────────────────────────────────────────────────

// NeighborConfig holds the multi-port address for a neighbor.
type NeighborConfig struct {
	DataAddr   string `json:"DataAddr"`
	GossipAddr string `json:"GossipAddr"`
	ProbeAddr  string `json:"ProbeAddr"`
}

// NodeConfig is the router configuration loaded from JSON.
type NodeConfig struct {
	Name             string                    `json:"Name"`
	ListenAddr       string                    `json:"ListenAddr"`
	GossipPort       int                       `json:"GossipPort"`
	ProbePort        int                       `json:"ProbePort"`        // Explicit probe port — no arithmetic
	ProbeIntervalMs  int                       `json:"ProbeIntervalMs"`
	GossipIntervalMs int                       `json:"GossipIntervalMs"`
	MaxStalenessMs   int                       `json:"MaxStalenessMs"`
	WarnStalenessMs  int                       `json:"WarnStalenessMs"`
	FreshnessMs      int                       `json:"FreshnessMs"`
	MetricsPort      int                       `json:"MetricsPort"`      // HTTP port for /metrics, /healthz, /readyz
	Neighbors        map[string]NeighborConfig `json:"Neighbors"`
}

// applyDefaults fills zero-valued fields with production defaults.
func (c *NodeConfig) applyDefaults() {
	if c.ProbeIntervalMs == 0 {
		c.ProbeIntervalMs = DefaultProbeIntervalMs
	}
	if c.GossipIntervalMs == 0 {
		c.GossipIntervalMs = DefaultGossipIntervalMs
	}
	if c.GossipPort == 0 {
		c.GossipPort = DefaultGossipPort
	}
	if c.ProbePort == 0 {
		// Derive from GossipPort only as a backward-compatible fallback.
		c.ProbePort = c.GossipPort - 1000
	}
	if c.MaxStalenessMs == 0 {
		c.MaxStalenessMs = DefaultMaxStalenessMs
	}
	if c.WarnStalenessMs == 0 {
		c.WarnStalenessMs = DefaultWarnStalenessMs
	}
	if c.FreshnessMs == 0 {
		c.FreshnessMs = DefaultFreshnessMs
	}
	if c.MetricsPort == 0 {
		c.MetricsPort = DefaultMetricsPort
	}
}

func main() {
	logger := slog.Default()

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: router <config.json>")
		os.Exit(1)
	}

	// ── Load configuration ──
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		logger.Error("failed to read config", "path", os.Args[1], "err", err)
		os.Exit(1)
	}

	var config NodeConfig
	if err := json.Unmarshal(data, &config); err != nil {
		logger.Error("failed to parse config", "path", os.Args[1], "err", err)
		os.Exit(1)
	}
	config.applyDefaults()

	logger = slog.With("node", config.Name)

	logger.Info("═══════════════════════════════════════")
	logger.Info("Smart Packet Protocol Router v2.0")
	logger.Info("═══════════════════════════════════════")

	// ──────────────────────────────────────
	// Phase 1: Start metrics collector
	// ──────────────────────────────────────
	probeNeighbors := make(map[string]string, len(config.Neighbors))
	for name, neighbor := range config.Neighbors {
		probeNeighbors[name] = neighbor.ProbeAddr
	}

	probeListenAddr := fmt.Sprintf("0.0.0.0:%d", config.ProbePort)
	metricsCollector, err := metrics.NewCollector(metrics.CollectorConfig{
		NodeName:    config.Name,
		ListenAddr:  probeListenAddr,
		Neighbors:   probeNeighbors,
		IntervalMs:  config.ProbeIntervalMs,
		ProbeConfig: metrics.DefaultProbeConfig(),
	})
	if err != nil {
		logger.Warn("metrics collector failed — using defaults", "err", err)
		metricsCollector = nil
	} else {
		metricsCollector.Start()
		logger.Info("metrics collector active", "probe_addr", probeListenAddr)
	}

	// ──────────────────────────────────────
	// Phase 2: Start gossip protocol
	// ──────────────────────────────────────
	topoState := gossip.NewTopologyState()
	gossipNeighbors := make(map[string]string, len(config.Neighbors))
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
			MaxStalenessMs:  config.MaxStalenessMs,
			WarnStalenessMs: config.WarnStalenessMs,
		},
		topoState,
		metricsCollector,
	)
	if err != nil {
		logger.Warn("gossip failed", "err", err)
	} else {
		gossipNode.Start()
		logger.Info("gossip active", "addr", gossipListenAddr)
	}

	// ──────────────────────────────────────
	// Signal handling for graceful shutdown
	// ──────────────────────────────────────
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("received signal — shutting down", "signal", sig)
		if gossipNode != nil {
			gossipNode.Stop()
		}
		if metricsCollector != nil {
			metricsCollector.Stop()
		}
		os.Exit(0)
	}()

	freshnessThreshold := time.Duration(config.FreshnessMs) * time.Millisecond

	// ──────────────────────────────────────
	// Prometheus metrics + health endpoints
	// ──────────────────────────────────────
	prom := prommetrics.NewMetrics()
	mux := http.NewServeMux()
	mux.Handle("/metrics", prom.Handler())
	mux.Handle("/healthz", prom.HealthzHandler())
	mux.Handle("/readyz", prom.ReadyzHandler())
	go func() {
		metricsAddr := fmt.Sprintf(":%d", config.MetricsPort)
		logger.Info("metrics HTTP server starting", "addr", metricsAddr)
		if err := http.ListenAndServe(metricsAddr, mux); err != nil {
			logger.Warn("metrics HTTP server error", "err", err)
		}
	}()

	// Readiness updater — checks gossip freshness periodically.
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-sigCh:
				return
			case <-ticker.C:
				fresh := topoState.CountFresh(freshnessThreshold)
				prom.SetReady(fresh > 0)
				prom.SetGossipLinks(topoState.Size())
			}
		}
	}()

	// ──────────────────────────────────────
	// Data plane: listen for smart packets
	// ──────────────────────────────────────
	logger.Info("listening for data", "addr", config.ListenAddr)

	addr, err := net.ResolveUDPAddr("udp", config.ListenAddr)
	if err != nil {
		logger.Error("failed to resolve listen addr", "addr", config.ListenAddr, "err", err)
		os.Exit(1)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		logger.Error("failed to listen", "addr", config.ListenAddr, "err", err)
		os.Exit(1)
	}
	defer conn.Close()

	pool := connpool.New()
	defer pool.Close()
	buf := make([]byte, UDPMaxPayload)

	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}

		p, err := packet.Decode(buf[:n])
		if err != nil {
			logger.Warn("decode error", "err", err)
			continue
		}

		prom.IncReceived()
		forwardStart := time.Now()

		logger.Info("packet received",
			"dest", p.Destination,
			"path", p.PlannedPath,
			"hop", fmt.Sprintf("%d/%d", p.HopCount, p.MaxHops),
		)

		// ── Step 1: Check TTL ──
		if p.IsExpired() {
			logger.Warn("packet expired — dropping", "hops", p.HopCount, "max", p.MaxHops)
			prom.IncDropped("expired")
			continue
		}

		// ── Step 2: Stamp real congestion from metrics ──
		currentLoad := 0.0
		currentLatency := 0.0
		if metricsCollector != nil {
			m := metricsCollector.GetMetrics()
			currentLoad = m.LoadPct
			// Use average neighbor latency as representative.
			for _, pr := range m.Neighbors {
				currentLatency = pr.LatencyMs
				break
			}
		}
		p.LogHop(config.Name, currentLoad, currentLatency)
		logger.Info("stamped", "load", currentLoad, "latency_ms", currentLatency)

		// ── Step 3: Check for reroute ──
		if p.ShouldReroute(config.Name, currentLoad, currentLatency, packet.RerouteThreshold) {
			logger.Info("conditions diverged — rerouting")

			freshLinks := topoState.GetFreshLinks(freshnessThreshold)
			if len(freshLinks) > 0 {
				p.Reroute(config.Name, freshLinks)
				logger.Info("rerouted", "new_path", p.PlannedPath)
				prom.IncReroutes()
			} else {
				logger.Warn("no fresh gossip data for reroute")
			}
		}

		// ── Step 4: Determine next hop ──
		nextHop := p.NextHop()

		// ── Step 5: Loop detection ──
		if nextHop != "" && p.DetectLoop(nextHop) {
			logger.Warn("loop detected — force-forwarding", "loop_node", nextHop)
			prom.IncLoops()
			nextHop = p.ForceForward(config.Name)
			p.Degraded = true
			logger.Info("force-forward", "next", nextHop)
		}

		if nextHop == "" {
			nextHop = p.Destination
		}

		// ── Step 6: Find neighbor address and forward ──
		neighbor, ok := config.Neighbors[nextHop]
		if !ok {
			logger.Warn("cannot find neighbor", "next_hop", nextHop)
			prom.IncDropped("no_neighbor")
			continue
		}

		encoded, err := p.Encode()
		if err != nil {
			logger.Warn("encode error", "err", err)
			prom.IncDropped("encode_error")
			continue
		}

		if err := pool.Send(neighbor.DataAddr, encoded); err != nil {
			logger.Warn("forward error", "addr", neighbor.DataAddr, "err", err)
			continue
		}
		prom.IncForwarded()
		prom.ObserveLatency(time.Since(forwardStart).Seconds())
		prom.SetLoadPct(currentLoad)
		logger.Info("forwarded", "next_hop", nextHop, "addr", neighbor.DataAddr)
	}
}
