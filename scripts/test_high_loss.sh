#!/usr/bin/env bash
# ═══════════════════════════════════════════════════════════════
# SPP High Packet Loss Test — 7-Router Docker Topology
# ═══════════════════════════════════════════════════════════════
#
# Tests delivery reliability at extreme loss rates (30%, 50%, 70%)
# using tc netem on the real 7-router Docker topology.
#
# For each loss rate, sends packets via:
#   1. Static (dumb) routing — fixed path, no intelligence
#   2. SPP smart routing — gossip-aware Dijkstra with reliability intent
#
# Shows how SPP's reliability-aware routing avoids lossy paths.
#
# Usage:
#   bash scripts/test_high_loss.sh          # Run all loss scenarios
#   bash scripts/test_high_loss.sh 30       # Run only 30% loss
#   bash scripts/test_high_loss.sh 50       # Run only 50% loss
#   bash scripts/test_high_loss.sh 70       # Run only 70% loss
#
# ═══════════════════════════════════════════════════════════════
set -euo pipefail

COMPOSE="docker compose -f docker-compose.7router.yml"
GOSSIP_WAIT=12
DELIVERY_WAIT=8
PACKETS_PER_TEST=20

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

SELECTED="${1:-all}"

# ═══════════════════════════════════════════════════════════════
# Helpers
# ═══════════════════════════════════════════════════════════════

print_header() {
    echo -e "\n${CYAN}╔══════════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${CYAN}║  $1${NC}"
    echo -e "${CYAN}╚══════════════════════════════════════════════════════════════════╝${NC}"
}

print_step() { echo -e "\n${YELLOW}  ▶ $1${NC}"; }
print_ok()   { echo -e "  ${GREEN}✓ $1${NC}"; }
print_fail() { echo -e "  ${RED}✗ $1${NC}"; }

receiver_log_offset() {
    $COMPOSE logs receiver 2>&1 | wc -l
}

receiver_logs_since() {
    local offset="$1"
    $COMPOSE logs receiver 2>&1 | tail -n +"$((offset + 1))"
}

# Inject loss (and optional delay) on a container
# Usage: inject_loss <container> <loss_pct> [delay_ms]
inject_loss() {
    local container="$1"
    local loss="$2"
    local delay="${3:-0}"

    local ifaces
    ifaces=$(docker exec "$container" sh -c "ip -o link show | grep -v 'lo:' | awk -F': ' '{print \$2}' | cut -d'@' -f1")

    for iface in $ifaces; do
        docker exec "$container" tc qdisc add dev "$iface" root netem delay "${delay}ms" loss "${loss}%" 2>/dev/null || \
        docker exec "$container" tc qdisc change dev "$iface" root netem delay "${delay}ms" loss "${loss}%" 2>/dev/null || true
    done
}

clear_delay() {
    local container="$1"
    local ifaces
    ifaces=$(docker exec "$container" sh -c "ip -o link show | grep -v 'lo:' | awk -F': ' '{print \$2}' | cut -d'@' -f1" 2>/dev/null) || true
    for iface in $ifaces; do
        docker exec "$container" tc qdisc del dev "$iface" root 2>/dev/null || true
    done
}

clear_all() {
    for router in spp-router-a spp-router-b spp-router-c spp-router-d spp-router-e spp-router-f spp-router-g; do
        clear_delay "$router"
    done
}

