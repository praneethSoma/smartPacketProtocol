package prommetrics

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// ──────────────────────────────────────────────────────────────
// Counter — monotonically increasing metric.
// ──────────────────────────────────────────────────────────────

// Counter is a thread-safe monotonically increasing counter.
type Counter struct {
	value uint64
}

// Inc increments the counter by 1.
func (c *Counter) Inc() { atomic.AddUint64(&c.value, 1) }

// Get returns the current value.
func (c *Counter) Get() uint64 { return atomic.LoadUint64(&c.value) }

// ──────────────────────────────────────────────────────────────
// Gauge — a metric that can go up and down.
// ──────────────────────────────────────────────────────────────

// Gauge stores a float64 value using atomic operations on its bit pattern.
type Gauge struct {
	bits uint64
}

// Set stores a float64 value.
func (g *Gauge) Set(v float64) {
	atomic.StoreUint64(&g.bits, math.Float64bits(v))
}

// Get returns the current float64 value.
func (g *Gauge) Get() float64 {
	return math.Float64frombits(atomic.LoadUint64(&g.bits))
}

// ──────────────────────────────────────────────────────────────
// Histogram — fixed-bucket latency histogram.
// ──────────────────────────────────────────────────────────────

// Histogram tracks observations in fixed buckets. Thread-safe.
type Histogram struct {
	buckets    []float64 // upper bounds, sorted ascending
	counts     []uint64  // one per bucket (atomic)
	totalCount uint64    // total observations (atomic)
	totalSum   uint64    // sum encoded as float64 bits (requires mutex for add)
	mu         sync.Mutex
}

// NewHistogram creates a histogram with the given bucket boundaries.
func NewHistogram(buckets []float64) *Histogram {
	sorted := make([]float64, len(buckets))
	copy(sorted, buckets)
	sort.Float64s(sorted)
	return &Histogram{
		buckets: sorted,
		counts:  make([]uint64, len(sorted)),
	}
}

// Observe records a value.
func (h *Histogram) Observe(v float64) {
	for i, bound := range h.buckets {
		if v <= bound {
			atomic.AddUint64(&h.counts[i], 1)
		}
	}
	atomic.AddUint64(&h.totalCount, 1)

	h.mu.Lock()
	current := math.Float64frombits(atomic.LoadUint64(&h.totalSum))
	atomic.StoreUint64(&h.totalSum, math.Float64bits(current+v))
	h.mu.Unlock()
}

// ──────────────────────────────────────────────────────────────
// LabeledCounter — counter with a label dimension.
// ──────────────────────────────────────────────────────────────

// LabeledCounter holds one counter per label value.
type LabeledCounter struct {
	mu       sync.RWMutex
	counters map[string]*Counter
}

// NewLabeledCounter creates an empty labeled counter.
func NewLabeledCounter() *LabeledCounter {
	return &LabeledCounter{counters: make(map[string]*Counter)}
}

// Inc increments the counter for the given label.
func (lc *LabeledCounter) Inc(label string) {
	lc.mu.RLock()
	c, ok := lc.counters[label]
	lc.mu.RUnlock()
	if ok {
		c.Inc()
		return
	}
	lc.mu.Lock()
	if c, ok = lc.counters[label]; ok {
		lc.mu.Unlock()
		c.Inc()
		return
	}
	c = &Counter{}
	lc.counters[label] = c
	lc.mu.Unlock()
	c.Inc()
}

// GetAll returns a snapshot of all label→value pairs.
func (lc *LabeledCounter) GetAll() map[string]uint64 {
	lc.mu.RLock()
	defer lc.mu.RUnlock()
	result := make(map[string]uint64, len(lc.counters))
	for label, c := range lc.counters {
		result[label] = c.Get()
	}
	return result
}

// Total returns the sum across all labels.
func (lc *LabeledCounter) Total() uint64 {
	lc.mu.RLock()
	defer lc.mu.RUnlock()
	var total uint64
	for _, c := range lc.counters {
		total += c.Get()
	}
	return total
}

// ──────────────────────────────────────────────────────────────
// Metrics — aggregated metrics for the SPP router.
// ──────────────────────────────────────────────────────────────

// Default histogram buckets for forwarding latency (in seconds).
var defaultLatencyBuckets = []float64{
	0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1,
}

// Metrics holds all Prometheus-compatible metrics for an SPP router.
type Metrics struct {
	// Counters
	PacketsReceived  Counter
	PacketsForwarded Counter
	PacketsDropped   LabeledCounter // labeled by reason
	Reroutes         Counter
	LoopsDetected    Counter

	// Gauges
	CurrentLoadPct   Gauge
	GossipLinksKnown Gauge

	// Histograms
	ForwardingLatency *Histogram

	// Health state
	ready atomic.Value // bool
}

// NewMetrics creates a Metrics instance with default histogram buckets.
func NewMetrics() *Metrics {
	m := &Metrics{
		PacketsDropped:    *NewLabeledCounter(),
		ForwardingLatency: NewHistogram(defaultLatencyBuckets),
	}
	m.ready.Store(false)
	return m
}

// ── Increment helpers ────────────────────────────────────────

