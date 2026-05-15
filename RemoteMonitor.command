#!/bin/bash

BASE_DIR="$(cd "$(dirname "$0")" && pwd)"
PID_FILE="$BASE_DIR/server.pid"
LOCAL_IP=$(ipconfig getifaddr en0 2>/dev/null || ifconfig | grep "inet " | grep -v 127.0.0.1 | awk '{print $2}' | head -1)

start_server() {
    if lsof -ti :3000 >/dev/null 2>&1; then
        echo "Server already running on port 3000"
        return
    fi
    cd "$BASE_DIR"
    nohup node server.source.js > "$BASE_DIR/server.log" 2>&1 &
    echo $! > "$PID_FILE"
    sleep 2
    if lsof -ti :3000 >/dev/null 2>&1; then
        echo "Server started on port 3000"
    else
        echo "Failed to start server. Check server.log"
    fi
}

stop_server() {
    if [ -f "$PID_FILE" ]; then
        kill $(cat "$PID_FILE") 2>/dev/null
        rm -f "$PID_FILE"
    fi
    pkill -f "node server.source.js" 2>/dev/null
    echo "Server stopped"
}

while true; do
    clear
    echo "=========================================="
    echo "   REMOTE MONITOR - v6.0.9"
    echo "=========================================="
    echo ""
    echo "1) Check Status"
    echo "2) Start Server"
    echo "3) Stop Server"
    echo "4) Restart Server"
    echo "5) Open Dashboard"
    echo "6) View Connected Agents"
    echo "7) Find Server IP (scan network)"
    echo "8) Set Windows PC as Server"
    echo "9) Switch to Internal Mode (LAN only)"
    echo "10) Switch to Cloud Mode"
    echo "11) Install auto-start (Mac boot)"
    echo "12) Deploy to VPS instructions"
    echo "13) Exit"
    echo "=========================================="
    echo -n "Choose (1-13): "
    read c
    case "$c" in
        1)
            clear
            echo "=== STATUS ==="
            echo ""
            echo "--- Server ---"
            if lsof -ti :3000 >/dev/null 2>&1; then
                echo "Local Server: Running on port 3000"
                echo "Dashboard:    http://localhost:3000"
                echo "Local IP:     http://$LOCAL_IP:3000"
            else
                echo "Local Server: Stopped"
            fi
            echo ""
            echo "--- urls.ini (agent config) ---"
            if [ -f "$BASE_DIR/urls.ini" ]; then
                cat "$BASE_DIR/urls.ini"
            else
                echo "(using defaults)"
            fi
            echo ""
            echo "--- Connected Agents ---"
            curl -s -u 'puneet:puneet12' http://localhost:3000/api/agents 2>/dev/null | python3 -c "
import json,sys
try:
    agents=json.load(sys.stdin)
    if agents:
        for a in agents:
            print(f\"  {a['name']} ({a.get('ip','?')})\")
    else:
        print('  (no agents connected)')
except: print('  (server not running)')
" 2>/dev/null
            echo -n "Press Enter..."; read
            ;;
        2)
            start_server
            echo -n "Press Enter..."; read
            ;;
        3)
            stop_server
            echo -n "Press Enter..."; read
            ;;
        4)
            stop_server
            sleep 1
            start_server
            echo -n "Press Enter..."; read
            ;;
        5)
            open http://localhost:3000
            ;;
        6)
            clear
            echo "=== CONNECTED AGENTS ==="
            echo ""
            curl -s -u 'puneet:puneet12' http://localhost:3000/api/agents 2>/dev/null | python3 -c "
import json,sys
try:
    agents=json.load(sys.stdin)
    if not agents:
        print('No agents connected')
    else:
        for i,a in enumerate(agents):
            print(f'{i+1}) {a[\"name\"]} - IP: {a.get(\"ip\",\"?\")} - ID: {a[\"id\"]}')
except: print('Server not running')
" 2>/dev/null
            echo -n "Press Enter..."; read
            ;;
        7)
            clear
            echo "=== FIND SERVER IP ON NETWORK ==="
            echo ""
            echo "Scanning local network for active servers..."
            echo ""
            SUBNET=$(echo $LOCAL_IP | cut -d. -f1-3)
            FOUND=0
            for i in $(seq 1 254); do
                IP="$SUBNET.$i"
                result=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 0.5 "http://$IP:3000/" 2>/dev/null)
                if [ ! -z "$result" ] && [ "$result" != "000" ]; then
                    echo "  Found: $IP:3000"
                    FOUND=$((FOUND+1))
                fi
            done
            echo ""
            if [ "$FOUND" -eq 0 ]; then
                echo "  No server found on local network"
            else
                echo "  Found $FOUND server(s)"
            fi
            echo -n "Press Enter..."; read
            ;;
        8)
            clear
            echo "=== SET WINDOWS PC AS SERVER ==="
            echo ""
            echo "Fetching connected agents..."
            echo ""
            curl -s -u 'puneet:puneet12' http://localhost:3000/api/agents 2>/dev/null | python3 -c "
