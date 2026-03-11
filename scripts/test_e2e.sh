#!/usr/bin/env bash
# ═══════════════════════════════════════════════════════════
# SPP End-to-End Test
# Spins up the full 3-router topology, sends a packet,
# and checks that the receiver got it.
# ═══════════════════════════════════════════════════════════
set -euo pipefail

COMPOSE="docker compose"
WAIT_SECONDS=5          # Time for gossip convergence
TIMEOUT_SECONDS=10      # Max wait for receiver delivery

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${YELLOW}═══════════════════════════════════════${NC}"
echo -e "${YELLOW}  SPP End-to-End Test${NC}"
echo -e "${YELLOW}═══════════════════════════════════════${NC}"

# ── Step 1: Clean up any previous run ──
echo -e "\n[1/5] Cleaning up previous containers..."
$COMPOSE down --remove-orphans 2>/dev/null || true

# ── Step 2: Build the image ──
echo -e "\n[2/5] Building Docker image..."
$COMPOSE build

# ── Step 3: Start infrastructure (receiver + routers) ──
echo -e "\n[3/5] Starting receiver and routers..."
$COMPOSE up -d receiver router_a router_b router_c

echo -e "     Waiting ${WAIT_SECONDS}s for gossip convergence..."
sleep "$WAIT_SECONDS"

# Show router logs to confirm gossip started
echo -e "\n     Router A status:"
$COMPOSE logs --tail=5 router_a 2>&1 | head -10

# ── Step 4: Send a packet ──
echo -e "\n[4/5] Sending smart packet..."
$COMPOSE run --rm sender

# Wait a moment for the packet to traverse the network
sleep 2

# ── Step 5: Check delivery ──
echo -e "\n[5/5] Checking receiver logs..."
RECEIVER_LOGS=$($COMPOSE logs receiver 2>&1)

if echo "$RECEIVER_LOGS" | grep -q "SMART PACKET DELIVERED"; then
    echo -e "\n${GREEN}╔══════════════════════════════════════════╗${NC}"
    echo -e "${GREEN}║  ✅  TEST PASSED — Packet delivered!     ║${NC}"
    echo -e "${GREEN}╚══════════════════════════════════════════╝${NC}"
    echo -e "\nReceiver output:"
    echo "$RECEIVER_LOGS" | tail -25
    EXIT_CODE=0
else
    echo -e "\n${RED}╔══════════════════════════════════════════╗${NC}"
    echo -e "${RED}║  ❌  TEST FAILED — No delivery detected  ║${NC}"
    echo -e "${RED}╚══════════════════════════════════════════╝${NC}"
    echo -e "\nReceiver logs:"
    echo "$RECEIVER_LOGS"
    echo -e "\nAll container logs:"
    $COMPOSE logs 2>&1
    EXIT_CODE=1
fi

# ── Cleanup ──
echo -e "\nCleaning up..."
$COMPOSE down --remove-orphans

exit $EXIT_CODE
