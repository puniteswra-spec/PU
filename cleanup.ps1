# cleanup.ps1 - Complete PunMonitor removal (Windows)
$ErrorActionPreference = "Stop"
Write-Host "PunMonitor cleanup..." -ForegroundColor Cyan
# 1. Stop and remove watchdog
$watchdogPath = Join-Path $env:APPDATA "PunMonitor\PunMonitor-watchdog.exe"
if (Test-Path $watchdogPath) {
    Write-Host "Stopping watchdog..." -ForegroundColor Yellow
    $watchdogPidFile = Join-Path $env:APPDATA "PunMonitor\watchdog.pid"
    if (Test-Path $watchdogPidFile) {
        $pid = Get-Content $watchdogPidFile | Select-Object -First 1
        if ($pid -match '^\d+$') {
            try { Stop-Process -Id $pid -Force -ErrorAction SilentlyContinue } catch {}
        }
        Remove-Item $watchdogPidFile -Force -ErrorAction SilentlyContinue
    }
    Get-Process "PunMonitor-watchdog.exe" -ErrorAction SilentlyContinue | Stop-Process -Force
    Remove-Item $watchdogPath -Force -ErrorAction SilentlyContinue
}
# 2. Kill all PunMonitor.exe processes
Write-Host "Terminating PunMonitor processes..." -ForegroundColor Yellow
Get-Process "PunMonitor.exe" -ErrorAction SilentlyContinue | Stop-Process -Force
# 3. Stop Cloudflare tunnel (cloudflared)
Write-Host "Stopping Cloudflare tunnel..." -ForegroundColor Yellow
Get-Process "cloudflared.exe" -ErrorAction SilentlyContinue | Stop-Process -Force
# 4. Stop Node.js processes (free port 3000)
Write-Host "Stopping Node.js processes..." -ForegroundColor Yellow
Get-Process "node.exe" -ErrorAction SilentlyContinue | Stop-Process -Force
# 5. Remove autostart registry entry
$regPath = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run"
$valName = "PunMonitor"
try {
    Remove-ItemProperty -Path $regPath -Name $valName -ErrorAction SilentlyContinue
    Write-Host "Removed autostart registry entry." -ForegroundColor Green
} catch {}
# 4. Delete installation folder
$installDir = Join-Path $env:ProgramFiles "PunMonitor"
if (Test-Path $installDir) {
    Write-Host "Removing installation folder: $installDir" -ForegroundColor Yellow
    Remove-Item $installDir -Recurse -Force -ErrorAction SilentlyContinue
}
# 5. Delete AppData data
$appData = Join-Path $env:APPDATA "PunMonitor"
if (Test-Path $appData) {
    Write-Host "Removing AppData: $appData" -ForegroundColor Yellow
    Remove-Item $appData -Recurse -Force -ErrorAction SilentlyContinue
}
# 6. Delete executable from script directory
$scriptDir = Split-Path $MyInvocation.MyCommand.Path -Parent
$exePath = Join-Path $scriptDir "PunMonitor.exe"
if (Test-Path $exePath) {
    Write-Host "Deleting executable: $exePath" -ForegroundColor Yellow
    Remove-Item $exePath -Force -ErrorAction SilentlyContinue
}
# 7. Delete logs/csv/jsonl in common folders
$possibleLogs = @($env:TEMP, $env:APPDATA, $scriptDir)
foreach ($dir in $possibleLogs) {
    if (Test-Path $dir) {
        Get-ChildItem $dir -Filter "*.log" -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
        Get-ChildItem $dir -Filter "*.csv" -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
        Get-ChildItem $dir -Filter "*.jsonl" -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
    }
}
Write-Host "`nPunMonitor cleanup complete!`n" -ForegroundColor Green
Write-Host "You may want to restart your PC to ensure no handles remain." -ForegroundColor Cyan
