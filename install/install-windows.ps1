<#
.SYNOPSIS
  Build (if needed) and install gravinet as a Windows service.

.DESCRIPTION
  With no prebuilt binary present, this installs a Go toolchain if the host lacks
  one, builds the binary from the bundled source, fetches the signed Wintun
  driver, then installs and starts the service. It also installs Npcap (for
  the web admin's packet-capture tab) if it isn't already present.

.EXAMPLE
  # From an elevated PowerShell:
  .\install-windows.ps1
  .\install-windows.ps1 -Uninstall
  .\install-windows.ps1 -Bin .\gravinet-windows-amd64.exe -NoStart

.PARAMETER Bin       Prebuilt binary to install instead of building
.PARAMETER InstallDir Install directory (default: %ProgramFiles%\gravinet)
.PARAMETER ConfigDir  Config directory (default: %ProgramData%\gravinet)
.PARAMETER Wintun     Path to wintun.dll to ship beside the exe (auto-fetched when building)
.PARAMETER NoStart    Install/register but do not start the service now
.PARAMETER NoNpcap    Skip installing Npcap (packet capture in the web admin just stays unavailable)
.PARAMETER Uninstall  Remove the service, binary, and Wintun (config is left in place)
#>
[CmdletBinding()]
param(
  [string]$Bin = "",
  [string]$InstallDir = (Join-Path $env:ProgramFiles "gravinet"),
  [string]$ConfigDir  = (Join-Path $env:ProgramData "gravinet"),
  [string]$Wintun = "",
  [switch]$NoStart,
  [switch]$NoNpcap,
  [switch]$Uninstall
)
$ErrorActionPreference = "Stop"

$ServiceName = "gravinet"
$DisplayName = "gravinet"
$Description = "[gravinet] full-mesh encrypted overlay VPN daemon"
$Exe    = Join-Path $InstallDir "gravinet.exe"
$Config = Join-Path $ConfigDir  "config.json"
$GoMinMinor  = 21
# GravinetGoRoot is legacy: pre-v232 versions of this script downloaded Go
# directly from go.dev into this folder as their own fallback, rather than
# going through Chocolatey. It's kept only so Add-GravinetGoToPath can still
# find and reuse a Go install left behind by one of those older runs on a
# machine that's now upgrading; nothing in the current script writes here.
$GravinetGoRoot = Join-Path $env:ProgramFiles 'gravinet-go'
$WintunVer   = "0.14.1"
$WintunSha   = "07c256185d6ee3652e09fa55c0b673e2624b565e02c4b9091c79ca7d2f24ef51"
# Npcap enables the web admin's packet-capture tab (Windows has no raw-capture
# syscall of its own; this is the same driver Wireshark installs). Checksum is
# the one Wireshark's own build infrastructure pins for this release - see
# https://dev-libs.wireshark.org/windows/packages/Npcap/ - not npcap.com
# itself, which doesn't publish a checksum page.
$NpcapVer = "1.88"
$NpcapSha = "a2f4ec1e5ea353ff67efd24b2ebf081ba44532410fae8d5e146af0310aa4f56b"

# Require Administrator.
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
         ).IsInRole([Security.Principal.WindowsBuiltinRole]::Administrator)
if (-not $admin) { throw "Run this script from an elevated (Administrator) PowerShell." }

# Get-BinVersion runs "<path> version" and extracts just the version field
# from its "gravinet NNN (commit) os/arch" output (see cmd/gravinet/main.go's
# version subcommand) — the same parse install-linux.sh/install-macos.sh/
# install-freebsd.sh already use for the equivalent check on those platforms.
function Get-BinVersion($path) {
  if (-not $path -or -not (Test-Path $path)) { return "" }
  try {
    $out = (& $path version 2>$null | Out-String)
    if ($out -match 'gravinet\s+(\S+)') { return $Matches[1] }
  } catch {}
  return ""
}

# Get-SourceVersion extracts the version string baked into the bundled source
# (cmd/gravinet/main.go: `version = "NNN"`), so you can see exactly what this
# tree would build before actually building it. Empty if no source tree is
# present (e.g. a binary-only package) or the line can't be found.
function Get-SourceVersion($repo) {
  $mainGo = Join-Path $repo 'cmd\gravinet\main.go'
  if (-not (Test-Path $mainGo)) { return "" }
  $m = Select-String -Path $mainGo -Pattern '^\s*version\s*=\s*"([^"]*)"' | Select-Object -First 1
  if ($m) { return $m.Matches[0].Groups[1].Value }
  return ""
}

