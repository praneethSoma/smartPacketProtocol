package metrics

import (
	"fmt"
	"sync"
	"time"
)

// ──────────────────────────────────────────────────────────────
// Defaults
// ──────────────────────────────────────────────────────────────

const (
	// DefaultCollectionIntervalMs is the default metrics collection
	// interval in milliseconds.
	DefaultCollectionIntervalMs = 100

	// DefaultProbeListenAddr is the fallback address for the probe
	// listener when none is specified.
	DefaultProbeListenAddr = "0.0.0.0:6000"
)

// ──────────────────────────────────────────────────────────────
// LocalMetrics — combined snapshot of all local measurements.
// ──────────────────────────────────────────────────────────────

// LocalMetrics is the combined snapshot of all local measurements.
// This is what gets stamped into packets and shared via gossip.
type LocalMetrics struct {
	NodeName    string
	LoadPct     float64                // Combined system load 0–100
	CPULoad     float64                // CPU utilization 0–100
	Neighbors   map[string]ProbeResult // Latency/loss to each neighbor
	CollectedAt time.Time
}

// ──────────────────────────────────────────────────────────────
// Collector — background metrics aggregation.
// ──────────────────────────────────────────────────────────────

// Collector runs in the background and keeps LocalMetrics up to date
// by periodically reading system metrics and probe results.
type Collector struct {
	mu       sync.RWMutex
	system   *SystemMetrics
	prober   *Prober
	current  LocalMetrics
	stopCh   chan struct{}
	interval time.Duration
	nodeName string
}

// CollectorConfig holds configuration for the metrics collector.
type CollectorConfig struct {
	NodeName    string            // Name of this node (used in LocalMetrics)
	ListenAddr  string            // Address for probe listener (default "0.0.0.0:6000")
	Neighbors   map[string]string // neighbor name → probe address
	IntervalMs  int               // Collection interval in ms (default 100)
	ProbeConfig ProbeConfig       // Probe tuning parameters
}

// NewCollector creates and initializes a background metrics collector.
func NewCollector(cfg CollectorConfig) (*Collector, error) {
	system := NewSystemMetrics()

	probeListenAddr := cfg.ListenAddr
	if probeListenAddr == "" {
		probeListenAddr = DefaultProbeListenAddr
	}

	prober, err := NewProber(cfg.NodeName, probeListenAddr, cfg.ProbeConfig)
	if err != nil {
		return nil, fmt.Errorf("create prober: %w", err)
	}

	// Register all neighbors for probing.
	for name, addr := range cfg.Neighbors {
		prober.AddNeighbor(name, addr)
	}

	interval := DefaultCollectionIntervalMs
	if cfg.IntervalMs > 0 {
		interval = cfg.IntervalMs
	}

	return &Collector{
		system:   system,
		prober:   prober,
		stopCh:   make(chan struct{}),
		interval: time.Duration(interval) * time.Millisecond,
		nodeName: cfg.NodeName,
		current: LocalMetrics{
			NodeName:  cfg.NodeName,
			Neighbors: make(map[string]ProbeResult),
		},
	}, nil
}

// Start begins background collection and probing.
func (c *Collector) Start() {
	c.prober.Start()
	go c.collectLoop()
}

// Stop terminates background collection and the prober.
func (c *Collector) Stop() {
	close(c.stopCh)
	c.prober.Stop()
}

// GetMetrics returns the latest metrics snapshot.
func (c *Collector) GetMetrics() LocalMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.current
}

// GetLoad returns just the current load percentage.
func (c *Collector) GetLoad() float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.current.LoadPct
}

// GetNeighborLatency returns the measured latency to a specific neighbor.
func (c *Collector) GetNeighborLatency(name string) (float64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result, ok := c.current.Neighbors[name]
	if !ok {
		return 0, false
	}
	return result.LatencyMs, true
}

// GetNeighborLoss returns the measured loss % to a specific neighbor.
func (c *Collector) GetNeighborLoss(name string) (float64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result, ok := c.current.Neighbors[name]
	if !ok {
		return 0, false
	}
	return result.LossPct, true
}

// collectLoop periodically gathers system metrics and probe results.
func (c *Collector) collectLoop() {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.collect()
		}
	}
}

// collect gathers one snapshot of all metrics.
func (c *Collector) collect() {
	loadPct := c.system.GetSystemLoad()
	cpuLoad, _ := c.system.GetCPULoad()
	neighbors := c.prober.GetResults()

	c.mu.Lock()
	c.current = LocalMetrics{
		NodeName:    c.nodeName,
		LoadPct:     loadPct,
		CPULoad:     cpuLoad,
		Neighbors:   neighbors,
		CollectedAt: time.Now(),
	}
	c.mu.Unlock()
}
