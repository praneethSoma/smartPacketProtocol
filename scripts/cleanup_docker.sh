#!/usr/bin/env bash
# ═══════════════════════════════════════════════════════════════
# Docker Cleanup Helper Script for SPP Project
# ═══════════════════════════════════════════════════════════════
#
# Usage:
#   bash scripts/cleanup_docker.sh           # Safe cleanup (build cache only)
#   bash scripts/cleanup_docker.sh full      # Full cleanup (everything unused)
#   bash scripts/cleanup_docker.sh spp       # Clean SPP containers only
#   bash scripts/cleanup_docker.sh check     # Just check disk usage
#
# ═══════════════════════════════════════════════════════════════

set -euo pipefail

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

MODE="${1:-safe}"

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

show_usage() {
    echo -e "${BOLD}Docker Cleanup Helper Script${NC}"
    echo ""
    echo -e "${CYAN}Usage:${NC}"
    echo "  bash scripts/cleanup_docker.sh           # Safe cleanup (build cache only)"
    echo "  bash scripts/cleanup_docker.sh full      # Full cleanup (everything unused)"
    echo "  bash scripts/cleanup_docker.sh spp       # Clean SPP containers only"
    echo "  bash scripts/cleanup_docker.sh check     # Just check disk usage"
    echo ""
    echo -e "${CYAN}Options:${NC}"
    echo "  safe  - Remove build cache only (default, safest)"
    echo "  full  - Remove build cache + unused images + stopped containers"
    echo "  spp   - Stop and remove SPP containers only"
    echo "  check - Display current Docker disk usage"
    exit 0
}

check_disk_usage() {
    print_header "Docker Disk Usage"
    docker system df
    echo ""
    echo -e "${CYAN}Memory Usage:${NC}"
    free -h
}

cleanup_safe() {
    print_header "Safe Cleanup - Build Cache Only"

    print_step "Removing Docker build cache..."
    local reclaimed
    reclaimed=$(docker builder prune -f 2>&1 | grep "Total reclaimed space:" || echo "Total reclaimed space: 0B")
    print_ok "$reclaimed"

    echo ""
    check_disk_usage
    print_ok "Safe cleanup complete!"
}

cleanup_full() {
    print_header "Full Cleanup - All Unused Resources"

    echo -e "${RED}${BOLD}⚠ WARNING: This will remove:${NC}"
    echo -e "${RED}  - All build cache${NC}"
    echo -e "${RED}  - All stopped containers${NC}"
    echo -e "${RED}  - All unused images${NC}"
    echo -e "${RED}  - All unused networks${NC}"
    echo ""
    echo -e "${YELLOW}Your running SPP containers will NOT be affected.${NC}"
    echo ""
    read -p "Continue? (y/N): " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        echo "Cancelled."
        exit 0
    fi

    print_step "Removing build cache..."
    docker builder prune -af

    print_step "Removing stopped containers..."
    docker container prune -f

    print_step "Removing unused images..."
    docker image prune -af

    print_step "Removing unused networks..."
    docker network prune -f

    echo ""
    check_disk_usage
    print_ok "Full cleanup complete!"
}

cleanup_spp() {
    print_header "SPP Containers Cleanup"

    print_step "Stopping and removing SPP containers..."
    docker compose -f docker-compose.7router.yml down --remove-orphans
    print_ok "SPP containers removed"

    echo ""
    docker ps -a | grep spp || echo -e "  ${GREEN}No SPP containers running${NC}"
}

# ═══════════════════════════════════════════════════════════════
# Main
# ═══════════════════════════════════════════════════════════════

case "$MODE" in
    safe)
        cleanup_safe
        ;;
    full)
        cleanup_full
        ;;
    spp)
        cleanup_spp
        ;;
    check)
        check_disk_usage
        ;;
    -h|--help|help)
        show_usage
        ;;
    *)
        echo -e "${RED}Unknown option: $MODE${NC}"
        echo ""
        show_usage
        ;;
esac

echo ""
echo -e "${GREEN}${BOLD}Tip:${NC} Run this weekly to prevent cache buildup:"
echo -e "  ${CYAN}bash scripts/cleanup_docker.sh${NC}"
echo ""
