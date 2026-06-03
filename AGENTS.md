# Anchored Summary

## Goal
Single binary, zero config shipped — self-configures from GitHub on first run. Everything manageable through dashboard. Multi-machine leader election via GitHub. SSH server for command-line access. Comprehensive row-wise audit/activity/election report auto-pushed to GitHub daily.

## Current State (v10.0.38)
- **Live on** this machine: v10.0.38 (commit fcf4d82) — Take Control / Release button visual feedback fix
- **GitHub auth**: working — token `ghp_…Rae` (user `puniteswra-spec`) verified after restart
- **GitHub settings now have `server_url` and `tunnel_provider`** — new users skip setup wizard entirely
- **All transports**: WebRTC (priority 1), QUIC (UDP 4444), GitHub fallback, Cloudflare tunnel at `relay.recruitedge.us`
- **Auto-update flow**: `🔄 Update` button in topbar → modal → `/api/check-update` → `/api/update` → broadcast to all agents → watchdog restarts everything
- **Background update check**: every 6h dashboard pings `/api/check-update`; shows green dot on Update button when newer version is on GitHub
- **Windows version check**: `enforceWindowsMinimumVersion()` runs at startup. Logs full OS version. On Win7/8/8.1 shows `MessageBoxW` error and sleeps forever.
- **Windows Service**: optional — `--install-service` / `--remove-service` flags; uses `C:\ProgramData\PunMonitor\` for settings; machine-level DPAPI in service mode. Tested: kill worker → service respawns.
- **Just-give-the-binary deployment**: GitHub settings now have all config (server_url, tunnel_provider, tunnel_hostname, cloudflare credentials) — new machines auto-configure from GitHub sync.
- **Take Control / Release UX (v10.0.38)**: button now changes to "✓ Controlling" with green styling when active; Release becomes primary action; control bar gets green border glow.

## Architecture
- **Go files** (package main):
  - `main.go` (~6725 lines, v10.0.37): core — Config, SettingsFile, all HTTP handlers, `runAgentClient`, `startScreenCapture`, `safeWriteMessage`, `connWriteMu`, `broadcastFrame`, `selfUpdate`, `cleanOldFiles`, `runWatchdog`, `startWatchdogProcess`, `monitorWatchdogProcess`, `addHeartbeat`, `/api/assist-close`, `/api/settings` (POST re-tests auth + updates cached flag), `saveSettings`/`loadSettings`, `pushCredsToGitHub()` (v10.0.37: GETs existing SHA before PUT so updates to existing settings.json don't silently fail with 422), `syncFromGitHub()` (no longer pulls encrypted secrets), `/api/check-update`, `/api/update`, `enforceWindowsMinimumVersion()` call in main(), `ElectionStatus` struct, `setElectionStatus`, `tryClaimLeadership`/`renewLeadership`, `maskToken`, `compareVersions`, `runServerComponents`, `/api/election-history`, `/api/reports/list`, `/api/reports/merged`, `/api/report.xlsx`, `/api/election-status`, `/api/github/auth-test`, `/api/github/auth-status`, `/api/service/status`, `/api/service/sync-settings`, `var binaryVersion = "10.0.37"`, AgentID generation, `/api/system-info`, `/api/ssh-info`, native CPU/memory/boot, autostart + watchdog + `--install-service`/`--remove-service` flags + `detectAndRunService()`, `dataDir()` returns `C:\ProgramData\PunMonitor\` when `isServiceMode`.
  - `ssh_server.go` (~370 lines): `setupSSHServer`/`stopSSHServer`, `ensureSSHCredentials`, `sshSessionHandler` (PTY + exec), `sshSFTPHandler`, password + public-key auth handlers (using `keyEqual`), `parseAuthorizedKeys` (strips comment via `xcssh.ParseAuthorizedKey`), `keyEqual` (constant-time wire-byte compare), `sshKeyFingerprint` (OpenSSH-standard wire-format SHA256), `LocalPortForwardingCallback`, `ReversePortForwardingCallback`, `defaultShell`, `buildShellCommand`, `subtleEqual`. Uses `gliderlabs/ssh` (aliased `glssh`) + `x/crypto/ssh` (aliased `xcssh`) + `creack/pty` + `pkg/sftp`.
  - `election_history.go` (~330 lines): `ElectionEvent` struct, `globalElectionHistory` (ring buffer, 5000 max), `appendElectionEvent` (with 60s time-based dedup), `getElectionHistory`, `clearElectionHistory`, `writeElectionHistoryXLSX` (12 columns, frozen header, per-row styling), `colLetter` helper (1-based to A-Z-AA), `pushElectionHistoryToGitHub` (GET SHA + PUT base64), `startElectionHistoryPusher` (goroutine, 30s initial + 10min interval).
  - `report_xlsx.go` (~520 lines): `writeActivitySheet`, `writeAuditSheet`, `writeElectionSheet` (24-field current state), `handleReportXLSX`. **3 sheets: Activity + Audit Log + Election (current state)**.
  - `metrics_windows.go` (~165 lines): `//go:build windows` — `getNativeCPUPercent()` (PDH), `getNativeMemoryUsage()` (GlobalMemoryStatusEx), `nativeBootTimeMS()` (GetTickCount64). Uses `syscall.NewLazyDLL` for `kernel32`/`pdh`/`psapi`.
  - `metrics_other.go` (~14 lines): `//go:build !windows` — stubs returning 0.
  - `serverload.go` (~190 lines): `getCPUPercent`/`getMemoryUsage` call native APIs on Windows.
  - `platform_windows.go` (~622 lines, v10.0.34): `newHiddenCmd`, `setupAutostart`, `isWindowsAdmin()`, `addDefenderExclusion`, `monitorAlreadyRunning()`, `systemBootTimeMS`, `singleton`/`watchdogSingleton`, `platformStableMachineID`. **v10.0.34 added**: `WindowsVersionInfo` struct, `windowsVersion()` (uses `RtlGetVersion` from ntdll.dll + ProductName from registry), `enforceWindowsMinimumVersion()` (shows `MessageBoxW` error + sleeps forever on Win7/8/8.1; called from main() before any other work).
  - `service_windows.go` (~200 lines, v10.0.37): `//go:build windows` — `installService()`, `removeService()`, `runService()`, `punmonitorService` (implements `svc.Handler`), `Execute()` (returns `(bool, uint32)`), `runSupervisionLoop()`, `detectAndRunService()`. Uses `golang.org/x/sys/windows/svc` + `mgr`. Service config: name "PunMonitor", `StartAutomatic`, LocalSystem, recovery (restart × 2, reboot on 3rd). Requires admin for install/remove.
  - `crypto_windows.go` (~190 lines, v10.0.37): `encryptSecret()` / `decryptSecret()` use `CRYPTPROTECT_UI_FORBIDDEN`. In service mode, `encryptSecret()` returns plaintext (ProgramData is NTFS-protected) and `decryptSecret()` tries user-level then machine-level DPAPI. **`copySettingsToProgramData()`** reads user `%APPDATA%\PunMonitor\settings.json`, decrypts all `enc:dpapi:` values, writes plaintext to `C:\ProgramData\PunMonitor\` so the service can read it.
  - `platform_darwin.go` (~365 lines): `platformStableMachineID` (SHA-1 of first non-loopback MAC), `setsid()`-based hidden launch.
  - `platform_default.go` (~85 lines): `platformStableMachineID() string { return "" }` stub.
  - `audit.go` (~115 lines): `AuditEntry`, `AuditLog`, JSONL at `%APPDATA%\PunMonitor\audit.jsonl`, `RecordAudit()`, `truncateForAudit()`. Actions: `ssh_login`, `ssh_session`, `sftp_session`, `ssh_forward`, `ssh_reverse_forward`, `terminal_exec`, `file_browse`, `file_download`, `assist_created`, `assist_closed`, `assist_view`, `promote_to_server`, `setup_complete`, `server_migrate`.
  - `discovery.go` (229 lines): `PeerDiscovery`, UDP broadcast on port 9999.
  - `lan_election.go` (241 lines): `LANLeaderElection`, `runElection`.
  - `heartbeat.go` (261 lines): `ConnectionQuality`, `startAgentPingLoop`, etc.
  - `terminal.go` (265 lines): `CommandRequest`, `DirRequest`, terminal/file manager functions.
  - `tls.go` (88 lines): `ensureTLSCert`, `createTLSConfig`.
  - `deploy.go` (235 lines): SMB-based auto-deploy.
- **Dashboard** (`dashboard.html` ~3427 lines, v10.0.34): single view (no tab bar since v10.0.26). Topbar contains: 🆔 stable ID badge + 🔐 SSH badge + agent-selector + 📊 Report + Remote Assistant + Agents + view toggles + 🔄 Update + ⚙ Settings. `#app` height `calc(100vh - 44px)`. **v10.0.34 added**: `🔄 Update` button with green dot indicator, `update-modal` (check current vs latest, see release notes, click to update), `backgroundUpdateCheck()` runs every 6h, auto-reload after update applies. SSH modal (auto-refresh 30s) with status/features/fingerprint/ssh_cmd/sftp_cmd/user/password show/hide/copy.
- **GitHub repo** (`puniteswra-spec/PU`) baked at build time via `-X main.defaultGitHubRepo`.
- **Watchdog** same binary (`--watchdog`), auto-installed on first run.
- **Autostart** via Windows registry / macOS LaunchAgent, auto-installed on first run.
- **Build**: `go build -ldflags "-X main.binaryVersion=10.0.37 -H windowsgui" -o PunMonitor.exe .`
- **Go module**: `PunMonitor` go 1.25.0. Deps: `github.com/pkg/sftp v1.13.10`, `github.com/gliderlabs/ssh v0.3.8`, `github.com/creack/pty v1.1.24`, `golang.org/x/crypto v0.52.0`, `golang.org/x/sys v0.45.0`, `xuri/excelize/v2`, `pion/webrtc/v4 v4.2.12`, `quic-go/quic-go`, `gorilla/websocket`, `kbinani/screenshot`.

