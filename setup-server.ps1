#Requires -RunAsAdministrator
# Remote Monitor Server Setup Script (PowerShell)
# Designed by Puneet Upreti

$ErrorActionPreference = "Stop"
[Console]::Title = "Remote Monitor Server Setup"
[Console]::ForegroundColor = "Green"

Write-Host "============================================================" -ForegroundColor Cyan
Write-Host "        Remote Monitor Server - Setup Script" -ForegroundColor Cyan
Write-Host "        Designed by Puneet Upreti" -ForegroundColor Cyan
Write-Host "============================================================" -ForegroundColor Cyan
Write-Host ""

# [1/8] Check system requirements
Write-Host "[1/8] Checking system requirements..." -ForegroundColor Yellow
Write-Host ""

# Check Node.js
Write-Host "Checking Node.js..."
$nodeCmd = Get-Command node -ErrorAction SilentlyContinue
if (-not $nodeCmd) {
    Write-Host "[!] Node.js NOT found." -ForegroundColor Red
    Write-Host ""
    Write-Host "Please install Node.js from: https://nodejs.org/" -ForegroundColor Yellow
    Write-Host "Download the LTS version and run the installer." -ForegroundColor Yellow
    Write-Host "After installation, run this script again." -ForegroundColor Yellow
    Write-Host ""
    Write-Host "Opening Node.js download page..."
    Start-Process "https://nodejs.org/"
    Read-Host "Press Enter to exit"
    exit 1
} else {
    $nodeVersion = (node -v).Trim()
    Write-Host "[OK] Node.js found: $nodeVersion" -ForegroundColor Green
}

# Check npm
Write-Host "Checking npm..."
$npmCmd = Get-Command npm -ErrorAction SilentlyContinue
if (-not $npmCmd) {
    Write-Host "[!] npm NOT found. Please reinstall Node.js." -ForegroundColor Red
    Read-Host "Press Enter to exit"
    exit 1
} else {
    $npmVersion = (npm -v).Trim()
    Write-Host "[OK] npm found: $npmVersion" -ForegroundColor Green
}

Write-Host ""
Write-Host "[2/8] Setting up server directory..." -ForegroundColor Yellow
Write-Host ""

# Ask for install directory
$defaultDir = "C:\RemoteMonitor"
$installDir = Read-Host "Enter server installation path [$defaultDir]"
if ([string]::IsNullOrWhiteSpace($installDir)) { $installDir = $defaultDir }

if (-not (Test-Path $installDir)) {
    Write-Host "Creating directory: $installDir"
    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
}

Set-Location $installDir
Write-Host "Server directory: $installDir" -ForegroundColor Green
Write-Host ""

# Check for server files
Write-Host "Checking server files..."
if (-not (Test-Path "server.source.js")) {
    Write-Host "[!] server.source.js NOT found in $installDir" -ForegroundColor Red
    Write-Host ""
    Write-Host "Please copy these files from your project to $installDir :" -ForegroundColor Yellow
    Write-Host "  - server.source.js" -ForegroundColor Yellow
    Write-Host "  - package.json" -ForegroundColor Yellow
    Write-Host ""
    Write-Host "Current project location: P:\Opencode\RemoteMonitor-Merged\" -ForegroundColor Yellow
    Write-Host ""
    $copyNow = Read-Host "Copy files now from project? (Y/N)"
    if ($copyNow -eq "Y" -or $copyNow -eq "y") {
        Write-Host "Copying files..."
        Copy-Item "P:\Opencode\RemoteMonitor-Merged\server.source.js" $installDir -ErrorAction SilentlyContinue
        Copy-Item "P:\Opencode\RemoteMonitor-Merged\package.json" $installDir -ErrorAction SilentlyContinue
        if (Test-Path "server.source.js") {
            Write-Host "[OK] Files copied successfully." -ForegroundColor Green
        } else {
            Write-Host "[!] Copy failed. Please copy manually." -ForegroundColor Red
            Read-Host "Press Enter to exit"
            exit 1
        }
    } else {
        Write-Host "Please copy the files manually and run this script again."
        Read-Host "Press Enter to exit"
        exit 1
    }
} else {
    Write-Host "[OK] server.source.js found." -ForegroundColor Green
}

if (-not (Test-Path "package.json")) {
    Write-Host "[!] package.json NOT found. Creating default..." -ForegroundColor Yellow
    @"
{
  "name": "remote-monitor-server",
  "version": "7.0.0",
  "description": "Remote Monitor Server",
  "main": "server.source.js",
  "scripts": {
    "start": "node server.source.js"
  },
  "dependencies": {
    "express": "^4.18.2",
    "ws": "^8.14.2"
  }
}
"@ | Out-File -FilePath "package.json" -Encoding UTF8
    Write-Host "[OK] package.json created." -ForegroundColor Green
} else {
    Write-Host "[OK] package.json found." -ForegroundColor Green
}

