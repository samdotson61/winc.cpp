@echo off
REM Double-clickable wrapper for install.ps1.
REM Sets cwd to this folder, bypasses ExecutionPolicy, keeps window open on exit.
setlocal
cd /d "%~dp0"

where powershell >nul 2>nul
if errorlevel 1 (
    echo [x] PowerShell not found on PATH.
    echo     This script needs Windows PowerShell or PowerShell 7+.
    pause
    exit /b 1
)

echo === winc.cpp installer ===
echo Folder: %~dp0
echo.

powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" %*
set EC=%ERRORLEVEL%

echo.
if "%EC%"=="0" (
    echo === Done. ===
) else (
    echo === Installer exited with code %EC%. ===
)
echo Press any key to close this window . . .
pause >nul
exit /b %EC%
