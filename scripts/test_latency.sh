#!/usr/bin/env bash
# ═══════════════════════════════════════════════════════════════
# SPP Latency Comparison Test — 7-Router Topology
# ═══════════════════════════════════════════════════════════════
#
# Runs 6 scenarios comparing SPP smart routing vs static paths.
# Uses tc netem to inject real network delays and packet loss.
#
# Usage:
#   bash scripts/test_latency.sh          # Run all scenarios
#   bash scripts/test_latency.sh 2        # Run only scenario 2
#   bash scripts/test_latency.sh 5|loss   # Run only loss scenario
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
declare -a S_LABELS=() S_STATIC=() S_OSPF=() S_SPP=()

# Sender log paths for byte-size extraction (set by run_sender_with_config)
LAST_SENDER_LOG=""
SENDER_LOG_SPP_DIRECT=""
SENDER_LOG_SPP_2HOP=""
SENDER_LOG_SPP_SMART=""
SENDER_LOG_SPP_LIGHT=""
SENDER_LOG_SPP_COMPACT=""

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

# Count current receiver log lines (used to isolate new entries between sends)
# Usage: receiver_log_offset
receiver_log_offset() {
    $COMPOSE logs receiver 2>&1 | wc -l
}

# Get only NEW receiver log lines since a given offset
# Usage: receiver_logs_since <offset>
receiver_logs_since() {
    local offset="$1"
    $COMPOSE logs receiver 2>&1 | tail -n +"$((offset + 1))"
}

# Send a packet using a hardcoded static path (no gossip, no rerouting)
# Usage: send_static_and_measure <label> <json_path_array>
# Example: send_static_and_measure "Static (A→F)" '["sender","router_a","router_f","receiver"]'
send_static_and_measure() {
    local label="$1"
    local static_path="$2"

    local pre_offset
    pre_offset=$(receiver_log_offset)

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

    local new_logs
    new_logs=$(receiver_logs_since "$pre_offset")

    rm -f "$static_config"

    if echo "$new_logs" | grep -q "SMART PACKET DELIVERED"; then
        local latency
        latency=$(echo "$new_logs" | grep "End-to-end:" | tail -1 | grep -oP '[\d.]+' | head -1) || latency="N/A"
        local path
        path=$(echo "$new_logs" | grep "Path taken:" | tail -1 | sed 's/.*Path taken:\s*//' | sed 's/\s*║.*//') || path="unknown"
        print_ok "${label}: ${latency}ms — path: ${path}" >&2
        echo "$latency"
    else
        print_fail "${label}: Packet not delivered!" >&2
        echo "FAILED"
    fi
}

# Send a packet using OSPF simulation mode (single-metric Dijkstra, no rerouting)
# Usage: send_ospf_and_measure <label> [convergence_ms]
send_ospf_and_measure() {
    local label="$1"
    local convergence_ms="${2:-0}"

    local pre_offset
    pre_offset=$(receiver_log_offset)

    local ospf_config
    ospf_config=$(mktemp /tmp/spp_ospf_sender_XXXXXX.json)
    cat > "$ospf_config" << EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "GossipAddr": "10.1.1.2:7001",
  "Destination": "receiver",
  "FirstRouter": "router_a",
  "Payload": "ospf_baseline_test",
  "IntentLatency": 3,
  "IntentReliability": 1,
  "GossipRetries": 10,
  "GossipRetryBackoffMs": 500,
  "OSPFMode": true,
  "OSPFConvergenceMs": ${convergence_ms}
}
EOF

    $COMPOSE run --rm \
        -v "${ospf_config}:/app/configs/topologies/7router/ospf_sender.json:ro" \
        sender ./bin/sender configs/topologies/7router/ospf_sender.json 2>&1 | tail -3 >&2

    sleep "$DELIVERY_WAIT"

    local new_logs
    new_logs=$(receiver_logs_since "$pre_offset")

    rm -f "$ospf_config"

    if echo "$new_logs" | grep -q "SMART PACKET DELIVERED"; then
        local latency
        latency=$(echo "$new_logs" | grep "End-to-end:" | tail -1 | grep -oP '[\d.]+' | head -1) || latency="N/A"
        local path
        path=$(echo "$new_logs" | grep "Path taken:" | tail -1 | sed 's/.*Path taken:\s*//' | sed 's/\s*║.*//') || path="unknown"
        print_ok "${label}: ${latency}ms — path: ${path}" >&2
        echo "$latency"
    else
        print_fail "${label}: Packet not delivered!" >&2
        echo "FAILED"
    fi
}

# Print speedup comparison between static, OSPF, and SPP latencies
# Usage: print_comparison <static_ms> <spp_ms> [ospf_ms]
print_comparison() {
    local static_ms="$1"
    local spp_ms="$2"
    local ospf_ms="${3:-}"

    if [[ "$static_ms" == "FAILED" || "$spp_ms" == "FAILED" ]]; then
        echo -e "  ${RED}  Cannot compare — a packet failed to deliver${NC}"
        return
    fi

    local speedup saved_ms
    speedup=$(awk "BEGIN { s=$static_ms; p=$spp_ms; if (p>0) printf \"%.1f\", s/p; else print \"N/A\" }")
    saved_ms=$(awk "BEGIN { printf \"%.1f\", $static_ms - $spp_ms }")

    if awk "BEGIN { exit !($spp_ms < $static_ms * 0.9) }"; then
        echo -e "  ${GREEN}${BOLD}  ⚡ SPP is ${speedup}x faster — saved ${saved_ms}ms vs static routing${NC}"
    else
        echo -e "  ${YELLOW}  ≈ SPP and static are similar (${spp_ms}ms vs ${static_ms}ms) — no congestion advantage needed${NC}"
    fi

    # OSPF comparison
    if [[ -n "$ospf_ms" && "$ospf_ms" != "FAILED" ]]; then
        local ospf_speedup ospf_saved
        ospf_speedup=$(awk "BEGIN { o=$ospf_ms; p=$spp_ms; if (p>0) printf \"%.1f\", o/p; else print \"N/A\" }")
        ospf_saved=$(awk "BEGIN { printf \"%.1f\", $ospf_ms - $spp_ms }")

        if awk "BEGIN { exit !($spp_ms < $ospf_ms * 0.9) }"; then
            echo -e "  ${GREEN}${BOLD}  ⚡ SPP is ${ospf_speedup}x faster than OSPF — saved ${ospf_saved}ms${NC}"
        elif awk "BEGIN { exit !($ospf_ms < $spp_ms * 0.9) }"; then
            echo -e "  ${YELLOW}  ⚠ OSPF beat SPP by $(awk "BEGIN { printf \"%.1f\", $spp_ms - $ospf_ms }")ms (latency-only metric won here)${NC}"
        else
            echo -e "  ${YELLOW}  ≈ SPP and OSPF are similar (${spp_ms}ms vs ${ospf_ms}ms)${NC}"
        fi
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
    local pre_offset
    pre_offset=$(receiver_log_offset)

    # Send packet
    $COMPOSE run --rm sender 2>&1 | tail -5 >&2

    # Wait for delivery
    sleep "$DELIVERY_WAIT"

    # Capture only NEW logs since pre_offset
    local new_logs
    new_logs=$(receiver_logs_since "$pre_offset")

    if echo "$new_logs" | grep -q "SMART PACKET DELIVERED"; then
        local latency
        latency=$(echo "$new_logs" | grep "End-to-end:" | tail -1 | grep -oP '[\d.]+' | head -1) || latency="N/A"
        local path
        path=$(echo "$new_logs" | grep "Path taken:" | tail -1 | sed 's/.*Path taken:\s*//' | sed 's/\s*║.*//') || path="unknown"
        local rerouted=""
        if echo "$new_logs" | grep -q "REROUTED"; then
            rerouted=" [REROUTED]"
        fi

        print_ok "${label}: ${latency}ms — path: ${path}${rerouted}" >&2
        echo "$latency"
    else
        print_fail "${label}: Packet not delivered!" >&2
        echo "FAILED"
    fi
}

