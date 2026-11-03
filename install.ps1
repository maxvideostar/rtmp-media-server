# RTMP Media Server - installer / updater / uninstaller pentru Windows
#   Din PowerShell ca Administrator:
#     irm https://raw.githubusercontent.com/maxvideostar/rtmp-media-server/main/install.ps1 | iex
#   Sau din arhiva zip: dublu-click pe install.bat
#   Dezinstalare: install.bat uninstall   (sau: .\install.ps1 uninstall)
$Action = if ($args.Count -gt 0) { [string]$args[0] } else { "install" }
$ErrorActionPreference = "Stop"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$Repo = "maxvideostar/rtmp-media-server"
$Dir  = "C:\rtmp-media-server"
$Exe  = Join-Path $Dir "rtmp-media-server.exe"
$Svc  = "rtmp-media-server"

function Say($m)  { Write-Host "==> $m" -ForegroundColor Green }
function Warn($m) { Write-Host "!!  $m" -ForegroundColor Yellow }

$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $admin) {
    if ($PSCommandPath) {
        Start-Process powershell -Verb RunAs -ArgumentList "-NoProfile -ExecutionPolicy Bypass -File `"$PSCommandPath`" $Action"
        exit
    }
    Write-Host "Rulează PowerShell ca Administrator." -ForegroundColor Red
    return
}

if ($Action -eq "uninstall") {
    if (Test-Path $Exe) { & $Exe stop 2>$null | Out-Null; & $Exe uninstall }
    netsh advfirewall firewall delete rule name="RTMP Media Server" | Out-Null
    Warn "Config și înregistrări au rămas în $Dir (șterge manual dacă vrei)."
    return
}

New-Item -ItemType Directory -Force -Path $Dir | Out-Null
$existing = Get-Service -Name $Svc -ErrorAction SilentlyContinue
if ($existing) {
    Say "Serviciu existent - opresc pentru actualizare"
    Stop-Service $Svc -Force -ErrorAction SilentlyContinue
    Start-Sleep 2
}

# exe local (din zip) sau descărcare din GitHub Releases
$local = $null
if ($PSScriptRoot) { $local = Join-Path $PSScriptRoot "rtmp-media-server.exe" }
if ($local -and (Test-Path $local) -and ((Resolve-Path $local).Path -ne $Exe)) {
    Say "Copiez $local"
    Copy-Item $local $Exe -Force
} elseif (-not ($local -and (Resolve-Path $local -ErrorAction SilentlyContinue).Path -eq $Exe)) {
    $url = "https://github.com/$Repo/releases/latest/download/rtmp-media-server-windows-amd64.exe"
    Say "Descarc $url"
    $ProgressPreference = "SilentlyContinue"
    Invoke-WebRequest -Uri $url -OutFile "$Exe.new" -UseBasicParsing
    Move-Item "$Exe.new" $Exe -Force
}

if (-not $existing) {
    & $Exe install
    sc.exe failure $Svc reset= 86400 actions= restart/5000/restart/5000/restart/5000 | Out-Null
}
Start-Service $Svc
Start-Sleep 2
if ((Get-Service $Svc).Status -ne "Running") { throw "Serviciul nu a pornit - vezi $Dir\rtmp-media-server.log" }

$cfg  = Get-Content (Join-Path $Dir "config.json") -Raw | ConvertFrom-Json
$rtmp = $cfg.rtmp_port; $http = $cfg.http_port
netsh advfirewall firewall delete rule name="RTMP Media Server" | Out-Null
netsh advfirewall firewall add rule name="RTMP Media Server" dir=in action=allow protocol=TCP localport="$rtmp,$http" | Out-Null
Say "Firewall: deschis TCP $rtmp, $http"

$ip = (Get-NetIPAddress -AddressFamily IPv4 | Where-Object { $_.IPAddress -notlike "127.*" -and $_.IPAddress -notlike "169.254.*" } | Select-Object -First 1).IPAddress
Write-Host ""
Say "RTMP Media Server $(& $Exe version) rulează ca serviciu Windows"
Write-Host "   Panou:     http://${ip}:$http/panel/   (prima dată: admin / admin)" -ForegroundColor Cyan
Write-Host "   Publicare: rtmp://${ip}:$rtmp/live   cheie: <nume>?key=<cheie>"
Write-Host "   Config:    $Dir\config.json    Log: $Dir\rtmp-media-server.log"
Start-Process "http://localhost:$http/panel/"

if ($PSCommandPath) { Read-Host "Gata. Apasă Enter pentru a închide" | Out-Null }
