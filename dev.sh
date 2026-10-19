#!/usr/bin/env bash
# dev.sh — Starts proxy, admin API, and UI in dev mode.
# Ctrl+C kills all processes via process group trap.
set -e

BIN_DIR="$1"
UI_DIR="$2"

# Use PROXY_ADMIN_KEY from environment if already set, otherwise generate one.
if [ -z "${PROXY_ADMIN_KEY:-}" ]; then
  PROXY_ADMIN_KEY="adm_dev_$(openssl rand -hex 16)"
fi
export PROXY_ADMIN_KEY
ADMIN_KEY="$PROXY_ADMIN_KEY"

cleanup() {
  echo ""
  echo "  → Shutting down all services..."
  kill 0 2>/dev/null
  exit 0
}
trap cleanup INT TERM EXIT

echo ""
echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║     PiProxy — Dev Mode                              ║"
echo "  ║                                                    ║"
echo "  ║  Proxy       →  http://localhost:8080               ║"
echo "  ║  Admin API   →  http://localhost:8081               ║"
echo "  ║  Admin UI    →  http://localhost:3333               ║"
echo "  ║  Metrics     →  http://localhost:9090/metrics       ║"
echo "  ║                                                    ║"
echo "  ║  Login key:  $ADMIN_KEY                             ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo ""
echo "  → Starting proxy, admin, and UI (Ctrl+C to stop all)..."
echo ""

"$BIN_DIR/proxy" 2>&1 | sed 's/^/[proxy] /' &
"$BIN_DIR/admin" 2>&1 | sed 's/^/[admin] /' &

if [ -d "$UI_DIR/node_modules" ] && [ -f "$UI_DIR/package.json" ]; then
  cd "$UI_DIR" && pnpm dev 2>&1 | sed 's/^/[ui] /'
else
  echo "[dev] UI not set up — run 'make ui-install' first"
  while true; do sleep 1; done
fi