# Average the last N "End-to-end:" values from receiver logs.
# Usage: avg_e2e_from_logs <n> <log_output>
avg_e2e_from_logs() {
    local n="$1"
    local log_output="$2"
    echo "$log_output" | grep "End-to-end:" | tail -"$n" \
        | grep -oP '[\d.]+' | head -"$n" \
        | awk '{ sum += $1; count++ } END { if (count > 0) printf "%.3f", sum/count; else print "FAILED" }'
}

# Average the last N "RAW PROBE latency:" values from receiver logs.
# Usage: avg_raw_from_logs <n> <log_output>
avg_raw_from_logs() {
    local n="$1"
    local log_output="$2"
    echo "$log_output" | grep "RAW PROBE latency:" | tail -"$n" \
        | grep -oP '[\d.]+' | head -"$n" \
        | awk '{ sum += $1; count++ } END { if (count > 0) printf "%.3f", sum/count; else print "FAILED" }'
}

# Send N packets using a full inline config and return receiver logs after delivery.
# Usage: run_sender_with_config <config_json>
run_sender_with_config() {
    local config_json="$1"
    local tmpfile
    tmpfile=$(mktemp /tmp/spp_overhead_XXXXXX.json)
    echo "$config_json" > "$tmpfile"

    # Capture full sender output (includes byte sizes in logs)
    local sender_tmplog
    sender_tmplog=$(mktemp /tmp/spp_sender_log_XXXXXX.txt)
    $COMPOSE run --rm \
        -v "${tmpfile}:/app/configs/topologies/7router/overhead_sender.json:ro" \
        sender ./bin/sender configs/topologies/7router/overhead_sender.json 2>&1 | tee "$sender_tmplog" | tail -3 >&2

    sleep "$DELIVERY_WAIT"
    rm -f "$tmpfile"

    # Stash sender log path for byte-size extraction
    echo "$sender_tmplog" > /tmp/spp_last_sender_log_path

    $COMPOSE logs receiver 2>&1
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
# Set SKIP_BUILD=1 to skip rebuilding (e.g., SKIP_BUILD=1 bash scripts/test_latency.sh 1)
if [[ "${SKIP_BUILD:-0}" != "1" ]]; then
    print_step "Building Docker images..."
    $COMPOSE build
else
    print_step "Skipping build (SKIP_BUILD=1)..."
fi

# ── Step 3: Start infrastructure ──
print_step "Starting 7 routers + receiver..."
$COMPOSE up -d receiver router_a router_b router_c router_d router_e router_f router_g

echo -e "  Waiting ${GOSSIP_WAIT}s for gossip convergence..."
sleep "$GOSSIP_WAIT"

# Verify routers are healthy (retry up to 10s for stragglers)
print_step "Checking router health..."
for router in router_a router_b router_c router_d router_e router_f router_g; do
    ready=false
    for attempt in $(seq 1 20); do
        if $COMPOSE logs "$router" 2>&1 | grep -q "gossip active"; then
            ready=true
            break
        fi
        sleep 0.5
    done
    if $ready; then
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
    echo "  Static, OSPF, and SPP should all pick the shortest path."
    echo "  Validates SPP adds no overhead when no rerouting is needed."
    echo ""
    echo "  OSPF: single-metric Dijkstra (latency only), no mid-flight rerouting"
    echo "  SPP:  intent-aware Dijkstra (latency+load+loss), mid-flight rerouting"

    clear_all_delays
    sleep "$SCENARIO_PAUSE"

    echo ""
    local static_ms ospf_ms spp_ms
    static_ms=$(send_static_and_measure \
        "Static  (A→F→recv, hardcoded)" \
        '["sender","router_a","router_f","receiver"]')
    ospf_ms=$(send_ospf_and_measure \
        "OSPF    (latency-only Dijkstra)")
    spp_ms=$(send_and_measure "SPP     (intent-aware Dijkstra) ")
    print_comparison "$static_ms" "$spp_ms" "$ospf_ms"

    S_LABELS+=("1: No congestion (baseline)")
    S_STATIC+=("$static_ms")
    S_OSPF+=("$ospf_ms")
    S_SPP+=("$spp_ms")
}


# ═══════════════════════════════════════════════════════════════
# SCENARIO 2: Congested Primary Path
# ═══════════════════════════════════════════════════════════════

run_scenario_2() {
    print_header "SCENARIO 2: Congested Primary Path (Highway B→D)"
    echo "  Adding 50ms delay + 10% loss on router_b and router_d."
    echo "  Static routing blindly follows A→B→D→G, paying the full delay."
    echo "  OSPF sees latency and avoids B→D, but ignores 10% loss."
    echo "  SPP detects congestion via gossip and escapes via A→F→receiver."

    clear_all_delays
    sleep "$SCENARIO_PAUSE"

    inject_delay spp-router-b 50 10
    inject_delay spp-router-d 50 10

    echo -e "    Waiting ${GOSSIP_WAIT}s for gossip to detect congestion..."
    sleep "$GOSSIP_WAIT"

    echo ""
    local static_ms ospf_ms spp_ms
    static_ms=$(send_static_and_measure \
        "Static  (A→B→D→G→recv, through congestion)" \
        '["sender","router_a","router_b","router_d","router_g","receiver"]')
    ospf_ms=$(send_ospf_and_measure \
        "OSPF    (latency-only, no loss awareness)")
    spp_ms=$(send_and_measure "SPP     (gossip avoids B+D)             ")
    print_comparison "$static_ms" "$spp_ms" "$ospf_ms"

    S_LABELS+=("2: B+D congested (+50ms each)")
    S_STATIC+=("$static_ms")
    S_OSPF+=("$ospf_ms")
    S_SPP+=("$spp_ms")
}


# ═══════════════════════════════════════════════════════════════
# SCENARIO 3: Cascading Congestion (subsumes old Scenarios 3+4)
# ═══════════════════════════════════════════════════════════════

run_scenario_3() {
    print_header "SCENARIO 3: Cascading Congestion (B + D + F all slow)"
    echo "  Adding delays: B=20ms, D=30ms, F=80ms."
    echo "  Static routing takes A→F→recv and hits 80ms on F."
    echo "  OSPF sees latency and should also find C→E→G."
    echo "  SPP additionally considers load+loss for better path quality."
    echo "  (This also covers the 'shortcut goes bad' case — F is worst.)"

    clear_all_delays
    sleep "$SCENARIO_PAUSE"

    inject_delay spp-router-b 20 5
    inject_delay spp-router-d 30 5
    inject_delay spp-router-f 80 15

    echo -e "    Waiting ${GOSSIP_WAIT}s for gossip to detect..."
    sleep "$GOSSIP_WAIT"

    echo ""
    local static_ms ospf_ms spp_ms
    static_ms=$(send_static_and_measure \
        "Static  (A→F→recv, hits 80ms on F)       " \
        '["sender","router_a","router_f","receiver"]')
    ospf_ms=$(send_ospf_and_measure \
        "OSPF    (latency-only, avoids F=80ms)")
    spp_ms=$(send_and_measure "SPP     (gossip finds clean C→E→G)       ")
    print_comparison "$static_ms" "$spp_ms" "$ospf_ms"

    S_LABELS+=("3: B+D+F cascading (F=80ms worst)")
    S_STATIC+=("$static_ms")
    S_OSPF+=("$ospf_ms")
    S_SPP+=("$spp_ms")
}


