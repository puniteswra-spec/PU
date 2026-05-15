@echo off
title Remote Monitor Manager
cd /d "%~dp0"
setlocal enabledelayedexpansion

:MENU
cls
echo ==========================================
echo   REMOTE MONITOR MANAGER - v6.0.9
echo ==========================================
echo.
echo  -- Server --
echo  1) Check Status
echo  2) Start Local Server
echo  3) Stop Local Server
echo  4) Open Dashboard
echo.
echo  -- Agents --
echo  5) View Connected Agents
echo  6) Make THIS PC the Server
echo  7) Make Any PC Server (from list)
echo  8) Find Server IP (scan network)
echo.
echo  -- Config --
echo  9) Choose Server Mode
echo  10) Switch to Internal Mode (LAN only)
echo  11) Create Organization Config
echo.
echo  -- Maintenance --
echo  12) Cleanup Logs
echo  13) Stop All Processes
echo.
echo  0) Exit
echo ==========================================
set /p c="Choose: "

if "%c%"=="1"  goto STATUS
if "%c%"=="2"  goto START_SERVER
if "%c%"=="3"  goto STOP_SERVER
if "%c%"=="4"  goto DASHBOARD
if "%c%"=="5"  goto VIEW_AGENTS
if "%c%"=="6"  goto MAKE_SERVER
if "%c%"=="7"  goto MAKE_SERVER_LIST
if "%c%"=="8"  goto FIND_IP
if "%c%"=="9"  goto CHOOSE_MODE
if "%c%"=="10" goto INTERNAL_SETUP
if "%c%"=="11" goto ORG_SETUP
if "%c%"=="12" goto CLEANUP
if "%c%"=="13" goto STOP_ALL
if "%c%"=="0"  exit /b
goto MENU

:: ──────────────────────────────────────────
:STATUS
cls
echo ==========================================
echo   SYSTEM STATUS
echo ==========================================
echo.
netstat -an | find ":3000" | find "LISTEN" >nul
if %errorlevel% equ 0 (echo Local Server:  Running on port 3000) else (echo Local Server:  Not running)
echo.
echo Connected agents:
powershell -Command "try{$r=Invoke-WebRequest -Uri 'http://localhost:3000/api/agents' -TimeoutSec 5 -Headers @{'Authorization'='Basic cHVuZWV0OnB1bmVldDEy'}; ($r.Content|ConvertFrom-Json)|%%{Write-Host '  ' $_.name ' - IP: ' $_.ip}}catch{Write-Host '  (server not running or no agents)'}"
echo.
echo urls.ini config:
if exist urls.ini (type urls.ini) else (echo (using defaults))
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:START_SERVER
cls
echo Starting local server...
if exist "server.source.js" (
    netstat -an | find ":3000" | find "LISTEN" >nul
    if %errorlevel% equ 0 (
        echo Server already running on port 3000
    ) else (
        start /min cmd /c "node server.source.js"
        timeout /t 3 /nobreak >nul
        echo Server started. Dashboard: http://localhost:3000
    )
) else if exist "server.js" (
    netstat -an | find ":3000" | find "LISTEN" >nul
    if %errorlevel% equ 0 (
        echo Server already running on port 3000
    ) else (
        start /min cmd /c "node server.js"
        timeout /t 3 /nobreak >nul
        echo Server started. Dashboard: http://localhost:3000
    )
) else (
    echo server.source.js not found. Install Node.js and run: npm install
)
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:STOP_SERVER
cls
echo Stopping local server...
taskkill /f /im node.exe 2>nul
echo Server stopped.
timeout /t 2 /nobreak >nul
goto MENU

:: ──────────────────────────────────────────
:DASHBOARD
cls
echo Opening dashboard...
start http://localhost:3000
goto MENU

:: ──────────────────────────────────────────
:VIEW_AGENTS
cls
echo ==========================================
echo   CONNECTED AGENTS
echo ==========================================
echo.
powershell -Command "try{$r=Invoke-WebRequest -Uri 'http://localhost:3000/api/agents' -TimeoutSec 5 -Headers @{'Authorization'='Basic cHVuZWV0OnB1bmVldDEy'}; $agents=($r.Content|ConvertFrom-Json); $i=1; $agents|%%{Write-Host ($i++).ToString()+') '+$_.name+' - IP: '+$_.ip+' - ID: '+$_.id}}catch{Write-Host 'Server not running or no agents'}"
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:MAKE_SERVER
cls
echo Making THIS PC the server...
if exist "SystemHelper.exe" (
    start "" SystemHelper.exe --server
    echo Server started. Dashboard: http://localhost:3000
) else (
    echo SystemHelper.exe not found in this folder.
    echo Run the Node.js server instead: Option 2
)
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:MAKE_SERVER_LIST
cls
echo Fetching connected agents...
echo.
powershell -Command "try{$r=Invoke-WebRequest -Uri 'http://localhost:3000/api/agents' -TimeoutSec 5 -Headers @{'Authorization'='Basic cHVuZWV0OnB1bmVldDEy'}; $agents=($r.Content|ConvertFrom-Json); $i=1; $agents|%%{Write-Host ($i++).ToString()+') '+$_.name+' ('+$_.ip+')'}; $c=Read-Host 'Enter number to make server'; if($c -gt 0 -and $c -le $agents.Count){$a=$agents[$c-1]; Invoke-WebRequest -Method POST -Uri ('http://localhost:3000/api/make-server/'+$a.id) -Headers @{'Authorization'='Basic cHVuZWV0OnB1bmVldDEy'}|Out-Null; Write-Host 'Sent to ' $a.name}else{Write-Host 'Invalid'}}catch{Write-Host 'Error: ' $_.Exception.Message}"
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:FIND_IP
cls
echo Scanning local network for active servers...
echo.
for /f "tokens=2 delims=:" %%a in ('ipconfig ^| find "IPv4"') do set IP=%%a
set IP=%IP: =%
for /f "tokens=1-3 delims=." %%a in ("%IP%") do set SUBNET=%%a.%%b.%%c
echo Scanning %SUBNET%.1-254...
echo.
for /l %%i in (1,1,254) do (
    powershell -Command "try{if((Invoke-WebRequest -Uri 'http://%SUBNET%.%%i:3000' -TimeoutSec 1 -UseBasicParsing).StatusCode -eq 200){Write-Host '  Found: %SUBNET%.%%i:3000'}}catch{}" 2>nul
)
echo.
echo Done.
pause
goto MENU