# Send N packets and count deliveries + extract latency stats.
# Usage: send_and_count <config_json> <label>
# Returns: "delivered|avg_ms|min_ms|max_ms"
send_and_count() {
    local config_json="$1"
    local label="$2"
    local tmpfile
    tmpfile=$(mktemp /tmp/spp_loss_XXXXXX.json)
    echo "$config_json" > "$tmpfile"

    local pre_offset
    pre_offset=$(receiver_log_offset)

    $COMPOSE run --rm \
        -v "${tmpfile}:/app/configs/topologies/7router/loss_sender.json:ro" \
        sender ./bin/sender configs/topologies/7router/loss_sender.json 2>&1 | tail -3 >&2

    sleep "$DELIVERY_WAIT"

    local new_logs
    new_logs=$(receiver_logs_since "$pre_offset")
    rm -f "$tmpfile"

    local delivered
    delivered=$(echo "$new_logs" | grep -c "SMART PACKET DELIVERED") || delivered=0

    # Extract latency stats from "End-to-end:" lines
    local avg_ms="—" min_ms="—" max_ms="—"
    if [[ "$delivered" -gt 0 ]]; then
        local latencies
        latencies=$(echo "$new_logs" | grep "End-to-end:" | grep -oP '[\d.]+' | head -"$delivered")
        if [[ -n "$latencies" ]]; then
            avg_ms=$(echo "$latencies" | awk '{ sum += $1; count++ } END { if (count > 0) printf "%.2f", sum/count; else print "—" }')
            min_ms=$(echo "$latencies" | sort -g | head -1)
            max_ms=$(echo "$latencies" | sort -g | tail -1)
        fi
    fi

    echo "${delivered}|${avg_ms}|${min_ms}|${max_ms}"
}

# Parse send_and_count result fields
get_delivered() { echo "$1" | cut -d'|' -f1; }
get_avg_ms()    { echo "$1" | cut -d'|' -f2; }
get_min_ms()    { echo "$1" | cut -d'|' -f3; }
get_max_ms()    { echo "$1" | cut -d'|' -f4; }

# ═══════════════════════════════════════════════════════════════
# Run a single loss scenario
# ═══════════════════════════════════════════════════════════════

# Result storage for summary table
declare -a R_LOSS=() R_STATIC_DEL=() R_SPP_DEL=() R_STATIC_PCT=() R_SPP_PCT=()
declare -a R_STATIC_AVG=() R_SPP_AVG=() R_STATIC_MIN=() R_SPP_MIN=() R_STATIC_MAX=() R_SPP_MAX=()

