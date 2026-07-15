<#
.SYNOPSIS
  Remove the gravinet Windows service and binary.

.EXAMPLE
  # From an elevated PowerShell:
  .\uninstall-windows.ps1
  .\uninstall-windows.ps1 -Purge     # also remove %ProgramData%\gravinet

.PARAMETER InstallDir Install directory (default: %ProgramFiles%\gravinet)
.PARAMETER ConfigDir  Config directory (default: %ProgramData%\gravinet)
.PARAMETER Purge      Also delete the config directory
#>
[CmdletBinding()]
param(
  [string]$InstallDir = (Join-Path $env:ProgramFiles "gravinet"),
  [string]$ConfigDir  = (Join-Path $env:ProgramData "gravinet"),
  [switch]$Purge
)
$ErrorActionPreference = "Stop"
$ServiceName = "gravinet"

$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
         ).IsInRole([Security.Principal.WindowsBuiltinRole]::Administrator)
if (-not $admin) { throw "Run this script from an elevated (Administrator) PowerShell." }

$svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($svc) {
  Write-Host "==> stopping and removing service $ServiceName"
  if ($svc.Status -ne 'Stopped') {
    # Stop-Service blocks until the SCM reports Stopped, with no timeout of
    # its own - if gravinet.exe is wedged and never acknowledges the stop
    # control, that call (and this whole script) hangs forever, which is
    # exactly what used to force people to kill the process by hand instead.
    # sc.exe stop only *requests* the stop and returns immediately, so poll
    # for the real outcome ourselves and cap the wait.
    sc.exe stop $ServiceName | Out-Null
  }
  $stopped = $false
  for ($i = 0; $i -lt 40; $i++) {
    if ((Get-Service $ServiceName -ErrorAction SilentlyContinue).Status -eq 'Stopped') { $stopped = $true; break }
    Start-Sleep -Milliseconds 250
  }
  if (-not $stopped) {
    Write-Host "    service didn't stop within 10s - killing gravinet.exe directly"
  }
  sc.exe delete $ServiceName | Out-Null
}

# Belt-and-suspenders: the loop above gives the service 10 seconds and then
# moves on regardless of whether it actually stopped on its own. A still-
# running gravinet.exe holds its own binary, wintun.dll, and log files open,
# which used to turn the very first Remove-Item below into an unhandled,
# terminating error ($ErrorActionPreference = "Stop" plus no try/catch
# anywhere in this script meant that ONE locked file aborted everything after
# it) - most importantly, that could abort the script before ever reaching
# the -Purge step, making -Purge look broken when the real problem was
# upstream of it and never even visible: a script that dies from an elevated
# relaunch can take its console window down with it before you get to read
# the error. Make sure nothing is left running before touching any files.
Get-Process -Name gravinet -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Milliseconds 250

Write-Host "==> removing firewall rule"
Remove-NetFirewallRule -DisplayName $ServiceName -ErrorAction SilentlyContinue

Write-Host "==> removing binary and Wintun files"
$filesFailed = $false
foreach ($f in @("gravinet.exe", "wintun.dll", "wintun-prebuilt-binaries-license.txt", "README.md", "LICENSE", "getting-started.md", "meshping.bat")) {
  $p = Join-Path $InstallDir $f
  if (Test-Path $p) {
    try { Remove-Item $p -Force -ErrorAction Stop }
    catch { Write-Warning "could not remove $p ($($_.Exception.Message))"; $filesFailed = $true }
  }
}
if ((Test-Path $InstallDir) -and -not (Get-ChildItem $InstallDir -Force -ErrorAction SilentlyContinue)) {
  Remove-Item $InstallDir -Force -ErrorAction SilentlyContinue
}
if ($filesFailed) {
  Write-Warning "some files in $InstallDir are still in use - close anything using gravinet (including another open uninstall/install run) and re-run this script"
}

if ($Purge) {
  if (Test-Path $ConfigDir) {
    # Retry a few times: a transient lock (antivirus or the search indexer
    # briefly opening a just-touched file, Explorer's preview pane, etc.) can
    # clear itself within a second or two. A file someone has open in an
    # editor won't clear on its own, which is what the diagnostic listing
    # below is for.
    $purged = $false
    for ($i = 0; $i -lt 6 -and -not $purged; $i++) {
      try {
        Remove-Item $ConfigDir -Recurse -Force -ErrorAction Stop
        $purged = $true
      } catch {
        if ($i -lt 5) { Start-Sleep -Milliseconds 500 }
      }
    }
    if ($purged) {
      Write-Host "==> purged $ConfigDir"
    } else {
      Write-Warning "could not purge $ConfigDir - something still has a file in it open. Remaining:"
      Get-ChildItem $ConfigDir -Recurse -Force -ErrorAction SilentlyContinue | ForEach-Object { Write-Warning "    $($_.FullName)" }
      Write-Warning "close whatever has those open (a text editor on config.json is the usual suspect) and re-run this script, or delete $ConfigDir by hand."
    }
  } else {
    Write-Host "==> $ConfigDir already gone"
  }
} else {
  Write-Host "    left config in $ConfigDir (use -Purge to remove)"
}
Write-Host "gravinet uninstalled."
