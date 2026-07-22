@echo off
:: meshping.bat - Windows equivalent of the POSIX `meshping` script.
::
:: Pings every host in the gravinet-managed block(s) of the Windows hosts
:: file (%SystemRoot%\System32\drivers\etc\hosts) and prints an ALIVE/DEAD
:: table. gravinet writes one delimited block per network it manages,
:: each opening with a line starting "# BEGIN gravinet " and closing with
:: one starting "# END gravinet " (see internal/hosts/hosts.go) - this
:: matches on that prefix, the same way the POSIX script's sed range
:: pattern matches a substring rather than requiring an exact line, so
:: multiple per-network blocks (each with its own tag) are all picked up.
::
:: Usage:
::   meshping.bat            ping both IPv4 and IPv6 entries
::   meshping.bat -4         ping only IPv4 entries
::   meshping.bat -6         ping only IPv6 entries
setlocal EnableDelayedExpansion

set "HOSTS_FILE=%SystemRoot%\System32\drivers\etc\hosts"
set "PING_4=1"
set "PING_6=1"

:: --- parse command line options -------------------------------------------
:parse_args
if "%~1"=="" goto args_done
if "%~1"=="-4" (
    set "PING_4=1"
    set "PING_6=0"
    shift
    goto parse_args
)
if "%~1"=="-6" (
    set "PING_4=0"
    set "PING_6=1"
    shift
    goto parse_args
)
echo Usage: %~nx0 [-4] [-6] 1>&2
exit /b 1

:args_done

if not exist "%HOSTS_FILE%" (
    echo Error: %HOSTS_FILE% not found. 1>&2
    exit /b 1
)

set "SEP=---------------------------------------------------------------"
echo %SEP%
call :print_row "IP Address" "Hostname" "Status"
echo %SEP%

set "INSIDE=0"
for /f "usebackq delims=" %%L in ("%HOSTS_FILE%") do call :handle_line "%%L"

echo.
exit /b 0

:: --- subroutines ------------------------------------------------------------

:: handle_line processes a single hosts-file line, tracking whether we're
:: currently inside a "# BEGIN gravinet ..." / "# END gravinet ..." block
:: (INSIDE persists across calls since this isn't wrapped in its own
:: setlocal) and, for data lines inside such a block, pinging the address.
:handle_line
set "LINE=%~1"

if "!LINE:~0,16!"=="# BEGIN gravinet" (
    set "INSIDE=1"
    goto :eof
)
if "!LINE:~0,14!"=="# END gravinet" (
    set "INSIDE=0"
    goto :eof
)
if not "!INSIDE!"=="1" goto :eof
if "!LINE!"=="" goto :eof
if "!LINE:~0,1!"=="#" goto :eof

set "IP="
set "HOST="
for /f "tokens=1,2" %%a in ("!LINE!") do (
    set "IP=%%a"
    set "HOST=%%b"
)
if "!IP!"=="" goto :eof
if "!HOST!"=="" goto :eof

:: Detect address family (IPv6 addresses contain a colon).
set "IS6=0"
echo !IP! | findstr /c:":" >nul 2>&1 && set "IS6=1"

:: Skip addresses based on the -4/-6 flags.
if "!IS6!"=="1" if "!PING_6!"=="0" goto :eof
if "!IS6!"=="0" if "!PING_4!"=="0" goto :eof

if "!IS6!"=="1" (
    ping -6 -n 1 -w 2000 !IP! >nul 2>&1
) else (
    ping -4 -n 1 -w 2000 !IP! >nul 2>&1
)

if !errorlevel! equ 0 (
    set "STATUS=ALIVE"
) else (
    set "STATUS=DEAD"
)

call :print_row "!IP!" "!HOST!" "!STATUS!"
goto :eof

:: print_row prints three columns padded/truncated to the same widths as
:: the POSIX script's `printf "%-40s %-15s %s\n"`.
:print_row
set "C1=%~1"
set "C2=%~2"
set "C3=%~3"
set "PAD=                                                                "
set "C1=!C1!!PAD!"
set "C1=!C1:~0,40!"
set "C2=!C2!!PAD!"
set "C2=!C2:~0,15!"
echo !C1! !C2! !C3!
goto :eof
