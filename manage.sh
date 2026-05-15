#!/bin/bash
# Remote Monitor - Server Management Script
# Usage: ./manage.sh {start|stop|restart|status|watchdog}
#
# Commands:
#   start    - Start the Node.js server
#   stop     - Stop the server
#   restart  - Restart the server
#   status   - Check server status
#   watchdog - Keep server alive (auto-restart loop)

BASE_DIR="$(cd "$(dirname "$0")" && pwd)"
PID_FILE="$BASE_DIR/server.pid"
LOG_FILE="$BASE_DIR/server.log"

# ──────────────────────────────────────────────
# START
# ──────────────────────────────────────────────
cmd_start() {
    if [ -f "$PID_FILE" ]; then
        PID=$(cat "$PID_FILE")
        if kill -0 $PID 2>/dev/null; then
            echo "Server already running (PID: $PID)"
            echo "   Dashboard: http://localhost:3000"
            return 0
        else
            rm -f "$PID_FILE"
        fi
    fi

    echo "Starting Remote Monitor Server..."
    cd "$BASE_DIR"
    nohup node server.source.js > "$LOG_FILE" 2>&1 &
    SERVER_PID=$!
    echo $SERVER_PID > "$PID_FILE"
    sleep 3

    if kill -0 $SERVER_PID 2>/dev/null; then
        echo "Server started (PID: $SERVER_PID)"
        echo "   Dashboard: http://localhost:3000"
        echo "   Login:     puneet / puneet12"
    else
        echo "Failed to start server. Check $LOG_FILE"
        rm -f "$PID_FILE"
        return 1
    fi
}

# ──────────────────────────────────────────────
# STOP
# ──────────────────────────────────────────────
cmd_stop() {
    if [ ! -f "$PID_FILE" ]; then
        echo "Server is not running"
        return 0
    fi

    PID=$(cat "$PID_FILE")
    if kill -0 $PID 2>/dev/null; then
        echo "Stopping server (PID: $PID)..."
        kill $PID
        sleep 2
        kill -9 $PID 2>/dev/null
        echo "Server stopped"
    else
        echo "Server was not running (stale PID)"
    fi
    rm -f "$PID_FILE"
}

# ──────────────────────────────────────────────
# STATUS
# ──────────────────────────────────────────────
cmd_status() {
    echo "=== Server Status ==="
    if [ -f "$PID_FILE" ]; then
        PID=$(cat "$PID_FILE")
        if kill -0 $PID 2>/dev/null; then
            echo "Server running (PID: $PID)"
            echo "   Dashboard: http://localhost:3000"
        else
            echo "Server not running (stale PID)"
            rm -f "$PID_FILE"
        fi
    else
        echo "Server not running"
    fi

    echo ""
    echo "=== Connected Agents ==="
    curl -s -u 'puneet:puneet12' http://localhost:3000/api/agents 2>/dev/null | \
        python3 -c "
import json,sys
try:
    agents=json.load(sys.stdin)
    if agents:
        for a in agents: print(f\"  {a['name']} ({a.get('ip','?')})\")
    else: print('  (none connected)')
except: print('  (server not running)')
" 2>/dev/null
}

# ──────────────────────────────────────────────
# WATCHDOG  (blocking loop — run in background)
# ──────────────────────────────────────────────
cmd_watchdog() {
    echo "Watchdog started — checks every 30s, auto-restarts if server is down"
    echo "Press Ctrl+C to stop."
    while true; do
        if ! curl -s -o /dev/null -u 'puneet:puneet12' http://localhost:3000 2>/dev/null; then
            echo "$(date): Server down — restarting..."
            cmd_start
        fi
        sleep 30
    done
}

# ──────────────────────────────────────────────
# DISPATCH
# ──────────────────────────────────────────────
case "$1" in
    start)    cmd_start ;;
    stop)     cmd_stop ;;
    restart)  cmd_stop; sleep 2; cmd_start ;;
    status)   cmd_status ;;
    watchdog) cmd_watchdog ;;
    *)
        echo "Remote Monitor - Server Management"
        echo ""
        echo "Usage: $0 {start|stop|restart|status|watchdog}"
        echo ""
        echo "  start    - Start the Node.js server"
        echo "  stop     - Stop the server"
        echo "  restart  - Restart the server"
        echo "  status   - Show server + agent status"
        echo "  watchdog - Auto-restart loop (run in background with &)"
        echo ""
        cmd_status
        ;;
esac
