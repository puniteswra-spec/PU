@echo off
setlocal enabledelayedexpansion
title Remote Monitor Server Setup
color 0A

echo ============================================================
echo        Remote Monitor Server - Setup Script
echo        Designed by Puneet Upreti
echo ============================================================
echo.

:: Check for Admin privileges
net session >nul 2>&1
if %errorlevel% neq 0 (
    echo [ERROR] This script must be run as Administrator!
    echo Right-click this file and select "Run as administrator"
    echo.
    pause
    exit /b 1
)

echo [1/8] Checking system requirements...
echo.

:: Check Node.js
echo Checking Node.js...
where node >nul 2>&1
if %errorlevel% neq 0 (
    echo [!] Node.js NOT found.
    echo.
    echo Please install Node.js from: https://nodejs.org/
    echo Download the LTS version and run the installer.
    echo After installation, run this script again.
    echo.
    echo Opening Node.js download page...
    start https://nodejs.org/
    pause
    exit /b 1
) else (
    for /f "tokens=*" %%i in ('node -v') do set NODE_VERSION=%%i
    echo [OK] Node.js found: %NODE_VERSION%
)

:: Check npm
echo Checking npm...
where npm >nul 2>&1
if %errorlevel% neq 0 (
    echo [!] npm NOT found. Please reinstall Node.js.
    pause
    exit /b 1
) else (
    for /f "tokens=*" %%i in ('npm -v') do set NPM_VERSION=%%i
    echo [OK] npm found: %NPM_VERSION%
)

echo.
echo [2/8] Setting up server directory...
echo.

:: Ask for install directory
set "DEFAULT_DIR=C:\RemoteMonitor"
set /p "INSTALL_DIR=Enter server installation path [%DEFAULT_DIR%]: "
if "!INSTALL_DIR!"=="" set "INSTALL_DIR=%DEFAULT_DIR%"

if not exist "!INSTALL_DIR!" (
    echo Creating directory: !INSTALL_DIR!
    mkdir "!INSTALL_DIR!"
)

cd /d "!INSTALL_DIR!"
echo Server directory: !INSTALL_DIR!
echo.

:: Check for server files
echo Checking server files...
if not exist "server.source.js" (
    echo [!] server.source.js NOT found in !INSTALL_DIR!
    echo.
    echo Please copy these files from your project to !INSTALL_DIR!:
    echo   - server.source.js
    echo   - package.json
    echo.
    echo Current project location: P:\Opencode\RemoteMonitor-Merged\
    echo.
    set /p "COPY_NOW=Copy files now from project? (Y/N): "
    if /i "!COPY_NOW!"=="Y" (
        echo Copying files...
        copy /Y "P:\Opencode\RemoteMonitor-Merged\server.source.js" "!INSTALL_DIR!" >nul 2>&1
        copy /Y "P:\Opencode\RemoteMonitor-Merged\package.json" "!INSTALL_DIR!" >nul 2>&1
        if exist "server.source.js" (
            echo [OK] Files copied successfully.
        ) else (
            echo [!] Copy failed. Please copy manually.
            pause
            exit /b 1
        )
    ) else (
        echo Please copy the files manually and run this script again.
        pause
        exit /b 1
    )
) else (
    echo [OK] server.source.js found.
)

if not exist "package.json" (
    echo [!] package.json NOT found. Creating default...
    echo {^
  "name": "remote-monitor-server",^
  "version": "7.0.0",^
  "description": "Remote Monitor Server",^
  "main": "server.source.js",^
  "scripts": {^
    "start": "node server.source.js"^
  },^
  "dependencies": {^
    "express": "^4.18.2",^
    "ws": "^8.14.2"^
  }^
} > package.json
    echo [OK] package.json created.
) else (
    echo [OK] package.json found.
)

echo.
echo [3/8] Installing dependencies...
echo.

:: Check if node_modules exists
if exist "node_modules" (
    echo node_modules already exists.
    set /p "REINSTALL=Reinstall dependencies? (Y/N): "
    if /i "!REINSTALL!"=="Y" (
        echo Reinstalling...
        call npm install
    ) else (
        echo Skipping installation.
    )
) else (
    echo Installing npm packages...
    call npm install
    if %errorlevel% neq 0 (
        echo [!] npm install failed. Please check your internet connection.
        pause
        exit /b 1
    )
)