run_loss_scenario() {
    local loss_pct="$1"
    local N="$PACKETS_PER_TEST"

    print_header "HIGH LOSS SCENARIO: ${loss_pct}% packet loss per hop"

    echo -e "\n  ${BOLD}Topology:${NC}"
    echo "                    ┌─── router_b ─── router_d ───┐"
    echo "  sender → router_a ──┤                              ├── router_g → receiver"
    echo "                    ├─── router_c ─── router_e ───┘"
    echo "                    └─── router_f ────────────────────────→ receiver"
    echo ""
    echo -e "  ${BOLD}Test:${NC} Inject ${loss_pct}% loss on ALL routers, send ${N} packets."
    echo "  Compare: dumb static routing vs SPP reliability-aware routing."

    # ── Clear and inject loss ──
    print_step "Clearing previous tc rules..."
    clear_all

    print_step "Injecting ${loss_pct}% packet loss on ALL routers..."
    for router in spp-router-a spp-router-b spp-router-c spp-router-d spp-router-e spp-router-f spp-router-g; do
        inject_loss "$router" "$loss_pct" 2
        echo -e "    ${BOLD}${router}: ${loss_pct}% loss${NC}"
    done

    echo -e "    Waiting ${GOSSIP_WAIT}s for gossip to detect loss conditions..."
    sleep "$GOSSIP_WAIT"

    # ── Test 1: Static (dumb) routing — fixed 4-hop path ──
    echo ""
    echo -e "  ${YELLOW}▶ [DUMB] Static path: A → B → D → G → receiver (${N} pkts)${NC}"
    echo -e "    ${BOLD}No intelligence — fixed path through ${loss_pct}% loss at each hop${NC}"
    echo -e "    Theoretical survival: (1-${loss_pct}/100)^4 = $(awk "BEGIN { printf \"%.1f\", ((1-${loss_pct}/100)^4)*100 }")%"

    local static_result static_delivered static_avg static_min static_max
    static_result=$(send_and_count "$(cat <<EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "Destination": "receiver",
  "Payload": "loss_test_static",
  "IntentLatency": 3,
  "IntentReliability": 1,
  "StaticPath": ["sender", "router_a", "router_b", "router_d", "router_g", "receiver"],
  "PacketCount": $N,
  "IntervalMs": 200
}
EOF
)" "Static")
    static_delivered=$(get_delivered "$static_result")
    static_avg=$(get_avg_ms "$static_result")
    static_min=$(get_min_ms "$static_result")
    static_max=$(get_max_ms "$static_result")

    local static_rate
    static_rate=$(awk "BEGIN { printf \"%.1f\", ($static_delivered / $N) * 100 }")
    print_ok "Static: ${static_delivered}/${N} delivered (${static_rate}%)  latency: avg=${static_avg}ms min=${static_min}ms max=${static_max}ms"

    # ── Test 2: SPP smart routing — reliability-aware ──
    echo ""
    echo -e "  ${YELLOW}▶ [SPP] Smart routing with Reliability=2 (max) (${N} pkts)${NC}"
    echo -e "    ${BOLD}Gossip-aware Dijkstra penalizes lossy links, picks shortest clean-ish path${NC}"

    local spp_result spp_delivered spp_avg spp_min spp_max
    spp_result=$(send_and_count "$(cat <<EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "GossipAddr": "10.1.1.2:7001",
  "Destination": "receiver",
  "FirstRouter": "router_a",
  "Payload": "loss_test_spp",
  "IntentLatency": 1,
  "IntentReliability": 2,
  "GossipRetries": 10,
  "GossipRetryBackoffMs": 500,
  "PacketCount": $N,
  "IntervalMs": 200
}
EOF
)" "SPP")
    spp_delivered=$(get_delivered "$spp_result")
    spp_avg=$(get_avg_ms "$spp_result")
    spp_min=$(get_min_ms "$spp_result")
    spp_max=$(get_max_ms "$spp_result")

    local spp_rate
    spp_rate=$(awk "BEGIN { printf \"%.1f\", ($spp_delivered / $N) * 100 }")
    print_ok "SPP:    ${spp_delivered}/${N} delivered (${spp_rate}%)  latency: avg=${spp_avg}ms min=${spp_min}ms max=${spp_max}ms"

    # ── Comparison ──
    echo ""
    if [[ "$spp_delivered" -gt "$static_delivered" ]]; then
        if [[ "$static_delivered" -gt 0 ]]; then
            local improvement
            improvement=$(awk "BEGIN { printf \"%.1f\", ($spp_delivered / $static_delivered) }")
            echo -e "  ${GREEN}${BOLD}⚡ SPP delivered ${improvement}x more packets than static routing!${NC}"
        else
            echo -e "  ${GREEN}${BOLD}⚡ SPP delivered ${spp_delivered} packets, static delivered ZERO!${NC}"
        fi
    elif [[ "$spp_delivered" -eq "$static_delivered" ]]; then
        echo -e "  ${YELLOW}≈ Both delivered the same number of packets${NC}"
    else
        echo -e "  ${RED}Static delivered more — uniform loss means no routing advantage${NC}"
    fi

    # Store for summary
    R_LOSS+=("$loss_pct")
    R_STATIC_DEL+=("$static_delivered")
    R_SPP_DEL+=("$spp_delivered")
    R_STATIC_PCT+=("$static_rate")
    R_SPP_PCT+=("$spp_rate")
    R_STATIC_AVG+=("$static_avg")
    R_SPP_AVG+=("$spp_avg")
    R_STATIC_MIN+=("$static_min")
    R_SPP_MIN+=("$spp_min")
    R_STATIC_MAX+=("$static_max")
    R_SPP_MAX+=("$spp_max")
}

# ═══════════════════════════════════════════════════════════════
# Bonus: Selective loss — lossy vs clean path
# ═══════════════════════════════════════════════════════════════

