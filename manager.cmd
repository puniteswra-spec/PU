@echo off
setlocal enabledelayedexpansion
title Remote Monitor Manager v9.0

set "EXE=PunMonitor.exe"
set "CLOUDFLARED=cloudflared"
set "TUNNEL_TYPE=cloudflare"
set "TUNNEL_URL="
set "PORT=8181"
set "CONFIG_DIR=%APPDATA%\PunMonitor"
set "CONFIG_FILE=%CONFIG_DIR%\config.json"
set "URL_FILE=%CONFIG_DIR%\urls.ini"
set "GITHUB_REPO=puniteswra-spec/PU"
set "DNS_DOMAIN="
set "AUTH_USER=puneet"
set "AUTH_PASS=puneet12"

:: Ensure config directory exists
if not exist "%CONFIG_DIR%" mkdir "%CONFIG_DIR%"

:: Load config from config.json
if exist "%CONFIG_FILE%" (
    for /f "tokens=*" %%a in ('powershell -Command "(Get-Content '%CONFIG_FILE%' | ConvertFrom-Json).config_port" 2^>nul') do set "PORT=%%a"
    for /f "tokens=*" %%a in ('powershell -Command "(Get-Content '%CONFIG_FILE%' | ConvertFrom-Json).tunnel_mode" 2^>nul') do set "TUNNEL_TYPE=%%a"
    for /f "tokens=*" %%a in ('powershell -Command "(Get-Content '%CONFIG_FILE%' | ConvertFrom-Json).github_repo" 2^>nul') do set "GITHUB_REPO=%%a"
    for /f "tokens=*" %%a in ('powershell -Command "(Get-Content '%CONFIG_FILE%' | ConvertFrom-Json).auth_user" 2^>nul') do set "AUTH_USER=%%a"
    for /f "tokens=*" %%a in ('powershell -Command "(Get-Content '%CONFIG_FILE%' | ConvertFrom-Json).auth_pass" 2^>nul') do set "AUTH_PASS=%%a"
    for /f "tokens=*" %%a in ('powershell -Command "(Get-Content '%CONFIG_FILE%' | ConvertFrom-Json).dns_domain" 2^>nul') do set "DNS_DOMAIN=%%a"
)
if "!PORT!"=="" set "PORT=8181"
if "!TUNNEL_TYPE!"=="" set "TUNNEL_TYPE=cloudflare"
if "!GITHUB_REPO!"=="" set "GITHUB_REPO=puniteswra-spec/PU"

:: Check for cloudflared
where cloudflared >nul 2>&1
if %errorlevel% neq 0 (
    set "CLOUDFLARED=%SystemRoot%\system32\cloudflared.exe"
)

:MENU
set "ERRORLEVEL="
cls
echo ==========================================
echo   REMOTE MONITOR MANAGER v9.0
echo ==========================================
echo.
echo  -- Quick Start --
echo  1) Start Everything (Server + Tunnel)
echo.
echo  -- Server Control --
echo  2) Check Status
echo  3) Stop Server
echo  4) Open Dashboard (local)
echo  5) View Connected Agents
echo.
echo  -- Tunnel Control --
echo  6) Start Tunnel
echo  7) Stop Tunnel
echo  8) Open Tunnel URL (remote)
echo  9) Switch Tunnel Type [current: !TUNNEL_TYPE!]
echo.
echo  -- Agent Control --
echo 10) Start Agent (this PC as monitored)
echo 11) Stop Agent
echo.
echo  -- Config --
echo 12) Switch to LAN Only Mode
echo 13) Change Dashboard Port [current: !PORT!]
echo 14) Set Server URLs (urls.ini)
echo 15) Set GitHub Repo [current: !GITHUB_REPO!]
echo 16) Set DNS Domain for URL Discovery
echo 17) Set Auth Credentials
echo.
echo  -- Admin Actions --
echo 18) Push URLs to All Connected Agents
echo 19) Push Update (.exe) to All Agents
echo 20) Hide/Unhide Agent
echo 21) Remove Agent
echo.
echo  -- Deployment --
echo 22) Generate Agent Package (for remote install)
echo 23) Install Agent on This PC
echo.
echo  -- Maintenance --
echo 24) Cleanup Logs
echo 25) Stop All
echo 26) Restart Everything
echo.
echo  -- Uninstall --
echo 27) Uninstall Agent
echo 28) Full Reset
echo.
echo  0) Exit
echo ==========================================
echo.
set /p "c=Choose: "

