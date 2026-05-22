param(
    [int]$Port = 8181,
    [string]$TunnelName = "remote-monitor",
    [switch]$OpenBrowser = $false,
    [switch]$Check = $false
)

if ($Check) {
    Write-Host "+-------------------------------------------------------+" -ForegroundColor Cyan
    Write-Host "|   Remote Monitor — Cloudflare Tunnel Diagnostics      |" -ForegroundColor Cyan
    Write-Host "+-------------------------------------------------------+" -ForegroundColor Cyan
    Write-Host ""

    # Check cloudflared
    $cf = Get-Command "cloudflared" -ErrorAction SilentlyContinue
    if ($cf) {
        Write-Host "[OK] cloudflared installed at: $($cf.Source)" -ForegroundColor Green
    } else {
        Write-Host "[MISSING] cloudflared not found in PATH" -ForegroundColor Red
        Write-Host "  Download from: https://github.com/cloudflare/cloudflared/releases" -ForegroundColor Yellow
    }

    # Check auth
    $homeDir = $env:USERPROFILE
    $certPaths = @(
        "$homeDir\.cloudflared\cert.pem",
        "$homeDir\.cloudflare-warp\cert.pem"
    )
    $hasCert = $false
    foreach ($cp in $certPaths) {
        if (Test-Path $cp) {
            Write-Host "[OK] Cloudflare auth: $cp" -ForegroundColor Green
            $hasCert = $true
            break
        }
    }
    if (-not $hasCert) {
        Write-Host "[MISSING] Cloudflare login required (no cert.pem found)" -ForegroundColor Red
        Write-Host "  Run: cloudflared tunnel login" -ForegroundColor Yellow
    }

    # Check PunMonitor
    $proc = Get-Process "PunMonitor" -ErrorAction SilentlyContinue
    if ($proc) {
        Write-Host "[OK] PunMonitor running (PID $($proc.Id))" -ForegroundColor Green
    } else {
        Write-Host "[INFO] PunMonitor not running - start with: PunMonitor.exe --server" -ForegroundColor DarkGray
    }

    # Check port
    $listener = Get-NetTCPConnection -LocalPort $Port -ErrorAction SilentlyContinue
    if ($listener) {
        Write-Host "[OK] Port $Port is in use" -ForegroundColor Green
    } else {
        Write-Host "[INFO] Port $Port available" -ForegroundColor DarkGray
    }

    Write-Host ""
    Write-Host "To start tunnel: .\cloudflare.ps1" -ForegroundColor Cyan
    exit 0
}

$ErrorActionPreference = "Continue"
$urlFile = "$env:USERPROFILE\Desktop\monitor-tunnel-url.txt"
$altUrlFile = "$env:USERPROFILE\Desktop\REMOTE-MONITOR-URL.txt"
$logFile = "$env:TEMP\cloudflared-tunnel.log"

Write-Host "+-------------------------------------------------------+" -ForegroundColor Cyan
Write-Host "|   Remote Monitor -- Cloudflare Tunnel                 |" -ForegroundColor Cyan
Write-Host "+-------------------------------------------------------+" -ForegroundColor Cyan
Write-Host ""

# -- 1. Check / install cloudflared --
$cf = Get-Command "cloudflared" -ErrorAction SilentlyContinue
if (-not $cf) {
    Write-Host ">> cloudflared not found, downloading..." -ForegroundColor Yellow
    $arch = if ([Environment]::Is64BitOperatingSystem) { "amd64" } else { "386" }
    $url = "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-windows-$arch.zip"
    $zip = "$env:TEMP\cloudflared.zip"
    try {
        Invoke-WebRequest -Uri $url -OutFile $zip -UseBasicParsing
        Expand-Archive -Path $zip -DestinationPath "$env:TEMP\cloudflared" -Force
        Move-Item "$env:TEMP\cloudflared\cloudflared.exe" "$env:SystemRoot\system32\cloudflared.exe" -Force
        Remove-Item "$env:TEMP\cloudflared" -Recurse -Force -ErrorAction SilentlyContinue
        Remove-Item $zip -Force -ErrorAction SilentlyContinue
        Write-Host ">> installed to $env:SystemRoot\system32\cloudflared.exe" -ForegroundColor Green
        $cf = Get-Command "cloudflared"
    } catch {
        Write-Host ">> Failed to download cloudflared." -ForegroundColor Red
        Write-Host "  Download from: https://github.com/cloudflare/cloudflared/releases"
        Write-Host "  Place cloudflared.exe in PATH and re-run."
        pause; exit 1
    }
}

# -- 2. Quick tunnel doesn't need login anymore (cloudflared handles it) --

# -- 2. Kill stale cloudflared --
Get-Process "cloudflared" -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Milliseconds 500
Remove-Item $logFile -ErrorAction SilentlyContinue

# -- 3. Ensure PunMonitor server is running --
$proc = Get-Process "PunMonitor" -ErrorAction SilentlyContinue
if (-not $proc) {
    Write-Host ">> Starting PunMonitor in server mode..." -ForegroundColor Yellow
    $exe = if (Test-Path ".\PunMonitor.exe") { ".\PunMonitor.exe" } else { "PunMonitor.exe" }
    Start-Process -FilePath $exe -ArgumentList "--server" -WindowStyle Hidden
    Start-Sleep -Seconds 3
} else {
    Write-Host ">> PunMonitor already running (use --force to restart)" -ForegroundColor DarkGray
}

