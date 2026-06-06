@echo off
REM winc.cpp one-click setup (Windows). Double-click me.
setlocal
cd /d "%~dp0"

if not exist "winc.exe" (
  where go >nul 2>nul
  if errorlevel 1 (
    echo [x] winc.exe not found and Go is not installed.
    echo     Either download a prebuilt winc.exe release into this folder,
    echo     or install Go from https://go.dev/dl/ and re-run this script.
    pause
    exit /b 1
  )
  echo Building winc.exe from source...
  go build -o winc.exe .\cmd\winc
  if errorlevel 1 ( echo [x] build failed & pause & exit /b 1 )
)

winc.exe setup
echo.
echo Done. Open a NEW terminal and run:  winc ls
pause