if "%c%"=="1" goto START_EVERYTHING
if "%c%"=="2" goto STATUS
if "%c%"=="3" goto STOP_SERVER
if "%c%"=="4" goto DASHBOARD_LOCAL
if "%c%"=="5" goto VIEW_AGENTS
if "%c%"=="6" goto START_TUNNEL
if "%c%"=="7" goto STOP_TUNNEL
if "%c%"=="8" goto DASHBOARD_TUNNEL
if "%c%"=="9" goto SWITCH_TUNNEL_TYPE
if "%c%"=="10" goto START_AGENT
if "%c%"=="11" goto STOP_AGENT
if "%c%"=="12" goto LAN_MODE
if "%c%"=="13" goto CHANGE_PORT
if "%c%"=="14" goto SET_URLS
if "%c%"=="15" goto SET_GITHUB_REPO
if "%c%"=="16" goto SET_DNS_DOMAIN
if "%c%"=="17" goto SET_AUTH
if "%c%"=="18" goto PUSH_URLS
if "%c%"=="19" goto PUSH_UPDATE
if "%c%"=="20" goto HIDE_AGENT
if "%c%"=="21" goto REMOVE_AGENT
if "%c%"=="22" goto GENERATE_PACKAGE
if "%c%"=="23" goto INSTALL_AGENT
if "%c%"=="24" goto CLEANUP
if "%c%"=="25" goto STOP_ALL
if "%c%"=="26" goto RESTART_ALL
if "%c%"=="27" goto UNINSTALL_AGENT
if "%c%"=="28" goto FULL_RESET
if "%c%"=="0" exit /b
goto MENU

:: ──────────────────────────────────────────
:START_EVERYTHING
cls
echo ==========================================
echo   STARTING EVERYTHING
echo ==========================================
echo.

:: Verify executable exists
if not exist "%EXE%" (
    echo [ERROR] %EXE% not found in current directory!
    echo Place PunMonitor.exe in this folder and try again.
    pause
    goto MENU
)

echo 1. Stopping existing processes...
taskkill /f /im PunMonitor.exe 2>nul
taskkill /f /im cloudflared.exe 2>nul
taskkill /f /im bore.exe 2>nul
timeout /t 2 /nobreak >nul

echo 2. Starting server...
start "" "%EXE%" --server
timeout /t 3 /nobreak >nul

:: Verify server started
netstat -an | find ":%PORT%" | find "LISTEN" >nul
if !errorlevel! equ 0 (
    echo [OK] Server running on port !PORT!
) else (
    echo [WARN] Server may not have started. Check logs.
)

echo 3. Starting !TUNNEL_TYPE! tunnel...
if /i "!TUNNEL_TYPE!"=="cloudflare" (
    where cloudflared >nul 2>&1
    if !errorlevel! neq 0 (
        echo [INFO] cloudflared not found. Downloading...
        powershell -Command "$arch=if([Environment]::Is64BitOperatingSystem){'amd64'}else{'386'}; Invoke-WebRequest -Uri 'https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-windows-arch.zip' -OutFile '$env:TEMP\cf.zip'; Expand-Archive '$env:TEMP\cf.zip' '$env:TEMP\cf' -Force; Move-Item '$env:TEMP\cf\cloudflared.exe' '$env:SystemRoot\system32\cloudflared.exe' -Force" 2>nul
    )
    start "" /b cloudflared tunnel --url http://localhost:!PORT! >nul 2>&1
) else if /i "!TUNNEL_TYPE!"=="bore" (
    where bore >nul 2>&1
    if !errorlevel! neq 0 (
        echo [WARN] bore not found. Install with: cargo install bore-cli
        pause
        goto MENU
    )
    start "" /b bore --to !PORT! bore.pub >nul 2>&1
)

echo.
echo Waiting for tunnel URL...
set "TUNNEL_URL="
for /l %%i in (1,1,18) do (
    if exist "%USERPROFILE%\Desktop\monitor-tunnel-url.txt" (
        for /f "usebackq delims=" %%u in ("%USERPROFILE%\Desktop\monitor-tunnel-url.txt") do set "TUNNEL_URL=%%u"
    )
    if defined TUNNEL_URL goto EVERYTHING_DONE
    timeout /t 5 /nobreak >nul
)

:EVERYTHING_DONE
echo.
if defined TUNNEL_URL (
    echo [OK] Everything running!
    echo.
    echo    Dashboard (local):  http://localhost:!PORT!
    echo    Dashboard (remote): !TUNNEL_URL!
) else (
    echo [OK] Server running!
    echo    Dashboard: http://localhost:!PORT!
    echo    Tunnel connecting... Check Status (Option 2)
)
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:GET_STATUS
set "TUNNEL_URL="
if exist "%USERPROFILE%\Desktop\monitor-tunnel-url.txt" (
    for /f "usebackq delims=" %%u in ("%USERPROFILE%\Desktop\monitor-tunnel-url.txt") do set "TUNNEL_URL=%%u"
)
exit /b

:: ──────────────────────────────────────────
:STATUS
call :GET_STATUS

cls
echo ==========================================
echo   SYSTEM STATUS
echo ==========================================
echo.