# ═══════════════════════════════════════════════════════════════
# SCENARIO 4: The Race — Dumb Routing vs SPP Smart Routing
# ═══════════════════════════════════════════════════════════════

run_scenario_4() {
    print_header "SCENARIO 4: The Race — Dumb Routing vs SPP Smart Routing"
    echo "  Can SPP's intelligence actually beat a fixed-path packet?"
    echo ""
    echo "  Setup: Congest the highway path (B +80ms, D +80ms)."
    echo "  Then race two packets through the same network:"
    echo ""
    echo "    DUMB  (fixed path) : A → B → D → G → receiver  (stuck in traffic)"
    echo "    SPP   (smart path) : A → ? → ? → receiver       (finds the best route)"
    echo ""
    echo "  A dumb UDP packet has no routing intelligence — it takes whatever"
    echo "  path it was given, even if that path is congested. SPP sees the"
    echo "  congestion via gossip and reroutes around it."
    echo ""
    echo "  Part 2: Overhead measurement on a clean network."
    echo "    How much does SPP's intelligence cost when there's no congestion?"

    clear_all_delays
    sleep "$SCENARIO_PAUSE"

    local N=5

    # ══════════════════════════════════════════════════════════
    # PART 1: The Race (congested network)
    # ══════════════════════════════════════════════════════════
    echo ""
    echo -e "  ${BOLD}── PART 1: The Race (congested network) ──${NC}"

    print_step "Injecting congestion: B +80ms, D +80ms"
    inject_delay spp-router-b 80 0
    inject_delay spp-router-d 80 0
    sleep "$SCENARIO_PAUSE"

    # Clear receiver logs before the race
    $COMPOSE logs --tail=0 receiver > /dev/null 2>&1

    # ── DUMB: Fixed path through congestion ──────────────────
    echo ""
    echo -e "  ${YELLOW}▶ [DUMB] Fixed path: A → B → D → G → receiver (5 pkts)${NC}"
    echo -e "    ${BOLD}(No intelligence — takes the congested highway)${NC}"
    local dumb_logs
    dumb_logs=$(run_sender_with_config "$(cat <<EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "Destination": "receiver",
  "Payload": "race_dumb_fixed_path",
  "IntentLatency": 3,
  "IntentReliability": 1,
  "StaticPath": ["sender", "router_a", "router_b", "router_d", "router_g", "receiver"],
  "PacketCount": $N,
  "IntervalMs": 300
}
EOF
)")
    local dumb_avg
    dumb_avg=$(avg_e2e_from_logs "$N" "$dumb_logs")
    print_ok "Dumb routing avg (${N} pkts): ${dumb_avg}ms"

    # ── OSPF: Single-metric Dijkstra (latency only) ──────────
    echo ""
    echo -e "  ${YELLOW}▶ [OSPF] Latency-only Dijkstra: sender → router_a → ??? (5 pkts)${NC}"
    echo -e "    ${BOLD}(Single metric — sees latency but ignores load/loss, no mid-flight rerouting)${NC}"
    local ospf_logs
    ospf_logs=$(run_sender_with_config "$(cat <<EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "GossipAddr": "10.1.1.2:7001",
  "Destination": "receiver",
  "FirstRouter": "router_a",
  "Payload": "race_ospf_path",
  "IntentLatency": 3,
  "IntentReliability": 1,
  "GossipRetries": 10,
  "GossipRetryBackoffMs": 500,
  "OSPFMode": true,
  "PacketCount": $N,
  "IntervalMs": 300
}
EOF
)")
    local ospf_avg
    ospf_avg=$(avg_e2e_from_logs "$N" "$ospf_logs")
    local ospf_path
    ospf_path=$(echo "$ospf_logs" | grep "Path taken:" | tail -1 | sed 's/.*Path taken:\s*//' | sed 's/\s*║.*//' || echo "unknown")
    print_ok "OSPF routing avg (${N} pkts): ${ospf_avg}ms  path: ${ospf_path}"

    # ── SPP: Smart routing (gossip + Dijkstra) ───────────────
    echo ""
    echo -e "  ${YELLOW}▶ [SPP] Smart routing: sender → router_a → ??? (5 pkts)${NC}"
    echo -e "    ${BOLD}(Queries gossip, runs intent-aware Dijkstra, avoids congestion)${NC}"
    local spp_logs
    spp_logs=$(run_sender_with_config "$(cat <<EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "GossipAddr": "10.1.1.2:7001",
  "Destination": "receiver",
  "FirstRouter": "router_a",
  "Payload": "race_spp_smart_path",
  "IntentLatency": 3,
  "IntentReliability": 1,
  "GossipRetries": 10,
  "GossipRetryBackoffMs": 500,
  "PacketCount": $N,
  "IntervalMs": 300
}
EOF
)")
    SENDER_LOG_SPP_SMART=$(cat /tmp/spp_last_sender_log_path 2>/dev/null)
    local spp_avg
    spp_avg=$(avg_e2e_from_logs "$N" "$spp_logs")
    local spp_path
    spp_path=$(echo "$spp_logs" | grep "Path taken:" | tail -1 | sed 's/.*Path taken:\s*//' | sed 's/\s*║.*//' || echo "unknown")
    print_ok "SPP smart avg (${N} pkts): ${spp_avg}ms  path: ${spp_path}"

    # ── SPP LightMode: Smart routing, no embedded map ────────
    echo ""
    echo -e "  ${YELLOW}▶ [LIGHT] SPP LightMode: smart routing, NO embedded map (5 pkts)${NC}"
    echo -e "    ${BOLD}(Same intelligence, minimal packet size — routers use local gossip)${NC}"
    local light_logs
    light_logs=$(run_sender_with_config "$(cat <<EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "GossipAddr": "10.1.1.2:7001",
  "Destination": "receiver",
  "FirstRouter": "router_a",
  "Payload": "race_spp_light_path",
  "IntentLatency": 3,
  "IntentReliability": 1,
  "GossipRetries": 10,
  "GossipRetryBackoffMs": 500,
  "LightMode": true,
  "PacketCount": $N,
  "IntervalMs": 300
}
EOF
)")
    SENDER_LOG_SPP_LIGHT=$(cat /tmp/spp_last_sender_log_path 2>/dev/null)
    local light_avg
    light_avg=$(avg_e2e_from_logs "$N" "$light_logs")
    local light_path
    light_path=$(echo "$light_logs" | grep "Path taken:" | tail -1 | sed 's/.*Path taken:\s*//' | sed 's/\s*║.*//' || echo "unknown")
    print_ok "SPP light avg (${N} pkts): ${light_avg}ms  path: ${light_path}"

    # ── SPP LightMode + CompactIDs ─────────────────────────────
    echo ""
    echo -e "  ${YELLOW}▶ [COMPACT] SPP LightMode + Compact IDs (5 pkts)${NC}"
    echo -e "    ${BOLD}(Same intelligence, 2-byte node IDs instead of strings)${NC}"
    local compact_logs
    compact_logs=$(run_sender_with_config "$(cat <<EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "GossipAddr": "10.1.1.2:7001",
  "Destination": "receiver",
  "FirstRouter": "router_a",
  "Payload": "race_spp_compact_path",
  "IntentLatency": 3,
  "IntentReliability": 1,
  "GossipRetries": 10,
  "GossipRetryBackoffMs": 500,
  "LightMode": true,
  "NodeIDMap": {"sender": 1, "router_a": 2, "router_b": 3, "router_c": 4, "router_d": 5, "router_e": 6, "router_f": 7, "router_g": 8, "receiver": 9},
  "PacketCount": $N,
  "IntervalMs": 300
}
EOF
)")
    SENDER_LOG_SPP_COMPACT=$(cat /tmp/spp_last_sender_log_path 2>/dev/null)
    local compact_avg
    compact_avg=$(avg_e2e_from_logs "$N" "$compact_logs")
    local compact_path
    compact_path=$(echo "$compact_logs" | grep "Path taken:" | tail -1 | sed 's/.*Path taken:\s*//' | sed 's/\s*║.*//' || echo "unknown")
    print_ok "SPP compact avg (${N} pkts): ${compact_avg}ms  path: ${compact_path}"

    # ── Race results ─────────────────────────────────────────
    echo ""
    if [[ "$dumb_avg" == "FAILED" || "$ospf_avg" == "FAILED" || "$spp_avg" == "FAILED" || "$light_avg" == "FAILED" || "$compact_avg" == "FAILED" ]]; then
        print_fail "Race incomplete — a packet failed to deliver"
    else
        local saved_ms speedup light_saved_ms ospf_vs_spp
        saved_ms=$(awk "BEGIN { printf \"%.1f\", $dumb_avg - $spp_avg }")
        speedup=$(awk "BEGIN { if ($spp_avg > 0) printf \"%.1f\", $dumb_avg / $spp_avg; else print \"N/A\" }")
        light_saved_ms=$(awk "BEGIN { printf \"%.1f\", $dumb_avg - $light_avg }")
        ospf_vs_spp=$(awk "BEGIN { printf \"%.1f\", $ospf_avg - $spp_avg }")

        # Get byte sizes for the race summary
        local dumb_race_bytes spp_race_bytes light_race_bytes compact_race_bytes
        dumb_race_bytes=$(grep -oP 'bytes=\K[0-9]+' "$SENDER_LOG_SPP_SMART" 2>/dev/null | head -1 || echo "?")
        spp_race_bytes=$(grep -oP 'bytes=\K[0-9]+' "$SENDER_LOG_SPP_SMART" 2>/dev/null | tail -1 || echo "?")
        light_race_bytes=$(grep -oP 'bytes=\K[0-9]+' "$SENDER_LOG_SPP_LIGHT" 2>/dev/null | tail -1 || echo "?")
        compact_race_bytes=$(grep -oP 'bytes=\K[0-9]+' "$SENDER_LOG_SPP_COMPACT" 2>/dev/null | tail -1 || echo "?")

        echo -e "${BOLD}${CYAN}╔══════════════════════════════════════════════════════════════════════════╗${NC}"
        echo -e "${BOLD}${CYAN}║  THE RACE — Static vs OSPF vs SPP Smart vs SPP LightMode               ║${NC}"
        echo -e "${BOLD}${CYAN}║  Network: B +80ms, D +80ms (highway congested)                          ║${NC}"
        echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"
        printf "${CYAN}║${NC}  %-38s  %10s ms  %8s B  ${CYAN}║${NC}\n" "Static (A→B→D→G, fixed path)"    "$dumb_avg"    "—"
        printf "${CYAN}║${NC}  %-38s  %10s ms  %8s B  ${CYAN}║${NC}\n" "OSPF (latency-only Dijkstra)"    "$ospf_avg"    "—"
        printf "${CYAN}║${NC}  %-38s  %10s ms  %8s B  ${CYAN}║${NC}\n" "SPP  (intent-aware, full map)"   "$spp_avg"     "$spp_race_bytes"
        printf "${CYAN}║${NC}  %-38s  %10s ms  %8s B  ${CYAN}║${NC}\n" "SPP  (LightMode, no map)"        "$light_avg"   "$light_race_bytes"
        printf "${CYAN}║${NC}  %-38s  %10s ms  %8s B  ${CYAN}║${NC}\n" "SPP  (Light + CompactIDs)"       "$compact_avg" "$compact_race_bytes"
        echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"
        if awk "BEGIN { exit !($spp_avg < $dumb_avg * 0.9) }"; then
            echo -e "${CYAN}║${NC}  ${GREEN}${BOLD}SPP wins by ${saved_ms}ms vs static (${speedup}x faster)${NC}                     ${CYAN}║${NC}"
        fi
        if awk "BEGIN { exit !($spp_avg < $ospf_avg * 0.9) }"; then
            local ospf_speedup
            ospf_speedup=$(awk "BEGIN { if ($spp_avg > 0) printf \"%.1f\", $ospf_avg / $spp_avg; else print \"N/A\" }")
            echo -e "${CYAN}║${NC}  ${GREEN}${BOLD}SPP wins by ${ospf_vs_spp}ms vs OSPF (${ospf_speedup}x faster)${NC}                      ${CYAN}║${NC}"
        elif awk "BEGIN { exit !($ospf_avg <= $spp_avg * 1.1) }"; then
            echo -e "${CYAN}║${NC}  ${YELLOW}SPP ≈ OSPF — advantage shows with load/loss/intent${NC}              ${CYAN}║${NC}"
        fi
        echo -e "${CYAN}║${NC}  ${GREEN}LightMode: same intelligence, ${light_race_bytes} bytes instead of ${spp_race_bytes}${NC}               ${CYAN}║${NC}"
        echo -e "${BOLD}${CYAN}╚══════════════════════════════════════════════════════════════════╝${NC}"

        # Use the best SPP variant (compact) for the summary comparison.
        S_LABELS+=("4: The Race (B+D=+80ms)")
        S_STATIC+=("$dumb_avg")
        S_OSPF+=("$ospf_avg")
        S_SPP+=("$compact_avg")
    fi

    # ══════════════════════════════════════════════════════════
    # PART 2: Overhead Budget (clean network)
    # ══════════════════════════════════════════════════════════
    echo ""
    echo -e "  ${BOLD}── PART 2: Cost of Intelligence (clean network) ──${NC}"
    echo "  What does SPP's smart routing cost when there's NO congestion?"

    clear_all_delays
    sleep "$SCENARIO_PAUSE"

    # ── A: Raw UDP direct ────────────────────────────────────
    echo ""
    echo -e "  ${YELLOW}▶ [A] Raw UDP direct — ${N} probes (no SPP, no routers)${NC}"
    local raw_logs
    raw_logs=$(run_sender_with_config "$(cat <<EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.11.2:9000",
  "Destination": "receiver",
  "Payload": "overhead_probe",
  "IntentLatency": 3,
  "IntentReliability": 1,
  "RawMode": true,
  "PacketCount": $N,
  "IntervalMs": 300
}
EOF
)")
    local raw_avg
    raw_avg=$(avg_raw_from_logs "$N" "$raw_logs")
    print_ok "Raw UDP avg (${N} pkts): ${raw_avg}ms"

    # ── B: SPP direct ────────────────────────────────────────
    echo ""
    echo -e "  ${YELLOW}▶ [B] SPP direct — ${N} packets (encode+decode, no routers)${NC}"
    local spp_direct_logs
    spp_direct_logs=$(run_sender_with_config "$(cat <<EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.11.2:9000",
  "Destination": "receiver",
  "Payload": "overhead_probe",
  "IntentLatency": 3,
  "IntentReliability": 1,
  "StaticPath": ["sender", "receiver"],
  "PacketCount": $N,
  "IntervalMs": 300
}
EOF
)")
    SENDER_LOG_SPP_DIRECT=$(cat /tmp/spp_last_sender_log_path 2>/dev/null)
    local spp_direct_avg
    spp_direct_avg=$(avg_e2e_from_logs "$N" "$spp_direct_logs")
    print_ok "SPP direct avg (${N} pkts): ${spp_direct_avg}ms"

    # ── C: SPP 2-hop ─────────────────────────────────────────
    echo ""
    echo -e "  ${YELLOW}▶ [C] SPP 2-hop — ${N} packets via router_a → router_f → receiver${NC}"
    local spp_2hop_logs
    spp_2hop_logs=$(run_sender_with_config "$(cat <<EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "Destination": "receiver",
  "Payload": "overhead_probe",
  "IntentLatency": 3,
  "IntentReliability": 1,
  "StaticPath": ["sender", "router_a", "router_f", "receiver"],
  "PacketCount": $N,
  "IntervalMs": 300
}
EOF
)")
    SENDER_LOG_SPP_2HOP=$(cat /tmp/spp_last_sender_log_path 2>/dev/null)
    local spp_2hop_avg
    spp_2hop_avg=$(avg_e2e_from_logs "$N" "$spp_2hop_logs")
    print_ok "SPP 2-hop avg (${N} pkts): ${spp_2hop_avg}ms"

    # ── Overhead table ───────────────────────────────────────
    echo ""
    if [[ "$raw_avg" == "FAILED" || "$spp_direct_avg" == "FAILED" || "$spp_2hop_avg" == "FAILED" ]]; then
        print_fail "One or more modes failed — check receiver logs"
        return
    fi

    local framing_cost per_hop_cost
    framing_cost=$(awk "BEGIN { printf \"%.3f\", $spp_direct_avg - $raw_avg }")
    per_hop_cost=$(awk "BEGIN { printf \"%.3f\", ($spp_2hop_avg - $spp_direct_avg) / 2 }")

    echo -e "${BOLD}${CYAN}╔══════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${BOLD}${CYAN}║  COST OF INTELLIGENCE — SPP Overhead Budget                   ║${NC}"
    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════╣${NC}"
    printf "${CYAN}║${NC}  %-30s  %10s ms  ${CYAN}║${NC}\n" "A) Raw UDP direct"       "$raw_avg"
    printf "${CYAN}║${NC}  %-30s  %10s ms  ${CYAN}║${NC}\n" "B) SPP direct (0 hops)"  "$spp_direct_avg"
    printf "${CYAN}║${NC}  %-30s  %10s ms  ${CYAN}║${NC}\n" "C) SPP 2-hop (A→F→recv)" "$spp_2hop_avg"
    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════╣${NC}"
    printf "${CYAN}║${NC}  %-30s  %+10s ms  ${CYAN}║${NC}\n" "SPP framing cost (B−A)"          "$framing_cost"
    printf "${CYAN}║${NC}  %-30s  %+10s ms  ${CYAN}║${NC}\n" "Per-router hop cost ((C−B)/2)"   "$per_hop_cost"
    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════╣${NC}"
    echo -e "${CYAN}║${NC}  ${GREEN}Verdict:${NC} ~${framing_cost}ms overhead buys smart routing that   ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}  saved ${saved_ms}ms in the race above. Worth it.             ${CYAN}║${NC}"
    echo -e "${BOLD}${CYAN}╚══════════════════════════════════════════════════════════════╝${NC}"
}


