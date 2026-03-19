#!/usr/bin/env bash
# ═══════════════════════════════════════════════════════════════
# SPP Path Flapping / Oscillation Test — 7-Router Topology
# ═══════════════════════════════════════════════════════════════
#
# Tests whether SPP ping-pongs between paths when congestion
# alternates on router_f (the shortcut): slow → clean → slow.
#
# Topology (docker-compose.7router.yml):
#
#                     ┌─── router_b ─── router_d ───┐
#   sender → router_a ──┤                              ├── router_g → receiver
#                     ├─── router_c ─── router_e ───┘
#                     └─── router_f ────────────────────────→ receiver
#
#   Path 1 (shortcut):  A → F → receiver           (2 hops — SPP default)
#   Path 2 (highway):   A → B → D → G → receiver   (4 hops)
#   Path 3 (alternate): A → C → E → G → receiver   (4 hops)
#
# We inject oscillating congestion on router_f to see if SPP
# flaps between the shortcut and longer paths.
#
# Usage:
#   bash scripts/test_flapping.sh
#   SKIP_BUILD=1 bash scripts/test_flapping.sh
# ═══════════════════════════════════════════════════════════════
set -euo pipefail

COMPOSE="docker compose -f docker-compose.7router.yml"
GOSSIP_WAIT=12
DELIVERY_WAIT=5

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

