package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"smartpacket/packet"
)

// ──────────────────────────────────────────────────────────────
// Configurable defaults
// ──────────────────────────────────────────────────────────────

const (
	// DefaultListenAddr is the fallback listen address when none is configured.
	DefaultListenAddr = "0.0.0.0:9000"

	// UDPMaxPayload is the maximum UDP datagram size.
	UDPMaxPayload = 65535
)

// ──────────────────────────────────────────────────────────────
// Configuration — loaded from JSON.
// ──────────────────────────────────────────────────────────────

// ReceiverConfig holds receiver configuration.
type ReceiverConfig struct {
	ListenAddr string `json:"ListenAddr"` // Address to listen for packets (default: "0.0.0.0:9000")
}

func main() {
	logger := slog.Default()

	// Load configuration: JSON file → CLI arg → default.
	config := ReceiverConfig{
		ListenAddr: DefaultListenAddr,
	}

	if len(os.Args) >= 2 {
		// Try as JSON config file first.
		data, err := os.ReadFile(os.Args[1])
		if err == nil {
			if jsonErr := json.Unmarshal(data, &config); jsonErr != nil {
				// Not valid JSON — treat as a raw listen address.
				config.ListenAddr = os.Args[1]
			}
		} else {
			// Can't read file — treat as a raw listen address.
			config.ListenAddr = os.Args[1]
		}
	}

	logger.Info("═══════════════════════════════════════")
	logger.Info("Smart Packet Protocol — Receiver v2.1")
	logger.Info("═══════════════════════════════════════")
	logger.Info("listening", "addr", config.ListenAddr)

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

	// ── Signal handling for graceful shutdown ──
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("received signal — shutting down", "signal", sig)
		conn.Close()
		os.Exit(0)
	}()

	buf := make([]byte, UDPMaxPayload)
	packetCount := 0

	// Latency stats for burst mode
	var totalLatencyMs float64
	var minLatencyMs float64 = 999999
	var maxLatencyMs float64

	for {
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}

		receivedAt := time.Now()
		packetCount++

		p, err := packet.Decode(buf[:n])
		if err != nil {
			logger.Warn("decode error", "err", err)
			continue
		}

		// Compute end-to-end latency if timestamp is present.
		var e2eLatencyMs float64
		hasTimestamp := p.CreatedAtNs > 0
		if hasTimestamp {
			e2eLatencyMs = float64(receivedAt.UnixNano()-p.CreatedAtNs) / 1e6
			totalLatencyMs += e2eLatencyMs
			if e2eLatencyMs < minLatencyMs {
				minLatencyMs = e2eLatencyMs
			}
			if e2eLatencyMs > maxLatencyMs {
				maxLatencyMs = e2eLatencyMs
			}
		}

		// Build status string.
		status := "DELIVERED"
		statusFlags := []string{}
		if p.Degraded {
			statusFlags = append(statusFlags, "DEGRADED")
		}
		if p.Rerouted {
			statusFlags = append(statusFlags, "REROUTED")
		}
		if len(statusFlags) > 0 {
			status += " [" + strings.Join(statusFlags, ", ") + "]"
		}

		fmt.Println()
		fmt.Println("╔══════════════════════════════════════════════════╗")
		fmt.Println("║         SMART PACKET DELIVERED                  ║")
		fmt.Println("╠══════════════════════════════════════════════════╣")
		fmt.Printf("║  Packet #:    %-35d║\n", packetCount)
		fmt.Printf("║  Status:      %-35s║\n", status)
		fmt.Printf("║  From:        %-35s║\n", remoteAddr.String())
		fmt.Printf("║  Received at: %-35s║\n", receivedAt.Format("15:04:05.000000"))
		fmt.Println("╠══════════════════════════════════════════════════╣")
		fmt.Printf("║  Payload:     %-35s║\n", string(p.Payload))
		fmt.Printf("║  Destination: %-35s║\n", p.Destination)
		fmt.Printf("║  Path taken:  %-35v║\n", p.PlannedPath)
		fmt.Printf("║  Total hops:  %-35d║\n", p.HopCount)
		fmt.Printf("║  Max hops:    %-35d║\n", p.MaxHops)
		fmt.Println("╠══════════════════════════════════════════════════╣")

		// Latency section
		if hasTimestamp {
			fmt.Println("║  LATENCY                                        ║")
			fmt.Printf("║    End-to-end: %8.3f ms                       ║\n", e2eLatencyMs)
			avgMs := totalLatencyMs / float64(packetCount)
			fmt.Printf("║    Avg (all):  %8.3f ms                       ║\n", avgMs)
			fmt.Printf("║    Min:        %8.3f ms  Max: %8.3f ms     ║\n", minLatencyMs, maxLatencyMs)
			fmt.Println("╠══════════════════════════════════════════════════╣")
		}

		fmt.Println("║  INTENT                                         ║")
		fmt.Printf("║    Latency:     %d (0=relaxed 3=critical)       ║\n", p.Intent.Latency)
		fmt.Printf("║    Reliability: %d (0=none 2=guaranteed)        ║\n", p.Intent.Reliability)
		fmt.Printf("║    Priority:    %d (0=low 3=critical)           ║\n", p.Intent.Priority)
		fmt.Println("╠══════════════════════════════════════════════════╣")
		fmt.Println("║  CONGESTION AUDIT TRAIL                         ║")

		if len(p.CongestionLog) == 0 {
			fmt.Println("║  (no congestion data recorded)                  ║")
		} else {
			for i, hop := range p.CongestionLog {
				fmt.Printf("║  Hop %d: %-10s load=%5.1f%%  latency=%7.2fms ║\n",
					i+1, hop.NodeName, hop.LoadPct, hop.LatencyMs)
			}
		}

		fmt.Println("╠══════════════════════════════════════════════════╣")
		fmt.Printf("║  Packet size: %-35s║\n", fmt.Sprintf("%d bytes", n))
		if p.Rerouted {
			fmt.Println("║  ⚡ This packet was REROUTED mid-flight         ║")
		}
		if p.Degraded {
			fmt.Println("║  ⚠ Delivery was DEGRADED (congested path used) ║")
		}
		fmt.Println("╚══════════════════════════════════════════════════╝")

		logger.Info("packet delivered",
			"count", packetCount,
			"status", status,
			"from", remoteAddr.String(),
			"hops", p.HopCount,
			"e2e_latency_ms", fmt.Sprintf("%.3f", e2eLatencyMs),
		)
	}
}