run_selective_loss() {
    local loss_pct="$1"
    local N="$PACKETS_PER_TEST"

    print_header "SELECTIVE LOSS: ${loss_pct}% on highway (B+D), clean alternate (C+E+F)"

    echo ""
    echo "  Highway path   (A→B→D→G): ${loss_pct}% loss on B and D"
    echo "  Alternate path (A→C→E→G): clean (0% loss)"
    echo "  Shortcut path  (A→F):     clean (0% loss)"
    echo ""
    echo "  Static routing is stuck on the lossy highway."
    echo "  SPP should detect loss via gossip and route around it."

    print_step "Clearing and injecting selective loss..."
    clear_all
    inject_loss spp-router-b "$loss_pct" 2
    inject_loss spp-router-d "$loss_pct" 2
    echo -e "    ${BOLD}router_b: ${loss_pct}% loss${NC}"
    echo -e "    ${BOLD}router_d: ${loss_pct}% loss${NC}"
    echo -e "    router_c, router_e, router_f, router_g: clean"

    echo -e "    Waiting ${GOSSIP_WAIT}s for gossip..."
    sleep "$GOSSIP_WAIT"

    # Static through lossy path
    echo ""
    echo -e "  ${YELLOW}▶ [DUMB] Static: A → B → D → G → receiver (through loss) (${N} pkts)${NC}"
    local static_result static_delivered static_avg static_min static_max
    static_result=$(send_and_count "$(cat <<EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "Destination": "receiver",
  "Payload": "selective_loss_static",
  "IntentLatency": 3,
  "IntentReliability": 1,
  "StaticPath": ["sender", "router_a", "router_b", "router_d", "router_g", "receiver"],
  "PacketCount": $N,
  "IntervalMs": 200
}
EOF
)" "Static-selective")
    static_delivered=$(get_delivered "$static_result")
    static_avg=$(get_avg_ms "$static_result")
    static_min=$(get_min_ms "$static_result")
    static_max=$(get_max_ms "$static_result")

    local static_rate
    static_rate=$(awk "BEGIN { printf \"%.1f\", ($static_delivered / $N) * 100 }")
    print_ok "Static (through loss): ${static_delivered}/${N} delivered (${static_rate}%)  latency: avg=${static_avg}ms min=${static_min}ms max=${static_max}ms"

    # SPP smart routing — should avoid B and D
    echo ""
    echo -e "  ${YELLOW}▶ [SPP] Smart routing (Reliability=2) — should avoid B+D (${N} pkts)${NC}"
    local spp_result spp_delivered spp_avg spp_min spp_max
    spp_result=$(send_and_count "$(cat <<EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "GossipAddr": "10.1.1.2:7001",
  "Destination": "receiver",
  "FirstRouter": "router_a",
  "Payload": "selective_loss_spp",
  "IntentLatency": 1,
  "IntentReliability": 2,
  "GossipRetries": 10,
  "GossipRetryBackoffMs": 500,
  "PacketCount": $N,
  "IntervalMs": 200
}
EOF
)" "SPP-selective")
    spp_delivered=$(get_delivered "$spp_result")
    spp_avg=$(get_avg_ms "$spp_result")
    spp_min=$(get_min_ms "$spp_result")
    spp_max=$(get_max_ms "$spp_result")

    local spp_rate
    spp_rate=$(awk "BEGIN { printf \"%.1f\", ($spp_delivered / $N) * 100 }")
    print_ok "SPP (avoids loss):    ${spp_delivered}/${N} delivered (${spp_rate}%)  latency: avg=${spp_avg}ms min=${spp_min}ms max=${spp_max}ms"

    echo ""
    if [[ "$spp_delivered" -gt "$static_delivered" ]]; then
        echo -e "  ${GREEN}${BOLD}⚡ SPP routed around ${loss_pct}% loss: ${spp_rate}% vs ${static_rate}% delivery!${NC}"
    fi

    R_LOSS+=("${loss_pct}(sel)")
    R_STATIC_DEL+=("$static_delivered")
    R_SPP_DEL+=("$spp_delivered")
    R_STATIC_PCT+=("$static_rate")
    R_SPP_PCT+=("$spp_rate")
    R_STATIC_AVG+=("$static_avg")
    R_SPP_AVG+=("$spp_avg")
    R_STATIC_MIN+=("$static_min")
    R_SPP_MIN+=("$spp_min")
    R_STATIC_MAX+=("$static_max")
    R_SPP_MAX+=("$spp_max")
}

