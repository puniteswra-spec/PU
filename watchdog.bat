@echo off
:loop
tasklist | find "SystemHelper" >nul
if errorlevel 1 start "" "P:\Opencode\RemoteMonitor-Merged\SystemHelper_debug.exe"
timeout /t 120 /nobreak >nul
goto loop