# -- 4. Start cloudflared (capture output to show live) --
Write-Host ">> Opening Cloudflare tunnel..." -ForegroundColor Yellow

$psi = New-Object System.Diagnostics.ProcessStartInfo
$psi.FileName = $cf.Source
$psi.Arguments = "tunnel --url http://localhost:$Port"
$psi.UseShellExecute = $false
$psi.RedirectStandardOutput = $true
$psi.RedirectStandardError = $true
$psi.CreateNoWindow = $true
$p = [System.Diagnostics.Process]::Start($psi)

# -- 5. Show output live + extract URL --
Write-Host ""
Write-Host "-- tunnel starting, detecting public URL... --" -ForegroundColor DarkGray

$tunnelUrl = $null
$fullLog = New-Object System.Text.StringBuilder

while (-not $p.HasExited) {
    $line = $null
    if (-not $p.StandardOutput.EndOfStream) {
        $line = $p.StandardOutput.ReadLine()
    }
    $errLine = $null
    if (-not $p.StandardError.EndOfStream) {
        $errLine = $p.StandardError.ReadLine()
    }
    
    foreach ($l in @($line, $errLine)) {
        if ($l) {
            $fullLog.AppendLine($l) | Out-Null
            if ($l -match 'trycloudflare\.com|https?://') {
                Write-Host $l -ForegroundColor Green
            } else {
                Write-Host $l
            }
            if (-not $tunnelUrl) {
                $match = [regex]::Match($l, 'https://[a-zA-Z0-9._-]+\.trycloudflare\.com')
                if ($match.Success) {
                    $tunnelUrl = $match.Value
                }
            }
        }
    }
    
    if (-not $line -and -not $errLine) {
        Start-Sleep -Milliseconds 200
    }

    if ($tunnelUrl) { break }
}

# If process already exited with no URL, read remaining
if (-not $tunnelUrl) {
    $remaining = $p.StandardOutput.ReadToEnd() + $p.StandardError.ReadToEnd()
    $fullLog.Append($remaining) | Out-Null
    Write-Host $remaining
    $match = [regex]::Match($remaining, 'https://[a-zA-Z0-9._-]+\.trycloudflare\.com')
    if ($match.Success) { $tunnelUrl = $match.Value }
}

# Save full log
$fullLog.ToString() | Out-File -FilePath $logFile -Encoding utf8

# -- 6. Display result --
Write-Host ""
if ($tunnelUrl) {
    $tunnelUrl | Out-File -FilePath $urlFile -Encoding utf8
    $tunnelUrl | Out-File -FilePath $altUrlFile -Encoding utf8

    Write-Host "+-----------------------------------------------------------------+" -ForegroundColor Green
    Write-Host "|                    TUNNEL IS RUNNING                          |" -ForegroundColor Green
    Write-Host "+-----------------------------------------------------------------+" -ForegroundColor Green
    Write-Host "|" -ForegroundColor Green
    Write-Host "|  $tunnelUrl" -ForegroundColor White
    Write-Host "|" -ForegroundColor Green
    Write-Host "|  Open this URL in any browser to access your" -ForegroundColor White
    Write-Host "|  Remote Monitor dashboard from anywhere." -ForegroundColor White
    Write-Host "|" -ForegroundColor Green
    Write-Host "|  Saved to Desktop:" -ForegroundColor DarkGray
    Write-Host "|    [1] monitor-tunnel-url.txt" -ForegroundColor DarkGray
    Write-Host "|    [2] REMOTE-MONITOR-URL.txt" -ForegroundColor DarkGray
    Write-Host "|" -ForegroundColor Green
    Write-Host "|  Close this window to STOP the tunnel." -ForegroundColor Yellow
    Write-Host "+-----------------------------------------------------------------+" -ForegroundColor Green
    Write-Host ""

    $openIt = $OpenBrowser
    if (-not $openIt) {
        Write-Host ">> Share the URL with others so they can view the dashboard." -ForegroundColor Cyan
        $response = Read-Host ">> Open in Edge now to copy the URL? (Y/n)"
        $openIt = ($response -eq '' -or $response -match '^y')
    }
    if ($openIt) {
        Start-Process "msedge" -ArgumentList $tunnelUrl
        Write-Host "  [OK] Edge opened. Copy the URL from the address bar.`n" -ForegroundColor Green
    } else {
        Write-Host "  [OK] URL files on Desktop. Open and share manually.`n" -ForegroundColor DarkGray
    }

    Write-Host "-- tunnel live log (closing this window stops the tunnel) --" -ForegroundColor DarkGray
    while (-not $p.HasExited) {
        if (-not $p.StandardOutput.EndOfStream) { $l = $p.StandardOutput.ReadLine(); if ($l) { Write-Host $l } }
        if (-not $p.StandardError.EndOfStream) { $l = $p.StandardError.ReadLine(); if ($l) { Write-Host $l -ForegroundColor DarkGray } }
        Start-Sleep -Milliseconds 100
    }
    Write-Host "`nTunnel stopped." -ForegroundColor Red
    pause
} else {
    Write-Host "+-----------------------------------------------------+" -ForegroundColor Red
    Write-Host "|  Could not detect tunnel URL.                       |" -ForegroundColor Red
    Write-Host "|  Check the log file:                                |" -ForegroundColor Red
    Write-Host "|  $logFile" -ForegroundColor White
    Write-Host "+-----------------------------------------------------+" -ForegroundColor Red
    Write-Host "`nFull output:"
    Write-Host ($fullLog.ToString())
    pause
}