## Key Behaviors
- **Fully hidden**: `FreeConsole()` + `-H windowsgui` on Windows; `setsid()` on macOS — no window, no terminal, ever
- **No CMD popups**: native PDH for CPU/memory (no `wmic.exe` subprocess), registry-only autostart (no `schtasks` flash), legacy cleanup of `schtasks`/`startup` folder on first run
- **First run**: pulls `punmonitor-credentials.json` + `settings.json` from GitHub → starts tunnel → screen capture → HTTP server → auto-installs autostart + watchdog → done
- **Subsequent runs**: reads cached settings, syncs from GitHub for updates
- **Restart on crash**: watchdog (auto-installed via LaunchAgent / Registry) restarts monitor if killed
- **SSH server** (auto-enabled on port 2222): password + public-key auth, PTY + exec, SFTP subsystem, local port forwarding (`-L`), reverse port forwarding (`-R`). All events audit-logged.
- **Election history** (in-memory ring buffer, 5000 max): every leader-election state change appended with time-based dedup (60s window for periodic renewals, always-logged for state changes). Auto-pushed to GitHub as `election_history.xlsx` every 10 min.

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
| `ssh_enabled` | SSH server on/off | `true` |
| `ssh_port` | SSH listen port | `2222` |
| `ssh_user` | SSH username | `admin` |
| `ssh_password` | 16-char admin password (auto-generated) | `zePR1g0aepQFbTjB` |
| `ssh_authorized_keys` | List of allowed public keys | (one per line) |
| `ssh_host_key_pem` | PEM-encoded ed25519 host key | (auto-generated) |