echo.
echo [OK] Dependencies installed.

echo.
echo [4/8] Creating data directories...
echo.

if not exist "data" mkdir "data"
if not exist "logs" mkdir "logs"
echo [OK] Created: data\, logs\

echo.
echo [5/8] Detecting network configuration...
echo.

:: Get Local IP
echo Detecting local IP address...
for /f "tokens=2 delims=:" %%a in ('ipconfig ^| findstr /c:"IPv4"') do (
    set "LOCAL_IP=%%a"
    goto :ip_found
)
:ip_found
set "LOCAL_IP=%LOCAL_IP:~1%"
echo [OK] Local IP: %LOCAL_IP%

:: Get Public IP
echo Detecting public IP address...
for /f "tokens=*" %%i in ('curl -s https://api.ipify.org 2^>nul') do set "PUBLIC_IP=%%i"
if "%PUBLIC_IP%"=="" (
    echo [!] Could not detect public IP. Please check manually at: https://api.ipify.org
    set /p "PUBLIC_IP=Enter your public IP manually: "
) else (
    echo [OK] Public IP: %PUBLIC_IP%
)

:: Get Default Gateway (Router IP)
echo Detecting router IP...
for /f "tokens=2 delims=:" %%a in ('ipconfig ^| findstr /c:"Default Gateway"') do (
    set "ROUTER_IP=%%a"
    goto :gw_found
)
:gw_found
set "ROUTER_IP=%ROUTER_IP:~1%"
if "!ROUTER_IP!"=="" set "ROUTER_IP=192.168.1.1"
echo [OK] Router IP: %ROUTER_IP%

echo.
echo [6/8] Configuring Windows Firewall...
echo.

:: Check if firewall rule exists
netsh advfirewall firewall show rule name="RemoteMonitor Server" >nul 2>&1
if %errorlevel% neq 0 (
    echo Creating firewall rule...
    netsh advfirewall firewall add rule name="RemoteMonitor Server" dir=in action=allow protocol=TCP localport=3000 profile=any >nul 2>&1
    echo [OK] Firewall rule created for port 3000.
) else (
    echo [OK] Firewall rule already exists.
)

echo.
echo [7/8] Setting up auto-start service...
echo.

:: Check if scheduled task exists
schtasks /query /tn "RemoteMonitorServer" >nul 2>&1
if %errorlevel% neq 0 (
    echo Creating scheduled task for auto-start...
    schtasks /create /tn "RemoteMonitorServer" /tr "cmd /c 'cd /d \"!INSTALL_DIR!\" && node server.source.js'" /sc onstart /ru SYSTEM /rl highest /f >nul 2>&1
    echo [OK] Auto-start task created.
) else (
    echo [OK] Auto-start task already exists.
    set /p "RECREATE=Recreate scheduled task? (Y/N): "
    if /i "!RECREATE!"=="Y" (
        schtasks /delete /tn "RemoteMonitorServer" /f >nul 2>&1
        schtasks /create /tn "RemoteMonitorServer" /tr "cmd /c 'cd /d \"!INSTALL_DIR!\" && node server.source.js'" /sc onstart /ru SYSTEM /rl highest /f >nul 2>&1
        echo [OK] Auto-start task recreated.
    )
)

echo.
echo [8/8] Generating setup report...
echo.

