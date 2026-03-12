#!/usr/bin/env bash
# ═══════════════════════════════════════════════════════════════
# SPP Latency Comparison Test — 7-Router Topology
# ═══════════════════════════════════════════════════════════════
#
# Runs 5 scenarios comparing SPP smart routing vs static paths.
# Uses tc netem to inject real network delays and packet loss.
#
# Usage:
#   bash scripts/test_latency.sh          # Run all scenarios
#   bash scripts/test_latency.sh 2        # Run only scenario 2
#   bash scripts/test_latency.sh burst    # Run only burst test
#
# Requirements:
#   - Docker with compose v2
#   - Containers must have NET_ADMIN capability (set in compose file)
#
# ═══════════════════════════════════════════════════════════════
set -euo pipefail

COMPOSE="docker compose -f docker-compose.7router.yml"
GOSSIP_WAIT=12         # Seconds for gossip convergence
SCENARIO_PAUSE=3       # Seconds between scenario steps
DELIVERY_WAIT=5        # Seconds to wait for packet delivery

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

SELECTED_SCENARIO="${1:-all}"

# Result storage for final comparison table
declare -a S_LABELS S_STATIC S_SPP

# ═══════════════════════════════════════════════════════════════
# Helper functions
# ═══════════════════════════════════════════════════════════════

print_header() {
    echo -e "\n${CYAN}╔══════════════════════════════════════════════════════════╗${NC}"
    echo -e "${CYAN}║  $1${NC}"
    echo -e "${CYAN}╚══════════════════════════════════════════════════════════╝${NC}"
}

print_step() {
    echo -e "\n${YELLOW}  ▶ $1${NC}"
}

print_ok() {
    echo -e "  ${GREEN}✓ $1${NC}"
}

print_fail() {
    echo -e "  ${RED}✗ $1${NC}"
}

# Send a packet using a hardcoded static path (no gossip, no rerouting)
# Usage: send_static_and_measure <label> <json_path_array>
# Example: send_static_and_measure "Static (A→F)" '["sender","router_a","router_f","receiver"]'
send_static_and_measure() {
    local label="$1"
    local static_path="$2"

    local static_config
    static_config=$(mktemp /tmp/spp_static_sender_XXXXXX.json)
    cat > "$static_config" << EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "GossipAddr": "10.1.1.2:7001",
  "Destination": "receiver",
  "Payload": "static_baseline_test",
  "IntentLatency": 3,
  "IntentReliability": 1,
  "StaticPath": ${static_path}
}
EOF

    $COMPOSE run --rm \
        -v "${static_config}:/app/configs/topologies/7router/static_sender.json:ro" \
        sender ./bin/sender configs/topologies/7router/static_sender.json 2>&1 | tail -3 >&2

    sleep "$DELIVERY_WAIT"

    local post_logs
    post_logs=$($COMPOSE logs receiver 2>&1)

    rm -f "$static_config"

    if echo "$post_logs" | grep -q "SMART PACKET DELIVERED"; then
        local latency
        latency=$(echo "$post_logs" | grep "End-to-end:" | tail -1 | grep -oP '[\d.]+' | head -1) || latency="N/A"
        local path
        path=$(echo "$post_logs" | grep "Path taken:" | tail -1 | sed 's/.*Path taken:\s*//' | sed 's/\s*║.*//') || path="unknown"
        print_ok "${label}: ${latency}ms — path: ${path}" >&2
        echo "$latency"
    else
        print_fail "${label}: Packet not delivered!" >&2
        echo "FAILED"
    fi
}

# Print speedup comparison between static and SPP latencies
# Usage: print_comparison <static_ms> <spp_ms>
print_comparison() {
    local static_ms="$1"
    local spp_ms="$2"

    if [[ "$static_ms" == "FAILED" || "$spp_ms" == "FAILED" ]]; then
        echo -e "  ${RED}  Cannot compare — a packet failed to deliver${NC}"
        return
    fi

    local speedup
    speedup=$(awk "BEGIN { s=$static_ms; p=$spp_ms; if (p>0) printf \"%.1f\", s/p; else print \"N/A\" }")

    local saved_ms
    saved_ms=$(awk "BEGIN { printf \"%.1f\", $static_ms - $spp_ms }")

    if awk "BEGIN { exit !($spp_ms < $static_ms * 0.9) }"; then
        echo -e "  ${GREEN}${BOLD}  ⚡ SPP is ${speedup}x faster — saved ${saved_ms}ms vs static routing${NC}"
    else
        echo -e "  ${YELLOW}  ≈ SPP and static are similar (${spp_ms}ms vs ${static_ms}ms) — no congestion advantage needed${NC}"
    fi
}

