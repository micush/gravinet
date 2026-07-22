@echo off
setlocal

:: gravinet Windows installer launcher.
::
:: This replaces the old graphical (NSIS) installer. It's a thin wrapper:
:: all the actual install logic still lives in install-windows.ps1, which
:: must sit in this same folder. All this file does is
::   1. relaunch itself elevated if it isn't already Administrator
::      (install-windows.ps1 requires that and throws otherwise), then
::   2. set the PowerShell execution policy to Bypass for THIS PROCESS ONLY
::      (-Scope Process; nothing changes machine- or user-wide), then
::   3. call install-windows.ps1.
::
:: Just double-click it, or run from a normal (non-admin) cmd/PowerShell
:: prompt. Any arguments are passed straight through to install-windows.ps1,
:: e.g.:
::   install-windows.bat -NoStart -NoNpcap
::   install-windows.bat -Uninstall

set "PS1=%~dp0install-windows.ps1"

if not exist "%PS1%" (
    echo error: install-windows.ps1 not found next to install-windows.bat
    echo ^(expected at "%PS1%"^)
    exit /b 1
)

:: Re-launch elevated if we're not already Administrator.
net session >nul 2>&1
if not "%errorlevel%"=="0" (
    echo Requesting Administrator rights...
    powershell -NoProfile -Command ^
        "Start-Process -FilePath '%~f0' -ArgumentList '%*' -Verb RunAs"
    exit /b %errorlevel%
)

powershell -NoProfile -Command ^
    "Set-ExecutionPolicy -Scope Process Bypass -Force; & '%PS1%' %*"

exit /b %errorlevel%