:: ──────────────────────────────────────────
:CHOOSE_MODE
cls
echo ==========================================
echo   CHOOSE SERVER MODE
echo ==========================================
echo.
echo 1) Local Server Only (this PC)
echo 2) Internal Network (LAN auto-discovery)
echo 3) Cloud + Local (Render.com fallback)
echo 4) Direct IP (specific server)
echo.
set /p sc="Choose (1-4): "
if "%sc%"=="1" (> urls.ini echo ws://127.0.0.1:3000 & echo Set to Local Server)
if "%sc%"=="2" (> urls.ini echo auto-local & echo Set to Internal Network)
if "%sc%"=="3" (> urls.ini echo wss://pu-k752.onrender.com & echo Set to Cloud + Local)
if "%sc%"=="4" (set /p ip="Enter server IP: " & > urls.ini echo ws://!ip!:3000 & echo Set to Direct IP)
echo.
echo urls.ini updated. Copy next to SystemHelper.exe on all PCs.
pause
goto MENU

:: ──────────────────────────────────────────
:INTERNAL_SETUP
cls
echo ==========================================
echo   INTERNAL NETWORK SETUP
echo ==========================================
echo.
echo Configure for internal network (no internet required).
echo.
echo 1) Make THIS PC the Server (internal)
echo 2) Make THIS PC an Agent (internal)
echo.
set /p st="Choose (1-2): "

if "%st%"=="1" (
    echo auto-local > urls.ini
    echo.
    echo urls.ini set to internal mode.
    echo Run:  SystemHelper.exe --server
    echo Or:   Option 2 (Start Local Server)
    echo Dashboard: http://[THIS-PC-IP]:3000
)
if "%st%"=="2" (
    echo auto-local > urls.ini
    echo.
    echo urls.ini set to internal mode.
    echo Run:  SystemHelper.exe
    echo Agent will auto-discover server on local network.
)
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:ORG_SETUP
cls
echo ==========================================
echo   ORGANIZATION CONFIG
echo ==========================================
echo.
set /p org_name="Enter organization name: "
if not "!org_name!"=="" (
    mkdir "!org_name!" 2>nul
    echo auto-local > "!org_name!\urls.ini"
    if exist SystemHelper.exe copy SystemHelper.exe "!org_name!\" /Y >nul 2>&1
    (
        echo INTERNAL SERVER - !org_name!
        echo ========================
        echo.
        echo Organization: !org_name!
        echo.
        echo SERVER SETUP:
        echo   1. Copy this folder to the server PC
        echo   2. Run: SystemHelper.exe --server
        echo   3. Or: Start Local Server from menu
        echo   4. Dashboard: http://[SERVER-IP]:3000
        echo.
        echo AGENT SETUP:
        echo   1. Copy this folder to each agent PC
        echo   2. Double-click SystemHelper.exe
        echo   3. Agents auto-connect to server
        echo.
        echo All PCs must be on the same network.
    ) > "!org_name!\README.txt"
    echo.
    echo Folder created: !org_name!\
    echo Copy to all PCs in the organization.
)
echo.
pause
goto MENU

:: ──────────────────────────────────────────
:CLEANUP
cls
set /p confirm="Clear all server logs + agent history? (y/n): "
if /i "%confirm%"=="y" (
    powershell -Command "try{$r=Invoke-WebRequest -Method POST -Uri 'http://localhost:3000/api/cleanup' -TimeoutSec 15 -Headers @{'Authorization'='Basic cHVuZWV0OnB1bmVldDEy'}; Write-Host $r.Content}catch{Write-Host 'Error: '$_.Exception.Message}"
    echo Cleanup done.
) else (echo Cancelled.)
pause
goto MENU

:: ──────────────────────────────────────────
:STOP_ALL
cls
echo Stopping all processes...
taskkill /f /im SystemHelper.exe 2>nul
taskkill /f /im node.exe 2>nul
del "%~dp0agent.lock" 2>nul
echo Done. All processes stopped.
timeout /t 2 /nobreak >nul
goto MENU
