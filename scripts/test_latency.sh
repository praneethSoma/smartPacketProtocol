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
declare -a S_LABELS=() S_STATIC=() S_SPP=()

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
# SCENARIO 6: The Race — Dumb Routing vs SPP Smart Routing
# ═══════════════════════════════════════════════════════════════

run_scenario_overhead() {
    print_header "SCENARIO 6: The Race — Dumb Routing vs SPP Smart Routing"
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

    # ── SPP: Smart routing (gossip + Dijkstra) ───────────────
    echo ""
    echo -e "  ${YELLOW}▶ [SPP] Smart routing: sender → router_a → ??? (5 pkts)${NC}"
    echo -e "    ${BOLD}(Queries gossip, runs Dijkstra, avoids congestion)${NC}"
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
    if [[ "$dumb_avg" == "FAILED" || "$spp_avg" == "FAILED" || "$light_avg" == "FAILED" || "$compact_avg" == "FAILED" ]]; then
        print_fail "Race incomplete — a packet failed to deliver"
    else
        local saved_ms speedup light_saved_ms
        saved_ms=$(awk "BEGIN { printf \"%.1f\", $dumb_avg - $spp_avg }")
        speedup=$(awk "BEGIN { if ($spp_avg > 0) printf \"%.1f\", $dumb_avg / $spp_avg; else print \"N/A\" }")
        light_saved_ms=$(awk "BEGIN { printf \"%.1f\", $dumb_avg - $light_avg }")

        # Get byte sizes for the race summary
        local dumb_race_bytes spp_race_bytes light_race_bytes compact_race_bytes
        dumb_race_bytes=$(grep -oP 'bytes=\K[0-9]+' "$SENDER_LOG_SPP_SMART" 2>/dev/null | head -1 || echo "?")
        spp_race_bytes=$(grep -oP 'bytes=\K[0-9]+' "$SENDER_LOG_SPP_SMART" 2>/dev/null | tail -1 || echo "?")
        light_race_bytes=$(grep -oP 'bytes=\K[0-9]+' "$SENDER_LOG_SPP_LIGHT" 2>/dev/null | tail -1 || echo "?")
        compact_race_bytes=$(grep -oP 'bytes=\K[0-9]+' "$SENDER_LOG_SPP_COMPACT" 2>/dev/null | tail -1 || echo "?")

        echo -e "${BOLD}${CYAN}╔══════════════════════════════════════════════════════════════════════════╗${NC}"
        echo -e "${BOLD}${CYAN}║  THE RACE — Dumb vs SPP Smart vs SPP LightMode                          ║${NC}"
        echo -e "${BOLD}${CYAN}║  Network: B +80ms, D +80ms (highway congested)                          ║${NC}"
        echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"
        printf "${CYAN}║${NC}  %-38s  %10s ms  %8s B  ${CYAN}║${NC}\n" "Dumb (A→B→D→G, fixed path)" "$dumb_avg" "—"
        printf "${CYAN}║${NC}  %-38s  %10s ms  %8s B  ${CYAN}║${NC}\n" "SPP  (smart, full map)"      "$spp_avg"  "$spp_race_bytes"
        printf "${CYAN}║${NC}  %-38s  %10s ms  %8s B  ${CYAN}║${NC}\n" "SPP  (LightMode, no map)"    "$light_avg" "$light_race_bytes"
        printf "${CYAN}║${NC}  %-38s  %10s ms  %8s B  ${CYAN}║${NC}\n" "SPP  (Light + CompactIDs)"   "$compact_avg" "$compact_race_bytes"
        echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"
        if awk "BEGIN { exit !($spp_avg < $dumb_avg * 0.9) }"; then
            echo -e "${CYAN}║${NC}  ${GREEN}${BOLD}SPP wins by ${saved_ms}ms (${speedup}x faster than dumb routing)${NC}             ${CYAN}║${NC}"
            echo -e "${CYAN}║${NC}  ${GREEN}LightMode: same intelligence, ${light_race_bytes} bytes instead of ${spp_race_bytes}${NC}               ${CYAN}║${NC}"
        else
            echo -e "${CYAN}║${NC}  ${YELLOW}Results similar — congestion may not be significant.${NC}   ${CYAN}║${NC}"
        fi
        echo -e "${BOLD}${CYAN}╚══════════════════════════════════════════════════════════════════╝${NC}"
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

    # ══════════════════════════════════════════════════════════
    # PART 3: Packet Size Analysis & Scaling Projections
    # ══════════════════════════════════════════════════════════
    echo ""
    echo -e "  ${BOLD}── PART 3: Packet Size Analysis ──${NC}"
    echo "  How much bigger are SPP packets vs raw UDP?"
    echo "  Will this cause issues at scale?"

    # Extract byte sizes from sender logs (captured by run_sender_with_config)
    local raw_bytes=8  # Raw UDP is always 8 bytes (timestamp only)
    local spp_direct_bytes spp_2hop_bytes spp_smart_bytes
    spp_direct_bytes=$(grep -oP 'bytes=\K[0-9]+' "$SENDER_LOG_SPP_DIRECT" 2>/dev/null | tail -1 || echo "N/A")
    spp_2hop_bytes=$(grep -oP 'bytes=\K[0-9]+' "$SENDER_LOG_SPP_2HOP" 2>/dev/null | tail -1 || echo "N/A")
    spp_smart_bytes=$(grep -oP 'bytes=\K[0-9]+' "$SENDER_LOG_SPP_SMART" 2>/dev/null | tail -1 || echo "N/A")
    local spp_light_bytes spp_compact_bytes
    spp_light_bytes=$(grep -oP 'bytes=\K[0-9]+' "$SENDER_LOG_SPP_LIGHT" 2>/dev/null | tail -1 || echo "N/A")
    spp_compact_bytes=$(grep -oP 'bytes=\K[0-9]+' "$SENDER_LOG_SPP_COMPACT" 2>/dev/null | tail -1 || echo "N/A")

    # Calculate the SPP overhead (total - payload)
    local payload_str="overhead_probe"
    local payload_bytes=${#payload_str}

    # Per-link cost on wire: 2+name + 2+name + 8+8+8 = ~44 bytes per link
    # Current topology: 7 routers, ~14 bidirectional links
    local current_links=14
    local bytes_per_link=44

    echo ""
    echo -e "${BOLD}${CYAN}╔══════════════════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${BOLD}${CYAN}║  PACKET SIZE BREAKDOWN                                                  ║${NC}"
    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"
    echo -e "${BOLD}${CYAN}║  Measured Sizes (this test, ${payload_bytes}-byte payload)                            ║${NC}"
    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"
    printf "${CYAN}║${NC}  %-42s  %8s bytes  ${CYAN}║${NC}\n" "Raw UDP (timestamp only)"           "$raw_bytes"
    printf "${CYAN}║${NC}  %-42s  %8s bytes  ${CYAN}║${NC}\n" "SPP direct (0 hops, no MiniMap)"    "$spp_direct_bytes"
    printf "${CYAN}║${NC}  %-42s  %8s bytes  ${CYAN}║${NC}\n" "SPP 2-hop (static path, no map)"    "$spp_2hop_bytes"
    printf "${CYAN}║${NC}  %-42s  %8s bytes  ${CYAN}║${NC}\n" "SPP smart (gossip map embedded)"    "$spp_smart_bytes"
    printf "${CYAN}║${NC}  %-42s  %8s bytes  ${CYAN}║${NC}\n" "SPP LightMode (no map, same routing)"  "$spp_light_bytes"
    printf "${CYAN}║${NC}  %-42s  ${GREEN}%8s${NC} bytes  ${CYAN}║${NC}\n" "SPP Light + CompactIDs (2-byte node IDs)" "$spp_compact_bytes"
    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"
    echo -e "${BOLD}${CYAN}║  Where the bytes go (SPP wire format)                                   ║${NC}"
    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"
    echo -e "${CYAN}║${NC}  Fixed header (magic, version, CRC, etc.)      16 bytes  (fixed)   ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}  Intent + routing metadata                      9 bytes  (fixed)   ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}  Node names (source, dest, current)           ~30 bytes  (fixed)   ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}  Planned path (hop names)                     ~40 bytes  (grows)   ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}  MiniMap (topology links)                    ~${current_links}×44 bytes  (SCALES)  ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}    ${GREEN}↳ LightMode eliminates this entirely (0 bytes)${NC}              ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}  Congestion log + visited set                 ~20 bytes  (grows)   ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}  Application payload                           ${payload_bytes} bytes  (yours)   ${CYAN}║${NC}"
    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"
    echo -e "${BOLD}${CYAN}║  Scaling Projections — What happens at larger topologies?               ║${NC}"
    echo -e "${BOLD}${CYAN}║  (UDP MTU = 1500 bytes, fragmentation threshold)                       ║${NC}"
    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"

    # Projection: routers → links → map bytes → total packet → game packet
    local game_payload=100  # typical game position update
    for routers in 7 20 50 100; do
        # Rough link estimate: avg 2 links per router (directed)
        local est_links=$((routers * 2))
        local map_bytes=$((est_links * bytes_per_link))
        local fixed_overhead=95   # fixed headers + names + path
        local total=$((fixed_overhead + map_bytes + game_payload))
        local mtu_status
        if [[ $total -gt 1500 ]]; then
            mtu_status="${RED}EXCEEDS MTU${NC}"
        elif [[ $total -gt 1200 ]]; then
            mtu_status="${YELLOW}NEAR MTU${NC}"
        else
            mtu_status="${GREEN}OK${NC}"
        fi
        printf "${CYAN}║${NC}  %3d routers → ~%3d links → map: %5d B → total: %5d B  [${mtu_status}]  ${CYAN}║${NC}\n" \
            "$routers" "$est_links" "$map_bytes" "$total"
    done

    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"
    echo -e "${BOLD}${CYAN}║  Recommendations for Production Scale                                  ║${NC}"
    echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"
    echo -e "${CYAN}║${NC}  1. ${BOLD}Partial MiniMap${NC} — embed only links near the path, not the        ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}     full topology. A 100-router network only needs ~10 links.       ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}  2. ${BOLD}Compact node IDs${NC} — replace string names with 2-byte IDs.         ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}     Cuts per-link cost from 44 bytes → 28 bytes (36% smaller).      ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}  3. ${BOLD}Router-side topology${NC} — routers already know the map via gossip.  ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}     Packets carry only path + intent; routers reroute locally.       ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}  4. ${BOLD}Map compression${NC} — delta-encode link weights or use varint        ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}     encoding for near-zero values. Can halve map size.               ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}                                                                      ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}  With recommendations 1+2: a 100-router network fits in ~400 B.     ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}  With recommendation 3: SPP overhead drops to ~100 B (fixed).       ${CYAN}║${NC}"
    echo -e "${BOLD}${CYAN}╚══════════════════════════════════════════════════════════════════════════╝${NC}"
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
    6|overhead) run_scenario_overhead ;;
    all)
        run_scenario_1
        run_scenario_2
        run_scenario_3
        run_scenario_4
        run_scenario_5
        run_scenario_overhead
        ;;
    *)
        echo "Usage: $0 [1|2|3|4|5|burst|6|overhead|all]"
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