# Resolve, cheaply, what would actually be installed if this run proceeds —
# without triggering the (possibly slow) Go bootstrap and build just to find
# out — so the currently-installed-vs-to-be-installed comparison below can
# run, and potentially skip the rest of this script entirely, before any of
# that happens. Mirrors the real resolution order used a few hundred lines
# down (explicit -Bin, then a prebuilt binary beside the script, then build
# from source): if neither of the first two resolves a file, whatever would
# get built from this exact source tree is what Get-SourceVersion reports.
# $Bin is assigned here (not just read) when a sibling prebuilt is found, so
# the later resolution block sees it already set and doesn't re-scan for it.
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
$repo = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
if (-not $Bin) {
  $here = Split-Path -Parent $MyInvocation.MyCommand.Path
  foreach ($c in @((Join-Path $here "gravinet-windows-$arch.exe"), (Join-Path $here "gravinet.exe"))) {
    if (Test-Path $c) { $Bin = $c; break }
  }
}
if ($Bin -and (Test-Path $Bin)) {
  $NewVer = Get-BinVersion $Bin
} else {
  $NewVer = Get-SourceVersion $repo
}
$CurVer = Get-BinVersion $Exe

if (-not $Uninstall) {
  Write-Host "==> currently installed version: $(if ($CurVer) { $CurVer } else { 'none' })"
  Write-Host "==> version to install: $(if ($NewVer) { $NewVer } else { 'unknown' })"
  if ($CurVer -and $NewVer -and $CurVer -eq $NewVer) {
    Write-Host "already up to date (version $CurVer) - skipping install"
    exit 0
  }
}

function Stop-GravinetService {
  $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
  if ($svc -and $svc.Status -ne 'Stopped') {
    Write-Host "==> stopping running service $ServiceName"
    # Stop-Service itself blocks until the SCM reports the service Stopped -
    # it has no timeout parameter of its own. If gravinet.exe is wedged and
    # never acknowledges the stop control, that call (and this whole script)
    # hangs forever, which is exactly what used to force people to kill the
    # process by hand. sc.exe stop only *requests* the stop and returns
    # immediately, so poll for the real outcome ourselves and cap the wait.
    sc.exe stop $ServiceName | Out-Null
    $stopped = $false
    for ($i = 0; $i -lt 40; $i++) {
      if ((Get-Service $ServiceName -ErrorAction SilentlyContinue).Status -eq 'Stopped') { $stopped = $true; break }
      Start-Sleep -Milliseconds 250
    }
    if (-not $stopped) {
      # 10s and it still hasn't stopped - stop being polite. Kill the
      # process directly so the .exe unlocks and the install can proceed
      # instead of hanging indefinitely.
      Write-Host "    service didn't stop within 10s - killing gravinet.exe directly"
      Get-Process -Name gravinet -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
      for ($i = 0; $i -lt 8 -and (Get-Service $ServiceName -ErrorAction SilentlyContinue).Status -ne 'Stopped'; $i++) {
        Start-Sleep -Milliseconds 250
      }
    }
  }
  return [bool]$svc
}

function Remove-GravinetService {
  $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
  if ($svc) {
    Stop-GravinetService | Out-Null
    sc.exe delete $ServiceName | Out-Null
  }
}

if ($Uninstall) {
  Remove-GravinetService
  # See the matching comment in uninstall-windows.ps1: Stop-GravinetService's
  # wait loop above gives up after 5 seconds regardless of outcome, and a
  # still-running gravinet.exe holding its own binary open would otherwise
  # turn the very first Remove-Item below into an unhandled, terminating
  # error - silently, since a script that dies from an elevated relaunch can
  # take its console window with it before there's a chance to read why.
  Get-Process -Name gravinet -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
  Start-Sleep -Milliseconds 250
  Remove-NetFirewallRule -DisplayName $ServiceName -ErrorAction SilentlyContinue
  $filesFailed = $false
  foreach ($f in @("gravinet.exe", "wintun.dll", "wintun-prebuilt-binaries-license.txt", "README.md", "LICENSE", "getting-started.md", "meshping.bat")) {
    $p = Join-Path $InstallDir $f
    if (Test-Path $p) {
      try { Remove-Item $p -Force -ErrorAction Stop }
      catch { Write-Warning "could not remove $p ($($_.Exception.Message))"; $filesFailed = $true }
    }
  }
  if ($filesFailed) {
    Write-Warning "some files in $InstallDir are still in use - close anything using gravinet and re-run this script"
  } else {
    Write-Host "==> removed binary and service (config at $ConfigDir left in place)"
  }
  Write-Host "    Npcap, if installed, is left in place too (other tools like Wireshark may depend on it);"
  Write-Host "    remove it yourself via Windows' Add/Remove Programs if you don't need it."
  return
}