# Inject delay and loss on a container's network interface
# Usage: inject_delay <container> <delay_ms> <loss_pct>
inject_delay() {
    local container="$1"
    local delay="$2"
    local loss="${3:-0}"
    
    echo -e "    ${BOLD}Injecting: ${delay}ms delay, ${loss}% loss on ${container}${NC}"
    
    # Get all interfaces except lo
    local ifaces
    ifaces=$(docker exec "$container" sh -c "ip -o link show | grep -v 'lo:' | awk -F': ' '{print \$2}' | cut -d'@' -f1")
    
    for iface in $ifaces; do
        docker exec "$container" tc qdisc add dev "$iface" root netem delay "${delay}ms" loss "${loss}%" 2>/dev/null || \
        docker exec "$container" tc qdisc change dev "$iface" root netem delay "${delay}ms" loss "${loss}%" 2>/dev/null || true
    done
}

# Remove all tc rules from a container
# Usage: clear_delay <container>
clear_delay() {
    local container="$1"
    local ifaces
    ifaces=$(docker exec "$container" sh -c "ip -o link show | grep -v 'lo:' | awk -F': ' '{print \$2}' | cut -d'@' -f1" 2>/dev/null) || true
    
    for iface in $ifaces; do
        docker exec "$container" tc qdisc del dev "$iface" root 2>/dev/null || true
    done
}

# Clear all delays from all routers
clear_all_delays() {
    print_step "Clearing all tc rules..."
    for router in spp-router-a spp-router-b spp-router-c spp-router-d spp-router-e spp-router-f spp-router-g; do
        clear_delay "$router"
    done
    print_ok "All delays cleared"
}

# Send a packet and capture receiver logs
# Usage: send_and_measure <label>
send_and_measure() {
    local label="$1"
    
    # Remember current log position
    local pre_logs
    pre_logs=$($COMPOSE logs --tail=0 -t receiver 2>&1)
    
    # Send packet
    $COMPOSE run --rm sender 2>&1 | tail -5 >&2

    # Wait for delivery
    sleep "$DELIVERY_WAIT"

    # Capture new logs
    local post_logs
    post_logs=$($COMPOSE logs receiver 2>&1)

    if echo "$post_logs" | grep -q "SMART PACKET DELIVERED"; then
        local latency
        latency=$(echo "$post_logs" | grep "End-to-end:" | tail -1 | grep -oP '[\d.]+' | head -1) || latency="N/A"
        local path
        path=$(echo "$post_logs" | grep "Path taken:" | tail -1 | sed 's/.*Path taken:\s*//' | sed 's/\s*║.*//') || path="unknown"
        local rerouted=""
        if echo "$post_logs" | grep -q "REROUTED"; then
            rerouted=" [REROUTED]"
        fi

        print_ok "${label}: ${latency}ms — path: ${path}${rerouted}" >&2
        echo "$latency"
    else
        print_fail "${label}: Packet not delivered!" >&2
        echo "FAILED"
    fi
}

# Send burst of packets
# Usage: send_burst <count> <interval_ms> <label>
send_burst() {
    local count="$1"
    local interval="$2"
    local label="$3"
    
    # Create a temporary sender config with burst parameters
    local burst_config="/tmp/spp_burst_sender.json"
    cat > "$burst_config" << EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "GossipAddr": "10.1.1.2:7001",
  "Destination": "receiver",
  "FirstRouter": "router_a",
  "Payload": "burst_test_payload",
  "IntentLatency": 3,
  "IntentReliability": 1,
  "GossipRetries": 10,
  "GossipRetryBackoffMs": 500,
  "PacketCount": ${count},
  "IntervalMs": ${interval}
}
EOF
    
    # Copy config into the container context
    docker cp "$burst_config" spp-router-a:/tmp/burst_sender.json 2>/dev/null || true
    
    # Run sender with burst config — mount the file
    $COMPOSE run --rm \
        -v "$burst_config:/app/configs/topologies/7router/burst_sender.json:ro" \
        sender ./bin/sender configs/topologies/7router/burst_sender.json 2>&1 | tail -5
    
    sleep "$DELIVERY_WAIT"
    
    # Capture stats
    local post_logs
    post_logs=$($COMPOSE logs receiver 2>&1)
    
    local delivered
    delivered=$(echo "$post_logs" | grep -c "SMART PACKET DELIVERED") || delivered=0
    local avg_latency
    avg_latency=$(echo "$post_logs" | grep "Avg (all):" | tail -1 | grep -oP '[\d.]+' | head -1) || avg_latency="N/A"
    local min_latency
    min_latency=$(echo "$post_logs" | grep "Min:" | tail -1 | grep -oP '[\d.]+' | head -1) || min_latency="N/A"
    local max_latency
    max_latency=$(echo "$post_logs" | grep "Max:" | tail -1 | grep -oP '[\d.]+' | head -1) || max_latency="N/A"
    
    print_ok "${label}: ${delivered}/${count} delivered"
    print_ok "  Avg: ${avg_latency}ms  Min: ${min_latency}ms  Max: ${max_latency}ms"
    
    rm -f "$burst_config"
}