Write-Host ""
Write-Host "[3/8] Installing dependencies..." -ForegroundColor Yellow
Write-Host ""

# Check if node_modules exists
if (Test-Path "node_modules") {
    Write-Host "node_modules already exists."
    $reinstall = Read-Host "Reinstall dependencies? (Y/N)"
    if ($reinstall -eq "Y" -or $reinstall -eq "y") {
        Write-Host "Reinstalling..."
        npm install
    } else {
        Write-Host "Skipping installation."
    }
} else {
    Write-Host "Installing npm packages..."
    npm install
    if ($LASTEXITCODE -ne 0) {
        Write-Host "[!] npm install failed. Please check your internet connection." -ForegroundColor Red
        Read-Host "Press Enter to exit"
        exit 1
    }
}

Write-Host ""
Write-Host "[OK] Dependencies installed." -ForegroundColor Green

Write-Host ""
Write-Host "[4/8] Creating data directories..." -ForegroundColor Yellow
Write-Host ""

if (-not (Test-Path "data")) { New-Item -ItemType Directory -Path "data" -Force | Out-Null }
if (-not (Test-Path "logs")) { New-Item -ItemType Directory -Path "logs" -Force | Out-Null }
Write-Host "[OK] Created: data\, logs\" -ForegroundColor Green

Write-Host ""
Write-Host "[5/8] Detecting network configuration..." -ForegroundColor Yellow
Write-Host ""

# Get Local IP
Write-Host "Detecting local IP address..."
$localIP = (Get-NetIPAddress -AddressFamily IPv4 | Where-Object { $_.InterfaceAlias -notmatch "Loopback|vEthernet|VMware|VirtualBox" -and $_.IPAddress -notmatch "^169\.254" } | Select-Object -First 1).IPAddress
if (-not $localIP) {
    $localIP = (Get-NetIPAddress -AddressFamily IPv4 | Where-Object { $_.InterfaceAlias -notmatch "Loopback" } | Select-Object -First 1).IPAddress
}
Write-Host "[OK] Local IP: $localIP" -ForegroundColor Green

# Get Public IP
Write-Host "Detecting public IP address..."
try {
    $publicIP = (Invoke-WebRequest -Uri "https://api.ipify.org" -UseBasicParsing).Content.Trim()
    Write-Host "[OK] Public IP: $publicIP" -ForegroundColor Green
} catch {
    Write-Host "[!] Could not detect public IP. Please check manually at: https://api.ipify.org" -ForegroundColor Yellow
    $publicIP = Read-Host "Enter your public IP manually"
}

# Get Default Gateway (Router IP)
Write-Host "Detecting router IP..."
$routerIP = (Get-NetRoute -DestinationPrefix "0.0.0.0/0" | Select-Object -First 1).NextHop
if (-not $routerIP) {
    $routerIP = "192.168.1.1"
}
Write-Host "[OK] Router IP: $routerIP" -ForegroundColor Green

Write-Host ""
Write-Host "[6/8] Configuring Windows Firewall..." -ForegroundColor Yellow
Write-Host ""

# Check if firewall rule exists
$fwRule = Get-NetFirewallRule -DisplayName "RemoteMonitor Server" -ErrorAction SilentlyContinue
if (-not $fwRule) {
    Write-Host "Creating firewall rule..."
    New-NetFirewallRule -DisplayName "RemoteMonitor Server" -Direction Inbound -LocalPort 3000 -Protocol TCP -Action Allow -Profile Any | Out-Null
    Write-Host "[OK] Firewall rule created for port 3000." -ForegroundColor Green
} else {
    Write-Host "[OK] Firewall rule already exists." -ForegroundColor Green
}

Write-Host ""
Write-Host "[7/8] Setting up auto-start service..." -ForegroundColor Yellow
Write-Host ""

# Check if scheduled task exists
$scheduledTask = Get-ScheduledTask -TaskName "RemoteMonitorServer" -ErrorAction SilentlyContinue
if (-not $scheduledTask) {
    Write-Host "Creating scheduled task for auto-start..."
    $action = New-ScheduledTaskAction -Execute "node" -Argument "server.source.js" -WorkingDirectory $installDir
    $trigger = New-ScheduledTaskTrigger -AtStartup
    Register-ScheduledTask -TaskName "RemoteMonitorServer" -Action $action -Trigger $trigger -RunLevel Highest -User "SYSTEM" | Out-Null
    Write-Host "[OK] Auto-start task created." -ForegroundColor Green
} else {
    Write-Host "[OK] Auto-start task already exists." -ForegroundColor Green
    $recreate = Read-Host "Recreate scheduled task? (Y/N)"
    if ($recreate -eq "Y" -or $recreate -eq "y") {
        Unregister-ScheduledTask -TaskName "RemoteMonitorServer" -Confirm:$false
        $action = New-ScheduledTaskAction -Execute "node" -Argument "server.source.js" -WorkingDirectory $installDir
        $trigger = New-ScheduledTaskTrigger -AtStartup
        Register-ScheduledTask -TaskName "RemoteMonitorServer" -Action $action -Trigger $trigger -RunLevel Highest -User "SYSTEM" | Out-Null
        Write-Host "[OK] Auto-start task recreated." -ForegroundColor Green
    }
}

