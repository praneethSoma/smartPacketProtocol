package metrics

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ──────────────────────────────────────────────────────────────
// Configurable defaults — no hardcoded magic numbers.
// ──────────────────────────────────────────────────────────────

const (
	// DefaultLinkCapacityBps is the assumed link speed in bytes/sec.
	// 1 Gbps = 125,000,000 bytes/sec. Override via SystemMetricsConfig.
	DefaultLinkCapacityBps = 125_000_000.0

	// DefaultCPUBlendWeight controls how much CPU load contributes
	// to the combined system load metric. Default: 40%.
	DefaultCPUBlendWeight = 0.4

	// DefaultNetworkBlendWeight controls how much network load
	// contributes to the combined system load metric. Default: 60%.
	// Network is weighted higher because routers are network-bound.
	DefaultNetworkBlendWeight = 0.6

	// DefaultLoopbackInterface is the interface name skipped when
	// computing aggregate network load.
	DefaultLoopbackInterface = "lo"
)

// SystemMetricsConfig holds tunable parameters for system metrics.
type SystemMetricsConfig struct {
	// LinkCapacityBps is the assumed link speed in bytes/sec.
	// Used to convert bytes/sec into a 0–100% utilization metric.
	LinkCapacityBps float64

	// CPUBlendWeight + NetworkBlendWeight should sum to 1.0.
	CPUBlendWeight     float64
	NetworkBlendWeight float64

	// LoopbackInterface is the name of the loopback interface to
	// exclude from aggregate network load. Typically "lo".
	LoopbackInterface string
}

// DefaultSystemMetricsConfig returns sensible defaults.
func DefaultSystemMetricsConfig() SystemMetricsConfig {
	return SystemMetricsConfig{
		LinkCapacityBps:    DefaultLinkCapacityBps,
		CPUBlendWeight:     DefaultCPUBlendWeight,
		NetworkBlendWeight: DefaultNetworkBlendWeight,
		LoopbackInterface:  DefaultLoopbackInterface,
	}
}

// ──────────────────────────────────────────────────────────────
// CPU statistics from /proc/stat
// ──────────────────────────────────────────────────────────────

// CPUStats holds raw CPU tick counts from /proc/stat.
type CPUStats struct {
	User    uint64
	Nice    uint64
	System  uint64
	Idle    uint64
	IOWait  uint64
	IRQ     uint64
	SoftIRQ uint64
	Steal   uint64
}

// Total returns the sum of all CPU ticks.
func (c CPUStats) Total() uint64 {
	return c.User + c.Nice + c.System + c.Idle + c.IOWait + c.IRQ + c.SoftIRQ + c.Steal
}

// IdleTotal returns idle + iowait ticks.
func (c CPUStats) IdleTotal() uint64 {
	return c.Idle + c.IOWait
}

// ──────────────────────────────────────────────────────────────
// Network statistics from /proc/net/dev
// ──────────────────────────────────────────────────────────────

// NetStats holds per-interface network counters from /proc/net/dev.
type NetStats struct {
	Interface string
	RxBytes   uint64
	RxPackets uint64
	RxErrors  uint64
	RxDrops   uint64
	TxBytes   uint64
	TxPackets uint64
	TxErrors  uint64
	TxDrops   uint64
}

// ──────────────────────────────────────────────────────────────
// SystemMetrics — real-time system load measurement.
// ──────────────────────────────────────────────────────────────

// SystemMetrics provides real-time CPU and network load measurement
// by computing deltas between successive reads of /proc counters.
type SystemMetrics struct {
	mu       sync.Mutex
	prevCPU  CPUStats
	prevNet  map[string]NetStats
	prevTime time.Time
	config   SystemMetricsConfig
}

// NewSystemMetrics creates a new system metrics collector with default config.
func NewSystemMetrics() *SystemMetrics {
	return NewSystemMetricsWithConfig(DefaultSystemMetricsConfig())
}

// NewSystemMetricsWithConfig creates a system metrics collector with
// the provided configuration.
func NewSystemMetricsWithConfig(cfg SystemMetricsConfig) *SystemMetrics {
	sm := &SystemMetrics{
		prevNet: make(map[string]NetStats),
		config:  cfg,
	}
	// Take an initial snapshot so deltas work on first call.
	sm.prevCPU, _ = readCPUStats()
	sm.prevNet, _ = readAllNetStats()
	sm.prevTime = time.Now()
	return sm
}

// GetCPULoad returns current CPU utilization as 0–100%.
// Computed as delta of non-idle ticks / delta of total ticks since last call.
func (sm *SystemMetrics) GetCPULoad() (float64, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	current, err := readCPUStats()
	if err != nil {
		return 0, fmt.Errorf("read cpu stats: %w", err)
	}

	totalDelta := float64(current.Total() - sm.prevCPU.Total())
	idleDelta := float64(current.IdleTotal() - sm.prevCPU.IdleTotal())

	sm.prevCPU = current

	if totalDelta == 0 {
		return 0, nil
	}

	load := ((totalDelta - idleDelta) / totalDelta) * 100.0
	return load, nil
}

