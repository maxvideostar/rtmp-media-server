@echo off
REM RTMP Media Server - instalare/actualizare (dublu-click). Dezinstalare: install.bat uninstall
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" %*
