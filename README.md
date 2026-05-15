# Remote Monitor

Remote screen monitoring — one Node.js server + Windows Go agent, all in a single flat directory.

## Project Structure

```
remote-desktop-app/
│
├── server.js            ← Node.js relay server
├── package.json
│
├── dashboard.html       ← Admin dashboard (CCTV wall)
├── redirect.html        ← Redirect page
│
├── main.go              ← Windows agent source (Go)
├── exec_windows.go      ← OS-specific build (Windows)
├── exec_other.go        ← OS-specific build (non-Windows)
├── go.mod / go.sum
│
├── SystemHelper.exe     ← Compiled Windows agent  ← deploy this
├── SystemHelper         ← macOS binary (for testing)
├── config.json          ← Agent config
├── urls.ini             ← Server URL list (one per line)
│
├── manage.sh            ← Mac: start/stop/tunnels/watchdog
├── RemoteMonitor.command← Mac: interactive management menu
│
├── RemoteMonitor.bat    ← Windows: full management menu
├── build_agent.sh       ← Build SystemHelper.exe from source
│
└── WindowsAgent.zip     ← Legacy Python package (archived)
```

## Quick Start

### Mac — Start the server
```bash
./manage.sh start
# Dashboard → http://localhost:3000   (login: puneet / puneet12)
```

### Mac — All commands
```bash
./manage.sh start     # Start server
./manage.sh stop      # Stop server + all tunnels
./manage.sh restart   # Restart
./manage.sh status    # Show status + connected agents
./manage.sh watchdog  # Auto-restart if server crashes
./manage.sh tunnels   # Start server + ngrok + cloudflare + serveo
```

### Mac — Interactive menu
```bash
./RemoteMonitor.command
```

### Windows — Agent
Double-click `SystemHelper.exe` — auto-connects to server.

### Windows — Management menu
Double-click `RemoteMonitor.bat`

## Server API

| Endpoint | Description |
|---|---|
| `GET /` | Dashboard |
| `GET /api/agents` | List connected agents |
| `GET /api/report?format=json\|csv\|html` | Activity report |
| `POST /api/upload-update` | Push .exe update to all agents |
| `POST /api/send-file/:agentId` | Send file to a specific agent |
| `POST /api/switch-server` | Tell agents to switch server URL |
| `POST /api/cleanup` | Clear history + agent logs |
| `GET /remote-session` | Browser-based screen sharing |

## Build Agent from Source (Mac → Windows)
```bash
./build_agent.sh 6.1.0
# Outputs: SystemHelper_v6.1.0.exe
```

## Agent Config (on Windows: `%APPDATA%\SystemHelper\`)

| File | Purpose |
|---|---|
| `auth.ini` | Change dashboard password |
| `urls.ini` | Custom server URLs (one per line) |
| `tunnel.ini` | Tunnel: `auto`, `bore`, `localhost.run`, `none` |
| `agent.id` | Persistent agent ID |
| `agent.log` | Connection log |