// IncReceived increments the packets received counter.
func (m *Metrics) IncReceived() { m.PacketsReceived.Inc() }

// IncForwarded increments the packets forwarded counter.
func (m *Metrics) IncForwarded() { m.PacketsForwarded.Inc() }

// IncDropped increments the dropped counter for the given reason.
func (m *Metrics) IncDropped(reason string) { m.PacketsDropped.Inc(reason) }

// IncReroutes increments the reroute counter.
func (m *Metrics) IncReroutes() { m.Reroutes.Inc() }

// IncLoops increments the loop detection counter.
func (m *Metrics) IncLoops() { m.LoopsDetected.Inc() }

// SetLoadPct sets the current load gauge.
func (m *Metrics) SetLoadPct(v float64) { m.CurrentLoadPct.Set(v) }

// SetGossipLinks sets the known gossip links gauge.
func (m *Metrics) SetGossipLinks(n int) { m.GossipLinksKnown.Set(float64(n)) }

// ObserveLatency records a forwarding latency observation (in seconds).
func (m *Metrics) ObserveLatency(seconds float64) {
	m.ForwardingLatency.Observe(seconds)
}

// ── Health state ────────────────────────────────────────────

// SetReady marks the router as ready (gossip has ≥1 fresh link).
func (m *Metrics) SetReady(ready bool) { m.ready.Store(ready) }

// IsReady returns the current readiness state.
func (m *Metrics) IsReady() bool {
	v, ok := m.ready.Load().(bool)
	return ok && v
}

// ── HTTP Handlers ────────────────────────────────────────────

// Handler returns an http.Handler that renders metrics in Prometheus
// text exposition format.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		var sb strings.Builder

		// Counters
		writeCounter(&sb, "spp_packets_received_total", "Total packets received", m.PacketsReceived.Get())
		writeCounter(&sb, "spp_packets_forwarded_total", "Total packets forwarded", m.PacketsForwarded.Get())
		writeCounter(&sb, "spp_reroutes_total", "Total reroute events", m.Reroutes.Get())
		writeCounter(&sb, "spp_loops_detected_total", "Total loops detected", m.LoopsDetected.Get())

		// Labeled counter: packets dropped
		sb.WriteString("# HELP spp_packets_dropped_total Total packets dropped\n")
		sb.WriteString("# TYPE spp_packets_dropped_total counter\n")
		dropped := m.PacketsDropped.GetAll()
		if len(dropped) == 0 {
			sb.WriteString("spp_packets_dropped_total 0\n")
		} else {
			// Sort keys for deterministic output
			reasons := make([]string, 0, len(dropped))
			for r := range dropped {
				reasons = append(reasons, r)
			}
			sort.Strings(reasons)
			for _, reason := range reasons {
				fmt.Fprintf(&sb, "spp_packets_dropped_total{reason=%q} %d\n", reason, dropped[reason])
			}
		}

		// Gauges
		writeGauge(&sb, "spp_current_load_pct", "Current system load percentage", m.CurrentLoadPct.Get())
		writeGauge(&sb, "spp_gossip_links_known", "Number of known gossip links", m.GossipLinksKnown.Get())

		// Histogram
		sb.WriteString("# HELP spp_forwarding_latency_seconds Forwarding latency histogram\n")
		sb.WriteString("# TYPE spp_forwarding_latency_seconds histogram\n")
		h := m.ForwardingLatency
		for i, bound := range h.buckets {
			fmt.Fprintf(&sb, "spp_forwarding_latency_seconds_bucket{le=\"%g\"} %d\n",
				bound, atomic.LoadUint64(&h.counts[i]))
		}
		fmt.Fprintf(&sb, "spp_forwarding_latency_seconds_bucket{le=\"+Inf\"} %d\n",
			atomic.LoadUint64(&h.totalCount))
		h.mu.Lock()
		sum := math.Float64frombits(atomic.LoadUint64(&h.totalSum))
		h.mu.Unlock()
		fmt.Fprintf(&sb, "spp_forwarding_latency_seconds_sum %g\n", sum)
		fmt.Fprintf(&sb, "spp_forwarding_latency_seconds_count %d\n",
			atomic.LoadUint64(&h.totalCount))

		w.Write([]byte(sb.String()))
	})
}

// HealthzHandler returns 200 if the server is running (liveness probe).
func (m *Metrics) HealthzHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	})
}

// ReadyzHandler returns 200 if the router is ready (readiness probe).
// Ready means gossip has at least 1 fresh link.
func (m *Metrics) ReadyzHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.IsReady() {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ready\n"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("not ready\n"))
		}
	})
}

// ── Format helpers ───────────────────────────────────────────

func writeCounter(sb *strings.Builder, name, help string, value uint64) {
	fmt.Fprintf(sb, "# HELP %s %s\n", name, help)
	fmt.Fprintf(sb, "# TYPE %s counter\n", name)
	fmt.Fprintf(sb, "%s %d\n", name, value)
}

func writeGauge(sb *strings.Builder, name, help string, value float64) {
	fmt.Fprintf(sb, "# HELP %s %s\n", name, help)
	fmt.Fprintf(sb, "# TYPE %s gauge\n", name)
	fmt.Fprintf(sb, "%s %g\n", name, value)
}
