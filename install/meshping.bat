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
:: Columns are Hostname, IP Address, Status, measured from the data before
:: anything is pinged and fitted to the console width - the same as the POSIX
:: script. See the comment on :measure_line below.
::
:: One deliberate divergence: the POSIX script asks the terminal its width and
:: falls back to 80, while this one is pinned at 80 outright. cmd.exe can
:: report a width through `mode con`, but only as localised text that has to be
:: parsed, so a machine in any other language reads back nothing useful and the
:: fallback would be doing the work anyway. 80 is that fallback, chosen
:: directly.
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

:: The table is laid out for an 80-column console. TABLE_W is a ceiling, not a
:: target: a mesh of short names produces a narrow table rather than one padded
:: out to 80.
set "TABLE_W=80"

:: First pass: measure the columns. Nothing is pinged here, so this costs a
:: read of the hosts file and no network time.
set "IP_W=10"
set "HOST_W=8"
set "STATUS_W=6"
set "INSIDE=0"
for /f "usebackq delims=" %%L in ("%HOSTS_FILE%") do call :measure_line "%%L"

:: Fitting to TABLE_W takes the space out of the hostname column and never out
:: of the address, for the reasons given on :handle_line's truncation below.
set /a HOST_MAX=TABLE_W - IP_W - 2 - 2 - STATUS_W
if !HOST_MAX! lss 8 set "HOST_MAX=8"
if !HOST_W! gtr !HOST_MAX! set "HOST_W=!HOST_MAX!"

:: The rule and the padding string are both built to the measured total, since
:: a hardcoded one no longer matches. PAD is at least as long as either column
:: because SEP_W is the sum of both plus the separators.
set /a SEP_W=HOST_W + 2 + IP_W + 2 + STATUS_W
set "SEP="
set "PAD="
for /l %%N in (1,1,%SEP_W%) do (
    set "SEP=!SEP!-"
    set "PAD=!PAD! "
)

echo !SEP!
call :print_row "Hostname" "IP Address" "Status"
echo !SEP!

:: Second pass: ping and print. Rows appear as each ping returns, rather than
:: the table being buffered until the last dead host has timed out.
set "INSIDE=0"
for /f "usebackq delims=" %%L in ("%HOSTS_FILE%") do call :handle_line "%%L"

echo.
exit /b 0

:: --- subroutines ------------------------------------------------------------

:: parse_line examines a single hosts-file line, tracking whether we're
:: currently inside a "# BEGIN gravinet ..." / "# END gravinet ..." block
:: (INSIDE persists across calls since this isn't wrapped in its own
:: setlocal). It sets ROW_IP / ROW_HOST / ROW_IS6 when the line is a data row
:: inside such a block that the -4/-6 flags select, and clears ROW_IP
:: otherwise. Both passes go through it, so the pass that measures the
:: columns and the pass that prints them can never disagree about which rows
:: are in the table - a -4 run must not size the address column around IPv6
:: rows it then skips.
:parse_line
set "LINE=%~1"
set "ROW_IP="
set "ROW_HOST="
set "ROW_IS6=0"

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

set "ROW_IP=!IP!"
set "ROW_HOST=!HOST!"
set "ROW_IS6=!IS6!"
goto :eof

:: measure_line widens the columns to fit one row, if that row needs it.
:measure_line
call :parse_line %1
if "!ROW_IP!"=="" goto :eof
call :strlen ROW_IP N
if !N! gtr !IP_W! set "IP_W=!N!"
call :strlen ROW_HOST N
if !N! gtr !HOST_W! set "HOST_W=!N!"
goto :eof

:: handle_line pings one row's address and prints it.
:handle_line
call :parse_line %1
if "!ROW_IP!"=="" goto :eof

if "!ROW_IS6!"=="1" (
    ping -6 -n 1 -w 2000 !ROW_IP! >nul 2>&1
) else (
    ping -4 -n 1 -w 2000 !ROW_IP! >nul 2>&1
)

if !errorlevel! equ 0 (
    set "STATUS=ALIVE"
) else (
    set "STATUS=DEAD"
)

:: A name too long for the fitted column is cut and marked with a trailing "~",
:: so a shortened name is never mistaken for the real one. The address is never
:: cut: it is bounded at 45 characters where a FQDN can be 253, and a cut
:: address is useless to paste into a ping where a cut name is still
:: recognisable.
call :strlen ROW_HOST N
if !N! gtr !HOST_W! (
    set /a CUT=HOST_W - 1
    for %%C in (!CUT!) do set "ROW_HOST=!ROW_HOST:~0,%%C!~"
)

call :print_row "!ROW_HOST!" "!ROW_IP!" "!STATUS!"
goto :eof

:: strlen measures the string held in the named variable into the named
:: result variable. Batch has no length operator, so this walks decreasing
:: powers of two, chopping the prefix off each time one fits. 64 is the
:: largest step because nothing in a hosts file line is near that long - a
:: full-length IPv6 literal is 45 characters.
:strlen
setlocal EnableDelayedExpansion
set "s=!%~1!#"
set "len=0"
for %%A in (64 32 16 8 4 2 1) do (
    if "!s:~%%A!" NEQ "" (
        set /a len+=%%A
        set "s=!s:~%%A!"
    )
)
endlocal & set "%~2=%len%"
goto :eof

:: print_row prints hostname, address and status at the measured widths, the
:: same layout as the POSIX script's
:: `printf "%-${HOST_W}s  %-${IP_W}s  %s\n"`. Status is last and needs no
:: padding of its own. Any shortening of the hostname has already happened in
:: :handle_line, where it can be marked; the clamp here only guards the
:: padding. The previous fixed 15-character hostname field truncated silently,
:: printing hw-macmini-macos as hw-macmini-maco with nothing to show for it.
:print_row
set "C1=%~1"
set "C2=%~2"
set "C3=%~3"
set "C1=!C1!!PAD!"
for %%W in (!HOST_W!) do set "C1=!C1:~0,%%W!"
set "C2=!C2!!PAD!"
for %%W in (!IP_W!) do set "C2=!C2:~0,%%W!"
echo !C1!  !C2!  !C3!
goto :eof