:: Check server
netstat -an | find ":%PORT%" | find "LISTEN" >nul
if !errorlevel! equ 0 (
    echo [OK] Server running on port !PORT!
) else (
    echo [ ] Server not running on port !PORT!
)

:: Check tunnel
if /i "!TUNNEL_TYPE!"=="cloudflare" (
    tasklist | findstr /i "cloudflared.exe" >nul
    if !errorlevel! equ 0 (
        if defined TUNNEL_URL (
            echo [OK] Cloudflared running
            echo       URL: !TUNNEL_URL!
        ) else (
            echo [OK] Cloudflared running (waiting for URL...)
        )
    ) else (
        echo [ ] Cloudflared not running
    )
) else if /i "!TUNNEL_TYPE!"=="bore" (
    tasklist | findstr /i "bore.exe" >nul
    if !errorlevel! equ 0 (
        echo [OK] Bore tunnel running
    ) else (
        echo [ ] Bore tunnel not running
    )
)

:: Check PunMonitor
tasklist | findstr /i "PunMonitor.exe" >nul
if !errorlevel! equ 0 (
    echo [OK] PunMonitor running
) else (
    echo [ ] PunMonitor not running
)

echo.
echo Tunnel type: !TUNNEL_TYPE!
echo Dashboard port: !PORT!
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:STOP_SERVER
cls
echo Stopping server...
taskkill /f /im PunMonitor.exe 2>nul
timeout /t 1 /nobreak >nul
echo Server stopped.
timeout /t 2 /nobreak >nul
goto MENU

:: ──────────────────────────────────────────
:DASHBOARD_LOCAL
cls
echo Opening local dashboard...
start http://localhost:!PORT!
goto MENU

:: ──────────────────────────────────────────
:START_TUNNEL
cls
echo ==========================================
echo   STARTING !TUNNEL_TYPE! TUNNEL
echo ==========================================
echo.

:: Stop existing tunnel first
if /i "!TUNNEL_TYPE!"=="cloudflare" (
    tasklist | findstr /i "cloudflared.exe" >nul
    if !errorlevel! equ 0 (
        echo Stopping existing cloudflared...
        taskkill /f /im cloudflared.exe 2>nul
        timeout /t 2 /nobreak >nul
    )
    echo Starting tunnel to localhost:!PORT!...
    start "" /b cloudflared tunnel --url http://localhost:!PORT! >nul 2>&1
) else if /i "!TUNNEL_TYPE!"=="bore" (
    tasklist | findstr /i "bore.exe" >nul
    if !errorlevel! equ 0 (
        echo Stopping existing bore...
        taskkill /f /im bore.exe 2>nul
        timeout /t 2 /nobreak >nul
    )
    echo Starting bore tunnel to localhost:!PORT!...
    start "" /b bore --to !PORT! bore.pub >nul 2>&1
)

echo Waiting for URL...
timeout /t 10 /nobreak >nul

:: Check for URL
set "URL_FOUND="
set "TUNNEL_URL="
for /l %%i in (1,1,12) do (
    if exist "%USERPROFILE%\Desktop\monitor-tunnel-url.txt" (
        for /f "usebackq delims=" %%u in ("%USERPROFILE%\Desktop\monitor-tunnel-url.txt") do set "TUNNEL_URL=%%u"
    )
    if defined TUNNEL_URL (
        set "URL_FOUND=1"
        goto TUNNEL_DONE
    )
    timeout /t 5 /nobreak >nul
)

:TUNNEL_DONE
echo.
if defined URL_FOUND (
    echo [OK] Tunnel ready!
    echo    !TUNNEL_URL!
) else (
    if /i "!TUNNEL_TYPE!"=="bore" (
        echo [OK] Bore tunnel started!
        echo    Access via: http://bore.pub:9999
    ) else (
        echo [WAIT] Still connecting...
        echo Check Status (Option 2) for URL.
    )
)
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:STOP_TUNNEL
cls
echo Stopping tunnel...
taskkill /f /im cloudflared.exe 2>nul
taskkill /f /im bore.exe 2>nul
del "%USERPROFILE%\Desktop\monitor-tunnel-url.txt" 2>nul
del "%USERPROFILE%\Desktop\REMOTE-MONITOR-URL.txt" 2>nul
echo Tunnel stopped.
timeout /t 2 /nobreak >nul
goto MENU

:: ──────────────────────────────────────────
:DASHBOARD_TUNNEL
call :GET_STATUS
if not "!TUNNEL_URL!"=="" (
    echo Opening: !TUNNEL_URL!
    start "" "!TUNNEL_URL!"
) else (
    echo No tunnel URL found. Start tunnel first ^(Option 6^).
    timeout /t 3 /nobreak >nul
)
goto MENU

