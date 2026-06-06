@echo off
REM Double-clickable wrapper for launcher.ps1.
setlocal
cd /d "%~dp0"

if not exist "%~dp0launcher.ps1" (
    echo [x] launcher.ps1 not found. Run install.cmd first.
    pause
    exit /b 1
)

powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0launcher.ps1" %*
set EC=%ERRORLEVEL%
echo.
echo === Launcher exited with code %EC%. ===
echo Press any key to close . . .
pause >nul
exit /b %EC%
