# RTMP Media Server - installer / updater / uninstaller for Windows
#   PowerShell as Administrator:
#     irm https://raw.githubusercontent.com/maxvideostar/rtmp-media-server/main/install.ps1 | iex
#   Or from the zip: double-click install.bat
#   Uninstall: install.bat uninstall   (or: .\install.ps1 uninstall)
# NOTE: keep this file pure ASCII without BOM (needed for "irm | iex" and Windows PowerShell 5.1)
$Action = if ($args.Count -gt 0) { [string]$args[0] } else { "install" }
$ErrorActionPreference = "Stop"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$Repo = "maxvideostar/rtmp-media-server"
$Dir  = "C:\rtmp-media-server"
$Exe  = Join-Path $Dir "rtmp-media-server.exe"
$Cfg  = Join-Path $Dir "config.json"
$Log  = Join-Path $Dir "rtmp-media-server.log"
$Svc  = "rtmp-media-server"

function Say($m)  { Write-Host "==> $m" -ForegroundColor Green }
function Warn($m) { Write-Host "!!  $m" -ForegroundColor Yellow }

$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $admin) {
    if ($PSCommandPath) {
        Start-Process powershell -Verb RunAs -ArgumentList "-NoProfile -ExecutionPolicy Bypass -File `"$PSCommandPath`" $Action"
        exit
    }
    Write-Host "Ruleaza PowerShell ca Administrator." -ForegroundColor Red
    return
}

if ($Action -eq "uninstall") {
    if (Test-Path $Exe) { & $Exe stop 2>$null | Out-Null; & $Exe uninstall }
    netsh advfirewall firewall delete rule name="RTMP Media Server" | Out-Null
    Warn "Config si inregistrari au ramas in $Dir (sterge manual daca vrei)."
    return
}

function PortOwner([int]$port) {
    $c = Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
    if (-not $c) { return $null }
    $p = Get-Process -Id $c.OwningProcess -ErrorAction SilentlyContinue
    if ($p) { return "$($p.ProcessName) (PID $($p.Id))" } else { return "PID $($c.OwningProcess)" }
}
function FreePort([int]$start) {
    $p = $start
    while (PortOwner $p) { $p++ }
    return $p
}

New-Item -ItemType Directory -Force -Path $Dir | Out-Null
$existing = Get-Service -Name $Svc -ErrorAction SilentlyContinue
if ($existing) {
    Say "Serviciu existent - opresc pentru actualizare"
    Stop-Service $Svc -Force -ErrorAction SilentlyContinue
    Start-Sleep 2
}

# exe: local (from zip) or download from GitHub Releases
$local = $null
if ($PSScriptRoot) { $local = Join-Path $PSScriptRoot "rtmp-media-server.exe" }
if ($local -and (Test-Path $local) -and ((Resolve-Path $local).Path -ne $Exe)) {
    Say "Copiez $local"
    Copy-Item $local $Exe -Force
} elseif (-not ($local -and (Test-Path $local))) {
    $url = "https://github.com/$Repo/releases/latest/download/rtmp-media-server-windows-amd64.exe"
    Say "Descarc $url"
    $ProgressPreference = "SilentlyContinue"
    Invoke-WebRequest -Uri $url -OutFile "$Exe.new" -UseBasicParsing
    Move-Item "$Exe.new" $Exe -Force
}
Unblock-File $Exe -ErrorAction SilentlyContinue

# FFmpeg for pull (HLS/SRT/RTSP/UDP) - only if missing
$ff = Join-Path $Dir "ffmpeg.exe"
if (-not (Test-Path $ff) -and -not (Get-Command ffmpeg -ErrorAction SilentlyContinue)) {
    try {
        Say "Descarc FFmpeg (pentru pull)..."
        $ProgressPreference = "SilentlyContinue"
        $zip = Join-Path $env:TEMP "ffmpeg-rms.zip"
        Invoke-WebRequest -Uri "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-gpl.zip" -OutFile $zip -UseBasicParsing
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        $z = [IO.Compression.ZipFile]::OpenRead($zip)
        $e = $z.Entries | Where-Object { $_.FullName -like "*/bin/ffmpeg.exe" } | Select-Object -First 1
        [IO.Compression.ZipFileExtensions]::ExtractToFile($e, $ff, $true)
        $z.Dispose(); Remove-Item $zip -Force
        Say "FFmpeg instalat: $ff"
    } catch { Warn "FFmpeg nu s-a putut descarca ($_). Pull-ul rtmp:// merge oricum." }
}

# config + port check (a busy port is moved to the next free one)
if (-not (Test-Path $Cfg)) { & $Exe passwd admin | Out-Null }
$json = Get-Content $Cfg -Raw | ConvertFrom-Json
$changed = $false
foreach ($k in @("rtmp_port", "http_port")) {
    $port = [int]$json.$k
    $owner = PortOwner $port
    if ($owner -and $owner -notlike "rtmp-media-server*") {
        $new = FreePort ($port + 1)
        if ($k -eq "http_port" -and $new -eq [int]$json.rtmp_port) { $new = FreePort ($new + 1) }
        Warn "Portul $port ($k) e ocupat de $owner -> folosesc $new"
        $json.$k = $new
        $changed = $true
    }
}
if ($changed) {
    $txt = $json | ConvertTo-Json -Depth 10
    [IO.File]::WriteAllText($Cfg, $txt, (New-Object Text.UTF8Encoding($false)))
}
$rtmp = [int]$json.rtmp_port; $http = [int]$json.http_port

if (-not $existing) {
    & $Exe install
    sc.exe failure $Svc reset= 86400 actions= restart/5000/restart/5000/restart/5000 | Out-Null
}
try { Start-Service $Svc } catch { Warn $_ }
Start-Sleep 3
if ((Get-Service $Svc).Status -ne "Running") {
    Write-Host ""
    Warn "Serviciul nu a pornit. Ultimele linii din $Log :"
    if (Test-Path $Log) { Get-Content $Log -Tail 15 }
    Write-Host ""
    Warn "Test in consola (Ctrl+C pentru oprire):  & '$Exe'"
    if ($PSCommandPath) { Read-Host "Apasa Enter pentru a inchide" | Out-Null }
    return
}

netsh advfirewall firewall delete rule name="RTMP Media Server" | Out-Null
netsh advfirewall firewall add rule name="RTMP Media Server" dir=in action=allow protocol=TCP localport="$rtmp,$http" | Out-Null
Say "Firewall: deschis TCP $rtmp, $http"

$ip = (Get-NetIPAddress -AddressFamily IPv4 | Where-Object { $_.IPAddress -notlike "127.*" -and $_.IPAddress -notlike "169.254.*" } | Select-Object -First 1).IPAddress
Write-Host ""
Say "RTMP Media Server $(& $Exe version) ruleaza ca serviciu Windows"
Write-Host "   Panou:     http://${ip}:$http/panel/   (prima data: admin / admin)" -ForegroundColor Cyan
Write-Host "   Publicare: rtmp://${ip}:$rtmp/live   cheie: <nume>?key=<cheie>"
Write-Host "   Config:    $Cfg    Log: $Log"
Start-Process "http://localhost:$http/panel/"
if ($PSCommandPath) { Read-Host "Gata. Apasa Enter pentru a inchide" | Out-Null }
