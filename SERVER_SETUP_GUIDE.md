# Remote Monitor Server Setup Guide

## Quick Start

### Option 1: PowerShell Script (Recommended)
1. Right-click `setup-server.ps1` → **Run with PowerShell** (as Administrator)
2. Follow the prompts
3. Script will:
   - Check Node.js installation
   - Set up server directory
   - Install dependencies
   - Detect network configuration
   - Configure Windows Firewall
   - Create auto-start service
   - Generate setup report with port forwarding instructions

### Option 2: Batch Script
1. Right-click `setup-server.bat` → **Run as administrator**
2. Follow the prompts

---

## Manual Setup (If Scripts Don't Work)

### 1. Install Node.js
- Download from: https://nodejs.org/
- Install LTS version
- Verify: `node -v` and `npm -v`

### 2. Create Server Folder
```powershell
mkdir C:\RemoteMonitor
cd C:\RemoteMonitor
```

### 3. Copy Files
Copy these from `P:\Opencode\RemoteMonitor-Merged\`:
- `server.source.js`
- `package.json`

### 4. Install Dependencies
```powershell
npm install
```

### 5. Create Data Folders
```powershell
mkdir data
mkdir logs
```

### 6. Run Server
```powershell
node server.source.js
```

### 7. Configure Firewall
```powershell
New-NetFirewallRule -DisplayName "RemoteMonitor Server" -Direction Inbound -LocalPort 3000 -Protocol TCP -Action Allow
```

### 8. Auto-Start on Boot
```powershell
$action = New-ScheduledTaskAction -Execute "node" -Argument "server.source.js" -WorkingDirectory "C:\RemoteMonitor"
$trigger = New-ScheduledTaskTrigger -AtStartup
Register-ScheduledTask -TaskName "RemoteMonitorServer" -Action $action -Trigger $trigger -RunLevel Highest -User "SYSTEM"
```

---

## Port Forwarding (Required for External Access)

### Step 1: Find Your IPs
The setup script detects these automatically, or manually:
- **Local IP:** `ipconfig` → Look for IPv4 Address
- **Public IP:** Visit https://api.ipify.org
- **Router IP:** `ipconfig` → Look for Default Gateway

### Step 2: Access Router Admin
Open browser → Go to your router IP (e.g., `192.168.1.1`)
Login with router credentials (check router sticker)

### Step 3: Add Port Forwarding Rule
| Field | Value |
|-------|-------|
| Service Name | RemoteMonitor |
| External Port | 3000 |
| Internal Port | 3000 |
| Protocol | TCP |
| Internal IP | Your PC's local IP |
| Enable | YES |

### Step 4: Test
From a different network (mobile data):
```
http://YOUR_PUBLIC_IP:3000
```

---

## Common Router Admin URLs
| Brand | URL |
|-------|-----|
| TP-Link | http://192.168.0.1 or http://tplinkwifi.net |
| D-Link | http://192.168.0.1 or http://dlinkrouter.local |
| Netgear | http://192.168.1.1 or http://routerlogin.net |
| ASUS | http://192.168.1.1 or http://router.asus.com |
| Linksys | http://192.168.1.1 |

---

## Security Checklist
- [ ] Change default password in `server.source.js`
- [ ] Windows Firewall rule created
- [ ] Regular backups of `data\` and `logs\` folders
- [ ] Consider using non-standard port (e.g., 8443)

---

## Hybrid Setup (Self-Hosted + Render Fallback)

On each agent machine, edit `%APPDATA%\SystemHelper\urls.ini`:
```ini
http://YOUR_PUBLIC_IP:3000
wss://pu-k752.onrender.com
```

Agents try your server first, fallback to Render if down.

Or use dashboard **"🌐 Switch Server"** button to switch all agents remotely.