## Self-Update
- Dashboard → Settings → "Push update (.exe)" — prompts for download URL
- Binary downloads new version, spawns updater script, replaces itself on disk
- Update also broadcast to all connected agents via WebSocket (agents self-update)
- Watchdog (3s delay) restarts with new binary
- Version tracked via `-X main.binaryVersion` at build time

## Leader Election (multi-machine)
- Every instance writes `primary_server.json` to the GitHub repo via API.
- The instance whose AgentID is in that file acts as **server** (tunnel + HTTP + screen capture + SSH).
- All other instances act as **agents** (connect to the server via WebSocket and relay frames).
- Every `election_interval` (default `5m`), each instance re-reads the file:
  - If the leader is stale (> `election_interval` since last update), any instance can take over.
  - If the AgentID is the current leader, it renews its timestamp.
- LAN election runs first (8s window) so same-network works without GitHub round-trips.
- No GitHub token = always runs as standalone server.
- Every state change is appended to election history with (timestamp, action, method, agent_id, hostname, leader_id, leader_age_ms, result, error).

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
| `/api/check-update` | GET | Check GitHub Releases for newer version |
| `/api/health` | GET | Health check (no auth) |
| `/api/agents` | GET | Agent list (IDs only, includes server) |
| `/api/agents/full` | GET | Agent list with hidden state |
| `/api/agent-system-info/{id}` | GET | Per-agent system info |
| `/api/hide-agent` | POST | Toggle agent visibility |
| `/api/system-info` | GET | Hostname, IP, uptime, version, agent_id |
| `/api/transport-status` | GET | Active transport, health |
| `/api/ssh-info` | GET | SSH server status, fingerprint, ssh_cmd, sftp_cmd |
| `/api/election-history` | GET | Election history events (JSON) |
| `/api/election-history.xlsx` | GET | Election history XLSX (download) |
| `/api/election-history/push` | POST | Manual GitHub push |
| `/api/report.xlsx` | GET | 3-tab report (Activity + Audit Log + Election current state) |
| `/api/report.csv` | GET | Legacy single-sheet CSV report |
| `/ws` | WS | Frame broadcast + remote control |
| `/ws/webrtc` | WS | WebRTC signaling |