:: ──────────────────────────────────────────
:SWITCH_TUNNEL_TYPE
cls
echo ==========================================
echo   SWITCH TUNNEL TYPE
echo ==========================================
echo.
echo  Current: !TUNNEL_TYPE!
echo.
echo  1) Cloudflare (free, no setup, quick tunnels)
echo  2) Bore (lightweight, requires bore-cli)
echo  3) None (local/LAN only)
echo.
set /p "t=Choose tunnel type: "

if "!t!"=="1" (
    set "TUNNEL_TYPE=cloudflare"
) else if "!t!"=="2" (
    set "TUNNEL_TYPE=bore"
) else if "!t!"=="3" (
    set "TUNNEL_TYPE=none"
) else (
    echo Invalid choice. Keeping current: !TUNNEL_TYPE!
    timeout /t 2 /nobreak >nul
    goto MENU
)

:: Save to config.json
powershell -Command "$f='!CONFIG_FILE!'; if(Test-Path $f){$c=Get-Content $f|ConvertFrom-Json; $c.tunnel_mode='!TUNNEL_TYPE!'; $c|ConvertTo-Json -Depth 3|Set-Content $f}else{@{tunnel_mode='!TUNNEL_TYPE!'}|ConvertTo-Json|Set-Content $f}"

echo.
echo Tunnel type set to: !TUNNEL_TYPE!
if /i "!TUNNEL_TYPE!"=="bore" (
    echo.
    echo NOTE: Bore requires installation: cargo install bore-cli
)
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:START_AGENT
cls
echo Starting agent...
tasklist | findstr /i "PunMonitor.exe" >nul
if !errorlevel! equ 0 (
    echo Agent already running!
) else (
    if not exist "%EXE%" (
        echo [ERROR] %EXE% not found!
        pause
        goto MENU
    )
    start "" "%EXE%"
    timeout /t 3 /nobreak >nul
    echo Agent started.
)
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:STOP_AGENT
cls
echo Stopping agent...
taskkill /f /im PunMonitor.exe 2>nul
timeout /t 1 /nobreak >nul
echo Agent stopped.
timeout /t 2 /nobreak >nul
goto MENU

:: ──────────────────────────────────────────
:FULL_RESET
cls
echo ==========================================
echo   FULL RESET (COMPLETE CLEANUP)
echo ==========================================
echo.
set /p "confirm=Type 'RESET' to confirm: "
if /i "%confirm%"=="RESET" (
    echo.
    echo Stopping all processes...
    call :STOP_ALL_QUIET
    timeout /t 2 /nobreak >nul

    echo Removing autostart registry entries...
    reg delete "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v "PunMonitor" /f 2>nul
    reg delete "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v "RemoteMonitor" /f 2>nul
    reg delete "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v "PunMonitorHelper" /f 2>nul

    echo Deleting Program Files folder...
    rmdir /s /q "%ProgramFiles%\PunMonitor" 2>nul

    echo Deleting AppData folder...
    rmdir /s /q "%CONFIG_DIR%" 2>nul

    echo Deleting desktop files...
    del "%USERPROFILE%\Desktop\monitor-tunnel-url.txt" 2>nul
    del "%USERPROFILE%\Desktop\REMOTE-MONITOR-URL.txt" 2>nul

    echo Deleting logs (*.log, *.csv, *.jsonl) from TEMP, APPDATA, and script folder...
    if exist "%TEMP%" (
        del /q /f "%TEMP%\*.log" 2>nul
        del /q /f "%TEMP%\*.csv" 2>nul
        del /q /f "%TEMP%\*.jsonl" 2>nul
    )
    if exist "%APPDATA%" (
        del /q /f "%APPDATA%\*.log" 2>nul
        del /q /f "%APPDATA%\*.csv" 2>nul
        del /q /f "%APPDATA%\*.jsonl" 2>nul
    )
    set "scriptDir=%~dp0"
    if exist "!scriptDir!" (
        del /q /f "!scriptDir!*.log" 2>nul
        del /q /f "!scriptDir!*.csv" 2>nul
        del /q /f "!scriptDir!*.jsonl" 2>nul
    )

    echo Deleting PunMonitor.exe from script directory...
    if exist "!scriptDir!PunMonitor.exe" del /f "!scriptDir!PunMonitor.exe" 2>nul
    if exist "!scriptDir!PunMonitor-watchdog.exe" del /f "!scriptDir!PunMonitor-watchdog.exe" 2>nul

    echo.
    echo Full reset complete!
) else (
    echo Cancelled.
)
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:LAN_MODE
cls
echo Switching to LAN Only Mode...
echo This removes server URLs, uses local discovery only.
echo.
set /p "confirm=Continue? (y/n): "
if /i "%confirm%"=="y" (
    if exist "%URL_FILE%" del "%URL_FILE%"
    echo Removed server URLs. Agent will use LAN discovery.
    echo Start agent with: PunMonitor.exe --lan
)
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:CHANGE_PORT
cls
echo ==========================================
echo   CHANGE DASHBOARD PORT
echo ==========================================
echo.
echo Current port: !PORT!
echo.
set /p "newport=Enter new port (1024-65535): "
if !newport! LSS 1024 (
    echo Port must be ^>= 1024
    timeout /t 2 /nobreak >nul
    goto MENU
)
if !newport! GTR 65535 (
    echo Port must be ^<= 65535
    timeout /t 2 /nobreak >nul
    goto MENU
)

