@echo off
echo Building PunMonitor.exe (hidden console — GUI subsystem)...
go build -ldflags="-H windowsgui -s -w" -o PunMonitor.exe
if %errorlevel% neq 0 (
    echo Build failed!
    exit /b 1
)
echo Done. PunMonitor.exe is ready.
echo.
echo Cross-compiling for macOS (ARM64)...
set GOOS=darwin
set GOARCH=arm64
go build -ldflags="-s -w" -o monitor-darwin-arm64
set GOOS=darwin
set GOARCH=amd64
go build -ldflags="-s -w" -o monitor-darwin-amd64
set GOOS=
set GOARCH=
echo macOS binaries built: monitor-darwin-arm64, monitor-darwin-amd64
echo.
echo Usage:
echo   PunMonitor                                            Run (hidden window)
echo   PunMonitor --github-repo owner/repo --github-token x  Run with GitHub bootstrap
echo   PunMonitor --watchdog                                 Run watchdog (restarts on crash)
echo   PunMonitor --install                                  Install autostart via watchdog
echo   PunMonitor --remove                                   Remove autostart
echo   PunMonitor --check                                    Run preflight diagnostics
echo   PunMonitor --help                                     Show help
echo.
echo To bake GitHub config at build time:
echo   go build -ldflags="-X main.defaultGitHubRepo=owner/repo -X main.defaultGitHubToken=ghp_xxx -H windowsgui -s -w"
