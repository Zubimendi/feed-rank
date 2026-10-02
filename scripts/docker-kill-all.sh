#!/usr/bin/env bash
set -eo pipefail

# ==============================================================================
# docker-kill-all: Stop and clean up Docker containers for clean project testing
# ==============================================================================

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

MODE="stop"

for arg in "$@"; do
    case "$arg" in
        -f|--force)
            MODE="force"
            ;;
        -c|--clean)
            MODE="clean"
            ;;
        --nuke)
            MODE="nuke"
            ;;
        -h|--help)
            echo "Usage: docker-kill-all [options]"
            echo ""
            echo "Options:"
            echo "  (no args)     Gracefully stops all currently running containers"
            echo "  -f, --force   Force-kills all running containers immediately"
            echo "  -c, --clean   Stops & removes ALL containers (running + exited) and dangling networks"
            echo "  --nuke        Clean slate: removes all containers, networks, and unused volumes"
            echo "  -h, --help    Show this help message"
            exit 0
            ;;
        *)
            echo -e "${RED}Unknown option: $arg${NC}"
            echo "Run 'docker-kill-all --help' for usage."
            exit 1
            ;;
    esac
done

echo -e "${BLUE}==> Checking running Docker containers...${NC}"
RUNNING=$(docker ps -q)

if [ -z "$RUNNING" ]; then
    echo -e "${GREEN}No running containers found.${NC}"
else
    COUNT=$(echo "$RUNNING" | wc -l)
    if [ "$MODE" = "force" ] || [ "$MODE" = "clean" ] || [ "$MODE" = "nuke" ]; then
        echo -e "${YELLOW}Killing $COUNT running container(s)...${NC}"
        docker kill $RUNNING >/dev/null
    else
        echo -e "${YELLOW}Stopping $COUNT running container(s)...${NC}"
        docker stop $RUNNING >/dev/null
    fi
    echo -e "${GREEN}✓ All running containers stopped.${NC}"
fi

if [ "$MODE" = "clean" ] || [ "$MODE" = "nuke" ]; then
    ALL_CONTAINERS=$(docker ps -aq)
    if [ -n "$ALL_CONTAINERS" ]; then
        echo -e "${YELLOW}Removing all stopped/exited containers...${NC}"
        docker rm -f $ALL_CONTAINERS >/dev/null
        echo -e "${GREEN}✓ Containers removed.${NC}"
    fi

    echo -e "${YELLOW}Pruning dangling docker networks...${NC}"
    docker network prune -f >/dev/null 2>&1 || true
    echo -e "${GREEN}✓ Unused networks cleaned up.${NC}"
fi

if [ "$MODE" = "nuke" ]; then
    echo -e "${YELLOW}Pruning dangling volumes...${NC}"
    docker volume prune -f >/dev/null 2>&1 || true
    echo -e "${GREEN}✓ Dangling volumes cleaned up.${NC}"
fi

echo -e "${GREEN}==> Done! Ready for your next project.${NC}"