set "PORT=!newport!"
:: Save to config.json
powershell -Command "$f='!CONFIG_FILE!'; if(Test-Path $f){$c=Get-Content $f|ConvertFrom-Json; $c.config_port=!PORT!; $c|ConvertTo-Json -Depth 3|Set-Content $f}else{@{config_port=!PORT!}|ConvertTo-Json|Set-Content $f}"

echo.
echo Port set to: !PORT!
echo NOTE: You must restart everything for this to take effect.
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:SET_URLS
cls
echo ==========================================
echo   SET SERVER URLs
echo ==========================================
echo.
echo Current URLs (from %URL_FILE%):
if exist "%URL_FILE%" (
    type "%URL_FILE%"
) else (
    echo No urls.ini found.
)
echo.
echo Enter server URLs (one per line, blank line to finish):
echo Examples:
echo   ws://192.168.1.100:8181/agent/ws
echo   wss://your-domain.com/agent/ws
echo   quic://192.168.1.100:8182
echo.

:: Create urls.ini
echo # Remote Monitor Server URLs > "%URL_FILE%"
echo # Format: ws://host:port/agent/ws or quic://host:port >> "%URL_FILE%"

:url_loop
set /p "url=URL (blank to finish): "
if "%url%"=="" goto urls_done
echo %url% >> "%URL_FILE%"
goto url_loop

:urls_done
echo.
echo URLs saved to %URL_FILE%
echo Restart agent to apply changes.
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:CLEANUP
cls
echo Clearing logs...
del "%CONFIG_DIR%\*.log" 2>nul
del "%TEMP%\cloudflared*.log" 2>nul
echo Cleanup done.
pause
goto MENU

:: ──────────────────────────────────────────
:STOP_ALL
cls
echo ==========================================
echo   STOP ALL PROCESSES
echo ==========================================
echo.
echo Stopping all processes...
call :STOP_ALL_QUIET
echo Done.
pause
goto MENU

:: ──────────────────────────────────────────
:RESTART_ALL
cls
echo Restarting everything...
call :STOP_ALL_QUIET
timeout /t 2 /nobreak >nul

if not exist "%EXE%" (
    echo [ERROR] %EXE% not found!
    pause
    goto MENU
)

start "" "%EXE%" --server
timeout /t 3 /nobreak >nul

if /i "!TUNNEL_TYPE!"=="cloudflare" (
    start "" /b cloudflared tunnel --url http://localhost:!PORT! >nul 2>&1
) else if /i "!TUNNEL_TYPE!"=="bore" (
    start "" /b bore --to !PORT! bore.pub >nul 2>&1
)

echo Waiting for tunnel URL...
set "TUNNEL_URL="
for /l %%i in (1,1,18) do (
    if exist "%USERPROFILE%\Desktop\monitor-tunnel-url.txt" (
        for /f "usebackq delims=" %%u in ("%USERPROFILE%\Desktop\monitor-tunnel-url.txt") do set "TUNNEL_URL=%%u"
    )
    if defined TUNNEL_URL goto RESTART_DONE
    timeout /t 5 /nobreak >nul
)

:RESTART_DONE
echo.
if defined TUNNEL_URL (
    echo Restarted!
    echo Tunnel: !TUNNEL_URL!
) else (
    echo Server started, tunnel connecting...
)
echo.
pause
goto MENU

:STOP_ALL_QUIET
:: Stop watchdog first
if exist "%CONFIG_DIR%\watchdog.stop" del "%CONFIG_DIR%\watchdog.stop"
echo 1 > "%CONFIG_DIR%\watchdog.stop"
timeout /t 1 /nobreak >nul
taskkill /f /im PunMonitor.exe 2>nul
taskkill /f /im PunMonitor-watchdog.exe 2>nul
taskkill /f /im cloudflared.exe 2>nul
taskkill /f /im bore.exe 2>nul
taskkill /f /im node.exe 2>nul
del "%CONFIG_DIR%\watchdog.stop" 2>nul
del "%CONFIG_DIR%\watchdog.pid" 2>nul
exit /b

