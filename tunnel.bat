@echo off
setlocal enabledelayedexpansion

set PORT=8181
if not "%1"=="" set PORT=%1

where bore >nul 2>&1
if %ERRORLEVEL% neq 0 (
    echo bore not found. Downloading...
    curl -L -o bore.exe https://github.com/ekzhang/bore/releases/latest/download/bore-x86_64-pc-windows-msvc.exe
    if %ERRORLEVEL% neq 0 (
        echo Failed to download bore. Install manually from https://github.com/ekzhang/bore
        pause
        exit /b 1
    )
)

echo Starting bore tunnel on port %PORT%...
bore local %PORT% --to bore.pub