import json,sys
try:
    agents=json.load(sys.stdin)
    if not agents:
        print('No agents connected')
    else:
        for i,a in enumerate(agents):
            print(f'{i+1}) {a[\"name\"]} ({a.get(\"ip\",\"?\")})')
except: print('Server not running or no auth')
" 2>/dev/null
            echo ""
            echo -n "Enter number to make server (or 0 to cancel): "
            read sn
            if [ "$sn" = "0" ] || [ -z "$sn" ]; then
                echo "Cancelled"
            else
                AGENT_ID=$(curl -s -u 'puneet:puneet12' http://localhost:3000/api/agents 2>/dev/null | python3 -c "
import json,sys
agents=json.load(sys.stdin)
try:
    i=int('$sn')-1
    if i>=0 and i<len(agents):
        print(agents[i]['id'])
except: pass
" 2>/dev/null)
                if [ ! -z "$AGENT_ID" ]; then
                    curl -s -X POST -u 'puneet:puneet12' "http://localhost:3000/api/make-server/$AGENT_ID" 2>/dev/null
                    echo "Server mode activated for agent"
                else
                    echo "Invalid selection"
                fi
            fi
            echo -n "Press Enter..."; read
            ;;
        9)
            clear
            echo "=== SWITCH TO INTERNAL MODE ==="
            echo ""
            echo "This disables all cloud connections."
            echo "Agents will ONLY work on the local network."
            echo ""
            echo "1) Set this Mac to Internal mode"
            echo "2) Create config for Windows PCs (internal)"
            echo ""
            echo -n "Choose (1-2): "
            read ic
            if [ "$ic" = "1" ]; then
                echo "auto-local" > "$BASE_DIR/urls.ini"
                echo "Mac set to Internal mode"
            elif [ "$ic" = "2" ]; then
                echo "auto-local" > "$BASE_DIR/urls.ini"
                cp "$BASE_DIR/urls.ini" ~/Desktop/urls.ini
                echo "urls.ini created on Desktop. Copy next to .exe on Windows PCs."
            fi
            echo -n "Press Enter..."; read
            ;;
        10)
            clear
            echo "=== SWITCH TO CLOUD MODE ==="
            echo ""
            echo "1) Render.com (default)"
            echo "2) Direct IP"
            echo "3) Auto (try all)"
            echo ""
            echo -n "Choose (1-3): "
            read sc
            case "$sc" in
                1) echo "wss://pu-k752.onrender.com" > "$BASE_DIR/urls.ini"; echo "Set to Render.com" ;;
                2) echo "ws://43.247.40.101:3000" > "$BASE_DIR/urls.ini"; echo "Set to Direct IP" ;;
                3) echo "auto" > "$BASE_DIR/urls.ini"; echo "Set to Auto mode" ;;
                *) echo "Invalid" ;;
            esac
            echo -n "Press Enter..."; read
            ;;
        11)
            clear
            echo "=== INSTALL AUTO-START ==="
            echo ""
            echo "This will create a LaunchAgent to start the server on Mac boot."
            echo ""
            echo -n "Continue? (y/n): "
            read yn
            if [ "$yn" = "y" ]; then
                launchDir="$HOME/Library/LaunchAgents"
                mkdir -p "$launchDir"
                cat > "$launchDir/com.remotemonitor.server.plist" << EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>com.remotemonitor.server</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/node</string>
        <string>$BASE_DIR/server.source.js</string>
    </array>
    <key>WorkingDirectory</key><string>$BASE_DIR</string>
    <key>KeepAlive</key><true/>
    <key>RunAtLoad</key><true/>
</dict>
</plist>
EOF
                launchctl load "$launchDir/com.remotemonitor.server.plist" 2>/dev/null
                echo "Auto-start installed. Server will start on Mac boot."
            fi
            echo -n "Press Enter..."; read
            ;;
        12)
            clear
            echo "=== DEPLOY SERVER ON VPS ==="
            echo ""
            echo "Run server on a \$5/month VPS instead of your laptop."
            echo ""
            echo "Steps:"
            echo "1. Get a VPS (DigitalOcean, Linode, Hetzner)"
            echo "2. SSH into it"
            echo "3. Install Node.js:"
            echo "   curl -fsSL https://deb.nodesource.com/setup_20.x | bash -"
            echo "   apt install -y nodejs"
            echo ""
            echo "4. Copy this folder to VPS:"
            echo "   scp -r . root@YOUR_VPS_IP:/opt/remote-monitor/"
            echo ""
            echo "5. Start server on VPS:"
            echo "   cd /opt/remote-monitor && npm install && node server.source.js &"
            echo ""
            echo "6. Update agents to use VPS IP:"
            echo "   Edit urls.ini -> ws://YOUR_VPS_IP:3000"
            echo ""
            echo "No tunnels! No laptop needed! Always on!"
            echo -n "Press Enter..."; read
            ;;
        13) exit 0
            ;;
        *) echo "Invalid"; echo -n "Press Enter..."; read
    esac
done
