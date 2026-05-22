#!/usr/bin/env bash
# cleanup.sh - PunMonitor + Cloudflare Tunnel + Node.js removal (macOS/Linux)
set -e
echo "PunMonitor cleanup..."
# 1. Kill processes (PunMonitor, cloudflared, node)
pkill -f "PunMonitor" 2>/dev/null || true
pkill -f "cloudflared" 2>/dev/null || true
pkill -f "node" 2>/dev/null || true
# 2. Remove common directories
DIRS=(
    "$HOME/.config/PunMonitor"
    "$HOME/.punmonitor"
    "$HOME/Library/Application Support/PunMonitor"
    "/usr/local/var/punmonitor"
    "/opt/punmonitor"
    "./PunMonitor.exe"
    "./punmonitor"
    "./logs"
)
for dir in "${DIRS[@]}"; do
    if [ -e "$dir" ]; then
        echo "Removing: $dir"
        rm -rf "$dir"
    fi
done
# 3. macOS launch agents/daemons
if [[ "$OSTYPE" == "darwin"* ]]; then
    LAUNCH_AGENTS=(
        "$HOME/Library/LaunchAgents/com.punmonitor.agent.plist"
        "/Library/LaunchDaemons/com.punmonitor.agent.plist"
    )
    for plist in "${LAUNCH_AGENTS[@]}"; do
        if [ -f "$plist" ]; then
            echo "Unloading: $plist"
            launchctl unload "$plist" 2>/dev/null || true
            rm -f "$plist"
        fi
    done
fi
echo -e "\nPunMonitor cleanup complete!"
echo "You may want to log out/in or restart to ensure no handles remain."