:: Create setup report
(
echo ============================================================
echo        Remote Monitor Server - Setup Complete
echo ============================================================
echo.
echo Server Location: !INSTALL_DIR!
echo Local IP: %LOCAL_IP%
echo Public IP: %PUBLIC_IP%
echo Router IP: %ROUTER_IP%
echo Server Port: 3000
echo.
echo ============================================================
echo        PORT FORWARDING REQUIRED
echo ============================================================
echo.
echo To access your server from outside your network, you MUST
echo set up port forwarding on your router.
echo.
echo STEP 1: Access Your Router Admin Panel
echo ----------------------------------------
echo Open browser and go to: http://%ROUTER_IP%
echo Login with your router username/password.
echo (Default is often admin/admin or check router sticker)
echo.
echo STEP 2: Add Port Forwarding Rule
echo ----------------------------------------
echo Find "Port Forwarding", "Virtual Server", or "NAT" section.
echo Add a new rule with these settings:
echo.
echo   Service Name: RemoteMonitor
echo   External Port: 3000
echo   Internal Port: 3000
echo   Protocol: TCP
echo   Internal IP: %LOCAL_IP%
echo   Enable: YES
echo.
echo STEP 3: Save and Restart Router
echo ----------------------------------------
echo Save the rule and restart your router if prompted.
echo.
echo STEP 4: Test External Access
echo ----------------------------------------
echo From a DIFFERENT network (mobile data or another location):
echo Open browser and go to: http://%PUBLIC_IP%:3000
echo.
echo If you see the dashboard, port forwarding is working!
echo.
echo ============================================================
echo        COMMON ROUTER ADMIN URLS
echo ============================================================
echo.
echo   TP-Link:    http://192.168.0.1 or http://tplinkwifi.net
echo   D-Link:     http://192.168.0.1 or http://dlinkrouter.local
echo   Netgear:    http://192.168.1.1 or http://routerlogin.net
echo   ASUS:       http://192.168.1.1 or http://router.asus.com
echo   Linksys:    http://192.168.1.1
echo   ISP Router: Check router sticker or call support
echo.
echo ============================================================
echo        START/STOP SERVER COMMANDS
echo ============================================================
echo.
echo Start Server:
echo   cd "!INSTALL_DIR!"
echo   node server.source.js
echo.
echo Start Server (Background):
echo   start /B node server.source.js
echo.
echo Stop Server:
echo   Press Ctrl+C in the server window
echo.
echo Auto-Start Task:
echo   schtasks /run /tn "RemoteMonitorServer"
echo.
echo Disable Auto-Start:
echo   schtasks /delete /tn "RemoteMonitorServer" /f
echo.
echo ============================================================
echo        SECURITY NOTES
echo ============================================================
echo.
echo 1. Change default password in server.source.js:
echo    const AUTH_USER = 'your_username';
echo    const AUTH_PASS = 'your_strong_password';
echo.
echo 2. Keep Windows Firewall enabled (rule already created).
echo.
echo 3. Consider using a non-standard port (e.g., 8443) to reduce
echo    automated scanning.
echo.
echo 4. Regularly backup the data\ and logs\ folders.
echo.
echo ============================================================
echo        HYBRID SETUP (Self-Hosted + Render Fallback)
echo ============================================================
echo.
echo To use both your server AND Render as fallback:
echo.
echo 1. On each agent machine, edit:
echo    %%APPDATA%%\SystemHelper\urls.ini
echo.
echo 2. Add both URLs (primary first):
echo    http://%PUBLIC_IP%:3000
echo    wss://pu-k752.onrender.com
echo.
echo 3. Agents will try your server first, fallback to Render if down.
echo.
echo Or use the dashboard "Switch Server" button to switch all agents
echo remotely with one click.
echo.
echo ============================================================
echo Setup completed on: %date% %time%
echo ============================================================
) > "!INSTALL_DIR!\SETUP_REPORT.txt"

echo [OK] Setup report saved to: !INSTALL_DIR!\SETUP_REPORT.txt
echo.
echo ============================================================
echo        SETUP COMPLETE!
echo ============================================================
echo.
echo Server is ready at: !INSTALL_DIR!
echo Local access: http://localhost:3000
echo External access: http://%PUBLIC_IP%:3000 (after port forwarding)
echo.
echo IMPORTANT: You MUST set up port forwarding on your router!
echo See SETUP_REPORT.txt for detailed instructions.
echo.
echo Opening setup report...
notepad "!INSTALL_DIR!\SETUP_REPORT.txt"
echo.
echo Starting server now...
echo.
cd /d "!INSTALL_DIR!"
start "Remote Monitor Server" cmd /k "node server.source.js"
echo.
echo Server started in a new window.
echo Close that window to stop the server.
echo.
pause
