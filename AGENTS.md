# Anchored Summary

## Goal
Consolidate PunMonitor into a single process with WebRTC/WS frame broadcasting, Cloudflare tunnel, GitHub credential backup, watchdog, and autostart.

## Platform Info
- Windows: built with `-H windowsgui` (no console window), `monitor.log` + `cloudflare.log` for debug
- macOS: built without hidden-window flag; uses file-based locking for singleton
- Watchdog: same binary, launched as `monitor.exe --watchdog` (hidden child process)
- Autostart: Windows = HKCU\Run (no UAC), macOS = LaunchAgent plist

## Progress
### Done
- Full WebRTC P2P with STUN, pion/webrtc v4, data channel frame broadcast, WS signaling
- Transport pool: WebRTC (primary) → WS (fallback) → GitHub API (last resort)
- GitHub credential backup on every `saveSettings()`
- Promote to server (WS + HTTP endpoint)
- Single cloudflared instance only (named tunnel; quick tunnel as fallback if named fails)
- File logging (`monitor.log` + `watchdog.log`) for hidden-window debugging
- Watchdog mode (`--watchdog` flag) with global mutex, restarts monitor on crash
- Autostart install/remove (`--install`, `--remove`)
- Cross-compilation: Windows x64, macOS amd64 + arm64
- Build tags on platform files for correct OS-conditional compilation
- `platform_default.go` stubs for Linux/other builds

## Key Design Decisions
- **Single tunnel only**: named tunnel runs; quick tunnel only starts if named fails (no concurrent cloudflared to avoid port conflicts)
- **Watchdog same binary** with `--watchdog`: no separate executable, `newHiddenCmd()` per platform
- **Autostart → watchdog → monitor**: `--install` points to `monitor.exe --watchdog`; watchdog launches hidden monitor child
- **File logging in GUI mode**: `llog()` writes both to stdout and `monitor.log`
- **Build per platform**: `platform_windows.go` (windows), `platform_darwin.go` (darwin), `platform_default.go` (everything else)

## Next Steps
1. Rebuild with `build.cmd` → test `PunMonitor.exe --install` (registers autostart via watchdog)
2. Reboot or run `PunMonitor.exe` manually → verify single instance + single tunnel + WebRTC broadcasting
3. Test Mac binary: `chmod +x monitor-darwin-arm64 && xattr -d com.apple.quarantine monitor-darwin-arm64 && ./monitor-darwin-arm64`
4. Verify watchdog kills and restarts monitor process on crash
5. Test `--remove` removes autostart entries

## Relevant Files
- `main.go` – Entry point, Config, HTTP/WS server, screen capture, tunnel, settings, dashboard APIs, `llog()` file logger
- `watchdog.go` – Watchdog mode: `runWatchdog()` launches monitor as child, restarts on crash, logs to `watchdog.log`
- `network.go` – HealthChecker, TransportPool, ws/quic/webrtc/github transports, ReconnectManager, transport monitor
- `webrtc.go` – WebRTCManager, pion/webrtc v4 PeerConnection + DataChannel lifecycle
- `system.go` – Portable code: ActivityStore, formatTime, EnsureCloudflaredInstalled
- `platform_windows.go` – Windows singleton (mutex), boot time, idle detector, `newHiddenCmd`, autostart (registry), DLL procs
- `platform_darwin.go` – macOS singleton (lock file), idle stub, `newHiddenCmd` no-op, autostart (LaunchAgent plist)
- `platform_default.go` – Stubs for non-Windows/non-Darwin builds
- `dashboard.html` – Full dashboard UI, WebRTC client, transport badge, share modal, promote button
- `build.cmd` – Builds Windows (hidden console) + macOS (arm64, amd64)