:: ──────────────────────────────────────────
:UNINSTALL_AGENT
cls
echo ==========================================
echo   UNINSTALL AGENT
echo ==========================================
echo.
set /p "confirm=Type 'YES' to confirm: "
if /i "%confirm%"=="YES" (
    :: Stop watchdog first to prevent restart
    echo 1 > "%CONFIG_DIR%\watchdog.stop"
    timeout /t 1 /nobreak >nul
    taskkill /f /im PunMonitor.exe 2>nul
    timeout /t 1 /nobreak >nul
    reg delete "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v "PunMonitor" /f 2>nul
    reg delete "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v "RemoteMonitor" /f 2>nul
    rmdir /s /q "%CONFIG_DIR%" 2>nul
    del "%USERPROFILE%\Desktop\monitor-tunnel-url.txt" 2>nul
    del "%USERPROFILE%\Desktop\REMOTE-MONITOR-URL.txt" 2>nul
    echo Agent uninstalled!
) else (
    echo Cancelled.
)
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:FULL_RESET
cls
echo ==========================================
echo   FULL RESET (COMPLETE CLEANUP)
echo ==========================================
echo.
set /p "confirm=Type 'RESET' to confirm: "
if /i "%confirm%"=="RESET" (
    echo.
    echo Stopping all processes...
    call :STOP_ALL_QUIET
    timeout /t 2 /nobreak >nul

    echo Removing autostart registry entries...
    reg delete "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v "PunMonitor" /f 2>nul
    reg delete "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v "RemoteMonitor" /f 2>nul
    reg delete "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v "PunMonitorHelper" /f 2>nul

    echo Deleting Program Files folder...
    rmdir /s /q "%ProgramFiles%\PunMonitor" 2>nul

    echo Deleting AppData folder...
    rmdir /s /q "%CONFIG_DIR%" 2>nul

    echo Deleting desktop files...
    del "%USERPROFILE%\Desktop\monitor-tunnel-url.txt" 2>nul
    del "%USERPROFILE%\Desktop\REMOTE-MONITOR-URL.txt" 2>nul

    echo Deleting logs (*.log, *.csv, *.jsonl) from TEMP, APPDATA, and script folder...
    if exist "%TEMP%" (
        del /q /f "%TEMP%\*.log" 2>nul
        del /q /f "%TEMP%\*.csv" 2>nul
        del /q /f "%TEMP%\*.jsonl" 2>nul
    )
    if exist "%APPDATA%" (
        del /q /f "%APPDATA%\*.log" 2>nul
        del /q /f "%APPDATA%\*.csv" 2>nul
        del /q /f "%APPDATA%\*.jsonl" 2>nul
    )
    set "scriptDir=%~dp0"
    if exist "!scriptDir!" (
        del /q /f "!scriptDir!*.log" 2>nul
        del /q /f "!scriptDir!*.csv" 2>nul
        del /q /f "!scriptDir!*.jsonl" 2>nul
    )

    echo Deleting PunMonitor.exe from script directory...
    if exist "!scriptDir!PunMonitor.exe" del /f "!scriptDir!PunMonitor.exe" 2>nul
    if exist "!scriptDir!PunMonitor-watchdog.exe" del /f "!scriptDir!PunMonitor-watchdog.exe" 2>nul

    echo.
    echo Full reset complete!
) else (
    echo Cancelled.
)
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:SET_GITHUB_REPO
cls
echo ==========================================
echo   SET GITHUB REPO FOR URL DISCOVERY
echo ==========================================
echo.
echo Current: !GITHUB_REPO!
echo.
echo The agent will check this repo's servers.json every 24 hours.
echo Format: user/repo (e.g., puniteswra-spec/PU)
echo.
set /p "repo=Enter GitHub repo (blank to keep current): "
if not "%repo%"=="" set "GITHUB_REPO=%repo%"

powershell -Command "$f='!CONFIG_FILE!'; if(Test-Path $f){$c=Get-Content $f|ConvertFrom-Json; $c.github_repo='!GITHUB_REPO!'; $c|ConvertTo-Json -Depth 3|Set-Content $f}else{@{github_repo='!GITHUB_REPO!'}|ConvertTo-Json|Set-Content $f}"