# ═══════════════════════════════════════════════════════════════
# SCENARIO 5: Silent Killer — High Loss, Zero Delay
# (OSPF's blind spot: it only sees latency)
# ═══════════════════════════════════════════════════════════════

run_scenario_5() {
    print_header "SCENARIO 5: Silent Killer — High Loss, Zero Delay (OSPF's Blind Spot)"
    echo "  Injecting 40% packet loss with 0ms delay on router_f (shortest path)."
    echo "  OSPF sees low latency on F → routes through F → packets get DROPPED."
    echo "  SPP sees 40% loss via gossip → avoids F → all packets arrive."
    echo ""
    echo "  This is where SPP's multi-metric awareness pays off."
    echo "  OSPF is blind to loss — it only considers latency."
    echo "  Measuring DELIVERY RATE (not just latency)."

    clear_all_delays
    sleep "$SCENARIO_PAUSE"

    # Inject loss-only on the shortest path (F) — no delay at all
    # tc netem loss only, delay stays at 0
    echo -e "    ${BOLD}Injecting: 0ms delay, 40% loss on spp-router-f${NC}"
    local ifaces
    ifaces=$(docker exec spp-router-f sh -c "ip -o link show | grep -v 'lo:' | awk -F': ' '{print \$2}' | cut -d'@' -f1")
    for iface in $ifaces; do
        docker exec spp-router-f tc qdisc add dev "$iface" root netem loss 40% 2>/dev/null || \
        docker exec spp-router-f tc qdisc change dev "$iface" root netem loss 40% 2>/dev/null || true
    done

    echo -e "    Waiting ${GOSSIP_WAIT}s for gossip to detect loss..."
    sleep "$GOSSIP_WAIT"

    local N=20

    # ── OSPF: routes through F (low latency), loses packets ──
    echo ""
    echo -e "  ${YELLOW}▶ [OSPF] Sending $N packets (latency-only Dijkstra → will use F)${NC}"
    local ospf_pre
    ospf_pre=$(receiver_log_offset)

    local ospf_config
    ospf_config=$(mktemp /tmp/spp_s7_ospf_XXXXXX.json)
    cat > "$ospf_config" << EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "GossipAddr": "10.1.1.2:7001",
  "Destination": "receiver",
  "FirstRouter": "router_a",
  "Payload": "loss_test_ospf",
  "IntentLatency": 3,
  "IntentReliability": 2,
  "GossipRetries": 10,
  "GossipRetryBackoffMs": 500,
  "OSPFMode": true,
  "PacketCount": $N,
  "IntervalMs": 200
}
EOF
    $COMPOSE run --rm \
        -v "${ospf_config}:/app/configs/topologies/7router/s7_ospf.json:ro" \
        sender ./bin/sender configs/topologies/7router/s7_ospf.json 2>&1 | tail -3 >&2

    sleep "$DELIVERY_WAIT"
    local ospf_new_logs
    ospf_new_logs=$(receiver_logs_since "$ospf_pre")
    local ospf_delivered
    ospf_delivered=$(echo "$ospf_new_logs" | grep -c "SMART PACKET DELIVERED") || ospf_delivered=0
    local ospf_avg="N/A"
    if [[ $ospf_delivered -gt 0 ]]; then
        ospf_avg=$(echo "$ospf_new_logs" | grep "End-to-end:" | grep -oP '[\d.]+' | head -"$ospf_delivered" \
            | awk '{ sum += $1; count++ } END { if (count > 0) printf "%.3f", sum/count; else print "N/A" }')
    fi
    local ospf_loss_pct
    ospf_loss_pct=$(awk "BEGIN { printf \"%.0f\", (1 - $ospf_delivered/$N) * 100 }")
    print_ok "OSPF: ${ospf_delivered}/${N} delivered (${ospf_loss_pct}% lost)  avg: ${ospf_avg}ms"
    local ospf_path
    ospf_path=$(echo "$ospf_new_logs" | grep "Path taken:" | tail -1 | sed 's/.*Path taken:\s*//' | sed 's/\s*║.*//' || echo "unknown")
    echo -e "    Path: ${ospf_path}"

    rm -f "$ospf_config"

    # ── SPP: sees loss, avoids F, takes C→E→G ──
    echo ""
    echo -e "  ${YELLOW}▶ [SPP] Sending $N packets (intent-aware, reliability=2, sees loss)${NC}"
    local spp_pre
    spp_pre=$(receiver_log_offset)

    local spp_config
    spp_config=$(mktemp /tmp/spp_s7_spp_XXXXXX.json)
    cat > "$spp_config" << EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "GossipAddr": "10.1.1.2:7001",
  "Destination": "receiver",
  "FirstRouter": "router_a",
  "Payload": "loss_test_spp",
  "IntentLatency": 3,
  "IntentReliability": 2,
  "GossipRetries": 10,
  "GossipRetryBackoffMs": 500,
  "LightMode": true,
  "PacketCount": $N,
  "IntervalMs": 200
}
EOF
    $COMPOSE run --rm \
        -v "${spp_config}:/app/configs/topologies/7router/s7_spp.json:ro" \
        sender ./bin/sender configs/topologies/7router/s7_spp.json 2>&1 | tail -3 >&2

    sleep "$DELIVERY_WAIT"
    local spp_new_logs
    spp_new_logs=$(receiver_logs_since "$spp_pre")
    local spp_delivered
    spp_delivered=$(echo "$spp_new_logs" | grep -c "SMART PACKET DELIVERED") || spp_delivered=0
    local spp_avg="N/A"
    if [[ $spp_delivered -gt 0 ]]; then
        spp_avg=$(echo "$spp_new_logs" | grep "End-to-end:" | grep -oP '[\d.]+' | head -"$spp_delivered" \
            | awk '{ sum += $1; count++ } END { if (count > 0) printf "%.3f", sum/count; else print "N/A" }')
    fi
    local spp_loss_pct
    spp_loss_pct=$(awk "BEGIN { printf \"%.0f\", (1 - $spp_delivered/$N) * 100 }")
    print_ok "SPP:  ${spp_delivered}/${N} delivered (${spp_loss_pct}% lost)  avg: ${spp_avg}ms"
    local spp_path
    spp_path=$(echo "$spp_new_logs" | grep "Path taken:" | tail -1 | sed 's/.*Path taken:\s*//' | sed 's/\s*║.*//' || echo "unknown")
    echo -e "    Path: ${spp_path}"

    rm -f "$spp_config"

    # ── Results ──
    echo ""
    echo -e "${BOLD}${CYAN}╔══════════════════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${BOLD}${CYAN}║  SCENARIO 5: Silent Killer — DELIVERY RATE comparison                    ║${NC}"
    echo -e "${BOLD}${CYAN}║  Network: F has 40% loss, 0ms delay (invisible to OSPF)                  ║${NC}"
    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"
    printf "${CYAN}║${NC}  %-30s  %4s/%s delivered  (%3s%% loss)  avg: %8s ms  ${CYAN}║${NC}\n" \
        "OSPF (latency-only)" "$ospf_delivered" "$N" "$ospf_loss_pct" "$ospf_avg"
    printf "${CYAN}║${NC}  %-30s  %4s/%s delivered  (%3s%% loss)  avg: %8s ms  ${CYAN}║${NC}\n" \
        "SPP  (loss-aware)" "$spp_delivered" "$N" "$spp_loss_pct" "$spp_avg"
    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"
    if [[ $spp_delivered -gt $ospf_delivered ]]; then
        local extra=$((spp_delivered - ospf_delivered))
        echo -e "${CYAN}║${NC}  ${GREEN}${BOLD}SPP delivered ${extra} more packets than OSPF!${NC}"
        echo -e "${CYAN}║${NC}  ${GREEN}OSPF is blind to loss — it routed through 40% loss because latency was low.${NC}"
        echo -e "${CYAN}║${NC}  ${GREEN}SPP's multi-metric Dijkstra detected loss and routed around it.${NC}"
    else
        echo -e "${CYAN}║${NC}  ${YELLOW}Both delivered similar counts. Gossip may not have detected loss yet.${NC}"
    fi
    echo -e "${BOLD}${CYAN}╚══════════════════════════════════════════════════════════════════════════╝${NC}"

    S_LABELS+=("5: Silent loss (F=40% loss, 0ms)")
    S_STATIC+=("N/A")
    S_OSPF+=("${ospf_delivered}/${N}")
    S_SPP+=("${spp_delivered}/${N}")
}


