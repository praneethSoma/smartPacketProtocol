package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"smartpacket/packet"
)

func main() {
	listenAddr := "0.0.0.0:9000"
	if len(os.Args) >= 2 {
		listenAddr = os.Args[1]
	}

	fmt.Println("═══════════════════════════════════════")
	fmt.Println("  Smart Packet Protocol — Receiver v1.0")
	fmt.Println("═══════════════════════════════════════")
	fmt.Printf("[receiver] Listening on %s\n\n", listenAddr)

	addr, _ := net.ResolveUDPAddr("udp", listenAddr)
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		fmt.Printf("[receiver] Failed to listen: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	buf := make([]byte, 65535)
	packetCount := 0

	for {
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}

		receivedAt := time.Now()
		packetCount++

		p, err := packet.Decode(buf[:n])
		if err != nil {
			fmt.Printf("[receiver] Decode error: %v\n", err)
			continue
		}

		// Build status string
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
		fmt.Printf("║  Received at: %-35s║\n", receivedAt.Format("15:04:05.000"))
		fmt.Println("╠══════════════════════════════════════════════════╣")
		fmt.Printf("║  Payload:     %-35s║\n", string(p.Payload))
		fmt.Printf("║  Destination: %-35s║\n", p.Destination)
		fmt.Printf("║  Path taken:  %-35v║\n", p.PlannedPath)
		fmt.Printf("║  Total hops:  %-35d║\n", p.HopCount)
		fmt.Printf("║  Max hops:    %-35d║\n", p.MaxHops)
		fmt.Println("╠══════════════════════════════════════════════════╣")
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
		fmt.Printf("║  Packet size: %d bytes                           ║\n", n)
		if p.Rerouted {
			fmt.Println("║  ⚡ This packet was REROUTED mid-flight         ║")
		}
		if p.Degraded {
			fmt.Println("║  ⚠ Delivery was DEGRADED (congested path used) ║")
		}
		fmt.Println("╚══════════════════════════════════════════════════╝")
	}
}