# --- Go toolchain + Wintun bootstrap -------------------------------------------

# Sync-EnvPath re-reads PATH from the registry (Machine, then User) into this
# process's own $env:PATH. A process only sees the PATH it inherited at
# launch; installing something system- or user-wide (Go's own MSI, winget,
# Chocolatey, a previous run of this very script) updates the registry, not
# any PowerShell session that was already open when that happened. Without
# this, Get-GoMinor's very first check can miss a perfectly good, already-
# installed Go for no reason other than stale PATH, and Install-Go proceeds
# to download and install a second one right on top of it.
function Sync-EnvPath {
  $env:PATH = [Environment]::GetEnvironmentVariable('Path','Machine') + ';' +
              [Environment]::GetEnvironmentVariable('Path','User')
}

function Get-GoMinor {
  if (-not (Get-Command go -ErrorAction SilentlyContinue)) { return -1 }
  try {
    if ((& go version) -match 'go(\d+)\.(\d+)') { return [int]$Matches[2] }
  } catch {}
  return -1
}

# Add-GravinetGoToPath puts a *previous* (pre-v232) run's own direct-download
# Go — back when this script fetched the zip from go.dev itself — onto this
# process's PATH if one is still sitting at GravinetGoRoot from before. That
# older code path also persisted itself to the machine PATH in the registry,
# so on any later run Sync-EnvPath alone should already find it; this is only
# a defensive fallback for a machine upgrading from that older script version
# where, for whatever reason, that persistence didn't stick.
function Add-GravinetGoToPath {
  $bin = Join-Path $GravinetGoRoot 'go\bin'
  if ((Test-Path (Join-Path $bin 'go.exe')) -and ($env:PATH -notlike "*$bin*")) {
    $env:PATH = "$bin;$env:PATH"
  }
}

function Test-ChocoPresent {
  return [bool](Get-Command choco -ErrorAction SilentlyContinue)
}

# Install-Choco bootstraps Chocolatey itself using its own official installer
# (see https://community.chocolatey.org/install.ps1 / docs.chocolatey.org
# setup instructions) - the same command Chocolatey's own docs hand out, run
# non-interactively. Idempotent: skipped entirely if choco is already on PATH.
function Install-Choco {
  if (Test-ChocoPresent) { return }
  Write-Host "==> Chocolatey not found; installing it"
  Set-ExecutionPolicy Bypass -Scope Process -Force
  # Chocolatey's installer needs TLS 1.2 to fetch itself; older Windows/.NET
  # defaults don't always negotiate it automatically (see Chocolatey's own
  # install docs), so this is set explicitly rather than left to chance.
  [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.ServicePointManager]::SecurityProtocol -bor 3072
  Invoke-Expression ((New-Object System.Net.WebClient).DownloadString('https://community.chocolatey.org/install.ps1'))
  Sync-EnvPath
  if (-not (Test-ChocoPresent)) {
    # The installer writes choco.exe here and updates the machine PATH itself,
    # but on some hosts that registry write doesn't take effect in *this*
    # process even after Sync-EnvPath - fall back to the well-known fixed path.
    $chocoBin = Join-Path $env:ProgramData 'chocolatey\bin'
    if ((Test-Path (Join-Path $chocoBin 'choco.exe')) -and ($env:PATH -notlike "*$chocoBin*")) {
      $env:PATH = "$chocoBin;$env:PATH"
    }
  }
  if (-not (Test-ChocoPresent)) { throw "Chocolatey install completed but 'choco' is still not on PATH" }
  Write-Host "    Chocolatey installed ($(& choco --version))"
}