# ═══════════════════════════════════════════════════════════════
# Infrastructure
# ═══════════════════════════════════════════════════════════════

print_header "SPP High Packet Loss Test — 7-Router Topology"

echo -e "\n${BOLD}Topology:${NC}"
echo "                    ┌─── router_b ─── router_d ───┐"
echo "  sender → router_a ──┤                              ├── router_g → receiver"
echo "                    ├─── router_c ─── router_e ───┘"
echo "                    └─── router_f ────────────────────────→ receiver"

print_step "Cleaning up previous containers..."
$COMPOSE down --remove-orphans 2>/dev/null || true

if [[ "${SKIP_BUILD:-0}" != "1" ]]; then
    print_step "Building Docker images..."
    $COMPOSE build
else
    print_step "Skipping build (SKIP_BUILD=1)..."
fi

print_step "Starting 7 routers + receiver..."
$COMPOSE up -d receiver router_a router_b router_c router_d router_e router_f router_g

echo -e "  Waiting ${GOSSIP_WAIT}s for gossip convergence..."
sleep "$GOSSIP_WAIT"

# ═══════════════════════════════════════════════════════════════
# Execute scenarios
# ═══════════════════════════════════════════════════════════════

case "$SELECTED" in
    30)  run_loss_scenario 30; run_selective_loss 30 ;;
    50)  run_loss_scenario 50; run_selective_loss 50 ;;
    70)  run_loss_scenario 70; run_selective_loss 70 ;;
    all)
        run_loss_scenario 30
        run_selective_loss 30
        run_loss_scenario 50
        run_selective_loss 50
        run_loss_scenario 70
        run_selective_loss 70
        ;;
    *)
        echo "Usage: $0 [30|50|70|all]"
        exit 1
        ;;
esac

# ═══════════════════════════════════════════════════════════════
# Summary Table
# ═══════════════════════════════════════════════════════════════

clear_all

echo ""
echo -e "${BOLD}${CYAN}╔════════════════════════════════════════════════════════════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}${CYAN}║  HIGH PACKET LOSS — DELIVERY & LATENCY SUMMARY                                                    ║${NC}"
echo -e "${BOLD}${CYAN}╠════════════════════════════════════════════════════════════════════════════════════════════════════╣${NC}"
printf "${BOLD}${CYAN}║  %-12s │ %-20s │ %-20s │ %-14s │ %-14s │ %-8s ║${NC}\n" \
    "Loss Rate" "Static: Delivery" "SPP: Delivery" "Static: Latency" "SPP: Latency" "Winner"
echo -e "${BOLD}${CYAN}╠════════════════════════════════════════════════════════════════════════════════════════════════════╣${NC}"

