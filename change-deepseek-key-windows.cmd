@echo off
setlocal
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%~dp0change-deepseek-key-windows.ps1"
set "EXIT_CODE=%ERRORLEVEL%"
pause
exit /b %EXIT_CODE%