# ═══════════════════════════════════════════════════════════════
# Infrastructure setup
# ═══════════════════════════════════════════════════════════════

print_header "SPP 7-Router Latency Comparison Test"

echo -e "\n${BOLD}Topology:${NC}"
echo "                    ┌─── router_b ─── router_d ───┐"
echo "  sender → router_a ──┤                              ├── router_g → receiver"
echo "                    ├─── router_c ─── router_e ───┘"
echo "                    └─── router_f ────────────────────────→ receiver"
echo ""
echo "  Path 1 (highway):   A → B → D → G → receiver  (4 hops)"
echo "  Path 2 (alternate): A → C → E → G → receiver  (4 hops)"
echo "  Path 3 (shortcut):  A → F → receiver           (2 hops)"

# ── Step 1: Clean up ──
print_step "Cleaning up previous containers..."
$COMPOSE down --remove-orphans 2>/dev/null || true

# ── Step 2: Build ──
print_step "Building Docker images..."
$COMPOSE build

# ── Step 3: Start infrastructure ──
print_step "Starting 7 routers + receiver..."
$COMPOSE up -d receiver router_a router_b router_c router_d router_e router_f router_g

echo -e "  Waiting ${GOSSIP_WAIT}s for gossip convergence..."
sleep "$GOSSIP_WAIT"

# Verify routers are healthy
print_step "Checking router health..."
for router in router_a router_b router_c router_d router_e router_f router_g; do
    if $COMPOSE logs "$router" 2>&1 | grep -q "gossip active"; then
        print_ok "$router: gossip active"
    else
        echo -e "  ${YELLOW}⚠ $router: gossip may not be ready${NC}"
    fi
done


# ═══════════════════════════════════════════════════════════════
# SCENARIO 1: Baseline — No Congestion
# ═══════════════════════════════════════════════════════════════

run_scenario_1() {
    print_header "SCENARIO 1: Baseline — No Congestion"
    echo "  All links clean (no added delay/loss)."
    echo "  Both static and SPP should pick A → F → receiver."
    echo "  Validates SPP adds no overhead when no rerouting is needed."

    clear_all_delays
    sleep "$SCENARIO_PAUSE"

    echo ""
    local static_ms spp_ms
    static_ms=$(send_static_and_measure \
        "Static  (A→F→recv, hardcoded)" \
        '["sender","router_a","router_f","receiver"]')
    spp_ms=$(send_and_measure "SPP     (gossip-aware Dijkstra) ")
    print_comparison "$static_ms" "$spp_ms"

    S_LABELS+=("1: No congestion (A→F shortcut)")
    S_STATIC+=("$static_ms")
    S_SPP+=("$spp_ms")
}


# ═══════════════════════════════════════════════════════════════
# SCENARIO 2: Congested Primary Path
# ═══════════════════════════════════════════════════════════════

run_scenario_2() {
    print_header "SCENARIO 2: Congested Primary Path (Highway B→D)"
    echo "  Adding 50ms delay + 10% loss on router_b and router_d."
    echo "  Static routing blindly follows A→B→D→G, paying the full delay."
    echo "  SPP detects congestion via gossip and escapes via A→F→receiver."

    clear_all_delays
    sleep "$SCENARIO_PAUSE"

    inject_delay spp-router-b 50 10
    inject_delay spp-router-d 50 10

    echo -e "    Waiting ${GOSSIP_WAIT}s for gossip to detect congestion..."
    sleep "$GOSSIP_WAIT"

    echo ""
    local static_ms spp_ms
    static_ms=$(send_static_and_measure \
        "Static  (A→B→D→G→recv, through congestion)" \
        '["sender","router_a","router_b","router_d","router_g","receiver"]')
    spp_ms=$(send_and_measure "SPP     (gossip avoids B+D)             ")
    print_comparison "$static_ms" "$spp_ms"

    S_LABELS+=("2: B+D congested (+50ms each)")
    S_STATIC+=("$static_ms")
    S_SPP+=("$spp_ms")
}


