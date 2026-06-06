@echo off
REM winc.cpp one-click setup (Windows). Double-click me.
REM Builds winc.exe from source (installing Go automatically if needed), then runs setup.
setlocal
cd /d "%~dp0"

if exist "winc.exe" goto setup

where go >nul 2>nul
if %errorlevel%==0 goto build

echo Go is not installed - winc needs it once, to build itself.

REM 1) Try winget.
where winget >nul 2>nul
if %errorlevel%==0 (
  echo Installing Go via winget...
  winget install --id GoLang.Go -e --accept-package-agreements --accept-source-agreements --disable-interactivity
)
call :findgo
if %errorlevel%==0 goto build

REM 2) Fallback: download + run the official Go MSI from go.dev (approve the UAC prompt).
echo Installing Go from the official MSI (go.dev)...
powershell -NoProfile -ExecutionPolicy Bypass -Command "$ErrorActionPreference='Stop'; $a= if($env:PROCESSOR_ARCHITECTURE -eq 'ARM64'){'arm64'}else{'amd64'}; $v=(Invoke-RestMethod 'https://go.dev/VERSION?m=text').Split([char]10)[0].Trim(); $m=Join-Path $env:TEMP ($v + '.windows-' + $a + '.msi'); Write-Host ('Downloading ' + $v + ' (' + $a + ')...'); Invoke-WebRequest ('https://go.dev/dl/' + $v + '.windows-' + $a + '.msi') -OutFile $m -UseBasicParsing; Write-Host 'Launching the Go installer (approve the UAC prompt)...'; Start-Process msiexec -ArgumentList '/i', $m, '/passive' -Verb RunAs -Wait"
call :findgo
if %errorlevel%==0 goto build

echo [x] Could not install Go automatically.
echo     Install it from https://go.dev/dl/ and re-run install.cmd.
pause
exit /b 1

:findgo
where go >nul 2>nul && exit /b 0
if exist "%ProgramFiles%\Go\bin\go.exe" set "PATH=%ProgramFiles%\Go\bin;%PATH%"
where go >nul 2>nul && exit /b 0
exit /b 1

:build
echo Building winc.exe from source...
go build -o winc.exe .\cmd\winc
if errorlevel 1 ( echo [x] build failed. & pause & exit /b 1 )

:setup
winc.exe setup
echo.
echo Done. Open a NEW terminal and run:  winc ls
pause