for i in "${!R_LOSS[@]}"; do
    local_loss="${R_LOSS[$i]}"
    local_s_del="${R_STATIC_DEL[$i]}"
    local_s_pct="${R_STATIC_PCT[$i]}"
    local_p_del="${R_SPP_DEL[$i]}"
    local_p_pct="${R_SPP_PCT[$i]}"
    local_s_avg="${R_STATIC_AVG[$i]}"
    local_p_avg="${R_SPP_AVG[$i]}"

    if [[ "$local_p_del" -gt "$local_s_del" ]]; then
        winner="${GREEN}${BOLD}SPP ⚡${NC}"
    elif [[ "$local_p_del" -eq "$local_s_del" ]]; then
        winner="${YELLOW}TIE${NC}"
    else
        winner="${RED}Static${NC}"
    fi

    # Format latency: show "avg ms" or "—" if no packets delivered
    s_lat_str=""
    p_lat_str=""
    if [[ "$local_s_avg" == "—" ]]; then
        s_lat_str="— (0 pkts)"
    else
        s_lat_str="${local_s_avg} ms"
    fi
    if [[ "$local_p_avg" == "—" ]]; then
        p_lat_str="— (0 pkts)"
    else
        p_lat_str="${local_p_avg} ms"
    fi

    printf "${CYAN}║${NC}  %-12s ${CYAN}│${NC} %4s/%-3s (%5s%%)    ${CYAN}│${NC} %4s/%-3s (%5s%%)    ${CYAN}│${NC} %14s ${CYAN}│${NC} %14s ${CYAN}│${NC} $(echo -e "$winner") ${CYAN}║${NC}\n" \
        "${local_loss}%" \
        "$local_s_del" "$PACKETS_PER_TEST" "$local_s_pct" \
        "$local_p_del" "$PACKETS_PER_TEST" "$local_p_pct" \
        "$s_lat_str" "$p_lat_str"
done

echo -e "${BOLD}${CYAN}╠════════════════════════════════════════════════════════════════════════════════════════════════════╣${NC}"

# Detailed latency breakdown (for scenarios that had deliveries on both sides)
echo -e "${CYAN}║${NC}                                                                                                    ${CYAN}║${NC}"
echo -e "${CYAN}║${NC}  ${BOLD}Latency Detail (min / avg / max ms):${NC}                                                            ${CYAN}║${NC}"
for i in "${!R_LOSS[@]}"; do
    local_loss="${R_LOSS[$i]}"
    local_s_min="${R_STATIC_MIN[$i]}"
    local_s_avg="${R_STATIC_AVG[$i]}"
    local_s_max="${R_STATIC_MAX[$i]}"
    local_p_min="${R_SPP_MIN[$i]}"
    local_p_avg="${R_SPP_AVG[$i]}"
    local_p_max="${R_SPP_MAX[$i]}"

    s_detail=""
    p_detail=""
    if [[ "$local_s_avg" == "—" ]]; then
        s_detail="no deliveries"
    else
        s_detail="${local_s_min} / ${local_s_avg} / ${local_s_max}"
    fi
    if [[ "$local_p_avg" == "—" ]]; then
        p_detail="no deliveries"
    else
        p_detail="${local_p_min} / ${local_p_avg} / ${local_p_max}"
    fi

    printf "${CYAN}║${NC}    %-10s  Static: %-28s  SPP: %-28s     ${CYAN}║${NC}\n" \
        "${local_loss}%" "$s_detail" "$p_detail"
done

echo -e "${CYAN}║${NC}                                                                                                    ${CYAN}║${NC}"
echo -e "${BOLD}${CYAN}╠════════════════════════════════════════════════════════════════════════════════════════════════════╣${NC}"
echo -e "${CYAN}║${NC}  ${BOLD}Key insight:${NC} When loss is uniform across ALL paths, neither routing strategy can help —          ${CYAN}║${NC}"
echo -e "${CYAN}║${NC}  packets just have to survive the gauntlet. But when loss is ${BOLD}selective${NC} (some paths lossy,          ${CYAN}║${NC}"
echo -e "${CYAN}║${NC}  some clean), SPP's reliability-aware Dijkstra shines by routing around the damage.                ${CYAN}║${NC}"
echo -e "${CYAN}║${NC}  SPP also picks shorter paths (2 hops vs 4), which improves both delivery rate AND latency.        ${CYAN}║${NC}"
echo -e "${BOLD}${CYAN}╚════════════════════════════════════════════════════════════════════════════════════════════════════╝${NC}"

# ── Cleanup ──
echo ""
echo -e "${YELLOW}Leave the topology running? (y/n)${NC}"
read -r keep_running
if [[ "$keep_running" != "y" ]]; then
    print_step "Tearing down..."
    $COMPOSE down --remove-orphans
    print_ok "Cleaned up"
fi