echo.
echo GitHub repo set to: !GITHUB_REPO!
echo Agent will check: https://raw.githubusercontent.com/!GITHUB_REPO!/main/servers.json
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:SET_DNS_DOMAIN
cls
echo ==========================================
echo   SET DNS DOMAIN FOR URL DISCOVERY
echo ==========================================
echo.
echo The agent will check DNS TXT records every 1 hour.
echo Create a TXT record: _punmonitor.yourdomain.com
echo Value: ws://server1:8181/agent/ws,wss://server2:443/agent/ws
echo.
echo Current: !DNS_DOMAIN!
echo.
set /p "domain=Enter domain (blank to disable): "
if "%domain%"=="" (
    echo DNS discovery disabled.
    powershell -Command "$f='!CONFIG_FILE!'; if(Test-Path $f){$c=Get-Content $f|ConvertFrom-Json; $c|Add-Member -NotePropertyName 'dns_domain' -NotePropertyValue '' -Force; $c|ConvertTo-Json -Depth 3|Set-Content $f}"
) else (
    set "DNS_DOMAIN=%domain%"
    powershell -Command "$f='!CONFIG_FILE!'; if(Test-Path $f){$c=Get-Content $f|ConvertFrom-Json; $c.dns_domain='!DNS_DOMAIN!'; $c|ConvertTo-Json -Depth 3|Set-Content $f}else{@{dns_domain='!DNS_DOMAIN!'}|ConvertTo-Json|Set-Content $f}"
    echo DNS domain set to: !DNS_DOMAIN!
)
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:SET_AUTH
cls
echo ==========================================
echo   SET AUTH CREDENTIALS
echo ==========================================
echo.
echo Current User: !AUTH_USER!
echo.
set /p "user=Enter username (blank to keep): "
if not "%user%"=="" set "AUTH_USER=%user%"

set /p "pass=Enter password (blank to keep): "
if not "%pass%"=="" set "AUTH_PASS=%pass%"

powershell -Command "$f='!CONFIG_FILE!'; if(Test-Path $f){$c=Get-Content $f|ConvertFrom-Json; $c.auth_user='!AUTH_USER!'; $c.auth_pass='!AUTH_PASS!'; $c|ConvertTo-Json -Depth 3|Set-Content $f}else{@{auth_user='!AUTH_USER!'; auth_pass='!AUTH_PASS!'}|ConvertTo-Json|Set-Content $f}"

echo.
echo Auth credentials updated.
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:PUSH_URLS
cls
echo ==========================================
echo   PUSH URLS TO ALL CONNECTED AGENTS
echo ==========================================
echo.
set /p "urls=Enter server URLs (comma-separated): "
if "%urls%"=="" goto MENU

powershell -Command "$body = @{github_repo='!GITHUB_REPO!'}; if ('!DNS_DOMAIN!' -ne '') { $body.dns_domain = '!DNS_DOMAIN!' }; Invoke-RestMethod -Uri 'http://localhost:!PORT!/api/settings' -Method Post -ContentType 'application/json' -Body ($body | ConvertTo-Json)" 2>nul

echo URLs pushed to all connected agents.
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:PUSH_UPDATE
cls
echo ==========================================
echo   PUSH UPDATE TO ALL AGENTS
echo ==========================================
echo.
echo Enter the download URL for the new PunMonitor.exe
echo (e.g., GitHub release URL, direct download link)
echo.
set /p "url=Download URL: "
if "%url%"=="" goto MENU

powershell -Command "Invoke-RestMethod -Uri 'http://localhost:!PORT!/api/push-update' -Method Post -ContentType 'application/json' -Body '{\"url\":\"%url%\"}'" 2>nul

echo Update pushed to all connected agents.
echo Agents will download and restart automatically.
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:HIDE_AGENT
cls
echo ==========================================
echo   HIDE/UNHIDE AGENT
echo ==========================================
echo.
echo Connected agents:
powershell -Command "try{$r=Invoke-WebRequest -Uri 'http://localhost:!PORT!/api/agents/full' -UseBasicParsing -TimeoutSec 5; $r.Content}catch{Write-Host '  Server not running'}"
echo.
set /p "agent=Enter agent ID to toggle: "
if "%agent%"=="" goto MENU

set /p "hide=Hide? (y/n): "
if /i "%hide%"=="y" (
    powershell -Command "Invoke-RestMethod -Uri 'http://localhost:!PORT!/api/hide-agent' -Method Post -ContentType 'application/json' -Body '{\"agent_id\":\"%agent%\",\"hide\":true}'" 2>nul
    echo Agent %agent% hidden from dashboard.
) else (
    powershell -Command "Invoke-RestMethod -Uri 'http://localhost:!PORT!/api/hide-agent' -Method Post -ContentType 'application/json' -Body '{\"agent_id\":\"%agent%\",\"hide\":false}'" 2>nul
    echo Agent %agent% unhidden.
)
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:REMOVE_AGENT
cls
echo ==========================================
echo   REMOVE AGENT
echo ==========================================
echo.
echo Connected agents:
powershell -Command "try{$r=Invoke-WebRequest -Uri 'http://localhost:!PORT!/api/agents/full' -UseBasicParsing -TimeoutSec 5; $r.Content}catch{Write-Host '  Server not running'}"
echo.
set /p "agent=Enter agent ID to remove: "
if "%agent%"=="" goto MENU

