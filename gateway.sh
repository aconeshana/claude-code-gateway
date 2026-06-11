#!/bin/bash
# Development helper — wraps `ccg` for convenience.
# The start/stop/restart/status/logs logic now lives in the ccg binary itself.
set -e

DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

BINARY="./ccg"

usage() {
    echo "Usage: $0 {build|start|stop|restart|status|logs}"
    echo ""
    echo "  build   - Build the ccg binary (go build + codesign)"
    echo "  start   - Build then delegate to: ccg start"
    echo "  stop    - Delegate to: ccg stop"
    echo "  restart - Delegate to: ccg restart"
    echo "  status  - Delegate to: ccg status"
    echo "  logs    - Delegate to: ccg logs"
    echo ""
    echo "For full options see: ccg help"
    exit 1
}

do_build() {
    echo "Building ccg..."
    go build -o ccg .
    codesign -s - ccg 2>/dev/null || true
    # macOS firewall whitelist
    sudo /usr/libexec/ApplicationFirewall/socketfilterfw --add "$DIR/ccg" > /dev/null 2>&1 || true
    sudo /usr/libexec/ApplicationFirewall/socketfilterfw --unblockapp "$DIR/ccg" > /dev/null 2>&1 || true
    echo "Build done."
}

case "${1:-}" in
    build)          do_build ;;
    start)          do_build; "$BINARY" start "${@:2}" ;;
    stop)           "$BINARY" stop ;;
    restart)        do_build; "$BINARY" restart "${@:2}" ;;
    status)         "$BINARY" status ;;
    logs)           "$BINARY" logs "${@:2}" ;;
    *)              usage ;;
esac