// GetNetworkLoad returns network utilization for a specific interface
// as 0–100%, based on bytes/sec relative to the configured link capacity.
func (sm *SystemMetrics) GetNetworkLoad(iface string) (float64, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	allStats, err := readAllNetStats()
	if err != nil {
		return 0, fmt.Errorf("read net stats: %w", err)
	}

	current, ok := allStats[iface]
	if !ok {
		return 0, fmt.Errorf("interface %s not found", iface)
	}

	prev, hasPrev := sm.prevNet[iface]
	elapsed := time.Since(sm.prevTime).Seconds()
	sm.prevNet = allStats
	sm.prevTime = time.Now()

	if !hasPrev || elapsed <= 0 {
		return 0, nil
	}

	// Calculate bytes per second (RX + TX combined).
	rxBytesPerSec := float64(current.RxBytes-prev.RxBytes) / elapsed
	txBytesPerSec := float64(current.TxBytes-prev.TxBytes) / elapsed
	totalBytesPerSec := rxBytesPerSec + txBytesPerSec

	load := (totalBytesPerSec / sm.config.LinkCapacityBps) * 100.0
	if load > 100 {
		load = 100
	}
	return load, nil
}

// GetSystemLoad returns a combined load metric (0–100) blending
// CPU and network utilization. This is the primary number stamped
// into packets by routers.
//
// Blend formula: combined = cpu × CPUBlendWeight + netAvg × NetworkBlendWeight
func (sm *SystemMetrics) GetSystemLoad() float64 {
	cpuLoad, err := sm.GetCPULoad()
	if err != nil {
		cpuLoad = 0
	}

	// Average load across all non-loopback interfaces.
	allStats, err := readAllNetStats()
	if err != nil {
		return cpuLoad
	}

	var netLoadSum float64
	var netCount int
	for iface := range allStats {
		if iface == sm.config.LoopbackInterface {
			continue
		}
		netLoad, err := sm.GetNetworkLoad(iface)
		if err == nil {
			netLoadSum += netLoad
			netCount++
		}
	}

	netLoadAvg := 0.0
	if netCount > 0 {
		netLoadAvg = netLoadSum / float64(netCount)
	}

	combined := cpuLoad*sm.config.CPUBlendWeight + netLoadAvg*sm.config.NetworkBlendWeight
	if combined > 100 {
		combined = 100
	}
	return combined
}

// GetNetworkErrors returns total errors and drops across all interfaces.
func (sm *SystemMetrics) GetNetworkErrors() (errors uint64, drops uint64) {
	allStats, err := readAllNetStats()
	if err != nil {
		return 0, 0
	}
	for _, stats := range allStats {
		errors += stats.RxErrors + stats.TxErrors
		drops += stats.RxDrops + stats.TxDrops
	}
	return
}

// ──────────────────────────────────────────────────────────────
// /proc readers — Linux-specific, read-only, no side effects.
// ──────────────────────────────────────────────────────────────

// readCPUStats reads the aggregate CPU line from /proc/stat.
func readCPUStats() (CPUStats, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return CPUStats{}, fmt.Errorf("open /proc/stat: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "cpu ") {
			return parseCPULine(line)
		}
	}
	return CPUStats{}, fmt.Errorf("cpu line not found in /proc/stat")
}

// parseCPULine parses "cpu  user nice system idle iowait irq softirq steal".
func parseCPULine(line string) (CPUStats, error) {
	fields := strings.Fields(line)
	if len(fields) < 9 {
		return CPUStats{}, fmt.Errorf("unexpected cpu line format: %s", line)
	}

	vals := make([]uint64, 8)
	for i := 0; i < 8; i++ {
		v, err := strconv.ParseUint(fields[i+1], 10, 64)
		if err != nil {
			return CPUStats{}, fmt.Errorf("parse cpu field %d: %w", i, err)
		}
		vals[i] = v
	}

	return CPUStats{
		User:    vals[0],
		Nice:    vals[1],
		System:  vals[2],
		Idle:    vals[3],
		IOWait:  vals[4],
		IRQ:     vals[5],
		SoftIRQ: vals[6],
		Steal:   vals[7],
	}, nil
}

// readAllNetStats reads all interfaces from /proc/net/dev.
func readAllNetStats() (map[string]NetStats, error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, fmt.Errorf("open /proc/net/dev: %w", err)
	}
	defer f.Close()

	result := make(map[string]NetStats)
	scanner := bufio.NewScanner(f)

	// Skip 2 header lines.
	scanner.Scan()
	scanner.Scan()

	for scanner.Scan() {
		stats, err := parseNetDevLine(scanner.Text())
		if err != nil {
			continue // Skip unparseable lines.
		}
		result[stats.Interface] = stats
	}

	return result, nil
}

// parseNetDevLine parses one line from /proc/net/dev.
// Format: iface: rx_bytes rx_packets rx_errs rx_drop ... tx_bytes tx_packets tx_errs tx_drop ...
func parseNetDevLine(line string) (NetStats, error) {
	parts := strings.SplitN(line, ":", 2)
	if len(parts) != 2 {
		return NetStats{}, fmt.Errorf("invalid /proc/net/dev line: %q", line)
	}

	iface := strings.TrimSpace(parts[0])
	fields := strings.Fields(parts[1])
	if len(fields) < 16 {
		return NetStats{}, fmt.Errorf("not enough fields for interface %s", iface)
	}

	parseU64 := func(s string) uint64 {
		v, _ := strconv.ParseUint(s, 10, 64)
		return v
	}

	return NetStats{
		Interface: iface,
		RxBytes:   parseU64(fields[0]),
		RxPackets: parseU64(fields[1]),
		RxErrors:  parseU64(fields[2]),
		RxDrops:   parseU64(fields[3]),
		TxBytes:   parseU64(fields[8]),
		TxPackets: parseU64(fields[9]),
		TxErrors:  parseU64(fields[10]),
		TxDrops:   parseU64(fields[11]),
	}, nil
}