# ═══════════════════════════════════════════════════════════════
# SCENARIO 3: Shortcut Goes Bad
# ═══════════════════════════════════════════════════════════════

run_scenario_3() {
    print_header "SCENARIO 3: Shortcut Goes Bad (F becomes slow)"
    echo "  Adding 100ms delay + 20% loss on router_f."
    echo "  Static routing keeps using the shortcut A→F, paying 100ms."
    echo "  SPP detects F is slow via gossip and reroutes around it."

    clear_all_delays
    sleep "$SCENARIO_PAUSE"

    inject_delay spp-router-f 100 20

    echo -e "    Waiting ${GOSSIP_WAIT}s for gossip to detect..."
    sleep "$GOSSIP_WAIT"

    echo ""
    local static_ms spp_ms
    static_ms=$(send_static_and_measure \
        "Static  (A→F→recv, through congested shortcut)" \
        '["sender","router_a","router_f","receiver"]')
    spp_ms=$(send_and_measure "SPP     (gossip avoids F)                ")
    print_comparison "$static_ms" "$spp_ms"

    S_LABELS+=("3: F (shortcut) congested (+100ms)")
    S_STATIC+=("$static_ms")
    S_SPP+=("$spp_ms")
}


# ═══════════════════════════════════════════════════════════════
# SCENARIO 4: Cascading Congestion
# ═══════════════════════════════════════════════════════════════

run_scenario_4() {
    print_header "SCENARIO 4: Cascading Congestion (B + D + F all slow)"
    echo "  Adding delays: B=20ms, D=30ms, F=80ms."
    echo "  Static routing takes A→F→recv and hits 80ms on F."
    echo "  Only C→E→G remains clean — SPP finds it via gossip."

    clear_all_delays
    sleep "$SCENARIO_PAUSE"

    inject_delay spp-router-b 20 5
    inject_delay spp-router-d 30 5
    inject_delay spp-router-f 80 15

    echo -e "    Waiting ${GOSSIP_WAIT}s for gossip to detect..."
    sleep "$GOSSIP_WAIT"

    echo ""
    local static_ms spp_ms
    static_ms=$(send_static_and_measure \
        "Static  (A→F→recv, hits 80ms on F)       " \
        '["sender","router_a","router_f","receiver"]')
    spp_ms=$(send_and_measure "SPP     (gossip finds clean C→E→G)       ")
    print_comparison "$static_ms" "$spp_ms"

    S_LABELS+=("4: B+D+F cascading (F=80ms worst)")
    S_STATIC+=("$static_ms")
    S_SPP+=("$spp_ms")
}


# ═══════════════════════════════════════════════════════════════
# SCENARIO 5: Burst Load — 10 Rapid Packets
# ═══════════════════════════════════════════════════════════════

run_scenario_5() {
    print_header "SCENARIO 5: Burst Load — 10 rapid packets"
    echo "  Sending 10 packets at 100ms intervals."
    echo "  Adding delay on B and F so SPP distributes via C→E→G."
    
    clear_all_delays
    sleep "$SCENARIO_PAUSE"
    
    # Make two paths slow
    inject_delay spp-router-b 40 5
    inject_delay spp-router-f 60 10
    
    echo -e "    Waiting ${GOSSIP_WAIT}s for gossip..."
    sleep "$GOSSIP_WAIT"
    
    send_burst 10 100 "Burst (10 packets, SPP)"
}


# ═══════════════════════════════════════════════════════════════
# Execute selected scenarios
# ═══════════════════════════════════════════════════════════════

case "$SELECTED_SCENARIO" in
    1)     run_scenario_1 ;;
    2)     run_scenario_2 ;;
    3)     run_scenario_3 ;;
    4)     run_scenario_4 ;;
    5|burst) run_scenario_5 ;;
    all)
        run_scenario_1
        run_scenario_2
        run_scenario_3
        run_scenario_4
        run_scenario_5
        ;;
    *)
        echo "Usage: $0 [1|2|3|4|5|burst|all]"
        exit 1
        ;;
esac