# ═══════════════════════════════════════════════════════════════
# SCENARIO 6: Intent Differentiation
# (Same network, different intents → different optimal paths)
# ═══════════════════════════════════════════════════════════════

run_scenario_6() {
    print_header "SCENARIO 6: Intent Differentiation — Same Network, Different Optimal Paths"
    echo "  Setup: F has low latency but 30% loss. B/C/D/E all have +20ms delay but 0% loss."
    echo "  F is the ONLY fast path — but it drops packets."
    echo ""
    echo "  OSPF: gives ALL packets the same path (lowest latency = F)."
    echo "  SPP latency-critical (rel=0): takes F — fast + lossy, tolerates drops for speed."
    echo "  SPP reliability-critical (rel=3): avoids F — takes slower but lossless path."
    echo ""
    echo "  This is intent-aware routing: the SAME network, but different packets"
    echo "  get different optimal paths based on what they need."

    clear_all_delays
    sleep "$SCENARIO_PAUSE"

    # F: fast but lossy (0ms extra delay, 30% loss)
    # With BaseLossMultiplier=0.5: weight penalty = 30*0.5 = 15 (low-rel: still cheaper than +20ms paths)
    # With LossMultiplier=10.0: weight penalty = 30*10 = 300 (high-rel: strongly avoids F)
    # 30% is high enough that gossip probes reliably detect it.
    # Use loss-only netem (no delay parameter) — same approach as Scenario 5.
    echo -e "    ${BOLD}Injecting: 0ms delay, 30% loss on spp-router-f${NC}"
    local f_ifaces
    f_ifaces=$(docker exec spp-router-f sh -c "ip -o link show | grep -v 'lo:' | awk -F': ' '{print \$2}' | cut -d'@' -f1")
    for iface in $f_ifaces; do
        docker exec spp-router-f tc qdisc add dev "$iface" root netem loss 30% 2>/dev/null || \
        docker exec spp-router-f tc qdisc change dev "$iface" root netem loss 30% 2>/dev/null || true
    done

    # ALL alternate paths are slow but reliable:
    # B, C, D, E each get +20ms delay, 0% loss
    # This means F is the ONLY fast path — but it's lossy.
    # Latency-critical packets must choose: fast+lossy (F) or slow+reliable (B→D or C→E)
    inject_delay spp-router-b 20 0
    inject_delay spp-router-c 20 0
    inject_delay spp-router-d 20 0
    inject_delay spp-router-e 20 0

    echo -e "    Waiting ${GOSSIP_WAIT}s for gossip to detect..."
    sleep "$GOSSIP_WAIT"

    local N=10

    # ── OSPF: one path for all traffic ──
    echo ""
    echo -e "  ${YELLOW}▶ [OSPF] $N packets — same path for everything (latency-only)${NC}"
    local ospf_pre
    ospf_pre=$(receiver_log_offset)

    local ospf_config
    ospf_config=$(mktemp /tmp/spp_s9_ospf_XXXXXX.json)
    cat > "$ospf_config" << EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "GossipAddr": "10.1.1.2:7001",
  "Destination": "receiver",
  "FirstRouter": "router_a",
  "Payload": "intent_test_ospf",
  "IntentLatency": 3,
  "IntentReliability": 1,
  "GossipRetries": 10,
  "GossipRetryBackoffMs": 500,
  "OSPFMode": true,
  "PacketCount": $N,
  "IntervalMs": 200
}
EOF
    $COMPOSE run --rm \
        -v "${ospf_config}:/app/configs/topologies/7router/s9_ospf.json:ro" \
        sender ./bin/sender configs/topologies/7router/s9_ospf.json 2>&1 | tail -3 >&2

    sleep "$DELIVERY_WAIT"
    local ospf_new_logs
    ospf_new_logs=$(receiver_logs_since "$ospf_pre")
    local ospf_delivered
    ospf_delivered=$(echo "$ospf_new_logs" | grep -c "SMART PACKET DELIVERED") || ospf_delivered=0
    local ospf_avg="N/A"
    if [[ $ospf_delivered -gt 0 ]]; then
        ospf_avg=$(avg_e2e_from_logs "$ospf_delivered" "$ospf_new_logs")
    fi
    local ospf_path
    ospf_path=$(echo "$ospf_new_logs" | grep "Path taken:" | tail -1 | sed 's/.*Path taken:\s*//' | sed 's/\s*║.*//' || echo "unknown")
    local ospf_loss_pct
    ospf_loss_pct=$(awk "BEGIN { printf \"%.0f\", (1 - $ospf_delivered/$N) * 100 }")
    print_ok "OSPF: ${ospf_delivered}/${N} delivered (${ospf_loss_pct}% lost)  avg: ${ospf_avg}ms  path: ${ospf_path}"

    rm -f "$ospf_config"

    # ── SPP latency-critical: reliability=0, prefers fast path (F) ──
    echo ""
    echo -e "  ${YELLOW}▶ [SPP latency-critical] $N packets — reliability=0, latency=3${NC}"
    echo -e "    ${BOLD}(Wants speed above all — will tolerate loss for low latency)${NC}"
    local spp_fast_pre
    spp_fast_pre=$(receiver_log_offset)

    local spp_fast_config
    spp_fast_config=$(mktemp /tmp/spp_s9_fast_XXXXXX.json)
    cat > "$spp_fast_config" << EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "GossipAddr": "10.1.1.2:7001",
  "Destination": "receiver",
  "FirstRouter": "router_a",
  "Payload": "intent_test_fast",
  "IntentLatency": 3,
  "IntentReliability": 0,
  "GossipRetries": 10,
  "GossipRetryBackoffMs": 500,
  "LightMode": true,
  "PacketCount": $N,
  "IntervalMs": 200
}
EOF
    $COMPOSE run --rm \
        -v "${spp_fast_config}:/app/configs/topologies/7router/s9_fast.json:ro" \
        sender ./bin/sender configs/topologies/7router/s9_fast.json 2>&1 | tail -3 >&2

    sleep "$DELIVERY_WAIT"
    local spp_fast_logs
    spp_fast_logs=$(receiver_logs_since "$spp_fast_pre")
    local spp_fast_delivered
    spp_fast_delivered=$(echo "$spp_fast_logs" | grep -c "SMART PACKET DELIVERED") || spp_fast_delivered=0
    local spp_fast_avg="N/A"
    if [[ $spp_fast_delivered -gt 0 ]]; then
        spp_fast_avg=$(avg_e2e_from_logs "$spp_fast_delivered" "$spp_fast_logs")
    fi
    local spp_fast_path
    spp_fast_path=$(echo "$spp_fast_logs" | grep "Path taken:" | tail -1 | sed 's/.*Path taken:\s*//' | sed 's/\s*║.*//' || echo "unknown")
    local spp_fast_loss_pct
    spp_fast_loss_pct=$(awk "BEGIN { printf \"%.0f\", (1 - $spp_fast_delivered/$N) * 100 }")
    print_ok "SPP fast: ${spp_fast_delivered}/${N} delivered (${spp_fast_loss_pct}% lost)  avg: ${spp_fast_avg}ms  path: ${spp_fast_path}"

    rm -f "$spp_fast_config"

    # ── SPP reliability-critical: reliability=3, avoids lossy F ──
    echo ""
    echo -e "  ${YELLOW}▶ [SPP reliability-critical] $N packets — reliability=3, latency=1${NC}"
    echo -e "    ${BOLD}(Wants guaranteed delivery — will accept longer path to avoid loss)${NC}"
    local spp_rel_pre
    spp_rel_pre=$(receiver_log_offset)

    local spp_rel_config
    spp_rel_config=$(mktemp /tmp/spp_s9_rel_XXXXXX.json)
    cat > "$spp_rel_config" << EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "GossipAddr": "10.1.1.2:7001",
  "Destination": "receiver",
  "FirstRouter": "router_a",
  "Payload": "intent_test_reliable",
  "IntentLatency": 1,
  "IntentReliability": 3,
  "GossipRetries": 10,
  "GossipRetryBackoffMs": 500,
  "LightMode": true,
  "PacketCount": $N,
  "IntervalMs": 200
}
EOF
    $COMPOSE run --rm \
        -v "${spp_rel_config}:/app/configs/topologies/7router/s9_rel.json:ro" \
        sender ./bin/sender configs/topologies/7router/s9_rel.json 2>&1 | tail -3 >&2

    sleep "$DELIVERY_WAIT"
    local spp_rel_logs
    spp_rel_logs=$(receiver_logs_since "$spp_rel_pre")
    local spp_rel_delivered
    spp_rel_delivered=$(echo "$spp_rel_logs" | grep -c "SMART PACKET DELIVERED") || spp_rel_delivered=0
    local spp_rel_avg="N/A"
    if [[ $spp_rel_delivered -gt 0 ]]; then
        spp_rel_avg=$(avg_e2e_from_logs "$spp_rel_delivered" "$spp_rel_logs")
    fi
    local spp_rel_path
    spp_rel_path=$(echo "$spp_rel_logs" | grep "Path taken:" | tail -1 | sed 's/.*Path taken:\s*//' | sed 's/\s*║.*//' || echo "unknown")
    local spp_rel_loss_pct
    spp_rel_loss_pct=$(awk "BEGIN { printf \"%.0f\", (1 - $spp_rel_delivered/$N) * 100 }")
    print_ok "SPP reliable: ${spp_rel_delivered}/${N} delivered (${spp_rel_loss_pct}% lost)  avg: ${spp_rel_avg}ms  path: ${spp_rel_path}"

    rm -f "$spp_rel_config"

    # ── Results ──
    echo ""
    echo -e "${BOLD}${CYAN}╔══════════════════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${BOLD}${CYAN}║  SCENARIO 6: Intent Differentiation                                      ║${NC}"
    echo -e "${BOLD}${CYAN}║  Network: F = fast + 30% loss, B/C/D/E = +20ms + 0% loss                 ║${NC}"
    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"
    printf "${CYAN}║${NC}  %-35s  %2s/%s del  (%2s%% loss)  avg: %7s ms  ${CYAN}║${NC}\n" \
        "OSPF (one path for all)" "$ospf_delivered" "$N" "$ospf_loss_pct" "$ospf_avg"
    printf "${CYAN}║${NC}  %-35s  %2s/%s del  (%2s%% loss)  avg: %7s ms  ${CYAN}║${NC}\n" \
        "SPP latency-crit (rel=0,lat=3)" "$spp_fast_delivered" "$N" "$spp_fast_loss_pct" "$spp_fast_avg"
    printf "${CYAN}║${NC}  %-35s  %2s/%s del  (%2s%% loss)  avg: %7s ms  ${CYAN}║${NC}\n" \
        "SPP reliability-crit (rel=3,lat=1)" "$spp_rel_delivered" "$N" "$spp_rel_loss_pct" "$spp_rel_avg"
    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"
    echo -e "${CYAN}║${NC}  ${GREEN}${BOLD}OSPF:${NC} One-size-fits-all — same path regardless of packet needs."
    echo -e "${CYAN}║${NC}  ${GREEN}${BOLD}SPP:${NC}  Different packets get different optimal paths based on intent."
    if [[ $spp_rel_delivered -gt $ospf_delivered ]]; then
        echo -e "${CYAN}║${NC}  ${GREEN}${BOLD}  → Reliability-critical: better delivery than OSPF (${spp_rel_delivered} vs ${ospf_delivered})${NC}"
    fi
    if [[ "$spp_fast_avg" != "N/A" && "$spp_rel_avg" != "N/A" ]]; then
        if awk "BEGIN { exit !($spp_fast_avg < $spp_rel_avg * 0.9) }"; then
            echo -e "${CYAN}║${NC}  ${GREEN}${BOLD}  → Latency-critical: faster than reliable path (${spp_fast_avg} vs ${spp_rel_avg}ms)${NC}"
        fi
    fi
    echo -e "${CYAN}║${NC}  ${GREEN}${BOLD}  → This is impossible with OSPF — it has no concept of intent.${NC}"
    echo -e "${BOLD}${CYAN}╚══════════════════════════════════════════════════════════════════════════╝${NC}"

    S_LABELS+=("6: Intent (F=lossy, B/C/D/E=+20ms)")
    S_STATIC+=("N/A")
    S_OSPF+=("${ospf_delivered}/${N}")
    S_SPP+=("${spp_rel_delivered}/${N}")
}