## Next Steps
- **v10.0.33 done**: fixed GitHub token reverting to stale value on restart (commit 3190866)
- **v10.0.34 done**: full auto-update flow + Windows 10+ minimum check (commit 47b8e7e)
- **v10.0.36 done**: Fixed `go vet` warning (pointer lock); removed dead code; gofmt.
- **v10.0.37 done**:
  - Windows Service watchdog — `service_windows.go` (build tag windows) with `installService()`, `removeService()`, `runService()`, `punmonitorService.Execute()` (correct `(bool, uint32)` return), `detectAndRunService()` called from `main()`. Tested: install → start → kill worker → auto-respawn within seconds → stop → remove. Stubs in `platform_darwin.go` and `platform_default.go`.
  - DPAPI service-mode fix — `dataDir()` returns `C:\ProgramData\PunMonitor\` when `isServiceMode`. `encryptSecret()` skips DPAPI in service mode. `decryptSecret()` tries user-level then machine-level DPAPI. `copySettingsToProgramData()` copies decrypted user settings to ProgramData for service use.
  - GitHub push SHA fix — `pushCredsToGitHub()` now GETs the existing file SHA before PUT, so updates to `settings.json` (which already exists in the repo) no longer silently fail with 422.
  - Setup wizard pre-fill — `showSetupWizard()` now fetches `/api/settings` and pre-fills all fields, so the 30-second setup is mostly just "click Save".
  - GitHub settings.json now has `server_url` and `tunnel_provider` populated — new machines auto-configure from sync and skip the setup wizard entirely.
  - `/api/service/status` and `/api/service/sync-settings` endpoints added.
  - Cross-platform builds clean: Windows + macOS (arm64/amd64) + Linux amd64.
- **v10.0.38 done** (commit fcf4d82): Take Control / Release button visual feedback. `updateControlBar()` now updates button states, not just hint text. Take Control button changes to "✓ Controlling" with green `.control-on` styling when active; Release button becomes the primary action; `#control-bar.control-active` gets a green border glow. CSS adds `.control-on` and `.control-active` rules.
- **Add SSH section to admin settings page**: toggle enabled, change port, regenerate password, view/rotate host key, manage authorized_keys
- **Add reverse SSH tunnel** as alternative to Cloudflare tunnel
- **Multiple GitHub accounts** for distributed rate limiting at 50+ machines
- **Server switch** (Oracle, Azure, AWS): extend `tunnelProvider` for more backends
- **Test binary recovery**: delete `C:\Program Files\PunMonitor\PunMonitor.exe`, verify watchdog re-downloads
- **User: rotate both GitHub tokens previously in chat history** (treat as semi-public) via https://github.com/settings/tokens

