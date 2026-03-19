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
	"sync"
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
	DefaultMaxStalenessMs = 5000

	// DefaultWarnStalenessMs is the fallback age at which links are penalized.
	DefaultWarnStalenessMs = 2000

	// DefaultFreshnessMs is the fallback freshness threshold for
	// GetFreshLinks() when deciding whether gossip data is usable.
	DefaultFreshnessMs = 3000

	// DefaultRerouteCooldownMs is the minimum time (in ms) between
	// consecutive reroute actions at this router. Prevents path
	// flapping when network conditions oscillate rapidly.
	DefaultRerouteCooldownMs = 2000

	// DefaultWorkerCount is the number of goroutines processing packets.
	DefaultWorkerCount = 4

	// DefaultWorkerChanSize is the buffered channel size for the worker pool.
	DefaultWorkerChanSize = 256

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
	RerouteCooldownMs int                       `json:"RerouteCooldownMs"` // Minimum ms between reroutes (anti-flap)
	WorkerCount      int                       `json:"WorkerCount"`      // Number of packet processing workers (default 4)
	Neighbors        map[string]NeighborConfig `json:"Neighbors"`
	NodeIDMap        map[string]uint16         `json:"NodeIDMap"`        // Compact node ID mapping for wire compression
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
	if c.RerouteCooldownMs == 0 {
		c.RerouteCooldownMs = DefaultRerouteCooldownMs
	}
	if c.WorkerCount == 0 {
		c.WorkerCount = DefaultWorkerCount
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

	var nodeIDTable *packet.NodeIDTable
	if len(config.NodeIDMap) > 0 {
		nodeIDTable = packet.NewNodeIDTable(config.NodeIDMap)
	}

	pool := connpool.New()
	defer pool.Close()

	// Anti-flap: per-destination reroute cooldown tracking.
	// Uses sync.Map to avoid mutex contention on the hot path —
	// every packet was previously acquiring rerouteMu just to read
	// the last reroute time, even when no reroute was needed.
	var lastRerouteByDest sync.Map
	rerouteCooldown := time.Duration(config.RerouteCooldownMs) * time.Millisecond
	logger.Info("reroute cooldown active", "cooldown", rerouteCooldown)

	// ── Worker pool ──
	type work struct {
		data []byte
	}
	workCh := make(chan work, DefaultWorkerChanSize)

	for i := 0; i < config.WorkerCount; i++ {
		go func(workerID int) {
			for w := range workCh {
				p, err := packet.DecodeWireWithIDs(w.data, nodeIDTable)
				if err != nil {
					logger.Warn("decode error", "worker", workerID, "err", err)
					continue
				}

				prom.IncReceived()
				forwardStart := time.Now()

				slog.Debug("packet received",
					"node", config.Name,
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

				// ── Step 2: Determine next hop first, then look up that neighbor's latency ──
				nextHop := p.NextHop()
				if nextHop == "" {
					nextHop = p.Destination
				}

				currentLoad := 0.0
				currentLatency := 0.0
				if metricsCollector != nil {
					m := metricsCollector.GetMetrics()
					currentLoad = m.LoadPct
					// Use the specific next-hop neighbor's latency.
					if pr, found := m.Neighbors[nextHop]; found {
						currentLatency = pr.LatencyMs
					}
				}
				p.LogHop(config.Name, currentLoad, currentLatency)
				slog.Debug("stamped",
					"node", config.Name,
					"load", currentLoad,
					"latency_ms", currentLatency,
				)

				// ── Step 3: Check for reroute (with per-destination cooldown) ──
				isCritical := p.Intent.Priority >= 2
				cooldownOk := true
				if !isCritical {
					if val, ok := lastRerouteByDest.Load(p.Destination); ok {
						cooldownOk = time.Since(val.(time.Time)) >= rerouteCooldown
					}
				}

				if p.LightMode {
					// Cheap divergence pre-check: scans gossip state under RLock
					// with zero allocations. Only if divergence is detected do we
					// pay for GetFreshLinks (which allocates a []packet.Link slice).
					effectiveThreshold := packet.RerouteThreshold
					if p.Rerouted {
						_, effectiveThreshold = packet.RerouteThresholdForIntent(p.Intent)
					}
					if topoState.CheckDivergence(config.Name, currentLoad, currentLatency, effectiveThreshold, packet.DefaultMinDivergenceLoad, packet.DefaultMinDivergenceLatencyMs) {
						freshLinks := topoState.GetFreshLinks(freshnessThreshold)
						if len(freshLinks) > 0 {
							if cooldownOk {
								logger.Info("conditions diverged — rerouting (light mode)", "dest", p.Destination)
								p.RerouteFromTopology(config.Name, freshLinks)
								logger.Info("rerouted", "new_path", p.PlannedPath)
								lastRerouteByDest.Store(p.Destination, time.Now())
								prom.IncReroutes()
							} else {
								slog.Debug("reroute suppressed — cooldown active",
									"node", config.Name,
									"dest", p.Destination)
							}
						}
					}
				} else if p.ShouldReroute(config.Name, currentLoad, currentLatency, packet.RerouteThreshold) {
					if cooldownOk {
						logger.Info("conditions diverged — rerouting", "dest", p.Destination)
						freshLinks := topoState.GetFreshLinks(freshnessThreshold)
						if len(freshLinks) > 0 {
							p.Reroute(config.Name, freshLinks)
							logger.Info("rerouted", "new_path", p.PlannedPath)
							lastRerouteByDest.Store(p.Destination, time.Now())
							prom.IncReroutes()
						} else {
							logger.Warn("no fresh gossip data for reroute")
						}
					} else {
						slog.Debug("reroute suppressed — cooldown active",
							"node", config.Name,
							"dest", p.Destination)
					}
				}

				// ── Step 4: Recalculate next hop (may have changed after reroute) ──
				nextHop = p.NextHop()

				// ── Step 5: Loop detection ──
				if nextHop != "" && p.DetectLoop(nextHop) {
					logger.Warn("loop detected — force-forwarding", "loop_node", nextHop)
					prom.IncLoops()
					if p.LightMode {
						freshLinks := topoState.GetFreshLinks(freshnessThreshold)
						nextHop = p.ForceForwardFromTopology(config.Name, freshLinks)
					} else {
						nextHop = p.ForceForward(config.Name)
					}
					p.Degraded = true
					slog.Debug("force-forward", "node", config.Name, "next", nextHop)
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

				var encoded []byte
				if nodeIDTable != nil {
					encoded, err = p.EncodeWireWithIDs(nodeIDTable)
				} else {
					encoded, err = p.EncodeWire()
				}
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
				slog.Debug("forwarded",
					"node", config.Name,
					"next_hop", nextHop,
					"addr", neighbor.DataAddr,
				)
			}
		}(i)
	}

	// ── Receive loop: read packets and dispatch to workers ──
	buf := make([]byte, UDPMaxPayload)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		// Copy buffer before dispatching to avoid data race.
		pktData := make([]byte, n)
		copy(pktData, buf[:n])
		workCh <- work{data: pktData}
	}
}