function Install-Go {
  Sync-EnvPath
  Add-GravinetGoToPath
  if ((Get-GoMinor) -ge $GoMinMinor) { Write-Host "==> Go $(& go version) already installed, skipping"; return }
  Install-Choco
  Write-Host "==> installing Go via Chocolatey"
  & choco install golang -y --no-progress | Out-Host
  if ($LASTEXITCODE -ne 0) { throw "choco install golang failed (exit $LASTEXITCODE)" }
  Sync-EnvPath
  if ((Get-GoMinor) -lt $GoMinMinor) { throw "could not obtain a Go toolchain (>=1.$GoMinMinor) via Chocolatey" }
}

function Get-Wintun {
  param([string]$Arch)
  $zip = Join-Path $env:TEMP "wintun-$WintunVer.zip"
  if (-not (Test-Path $zip)) {
    Write-Host "==> downloading Wintun $WintunVer"
    Invoke-WebRequest -UseBasicParsing "https://www.wintun.net/builds/wintun-$WintunVer.zip" -OutFile $zip
  }
  $got = (Get-FileHash $zip -Algorithm SHA256).Hash.ToLower()
  if ($got -ne $WintunSha) { Remove-Item $zip -Force; throw "Wintun checksum mismatch" }
  Write-Host "    Wintun $WintunVer checksum verified"
  $ex = Join-Path $env:TEMP "wintun-$WintunVer"
  if (Test-Path $ex) { Remove-Item $ex -Recurse -Force }
  Expand-Archive -Path $zip -DestinationPath $ex -Force
  $script:WintunLicense = Join-Path $ex "wintun\prebuilt-binaries-license.txt"
  return (Join-Path $ex "wintun\bin\$Arch\wintun.dll")
}

# Test-NpcapPresent checks specifically for wpcap.dll directly under System32,
# which is what gravinet (and anything else loading "wpcap.dll" by bare name)
# actually needs - that only exists if Npcap's "WinPcap API-compatible Mode"
# was enabled (the installer's default, and what /winpcap_mode=yes forces in
# Install-Npcap below). A Npcap install with that mode off would still fail
# our capture backend even though Npcap is technically present, so this is a
# more accurate check than looking for Npcap's own install directory.
function Test-NpcapPresent {
  return (Test-Path (Join-Path $env:SystemRoot "System32\wpcap.dll"))
}