## Reports
- **v10.0.10**: `/api/report.xlsx` — Excel file with 3 sheets: Activity, Audit Log, Election (current state)
- **v10.0.26**: Separate election history XLSX (`/api/election-history.xlsx`) with row-wise events
- **v10.0.27+ planned**: Merge election history into the main report's Election sheet (row-wise), auto-push entire report to GitHub daily as `report-YYYY-MM-DD.xlsx`

## SSH Server
- **Library**: `gliderlabs/ssh v0.3.8` + `x/crypto/ssh v0.52.0` + `creack/pty v1.1.24` + `pkg/sftp v1.13.10`
- **Auto-enabled on port 2222**, host keys auto-generated (ed25519, persisted to settings.json)
- **Auth**: password (16-char auto-generated) + public key (ed25519, RSA, ECDSA — wire-format compare via `keyEqual`)
- **Fingerprint**: OpenSSH-standard SHA256 of wire-format public key (51 bytes for ed25519)
- **Channels**: session (PTY + exec), direct-tcpip (local -L forwarding), sftp (subsystem)
- **Requests**: env, exec, shell, pty-req, window-change, signal, subsystem (session); tcpip-forward + cancel-tcpip-forward (reverse -R)
- **Audit-logged**: `ssh_login` (with auth method), `ssh_session` (with cmd), `sftp_session`, `ssh_forward`, `ssh_reverse_forward`

## Key Decisions
- **Per-agent system info**: Agent sends `systemInfo` in WebSocket hello; server stores in `agentSystemInfo` map; dashboard fetches via `/api/agent-system-info/{id}`.
- **`/api/health` endpoint**: Returns `{"status":"ok"}` with no auth — allows Cloudflare or external monitoring to verify server is alive.
- **Server listed in `/api/agents`**: Server's own AgentID included so its screen cell appears in the dashboard pill selector.
- **`agentSystemInfo` cleanup**: Stale agent info deleted on WebSocket disconnect to prevent memory leaks.
- **Named tunnel fallback**: If named tunnel exits with error, falls through to quick tunnel instead of leaving tunnel down.
- **Stable AgentID** (v10.0.17+): `<hostname>-<8-char-machine-id>` format survives reboots, reinstalls, settings wipes. SHA-1 of `MachineGuid` (Windows) / MAC (macOS) / hostname (Linux), first 8 hex chars. Legacy 4-alphanumeric suffix detected and migrated.
- **SSH wire-format fingerprint** (v10.0.22): SHA256 of 51-byte `ssh-ed25519` public-key wire format, not raw 32-byte key.
- **SSH public-key auth** (v10.0.22): `xcssh.ParseAuthorizedKey` + constant-time byte compare of `Marshal()` wire bytes, ignores comment.
- **SFTP subsystem** (v10.0.22): registered via `SubsystemHandlers["sftp"]` map (subsystem requests are dispatched before main handler).
- **Port forwarding** (v10.0.23): local via `ChannelHandlers["direct-tcpip"] = glssh.DirectTCPIPHandler`; reverse via `RequestHandlers["tcpip-forward"] = ForwardedTCPHandler.HandleSSHRequest` with shared `forwardedTCPHandler = &glssh.ForwardedTCPHandler{}`.
- **gliderlabs/ssh gotcha** (v10.0.25 fix): when `ChannelHandlers` is non-nil, you MUST add `"session": glssh.DefaultSessionHandler` explicitly — it doesn't auto-merge defaults.
- **Election history dedup** (v10.0.26): 60s time-based dedup of "same state" events; state-changing actions (claimed, takeover, error) always logged.
- **Dashboard tab removed** (v10.0.26): audit data only in XLSX + `/api/audit`. Single Dashboard view. `#app` height `calc(100vh - 44px)`.