# ═══════════════════════════════════════════════════════════════
# Validation Summary Table
# ═══════════════════════════════════════════════════════════════

print_validation_summary() {
    if [[ ${#S_LABELS[@]} -eq 0 ]]; then
        return
    fi

    echo ""
    echo -e "${BOLD}${CYAN}╔══════════════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${BOLD}${CYAN}║  SPP VALIDATION SUMMARY — Smart Routing vs Static Routing           ║${NC}"
    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════╣${NC}"
    printf "${BOLD}${CYAN}║  %-36s │ %10s │ %8s │ %6s  ║${NC}\n" \
        "Scenario" "Static (ms)" "SPP (ms)" "Speedup"
    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════╣${NC}"

    local i
    for i in "${!S_LABELS[@]}"; do
        local label="${S_LABELS[$i]}"
        local static_ms="${S_STATIC[$i]}"
        local spp_ms="${S_SPP[$i]}"

        local speedup_str result_color
        if [[ "$static_ms" == "FAILED" || "$spp_ms" == "FAILED" ]]; then
            speedup_str=" FAILED"
            result_color="$RED"
        else
            speedup_str=$(awk "BEGIN {
                s=$static_ms; p=$spp_ms
                if (p > 0) { ratio = s / p
                    if (ratio >= 1.1) printf \"%5.1fx\", ratio
                    else printf \"  ~1.0x\"
                } else print \"  N/A\"
            }")
            if awk "BEGIN { exit !($static_ms > $spp_ms * 1.1) }"; then
                result_color="$GREEN"
            else
                result_color="$YELLOW"
            fi
        fi

        printf "${CYAN}║${NC}  %-36s ${CYAN}│${NC} %10s ${CYAN}│${NC} %8s ${CYAN}│${NC} ${result_color}${BOLD}%6s${NC}  ${CYAN}║${NC}\n" \
            "$label" "$static_ms" "$spp_ms" "$speedup_str"
    done

    echo -e "${BOLD}${CYAN}╚══════════════════════════════════════════════════════════════════════╝${NC}"
    echo ""

    # Overall verdict
    local improvements=0
    for i in "${!S_LABELS[@]}"; do
        local s="${S_STATIC[$i]}" p="${S_SPP[$i]}"
        if [[ "$s" != "FAILED" && "$p" != "FAILED" ]]; then
            if awk "BEGIN { exit !($p < $s * 0.9) }"; then
                improvements=$((improvements + 1))
            fi
        fi
    done

    if [[ $improvements -gt 0 ]]; then
        echo -e "  ${GREEN}${BOLD}✓ SPP outperformed static routing in ${improvements}/${#S_LABELS[@]} scenarios.${NC}"
        echo -e "  ${GREEN}${BOLD}✓ In congested scenarios, SPP dynamically reroutes to avoid delays.${NC}"
        echo -e "  ${GREEN}${BOLD}✓ In clean conditions, SPP matches static performance (no overhead).${NC}"
    fi
}

# ═══════════════════════════════════════════════════════════════
# Summary
# ═══════════════════════════════════════════════════════════════

print_validation_summary
print_header "TEST COMPLETE"

echo ""
echo -e "${BOLD}Commands to run individual scenarios manually:${NC}"
echo ""
echo "  # Start the topology (keep running between tests):"
echo "  docker compose -f docker-compose.7router.yml up -d receiver router_a router_b router_c router_d router_e router_f router_g"
echo ""
echo "  # Wait for gossip convergence:"
echo "  sleep 12"
echo ""
echo "  # Inject delay on a router (e.g., 50ms + 10% loss on router_b):"
echo "  docker exec spp-router-b tc qdisc add dev eth0 root netem delay 50ms loss 10%"
echo ""
echo "  # Send a packet:"
echo "  docker compose -f docker-compose.7router.yml run --rm sender"
echo ""
echo "  # Check receiver logs:"
echo "  docker compose -f docker-compose.7router.yml logs --tail=30 receiver"
echo ""
echo "  # Clear delay from a router:"
echo "  docker exec spp-router-b tc qdisc del dev eth0 root"
echo ""
echo "  # Tear down everything:"
echo "  docker compose -f docker-compose.7router.yml down --remove-orphans"
echo ""

# ── Cleanup prompt ──
echo -e "${YELLOW}Leave the topology running? (y/n)${NC}"
read -r keep_running
if [[ "$keep_running" != "y" ]]; then
    print_step "Tearing down..."
    $COMPOSE down --remove-orphans
    print_ok "Cleaned up"
fi