# Install-NpcapViaChoco tries Chocolatey's community 'npcap' package before
# Install-Npcap's own direct, version-pinned download further below. Two
# reasons this is a first attempt rather than the primary path, not a full
# replacement for it:
#   - that package is old (mid-0.8x releases last time it was checked) and
#     unlisted/unmaintained - noticeably behind the 1.88 Install-Npcap
#     installs with a pinned checksum, so a choco success here may still
#     mean an outdated driver
#   - same root problem as Install-Npcap below: the free Npcap installer
#     isn't truly silent, and choco is known to sit waiting on that GUI
#     indefinitely (see chocolatey-packages issue #1823 for the same failure
#     mode on the "nmap" package, which bundles Npcap the same way) - exactly
#     the kind of hang this script is trying to avoid, so it isn't left to
#     just happen here unbounded
# so this runs choco as a child process with a hard timeout and, if it's
# still stuck past that, kills the whole process tree (choco plus whatever
# Npcap installer it spawned) rather than hanging this script the same way.
function Install-NpcapViaChoco {
  param([int]$TimeoutSeconds = 90)
  Install-Choco
  $chocoCmd = Get-Command choco -ErrorAction SilentlyContinue
  if (-not $chocoCmd) { Write-Host "    choco still not available; skipping to the direct installer"; return $false }
  Write-Host "==> trying Npcap via Chocolatey first (up to ${TimeoutSeconds}s, then falls back to the direct installer)"
  # -WindowStyle Hidden: this attempt is meant to succeed or fail on its own
  # within the timeout, not wait on someone noticing and clicking a wizard -
  # that interactive path is what the fallback below is for, visibly.
  $p = Start-Process -FilePath $chocoCmd.Source -ArgumentList @('install', 'npcap', '-y', '--no-progress') `
                      -PassThru -WindowStyle Hidden
  if (-not $p.WaitForExit($TimeoutSeconds * 1000)) {
    Write-Host "    choco install npcap didn't finish within ${TimeoutSeconds}s (likely stuck on Npcap's own install wizard) - stopping it and falling back"
    Get-CimInstance Win32_Process -Filter "ParentProcessId=$($p.Id)" -ErrorAction SilentlyContinue |
      ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
    Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
    # Belt-and-suspenders in case the Npcap installer ended up reparented
    # rather than staying a direct child of the choco process by now.
    Get-Process -Name 'npcap*' -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    return $false
  }
  if ($p.ExitCode -ne 0) { Write-Host "    choco install npcap exited with code $($p.ExitCode); falling back"; return $false }
  if (-not (Test-NpcapPresent)) { Write-Host "    choco reported success but wpcap.dll still isn't present; falling back"; return $false }
  Write-Host "    Npcap installed via Chocolatey"
  return $true
}

# Set-FirewallRule opens an inbound Windows Defender Firewall allow rule scoped
# to gravinet.exe specifically (a Program rule), rather than opening ports
# globally. This matters because a Windows service has no desktop to show
# Firewall's "allow this app" prompt on - that prompt only ever appears for an
# interactively logged-in user - so without an explicit rule here, gravinet's
# underlay socket would come up fine but every unsolicited inbound packet
# (i.e. every peer trying to reach this node first) gets silently dropped:
# the daemon and TUN interfaces look completely healthy, there's just no
# working inbound path, so the mesh can end up with zero peers. Scoping to the
# program rather than specific ports also means it doesn't need to track
# gravinet's primary/fallback UDP ports, the TCP fallback port, or any extra
# configured ports individually - whichever ports gravinet actually binds,
# this rule already covers them. -Profile Any because this is a VPN meant to
# work from arbitrary networks (public wifi included), not just trusted ones.
function Set-FirewallRule {
  Get-NetFirewallRule -DisplayName $ServiceName -ErrorAction SilentlyContinue | Remove-NetFirewallRule -ErrorAction SilentlyContinue
  New-NetFirewallRule -DisplayName $ServiceName -Direction Inbound -Program $Exe `
    -Action Allow -Profile Any | Out-Null
}

# Enable-IcmpEcho turns on Windows' own built-in "File and Printer Sharing
# (Echo Request)" inbound rules for ICMPv4/ICMPv6 - a completely separate
# firewall surface from the per-program rule above. The web admin's Latency
# tab (handleLocalLatency/pingRTT in internal/webadmin/sysinfo.go) measures
# mesh RTT by shelling out to the OS's own ping against each peer's overlay
# address; ICMP Echo Request is answered by the Windows kernel itself, not by
# gravinet.exe, so Set-FirewallRule's program-scoped allow rule has no effect
# on it. Without this, a host can be otherwise perfectly reachable (SSH, the
# web admin, the mesh data path all working) and still show up as "no reply"
# in every peer's Latency tab specifically, since ping is silently dropped
# before it even reaches the interface. These rules already exist on Windows,
# just disabled by default on many hosts/profiles, so this only flips them on
# rather than creating new ones - and only on install: uninstalling gravinet
# leaves them as found, since turning off ping host-wide isn't something
# removing gravinet should do as a side effect. Best-effort and non-fatal:
# the mesh itself works without this, only that one diagnostics tab doesn't.
function Enable-IcmpEcho {
  foreach ($name in @('FPS-ICMP4-ERQ-In', 'FPS-ICMP6-ERQ-In')) {
    $rule = Get-NetFirewallRule -Name $name -ErrorAction SilentlyContinue
    if ($rule) { Set-NetFirewallRule -Name $name -Enabled True -Profile Any }
  }
}

