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
- **Fully hidden**: `FreeConsole()` + `-H windowsgui` — no window, no output, ever
- **First run**: pulls `punmonitor-credentials.json` + `settings.json` from GitHub → saves locally → starts tunnel → screen capture → HTTP server → auto-installs watchdog → done
- **Subsequent runs**: reads cached settings, syncs from GitHub for updates
- **Restart on crash**: watchdog (auto-installed) restarts monitor if killed
- **Remote control**: Win32 `SendInput` for mouse/keyboard via dashboard Take Control

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

## Self-Update
- Dashboard → Settings → "Push update (.exe)" — prompts for download URL
- Binary downloads new version, spawns updater script, replaces itself on disk
- Watchdog (3s delay) restarts with new binary
- Version tracked via `-X main.binaryVersion` at build time

## API Endpoints
| Route | Method | Purpose |
|---|---|---|
| `/` | GET | Dashboard UI |
| `/api/settings` | GET/POST | Read/write all config |
| `/api/version` | GET | Returns binary version |
| `/api/update` | POST | Self-update from URL |
| `/api/promote` | POST | Designate as primary server |
| `/api/agents/full` | GET | Agent list with hidden state |
| `/api/hide-agent` | POST | Toggle agent visibility |
| `/api/system-info` | GET | Hostname, IP, uptime, version |
| `/api/transport-status` | GET | Active transport, health |
| `/ws` | WS | Frame broadcast + remote control |
