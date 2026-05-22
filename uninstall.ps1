<#
.SYNOPSIS
    Completely remove Remote Monitor Agent from this system.
.DESCRIPTION
    Kills the agent process, deletes data directory, removes autostart,
    cleans registry entries, and optionally deletes the executable.
#>

$ErrorActionPreference = "SilentlyContinue"
$dataDir = "$env:APPDATA\PunMonitor"
$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$exePath = "$scriptDir\PunMonitor.exe"
$sysDir = "$env:SystemRoot\Temp\PunMonitor.exe"

Write-Host "=== Remote Monitor Agent — Uninstall ===" -ForegroundColor Yellow
Write-Host ""

# 1. Kill process
Write-Host "[1/6] Killing agent process..."
$proc = Get-Process "PunMonitor" -ErrorAction SilentlyContinue
if ($proc) {
    $proc | Stop-Process -Force
    Write-Host "  -> Process killed."
} else {
    Write-Host "  -> Not running."
}

# 2. Remove autostart registry
Write-Host "[2/6] Removing autostart entries..."
$regPaths = @(
    "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run\PunMonitor",
    "HKLM:\Software\Microsoft\Windows\CurrentVersion\Run\PunMonitor"
)
foreach ($p in $regPaths) {
    if (Test-Path $p) {
        Remove-Item -Path $p -Force
        Write-Host "  -> Removed $p"
    }
}

# 3. Remove scheduled tasks
Write-Host "[3/6] Removing scheduled tasks..."
$tasks = @("PunMonitor", "RemoteMonitor", "SystemHelper")
foreach ($t in $tasks) {
    $task = Get-ScheduledTask -TaskName $t -ErrorAction SilentlyContinue
    if ($task) {
        Unregister-ScheduledTask -TaskName $t -Confirm:$false
        Write-Host "  -> Removed scheduled task '$t'"
    }
}
Write-Host "  -> Done."

# 4. Delete data directory (agent.id, agent.log, activity.log, cache)
Write-Host "[4/6] Deleting data directory: $dataDir"
if (Test-Path $dataDir) {
    Remove-Item -Path $dataDir -Recurse -Force
    Write-Host "  -> Deleted."
} else {
    Write-Host "  -> Not found."
}

# 5. Delete system-location copy (if stealth was used)
Write-Host "[5/6] Checking system-location copies..."
if (Test-Path $sysDir) {
    Remove-Item -Path $sysDir -Force
    Write-Host "  -> Deleted $sysDir"
} else {
    Write-Host "  -> Not found."
}
# Check common locations
$locations = @(
    "$env:WINDIR\Temp\PunMonitor.exe",
    "$env:WINDIR\System32\PunMonitor.exe",
    "$env:LOCALAPPDATA\Temp\PunMonitor.exe"
)
foreach ($loc in $locations) {
    if (Test-Path $loc) {
        Remove-Item -Path $loc -Force
        Write-Host "  -> Deleted $loc"
    }
}

# 6. Optionally delete the executable
Write-Host "[6/6] Cleaning up executable..."
if (Test-Path $exePath) {
    $answer = Read-Host "Delete the agent executable at '$exePath'? (y/N)"
    if ($answer -eq 'y' -or $answer -eq 'Y') {
        Remove-Item -Path $exePath -Force
        Write-Host "  -> Deleted."
    } else {
        Write-Host "  -> Skipped."
    }
}

Write-Host ""
Write-Host "=== Uninstall complete ===" -ForegroundColor Green
Write-Host "The agent has been removed from this system."