# Install-Npcap downloads, checksum-verifies, and launches Npcap's own
# installer - the driver Wireshark also installs, since Windows has no
# raw-capture syscall of its own. NOTE: the free Npcap edition does not
# support fully silent installation - per Npcap's own guide, the /S flag "is
# available only with Npcap OEM" (the paid, commercially-licensed edition).
# Several older third-party scripts pass /S anyway, but that's not backed by
# Npcap's official docs, and Nmap Project's own recommendation to other
# software authors is to have the *user* run the free installer rather than
# try to fully suppress it. So this launches the real (free) installer GUI -
# still pre-filling its checkboxes via command-line flags, which the guide
# confirms works in both editions - and waits for the user to click through
# it. Best-effort and non-fatal either way: gravinet's VPN function doesn't
# depend on Npcap, only the web admin's packet-capture tab does.
function Install-Npcap {
  $exeFile = Join-Path $env:TEMP "npcap-$NpcapVer.exe"
  if (-not (Test-Path $exeFile)) {
    Write-Host "==> downloading Npcap $NpcapVer (for packet capture in the web admin)"
    Invoke-WebRequest -UseBasicParsing "https://npcap.com/dist/npcap-$NpcapVer.exe" -OutFile $exeFile
  }
  $got = (Get-FileHash $exeFile -Algorithm SHA256).Hash.ToLower()
  if ($got -ne $NpcapSha) { Remove-Item $exeFile -Force; throw "Npcap checksum mismatch" }
  Write-Host "    Npcap $NpcapVer checksum verified"
  Write-Host "    launching the Npcap installer - the free edition requires clicking through its"
  Write-Host "    own setup wizard (only the paid Npcap OEM license supports a fully silent"
  Write-Host "    install); accept its license to enable packet capture in the web admin"
  # winpcap_mode=yes is required for gravinet's plain "wpcap.dll" load to find
  # it; npf_startup=yes starts the capture driver at boot rather than only on
  # first use; admin_only=no matches gravinet's own model (the web admin is
  # already behind its own auth, same as the rest of the panel). These are
  # checkbox pre-fills, not full silence, so they apply in GUI mode too.
  $installArgs = "/winpcap_mode=yes /npf_startup=yes /admin_only=no /loopback_support=yes " +
                 "/dot11_support=no /vlan_support=no /dlt_null=no"
  $p = Start-Process -FilePath $exeFile -ArgumentList $installArgs -Wait -PassThru
  if ($p.ExitCode -eq 1) { Write-Host "    Npcap installation was cancelled"; return }
  if ($p.ExitCode -ne 0) { throw "Npcap installer exited with code $($p.ExitCode)" }
  if (-not (Test-NpcapPresent)) { throw "Npcap installer reported success but wpcap.dll still isn't present" }
}