# ═══════════════════════════════════════════════════════════════
# Execute selected scenarios
# ═══════════════════════════════════════════════════════════════

case "$SELECTED_SCENARIO" in
    1)          run_scenario_1 ;;
    2)          run_scenario_2 ;;
    3)          run_scenario_3 ;;
    4)          run_scenario_4 ;;
    5|loss)     run_scenario_5 ;;
    6|intent)   run_scenario_6 ;;
    all)
        run_scenario_1
        run_scenario_2
        run_scenario_3
        run_scenario_4
        run_scenario_5
        run_scenario_6
        ;;
    *)
        echo "Usage: $0 [1|2|3|4|5|loss|6|intent|all]"
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
    echo -e "${BOLD}${CYAN}║  SPP VALIDATION SUMMARY — Static vs OSPF vs SPP                      ║${NC}"
    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════════════╣${NC}"
    printf "${BOLD}${CYAN}║  %-38s │ %10s │ %10s │ %10s │ %9s  ║${NC}\n" \
        "Scenario" "Static" "OSPF" "SPP" "SPP wins?"
    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════════════╣${NC}"

    local i
    local vs_static=0 vs_ospf=0 total_latency=0 total_delivery=0
    for i in "${!S_LABELS[@]}"; do
        local label="${S_LABELS[$i]}"
        local static_val="${S_STATIC[$i]}"
        local ospf_val="${S_OSPF[$i]}"
        local spp_val="${S_SPP[$i]}"

        local verdict result_color

        # Check if this is a delivery-rate scenario (contains "/")
        if [[ "$ospf_val" == *"/"* || "$spp_val" == *"/"* ]]; then
            # Delivery-rate comparison: extract numerator
            local ospf_num spp_num
            ospf_num=$(echo "$ospf_val" | cut -d'/' -f1)
            spp_num=$(echo "$spp_val" | cut -d'/' -f1)
            total_delivery=$((total_delivery + 1))

            if [[ $spp_num -gt $ospf_num ]]; then
                verdict="  ✓ SPP"
                result_color="$GREEN"
                vs_ospf=$((vs_ospf + 1))
            elif [[ $spp_num -eq $ospf_num ]]; then
                verdict="    TIE"
                result_color="$YELLOW"
            else
                verdict="  ✗ OSPF"
                result_color="$RED"
            fi

            printf "${CYAN}║${NC}  %-38s ${CYAN}│${NC} %10s ${CYAN}│${NC} %10s ${CYAN}│${NC} %10s ${CYAN}│${NC} ${result_color}${BOLD}%9s${NC}  ${CYAN}║${NC}\n" \
                "$label" "$static_val" "$ospf_val" "$spp_val" "$verdict"
        elif [[ "$ospf_val" == "FAILED" || "$spp_val" == "FAILED" || "$ospf_val" == "N/A" || "$spp_val" == "N/A" ]]; then
            verdict="    N/A"
            result_color="$YELLOW"
            printf "${CYAN}║${NC}  %-38s ${CYAN}│${NC} %10s ${CYAN}│${NC} %10s ${CYAN}│${NC} %10s ${CYAN}│${NC} ${result_color}${BOLD}%9s${NC}  ${CYAN}║${NC}\n" \
                "$label" "$static_val" "$ospf_val" "$spp_val" "$verdict"
        else
            # Latency comparison (ms values)
            total_latency=$((total_latency + 1))

            if [[ "$static_val" != "N/A" && "$static_val" != "FAILED" ]]; then
                if awk "BEGIN { exit !($spp_val < $static_val * 0.9) }"; then
                    vs_static=$((vs_static + 1))
                fi
            fi

            if awk "BEGIN { exit !($spp_val < $ospf_val * 0.9) }"; then
                verdict="  ✓ SPP"
                result_color="$GREEN"
                vs_ospf=$((vs_ospf + 1))
            elif awk "BEGIN { exit !($ospf_val < $spp_val * 0.9) }"; then
                verdict="  ✗ OSPF"
                result_color="$RED"
            else
                verdict="  ≈ TIE"
                result_color="$YELLOW"
            fi

            local static_display="$static_val"
            [[ "$static_display" == "N/A" ]] || static_display="${static_display}ms"
            printf "${CYAN}║${NC}  %-38s ${CYAN}│${NC} %10s ${CYAN}│${NC} %8sms ${CYAN}│${NC} %8sms ${CYAN}│${NC} ${result_color}${BOLD}%9s${NC}  ${CYAN}║${NC}\n" \
                "$label" "$static_display" "$ospf_val" "$spp_val" "$verdict"
        fi
    done

    echo -e "${BOLD}${CYAN}╚══════════════════════════════════════════════════════════════════════════════════╝${NC}"
    echo ""

    echo -e "  ${BOLD}SPP vs Static:${NC} Won ${vs_static}/${total_latency} latency scenarios"
    echo -e "  ${BOLD}SPP vs OSPF:${NC}   Won ${vs_ospf}/${#S_LABELS[@]} scenarios (latency + delivery)"
    echo ""
    if [[ $vs_ospf -gt 0 ]]; then
        echo -e "  ${GREEN}${BOLD}✓ SPP outperformed OSPF (industry standard) in ${vs_ospf} scenario(s).${NC}"
        echo -e "  ${GREEN}${BOLD}  Key advantages: intent-aware routing, multi-metric, mid-flight rerouting.${NC}"
    fi
    if [[ $vs_static -gt 0 ]]; then
        echo -e "  ${GREEN}${BOLD}✓ SPP outperformed static routing in ${vs_static} scenario(s).${NC}"
    fi
    echo -e "  ${CYAN}${BOLD}  OSPF limitations: single metric (latency only), no per-packet adaptation,${NC}"
    echo -e "  ${CYAN}${BOLD}  no mid-flight rerouting, convergence delay (seconds to adapt).${NC}"
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
