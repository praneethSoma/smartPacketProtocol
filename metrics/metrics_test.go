package metrics

import (
	"testing"
)

func TestGetSystemLoad(t *testing.T) {
	sm := NewSystemMetrics()

	load := sm.GetSystemLoad()
	if load < 0 || load > 100 {
		t.Fatalf("Load should be 0-100, got %.2f", load)
	}
	t.Logf("System load: %.2f%% ✓", load)
}

func TestGetSystemLoadCustomConfig(t *testing.T) {
	cfg := DefaultSystemMetricsConfig()
	cfg.CPUBlendWeight = 1.0
	cfg.NetworkBlendWeight = 0.0

	sm := NewSystemMetricsWithConfig(cfg)
	load := sm.GetSystemLoad()
	if load < 0 || load > 100 {
		t.Fatalf("CPU-only load should be 0-100, got %.2f", load)
	}
	t.Logf("CPU-only system load: %.2f%% ✓", load)
}

func TestGetCPULoad(t *testing.T) {
	sm := NewSystemMetrics()

	// First call may return 0 (no delta yet), so call twice
	sm.GetCPULoad()

	load, err := sm.GetCPULoad()
	if err != nil {
		t.Fatalf("GetCPULoad error: %v", err)
	}
	if load < 0 || load > 100 {
		t.Fatalf("CPU load should be 0-100, got %.2f", load)
	}
	t.Logf("CPU load: %.2f%% ✓", load)
}

func TestReadNetworkStats(t *testing.T) {
	stats, err := readAllNetStats()
	if err != nil {
		t.Fatalf("readAllNetStats error: %v", err)
	}
	if len(stats) == 0 {
		t.Fatal("Expected at least one network interface")
	}

	// Loopback should exist
	if _, ok := stats["lo"]; !ok {
		t.Fatal("Loopback interface 'lo' not found")
	}

	for iface, s := range stats {
		t.Logf("Interface %s: rx=%d tx=%d ✓", iface, s.RxBytes, s.TxBytes)
	}
}

func TestCPUStatsTotal(t *testing.T) {
	stats, err := readCPUStats()
	if err != nil {
		t.Fatalf("readCPUStats error: %v", err)
	}
	if stats.Total() == 0 {
		t.Fatal("CPU total ticks should not be 0")
	}
	t.Logf("CPU stats: user=%d system=%d idle=%d total=%d ✓",
		stats.User, stats.System, stats.Idle, stats.Total())
}

func TestDefaultProbeConfig(t *testing.T) {
	cfg := DefaultProbeConfig()
	if cfg.IntervalMs <= 0 {
		t.Fatalf("IntervalMs should be positive, got %d", cfg.IntervalMs)
	}
	if cfg.TimeoutMs <= 0 {
		t.Fatalf("TimeoutMs should be positive, got %d", cfg.TimeoutMs)
	}
	if cfg.WindowSize <= 0 {
		t.Fatalf("WindowSize should be positive, got %d", cfg.WindowSize)
	}
	if cfg.ProbePort != 0 {
		t.Fatalf("ProbePort should default to 0 (dynamic), got %d", cfg.ProbePort)
	}
}

func TestDefaultSystemMetricsConfig(t *testing.T) {
	cfg := DefaultSystemMetricsConfig()
	sum := cfg.CPUBlendWeight + cfg.NetworkBlendWeight
	if sum < 0.99 || sum > 1.01 {
		t.Fatalf("Blend weights should sum to ~1.0, got %.2f", sum)
	}
	if cfg.LinkCapacityBps <= 0 {
		t.Fatal("LinkCapacityBps should be positive")
	}
}
