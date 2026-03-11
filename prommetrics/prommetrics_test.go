package prommetrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCounterIncrement(t *testing.T) {
	m := NewMetrics()

	m.IncReceived()
	m.IncReceived()
	m.IncForwarded()
	m.IncReroutes()
	m.IncLoops()
	m.IncDropped("expired")
	m.IncDropped("expired")
	m.IncDropped("no_neighbor")

	if m.PacketsReceived.Get() != 2 {
		t.Fatalf("Received: got %d, want 2", m.PacketsReceived.Get())
	}
	if m.PacketsForwarded.Get() != 1 {
		t.Fatalf("Forwarded: got %d, want 1", m.PacketsForwarded.Get())
	}
	if m.Reroutes.Get() != 1 {
		t.Fatalf("Reroutes: got %d, want 1", m.Reroutes.Get())
	}
	if m.PacketsDropped.Total() != 3 {
		t.Fatalf("Dropped total: got %d, want 3", m.PacketsDropped.Total())
	}
	t.Log("Counter increments correct ✓")
}

func TestGaugeSetGet(t *testing.T) {
	m := NewMetrics()
	m.SetLoadPct(42.5)
	m.SetGossipLinks(7)

	if m.CurrentLoadPct.Get() != 42.5 {
		t.Fatalf("LoadPct: got %f, want 42.5", m.CurrentLoadPct.Get())
	}
	if m.GossipLinksKnown.Get() != 7.0 {
		t.Fatalf("GossipLinks: got %f, want 7", m.GossipLinksKnown.Get())
	}
	t.Log("Gauge set/get correct ✓")
}

func TestHistogramObserve(t *testing.T) {
	m := NewMetrics()

	// Observe some latencies (in seconds).
	m.ObserveLatency(0.00005) // below 0.0001 bucket
	m.ObserveLatency(0.0003)  // below 0.0005 bucket
	m.ObserveLatency(0.002)   // below 0.005 bucket
	m.ObserveLatency(0.08)    // below 0.1 bucket

	body := getMetricsBody(t, m)

	// Check that histogram lines exist.
	if !strings.Contains(body, "spp_forwarding_latency_seconds_bucket") {
		t.Fatal("Histogram bucket lines missing")
	}
	if !strings.Contains(body, "spp_forwarding_latency_seconds_count 4") {
		t.Fatalf("Histogram count should be 4, body:\n%s", body)
	}
	t.Log("Histogram observation correct ✓")
}

func TestPrometheusTextFormat(t *testing.T) {
	m := NewMetrics()
	m.IncReceived()
	m.IncForwarded()
	m.IncDropped("expired")
	m.SetLoadPct(25.5)

	body := getMetricsBody(t, m)

	// Check HELP and TYPE lines.
	requiredLines := []string{
		"# HELP spp_packets_received_total",
		"# TYPE spp_packets_received_total counter",
		"spp_packets_received_total 1",
		"# HELP spp_packets_forwarded_total",
		"spp_packets_forwarded_total 1",
		"# HELP spp_current_load_pct",
		"# TYPE spp_current_load_pct gauge",
		"spp_current_load_pct 25.5",
		"spp_packets_dropped_total{reason=\"expired\"} 1",
		"# TYPE spp_forwarding_latency_seconds histogram",
	}

	for _, line := range requiredLines {
		if !strings.Contains(body, line) {
			t.Fatalf("Missing required line: %q\nBody:\n%s", line, body)
		}
	}
	t.Log("Prometheus text format correct ✓")
}

func TestHealthzEndpoint(t *testing.T) {
	m := NewMetrics()

	rec := httptest.NewRecorder()
	m.HealthzHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("Healthz: got %d, want 200", rec.Code)
	}
	t.Log("/healthz returns 200 ✓")
}

func TestReadyzEndpoint(t *testing.T) {
	m := NewMetrics()

	// Not ready by default.
	rec := httptest.NewRecorder()
	m.ReadyzHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Readyz (not ready): got %d, want 503", rec.Code)
	}

	// Set ready.
	m.SetReady(true)
	rec = httptest.NewRecorder()
	m.ReadyzHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("Readyz (ready): got %d, want 200", rec.Code)
	}
	t.Log("/readyz toggles correctly ✓")
}

func TestConcurrentMetrics(t *testing.T) {
	m := NewMetrics()
	var wg sync.WaitGroup

	const goroutines = 100
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			m.IncReceived()
			m.IncForwarded()
			m.IncDropped("expired")
			m.IncReroutes()
			m.SetLoadPct(50.0)
			m.ObserveLatency(0.001)
		}()
	}
	wg.Wait()

	if m.PacketsReceived.Get() != goroutines {
		t.Fatalf("Concurrent received: got %d, want %d", m.PacketsReceived.Get(), goroutines)
	}
	t.Logf("Concurrent metrics from %d goroutines: correct ✓", goroutines)
}

// getMetricsBody fetches the /metrics handler output as a string.
func getMetricsBody(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("Metrics handler: got %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}