Write-Host ""
Write-Host "[8/8] Generating setup report..." -ForegroundColor Yellow
Write-Host ""

# Create setup report
$report = @"
============================================================
        Remote Monitor Server - Setup Complete
============================================================

Server Location: $installDir
Local IP: $localIP
Public IP: $publicIP
Router IP: $routerIP
Server Port: 3000

============================================================
        PORT FORWARDING REQUIRED
============================================================

To access your server from outside your network, you MUST
set up port forwarding on your router.

STEP 1: Access Your Router Admin Panel
----------------------------------------
Open browser and go to: http://$routerIP
Login with your router username/password.
(Default is often admin/admin or check router sticker)

STEP 2: Add Port Forwarding Rule
----------------------------------------
Find "Port Forwarding", "Virtual Server", or "NAT" section.
Add a new rule with these settings:

  Service Name: RemoteMonitor
  External Port: 3000
  Internal Port: 3000
  Protocol: TCP
  Internal IP: $localIP
  Enable: YES

STEP 3: Save and Restart Router
----------------------------------------
Save the rule and restart your router if prompted.

STEP 4: Test External Access
----------------------------------------
From a DIFFERENT network (mobile data or another location):
Open browser and go to: http://$publicIP`:3000

If you see the dashboard, port forwarding is working!

============================================================
        COMMON ROUTER ADMIN URLS
============================================================

  TP-Link:    http://192.168.0.1 or http://tplinkwifi.net
  D-Link:     http://192.168.0.1 or http://dlinkrouter.local
  Netgear:    http://192.168.1.1 or http://routerlogin.net
  ASUS:       http://192.168.1.1 or http://router.asus.com
  Linksys:    http://192.168.1.1
  ISP Router: Check router sticker or call support

============================================================
        START/STOP SERVER COMMANDS
============================================================

Start Server:
  cd "$installDir"
  node server.source.js

Start Server (Background):
  Start-Process node -ArgumentList "server.source.js" -WorkingDirectory "$installDir" -WindowStyle Hidden

Stop Server:
  Stop-Process -Name "node" -Force

Auto-Start Task:
  Start-ScheduledTask -TaskName "RemoteMonitorServer"

Disable Auto-Start:
  Unregister-ScheduledTask -TaskName "RemoteMonitorServer" -Confirm:`$false

============================================================
        SECURITY NOTES
============================================================

1. Change default password in server.source.js:
   const AUTH_USER = 'your_username';
   const AUTH_PASS = 'your_strong_password';

2. Keep Windows Firewall enabled (rule already created).

3. Consider using a non-standard port (e.g., 8443) to reduce
   automated scanning.

4. Regularly backup the data\ and logs\ folders.

============================================================
        HYBRID SETUP (Self-Hosted + Render Fallback)
============================================================

To use both your server AND Render as fallback:

1. On each agent machine, edit:
   `%APPDATA%\SystemHelper\urls.ini`

2. Add both URLs (primary first):
   http://$publicIP`:3000
   wss://pu-k752.onrender.com

3. Agents will try your server first, fallback to Render if down.

Or use the dashboard "Switch Server" button to switch all agents
remotely with one click.

============================================================
Setup completed on: $(Get-Date -Format "yyyy-MM-dd HH:mm:ss")
============================================================
"@

$report | Out-File -FilePath "$installDir\SETUP_REPORT.txt" -Encoding UTF8

Write-Host "[OK] Setup report saved to: $installDir\SETUP_REPORT.txt" -ForegroundColor Green
Write-Host ""
Write-Host "============================================================" -ForegroundColor Cyan
Write-Host "        SETUP COMPLETE!" -ForegroundColor Cyan
Write-Host "============================================================" -ForegroundColor Cyan
Write-Host ""
Write-Host "Server is ready at: $installDir"
Write-Host "Local access: http://localhost:3000"
Write-Host "External access: http://$publicIP`:3000 (after port forwarding)"
Write-Host ""
Write-Host "IMPORTANT: You MUST set up port forwarding on your router!" -ForegroundColor Yellow
Write-Host "See SETUP_REPORT.txt for detailed instructions."
Write-Host ""
Write-Host "Opening setup report..."
notepad "$installDir\SETUP_REPORT.txt"
Write-Host ""
Write-Host "Starting server now..."
Write-Host ""

Start-Process node -ArgumentList "server.source.js" -WorkingDirectory $installDir -WindowStyle Normal

Write-Host ""
Write-Host "Server started in a new window."
Write-Host "Close that window to stop the server."
Write-Host ""
Read-Host "Press Enter to exit"