print_header() {
    echo -e "\n${CYAN}╔══════════════════════════════════════════════════════════╗${NC}"
    echo -e "${CYAN}║  $1${NC}"
    echo -e "${CYAN}╚══════════════════════════════════════════════════════════╝${NC}"
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

inject_delay() {
    local container="$1" delay="$2" loss="${3:-0}"
    echo -e "    ${BOLD}Injecting: ${delay}ms delay, ${loss}% loss on ${container}${NC}"
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

clear_all_delays() {
    print_step "Clearing all tc rules..."
    for router in spp-router-a spp-router-b spp-router-c spp-router-d spp-router-e spp-router-f spp-router-g; do
        clear_delay "$router"
    done
    print_ok "All delays cleared"
}

# Send N packets and capture the last 3 lines of sender output
send_packets() {
    local count="$1" interval_ms="$2" label="$3"
    local config="/tmp/spp_flap_sender.json"
    cat > "$config" << EOF
{
  "NodeName": "sender",
  "RouterAddr": "10.1.1.2:8001",
  "GossipAddr": "10.1.1.2:7001",
  "Destination": "receiver",
  "FirstRouter": "router_a",
  "Payload": "flap_test_${label}",
  "IntentLatency": 3,
  "IntentReliability": 1,
  "GossipRetries": 10,
  "GossipRetryBackoffMs": 500,
  "PacketCount": ${count},
  "IntervalMs": ${interval_ms}
}
EOF

    $COMPOSE run --rm \
        -v "${config}:/app/configs/topologies/7router/flap_sender.json:ro" \
        sender ./bin/sender configs/topologies/7router/flap_sender.json 2>&1 | tail -3

    rm -f "$config"
}

# Extract a count from logs safely (avoids newline in grep -c output)
count_matches() {
    local pattern="$1"
    local logs="$2"
    local n
    n=$(echo "$logs" | grep -c "$pattern" 2>/dev/null || true)
    # Trim whitespace/newlines
    echo "$n" | tr -d '[:space:]'
}

# Extract the path from the last "Path taken:" line
extract_path() {
    local logs="$1"
    echo "$logs" | grep "Path taken:" | tail -1 | sed 's/.*Path taken:\s*//' | sed 's/\s*║.*//' | xargs 2>/dev/null || echo "unknown"
}

# ═══════════════════════════════════════════════════════════════
# Main
# ═══════════════════════════════════════════════════════════════

print_header "SPP Path Flapping / Oscillation Test (7-Router)"
echo ""
echo -e "${BOLD}Topology:${NC}"
echo "                    ┌─── router_b ─── router_d ───┐"
echo "  sender → router_a ──┤                              ├── router_g → receiver"
echo "                    ├─── router_c ─── router_e ───┘"
echo "                    └─── router_f ────────────────────────→ receiver"
echo ""
echo "  Shortcut:  A → F → receiver           (2 hops — SPP default)"
echo "  Highway:   A → B → D → G → receiver   (4 hops)"
echo "  Alternate: A → C → E → G → receiver   (4 hops)"
echo ""
echo "  We oscillate congestion on router_f (shortcut) to test flapping."

# ── Setup ──
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
echo "  Waiting ${GOSSIP_WAIT}s for gossip convergence..."
sleep "$GOSSIP_WAIT"

# Verify health (retry up to 10s for stragglers)
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

# Storage for per-phase results
declare -a PHASE_LABELS=()
declare -a PHASE_DELIVERED=()
declare -a PHASE_REROUTED=()
declare -a PHASE_PATHS=()

run_phase() {
    local phase_num="$1"
    local label="$2"
    local congestion_target="$3"  # container name or "none"
    local delay="${4:-0}"
    local loss="${5:-0}"

    print_header "Phase ${phase_num}: ${label}"

    if [[ "$congestion_target" == "none" ]]; then
        clear_all_delays
    else
        inject_delay "$congestion_target" "$delay" "$loss"
    fi

    echo "  Waiting ${GOSSIP_WAIT}s for gossip to propagate..."
    sleep "$GOSSIP_WAIT"

    local pre_offset
    pre_offset=$(receiver_log_offset)

    send_packets 10 500 "phase${phase_num}"
    sleep "$DELIVERY_WAIT"

    local logs
    logs=$(receiver_logs_since "$pre_offset")

    local delivered rerouted path
    delivered=$(count_matches "SMART PACKET DELIVERED" "$logs")
    rerouted=$(count_matches "REROUTED" "$logs")
    path=$(extract_path "$logs")

    print_ok "Delivered: ${delivered}/10, Rerouted: ${rerouted}"
    print_ok "Path: ${path}"

    PHASE_LABELS+=("$label")
    PHASE_DELIVERED+=("$delivered")
    PHASE_REROUTED+=("$rerouted")
    PHASE_PATHS+=("$path")
}

# ═══════════════════════════════════════════════════════════════
# Run 5 phases: oscillate congestion on router_f
# ═══════════════════════════════════════════════════════════════

run_phase 1 "Baseline — all healthy" "none"
run_phase 2 "F congested (+100ms, 15% loss)" "spp-router-f" 100 15
run_phase 3 "F recovered — congestion cleared" "none"
run_phase 4 "F congested again (+100ms, 15% loss)" "spp-router-f" 100 15
run_phase 5 "F recovered — final" "none"

clear_all_delays

# ═══════════════════════════════════════════════════════════════
# Analysis
# ═══════════════════════════════════════════════════════════════

echo ""
echo -e "${BOLD}${CYAN}╔══════════════════════════════════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}${CYAN}║  PATH FLAPPING ANALYSIS — 7-Router Topology                              ║${NC}"
echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"
printf "${CYAN}║${NC}  %-40s │ %9s │ %8s  ${CYAN}║${NC}\n" \
    "Phase" "Delivered" "Rerouted"
echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"

for i in "${!PHASE_LABELS[@]}"; do
    printf "${CYAN}║${NC}  %-40s │ %5s/10  │ %8s  ${CYAN}║${NC}\n" \
        "${PHASE_LABELS[$i]}" "${PHASE_DELIVERED[$i]}" "${PHASE_REROUTED[$i]}"
    printf "${CYAN}║${NC}    Path: %-60s  ${CYAN}║${NC}\n" "${PHASE_PATHS[$i]}"
done

echo -e "${BOLD}${CYAN}╠══════════════════════════════════════════════════════════════════════════╣${NC}"

# Detect path switches between phases by checking if path changed
flaps=0
for i in $(seq 1 $((${#PHASE_PATHS[@]} - 1))); do
    prev="${PHASE_PATHS[$((i-1))]}"
    curr="${PHASE_PATHS[$i]}"
    if [[ "$prev" != "$curr" && -n "$prev" && -n "$curr" && "$prev" != "unknown" && "$curr" != "unknown" ]]; then
        flaps=$((flaps + 1))
    fi
done

printf "${CYAN}║${NC}  ${BOLD}Path switches (flaps): %-47s${NC}  ${CYAN}║${NC}\n" "$flaps"

if [[ $flaps -ge 4 ]]; then
    echo -e "${CYAN}║${NC}                                                                        ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}  ${RED}${BOLD}FLAPPING CONFIRMED${NC} — SPP switches paths on every congestion        ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}  change with NO damping. In production, this causes packet              ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}  reordering and throughput degradation.                                  ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}                                                                        ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}  ${YELLOW}Recommended fixes:${NC}                                                    ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}    • Add reroute cooldown timer (e.g., 2s min between switches)         ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}    • Require sustained divergence (e.g., 3 consecutive checks)          ${CYAN}║${NC}"
    echo -e "${CYAN}║${NC}    • Add path stickiness / hysteresis                                   ${CYAN}║${NC}"
elif [[ $flaps -ge 2 ]]; then
    echo -e "${CYAN}║${NC}  ${YELLOW}Moderate flapping — SPP partially tracks oscillation.${NC}                 ${CYAN}║${NC}"
else
    echo -e "${CYAN}║${NC}  ${GREEN}Low/no flapping — SPP has natural damping or gossip lag.${NC}               ${CYAN}║${NC}"
fi

echo -e "${BOLD}${CYAN}╚══════════════════════════════════════════════════════════════════════════╝${NC}"

# Cleanup prompt
echo ""
echo -e "${YELLOW}Leave the topology running? (y/n)${NC}"
read -r keep_running
if [[ "$keep_running" != "y" ]]; then
    print_step "Tearing down..."
    $COMPOSE down --remove-orphans
    print_ok "Cleaned up"
fi
