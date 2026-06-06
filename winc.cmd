@echo off
REM winc - CLI wrapper so `winc ...` works from cmd.exe.
REM Add this folder to PATH to call `winc` from anywhere.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0winc.ps1" %*
