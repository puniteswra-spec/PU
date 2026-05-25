# Anchored Summary

## Goal
Single binary, zero config shipped — self-configures from GitHub on first run. Everything manageable through dashboard.

## Architecture
- 4 .go files: `main.go` + 3 platform files (`platform_windows.go`, `platform_darwin.go`, `platform_default.go`)
- Dashboard (`dashboard.html`) served on `:8080`
- GitHub repo (`puniteswra-spec/PU`) baked at build time via `-X main.defaultGitHubRepo`
- Watchdog same binary (`--watchdog`), auto-installed on first run
- Autostart via Windows registry / macOS LaunchAgent, auto-installed on first run

## Key Behaviors
- **Fully hidden**: `FreeConsole()` + `-H windowsgui` on Windows; `setsid()` on macOS — no window, no terminal, ever
- **First run**: pulls `punmonitor-credentials.json` + `settings.json` from GitHub → starts tunnel → screen capture → HTTP server → auto-installs autostart + watchdog → done
- **Subsequent runs**: reads cached settings, syncs from GitHub for updates
- **Restart on crash**: watchdog (auto-installed via LaunchAgent / Registry) restarts monitor if killed
- **Task manager kill**: autostart reinstalled every 2 minutes; LaunchAgent `KeepAlive` / Registry ensures restart on reboot or after kill
- **Remote control**: Win32 `SendInput` for mouse/keyboard via dashboard Take Control
- **Agent transport fallback**: WebSocket → WebRTC → GitHub (tries next transport if one fails)

## Config File (in GitHub repo)
| Field | Purpose | Example |
|---|---|---|
| `github_repo` | Source of truth for config | `puniteswra-spec/PU` |
| `github_token` | For write-back (credential backup) | `ghp_xxx` |
| `tunnel_provider` | `cloudflare`, `direct`, or custom | `cloudflare` |
| `tunnel_hostname` | Public hostname for ingress | `relay.recruitedge.us` |
| `server_url` | Override share link URL entirely | `https://my-server.com` |
| `cloudflare_account_tag` | CF tunnel account | |
| `cloudflare_tunnel_secret` | CF tunnel secret | |
| `cloudflare_tunnel_id` | CF tunnel ID | |
| `election_interval` | Leader re-election interval | `5m` |

## Self-Update
- Dashboard → Settings → "Push update (.exe)" — prompts for download URL
- Binary downloads new version, spawns updater script, replaces itself on disk
- Update also broadcast to all connected agents via WebSocket (agents self-update)
- Watchdog (3s delay) restarts with new binary
- Version tracked via `-X main.binaryVersion` at build time

## Leader Election (multi‑machine)
- Every instance writes `primary_server.json` to the GitHub repo via API.
- The instance whose AgentID is in that file acts as **server** (tunnel + HTTP + screen capture).
- All other instances act as **agents** (connect to the server via WebSocket and relay frames).
- Every `election_interval` (default `5m`), each instance re-reads the file:
  - If the leader is stale (> `election_interval` since last update), any instance can take over.
  - If the AgentID is the current leader, it renews its timestamp.
- No GitHub token = always runs as standalone server.

## Agent Transport Fallback
- **Priority order**: WebSocket → WebRTC → GitHub
- Agent first tries WebSocket; if that fails, tries WebRTC signaling via the server WS; if that fails, polls GitHub API
- On reconnect, starts from the top of the priority list
- Server also has transport pool: WebRTC (pri 10) → GitHub (pri 100)
- `/api/update` broadcasts to all agents via WebSocket

## API Endpoints
| Route | Method | Purpose |
|---|---|---|
| `/` | GET | Dashboard UI |
| `/api/settings` | GET/POST | Read/write all config |
| `/api/version` | GET | Returns binary version |
| `/api/update` | POST | Self-update from URL |
| `/api/promote` | POST | Designate as primary server |
| `/api/health` | GET | Health check (no auth) |
| `/api/agents` | GET | Agent list (IDs only, includes server) |
| `/api/agents/full` | GET | Agent list with hidden state |
| `/api/agent-system-info/{id}` | GET | Per-agent system info |
| `/api/hide-agent` | POST | Toggle agent visibility |
| `/api/system-info` | GET | Hostname, IP, uptime, version |
| `/api/transport-status` | GET | Active transport, health |
| `/ws` | WS | Frame broadcast + remote control |

## Next Steps
- Deploy updated binary and verify 502 error resolved.
- Verify agent cells show correct Host/IP/WAN per agent.
- Optionally notarize macOS binary to eliminate Gatekeeper dialogs.

## Key Decisions
- **Per-agent system info**: Agent sends `systemInfo` in WebSocket hello; server stores in `agentSystemInfo` map; dashboard fetches via `/api/agent-system-info/{id}`.
- **/api/health endpoint**: Returns `{"status":"ok"}` with no auth — allows Cloudflare or external monitoring to verify server is alive.
- **Server listed in /api/agents**: Server's own AgentID included so its screen cell appears in the dashboard pill selector.
- **agentSystemInfo cleanup**: Stale agent info deleted on WebSocket disconnect to prevent memory leaks.
- **Named tunnel fallback**: If named tunnel exits with error, falls through to quick tunnel instead of leaving tunnel down.