set /p "confirm=Type 'YES' to confirm removal: "
if /i "%confirm%"=="YES" (
    powershell -Command "Invoke-RestMethod -Uri 'http://localhost:!PORT!/api/remove-agent' -Method Post -ContentType 'application/json' -Body '{\"agent_id\":\"%agent%\"}'" 2>nul
    echo Agent %agent% removed.
) else (
    echo Cancelled.
)
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:GENERATE_PACKAGE
cls
echo ==========================================
echo   GENERATE AGENT PACKAGE
echo ==========================================
echo.
echo This creates a zip file with:
echo - PunMonitor.exe
echo - config.json (pre-configured)
echo - urls.ini (pre-configured)
echo - install_agent.cmd (one-click installer)
echo.
echo Current settings:
echo   Server URLs: from urls.ini
echo   GitHub Repo: !GITHUB_REPO!
echo   Auth User: !AUTH_USER!
echo   Port: !PORT!
echo.
set /p "confirm=Generate package? (y/n): "
if /i "%confirm%"=="y" (
    set "PKG_DIR=%TEMP%\PunMonitor_Package"
    if exist "%PKG_DIR%" rmdir /s /q "%PKG_DIR%"
    mkdir "%PKG_DIR%"

    if exist "%EXE%" copy "%EXE%" "%PKG_DIR%\" >nul

    powershell -Command "@{config_port=!PORT!; github_repo='!GITHUB_REPO!'; auth_user='!AUTH_USER!'; auth_pass='!AUTH_PASS!'; tunnel_mode='!TUNNEL_TYPE!'; monthly_limit_mb=5000; max_fps=15}|ConvertTo-Json -Depth 3|Set-Content '%PKG_DIR%\config.json'"

    if exist "%URL_FILE%" copy "%URL_FILE%" "%PKG_DIR%\" >nul

    (
        echo @echo off
        echo echo Installing PunMonitor Agent...
        echo.
        echo if not exist "%%ProgramFiles%%\PunMonitor" mkdir "%%ProgramFiles%%\PunMonitor"
        echo copy /Y "PunMonitor.exe" "%%ProgramFiles%%\PunMonitor\" ^>nul
        echo copy /Y "config.json" "%%ProgramFiles%%\PunMonitor\" ^>nul
        echo if exist "urls.ini" copy /Y "urls.ini" "%%ProgramFiles%%\PunMonitor\" ^>nul
        echo.
        echo reg add "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v "PunMonitor" /t REG_SZ /d "\"%%ProgramFiles%%\PunMonitor\PunMonitor.exe\"" /f ^>nul
        echo.
        echo start "" "%%ProgramFiles%%\PunMonitor\PunMonitor.exe"
        echo.
        echo echo Installation complete!
        echo pause
    ) > "%PKG_DIR%\install_agent.cmd"

    powershell -Command "Compress-Archive -Path '%PKG_DIR%\*' -DestinationPath '%USERPROFILE%\Desktop\PunMonitor_Agent_Package.zip' -Force"

    echo.
    echo Package created: %USERPROFILE%\Desktop\PunMonitor_Agent_Package.zip
    echo Send this to remote users - they just run install_agent.cmd
    rmdir /s /q "%PKG_DIR%"
)
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:INSTALL_AGENT
cls
echo ==========================================
echo   INSTALL AGENT ON THIS PC
echo ==========================================
echo.
echo This will:
echo 1. Copy PunMonitor.exe to %ProgramFiles%\PunMonitor\
echo 2. Create config.json with current settings
echo 3. Set up autostart
echo 4. Start the agent
echo.
set /p "confirm=Continue? (y/n): "
if /i "%confirm%"=="y" (
    if not exist "%EXE%" (
        echo [ERROR] %EXE% not found in current directory!
        pause
        goto MENU
    )

    if not exist "%ProgramFiles%\PunMonitor" mkdir "%ProgramFiles%\PunMonitor"

    copy /Y "%EXE%" "%ProgramFiles%\PunMonitor\" >nul
    echo Copied PunMonitor.exe

    powershell -Command "@{config_port=!PORT!; github_repo='!GITHUB_REPO!'; auth_user='!AUTH_USER!'; auth_pass='!AUTH_PASS!'; tunnel_mode='!TUNNEL_TYPE!'; monthly_limit_mb=5000; max_fps=15; autostart=$true}|ConvertTo-Json -Depth 3|Set-Content '%ProgramFiles%\PunMonitor\config.json'"
    echo Created config.json

    if exist "%URL_FILE%" (
        copy /Y "%URL_FILE%" "%ProgramFiles%\PunMonitor\" >nul
        echo Copied urls.ini
    )

    reg add "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v "PunMonitor" /t REG_SZ /d "\"%ProgramFiles%\PunMonitor\PunMonitor.exe\"" /f >nul
    echo Set autostart

    start "" "%ProgramFiles%\PunMonitor\PunMonitor.exe"
    timeout /t 2 /nobreak >nul
    echo.
    echo Agent started!
    echo It will auto-restart if killed (watchdog enabled).
)
echo.
pause
goto MENU
