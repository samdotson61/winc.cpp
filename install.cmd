@echo off
REM winc.cpp one-click setup (Windows). Double-click me.
REM Builds winc.exe from source (installing Go automatically if needed), then runs setup.
setlocal
cd /d "%~dp0"

if exist "winc.exe" goto setup

where go >nul 2>nul
if %errorlevel%==0 goto build

echo Go is not installed - winc needs it once, to build itself.
where winget >nul 2>nul
if %errorlevel%==0 (
  echo Installing Go via winget...
  winget install --id GoLang.Go -e --accept-package-agreements --accept-source-agreements --disable-interactivity
) else (
  echo [x] winget not found. Install Go from https://go.dev/dl/ then re-run this script.
  pause
  exit /b 1
)

REM winget often doesn't refresh PATH in this window; probe the default location.
where go >nul 2>nul
if %errorlevel%==0 goto build
if exist "%ProgramFiles%\Go\bin\go.exe" set "PATH=%ProgramFiles%\Go\bin;%PATH%"
where go >nul 2>nul
if %errorlevel%==0 goto build
echo [x] Go was installed but isn't on PATH yet. Open a NEW terminal and re-run install.cmd.
pause
exit /b 1

:build
echo Building winc.exe from source...
go build -o winc.exe .\cmd\winc
if errorlevel 1 (
  echo [x] build failed.
  pause
  exit /b 1
)

:setup
winc.exe setup
echo.
echo Done. Open a NEW terminal and run:  winc ls
pause