# Resolve the binary: explicit -Bin, else a prebuilt release binary beside the
# script, else build it from the bundled source.
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
if (-not $Bin) {
  $here = Split-Path -Parent $MyInvocation.MyCommand.Path
  foreach ($c in @((Join-Path $here "gravinet-windows-$arch.exe"), (Join-Path $here "gravinet.exe"))) {
    if (Test-Path $c) { $Bin = $c; break }
  }
}
if (-not $Bin -or -not (Test-Path $Bin)) {
  $repo = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
  if (-not (Test-Path (Join-Path $repo 'go.mod'))) { throw "No prebuilt binary and no source tree at $repo" }
  Install-Go
  Write-Host "==> building gravinet from source with $((& go version))"
  $out = Join-Path ([IO.Path]::GetTempPath()) ("gravinet-build-" + [guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Force -Path $out | Out-Null
  $built = Join-Path $out 'gravinet.exe'
  $env:CGO_ENABLED = '0'; $env:GOTOOLCHAIN = 'auto'
  Push-Location $repo
  try {
    & go build -buildvcs=false -trimpath -ldflags "-s -w" -o $built ./cmd/gravinet
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
  } finally { Pop-Location }
  $Bin = $built
}

# Fetch Wintun unless the caller already supplied one. This runs regardless of
# whether $Bin was just built or given via -Bin: a release binary embeds the
# real driver and never even looks at a side-by-side wintun.dll (see
# materializeWintun's "MZ" magic-byte check in internal/tun/tun_windows.go),
# so staging one is a harmless no-op there - but for a non-release/placeholder
# binary it's required, and there's no reliable way to tell the two apart
# from out here without just trying.
if (-not $Wintun) {
  try { $Wintun = Get-Wintun -Arch $arch }
  catch { Write-Host "warning: could not fetch Wintun ($($_.Exception.Message)); place wintun.dll beside the exe manually" }
}

if (-not $NoNpcap) {
  if (Test-NpcapPresent) {
    Write-Host "==> Npcap already present; packet capture in the web admin is available"
  } else {
    $npcapInstalled = $false
    try { $npcapInstalled = Install-NpcapViaChoco }
    catch { Write-Host "    Npcap via Chocolatey failed ($($_.Exception.Message)); falling back to the direct installer" }
    if (-not $npcapInstalled) {
      try { Install-Npcap }
      catch { Write-Host "warning: Npcap install failed ($($_.Exception.Message)); packet capture in the web admin will be unavailable until it's installed manually from https://npcap.com" }
    }
  }
}

Write-Host "==> allowing $Exe through Windows Defender Firewall (inbound, all profiles)"
try { Set-FirewallRule }
catch { Write-Host "warning: could not add a firewall rule ($($_.Exception.Message)); peers may be unable to reach this node until one is added manually" }

Write-Host "==> enabling inbound ICMP echo replies (so mesh peers can ping this host for the Latency tab)"
try { Enable-IcmpEcho }
catch { Write-Host "warning: could not enable ICMP echo reply rules ($($_.Exception.Message)); this host may show as unreachable in peers' Latency tab even though the mesh itself works fine" }

# Everything above this point (downloads, Npcap, the firewall rule) is
# deliberately ordered to finish before the running service ever stops.
# Npcap's free edition has no silent-install flag and launches a real GUI the
# person running this script has to click through (see Install-Npcap) — if
# that ran after the stop below on a machine being administered *through*
# gravinet itself, the dialog would appear on a session that no longer
# exists, and Start-Process -Wait would hang forever waiting for a click
# nobody can make. Putting it here means it's still interactive-safe even in
# that exact case. The remaining steps (copying the new binary in and
# restarting the service) can't be moved the same way — the running
# process's .exe and wintun.dll are locked until it actually stops — so this
# is close to the minimum unavoidable interruption, not just an ordering
# preference.
#
# If you're running this over a remote session whose only path to this
# machine is gravinet itself (RDP/SSH tunneled through the mesh, or this
# script fetched via a gravinet-managed connection), the next step will cut
# that session for a few seconds while the service restarts. It should
# reconnect on its own once the new version is back up — but if you have no
# other way to reach this machine (console access, a hypervisor/provider
# out-of-band console, a second network path), consider setting one up now
# before continuing, in case the restart doesn't come back cleanly.
Write-Host "==> stopping gravinet will briefly interrupt any connection riding over it (e.g. a remote session tunneled through the mesh) - it restarts automatically in a moment"

# Upgrade-in-place: stop a running instance BEFORE overwriting its locked .exe.
$wasRunning = $false
$existing = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($existing -and $existing.Status -eq 'Running') { $wasRunning = $true }
Stop-GravinetService | Out-Null

Write-Host "==> installing $Bin -> $Exe"
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Copy-Item $Bin $Exe -Force

# Install meshping.bat (the Windows equivalent of the POSIX meshping
# diagnostic script) beside the exe, the same way pkgman rides along with
# the binary on the other platforms.
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$meshpingSrc = Join-Path $here "meshping.bat"
if (Test-Path $meshpingSrc) {
  Write-Host "==> installing meshping.bat -> $InstallDir"
  Copy-Item $meshpingSrc (Join-Path $InstallDir "meshping.bat") -Force
} else {
  Write-Host "    note: meshping.bat not found in the package; skipping"
}

# Install the README, LICENSE, and getting-started.md beside the binary
# so the web admin's Readme, License, and Getting Started pages can show
# them.
foreach ($doc in @("README.md", "LICENSE", "getting-started.md")) {
  $src = ""
  foreach ($c in @((Join-Path $here $doc),
                   (Join-Path (Split-Path -Parent $here) $doc))) {
    if (Test-Path $c) { $src = $c; break }
  }
  if ($src) {
    $dst = Join-Path $InstallDir $doc
    # Copy-Item throws "cannot overwrite the item with itself" if $src and
    # $dst are the same file - which happens whenever this script itself is
    # run directly from inside $InstallDir. That's not an error - the doc is
    # already exactly where it needs to be - so treat it the same as
    # "already done".
    if ((Resolve-Path $src).Path -eq (Resolve-Path -ErrorAction SilentlyContinue $dst).Path) {
      Write-Host "    $doc already in place"
    } else {
      Write-Host "==> installing $doc beside the binary"
      Copy-Item $src $dst -Force
    }
  } else {
    Write-Host "    note: $doc not found in the package; its web admin page will be empty"
  }
}

if ($Wintun) {
  Write-Host "==> staging wintun.dll beside the binary"
  Copy-Item $Wintun (Join-Path $InstallDir "wintun.dll") -Force
  # Keep the Wintun prebuilt-binaries license alongside the DLL, as its terms
  # require. Prefer the copy from the downloaded zip, else the bundled one.
  $lic = $script:WintunLicense
  if (-not ($lic -and (Test-Path $lic))) {
    $repoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
    $lic = Join-Path $repoRoot 'third_party\wintun\prebuilt-binaries-license.txt'
  }
  if ($lic -and (Test-Path $lic)) {
    Copy-Item $lic (Join-Path $InstallDir "wintun-prebuilt-binaries-license.txt") -Force
    Write-Host "    staged Wintun license"
  }
}

Write-Host "==> config $Config"
New-Item -ItemType Directory -Force -Path $ConfigDir | Out-Null
if (-not (Test-Path $Config)) {
  & $Exe run -config $Config -init | Out-Null
  Write-Host "    scaffolded a default config (no networks yet)"
} else {
  # Say what's actually in it, not just that it exists - "keeping existing
  # config" alone reads the same whether that's expected (an upgrade) or a
  # surprise (e.g. -Purge on a prior uninstall silently failed to clear it,
  # so networks from before it "should" be a fresh install reappear in the
  # web admin with no obvious explanation why).
  $netCount = -1
  try { $netCount = @((Get-Content $Config -Raw | ConvertFrom-Json).networks).Count } catch {}
  if ($netCount -gt 0) {
    Write-Host "    keeping existing config ($netCount network(s) already defined)"
  } else {
    Write-Host "    keeping existing config"
  }
}

# Re-register the service (clears any stale binPath from a prior version).
# The daemon talks to the SCM itself (StartServiceCtrlDispatcher).
Remove-GravinetService
$binPath = "`"$Exe`" run -config `"$Config`""
Write-Host "==> creating service $ServiceName"
New-Service -Name $ServiceName -BinaryPathName $binPath -DisplayName $DisplayName `
            -Description $Description -StartupType Automatic | Out-Null

# Recovery: restart the service on the first, second, and subsequent failures,
# with a short escalating backoff, and reset the failure count after a day of
# stability. The failure-actions flag makes these fire on a non-zero-exit stop
# too, which is how gravinet comes back automatically after a settings change
# that needs a restart (it reports a failure exit rather than deadlocking on a
# self-restart). The daemon also re-applies this at startup, so it's set even
# on an upgrade that didn't rerun this installer.
Write-Host "==> configuring service recovery (restart on failure)"
sc.exe failure $ServiceName reset= 86400 actions= restart/5000/restart/10000/restart/30000 | Out-Null
sc.exe failureflag $ServiceName 1 | Out-Null

if (-not $NoStart) {
  Start-Service $ServiceName
  if ($wasRunning) { Write-Host "==> restarted $ServiceName" } else { Write-Host "==> started $ServiceName" }
}

@"

gravinet installed and running ($ServiceName).

Web admin:  https://127.0.0.1:8443   (self-signed TLS - accept the warning)
            log in with your Windows account
            Remote box? tunnel it:  ssh -L 8443:127.0.0.1:8443 <user>@<host>

Note: Windows needs the Wintun driver at runtime. A release build embeds it; a
plain build looks for wintun.dll beside the .exe (pass -Wintun to stage one).

Packet capture (web admin, Info -> Capture): $(if (Test-NpcapPresent) { "available (Npcap present)" } else { "unavailable - install Npcap from https://npcap.com, or re-run this script" })

Inbound firewall rule for ${Exe}: added (all profiles) - peers should be able to
reach this node. If they still can't, check any router/NAT/ISP-level blocking
between them and this host; that's outside gravinet's or Windows Firewall's
control.

ICMP echo replies (ping, used by peers' Latency tab): enabled inbound - this
host should show up with a real RTT there instead of "no reply".

To join a mesh:
  1. Generate keys:    & '$Exe' genkey -n 3
  2. Edit the config:  $Config   (set keys, subnet/seeds, enable the network)
  3. Apply changes:    Restart-Service $ServiceName
  4. Check status:     Get-Service $ServiceName
"@ | Write-Host
