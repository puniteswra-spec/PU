@echo off
echo Building PunMonitor.exe (hidden console — GUI subsystem)...
go build -ldflags="-H=windowsgui -s -w" -o PunMonitor.exe
if %errorlevel% neq 0 (
    echo Build failed!
    exit /b 1
)
echo Done. PunMonitor.exe is ready.
echo.
echo Usage:
echo   PunMonitor              Run as agent (no window)
echo   PunMonitor --server     Run as relay server (no window)
echo   PunMonitor --check      Run preflight diagnostics (window shows)
echo   PunMonitor --help       Show help
