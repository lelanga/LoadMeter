@echo off
title Load Meter
cd /d "%~dp0"
echo Starting Load Meter... a browser tab will open. Keep this window open; close it to stop.
loadmeter-windows-amd64.exe serve --open --workspace "%USERPROFILE%\LoadMeter-Workspace"
echo.
echo Load Meter stopped.
pause
