#!/usr/bin/env bash
# RTMP Media Server — installer / updater / uninstaller pentru Ubuntu/Debian
#   instalare / update:  curl -fsSL https://raw.githubusercontent.com/maxvideostar/rtmp-media-server/main/install.sh | sudo bash
#   dezinstalare:        curl -fsSL https://raw.githubusercontent.com/maxvideostar/rtmp-media-server/main/install.sh | sudo bash -s uninstall
set -euo pipefail

REPO="maxvideostar/rtmp-media-server"
DIR="/opt/rtmp-media-server"
BIN="$DIR/rtmp-media-server"
SVC="rtmp-media-server"

G='\033[1;32m'; Y='\033[1;33m'; R='\033[1;31m'; N='\033[0m'
say(){ echo -e "${G}==>${N} $*"; }
warn(){ echo -e "${Y}!!${N} $*"; }
die(){ echo -e "${R}EROARE:${N} $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "rulează cu sudo / root"

if [ "${1:-}" = "uninstall" ]; then
  say "Dezinstalare serviciu..."
  [ -x "$BIN" ] && { "$BIN" stop >/dev/null 2>&1 || true; "$BIN" uninstall >/dev/null 2>&1 || true; }
  systemctl daemon-reload || true
  warn "Fișierele (config + înregistrări) au rămas în $DIR. Șterge manual: rm -rf $DIR"
  exit 0
fi

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "arhitectură nesuportată: $(uname -m)" ;;
esac

command -v curl >/dev/null || { apt-get update -qq && apt-get install -y -qq curl; }
if ! command -v ffmpeg >/dev/null; then
  say "Instalez FFmpeg (pentru pull HLS/SRT/RTSP/UDP)..."
  apt-get update -qq && apt-get install -y -qq ffmpeg || warn "FFmpeg nu s-a putut instala — pull-ul rtmp:// merge oricum"
fi

mkdir -p "$DIR"
TMP="$(mktemp)"
URL="https://github.com/$REPO/releases/latest/download/rtmp-media-server-linux-$ARCH"
say "Descarc $URL"
if ! curl -fSL --retry 3 -o "$TMP" "$URL"; then
  warn "Release indisponibil — compilez din sursă"
  command -v git >/dev/null || apt-get install -y -qq git
  if ! command -v go >/dev/null; then
    say "Instalez Go..."
    GOV="1.24.7"
    curl -fsSL "https://go.dev/dl/go${GOV}.linux-${ARCH}.tar.gz" | tar -C /usr/local -xz
    export PATH="/usr/local/go/bin:$PATH"
  fi
  SRC="$(mktemp -d)"
  git clone --depth 1 "https://github.com/$REPO" "$SRC"
  (cd "$SRC" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$TMP" .)
  rm -rf "$SRC"
fi
chmod +x "$TMP"

UPDATE=0
if systemctl list-unit-files 2>/dev/null | grep -q "^$SVC.service"; then
  UPDATE=1
  say "Serviciu existent — actualizez"
  systemctl stop "$SVC" || true
fi
mv -f "$TMP" "$BIN"

if [ $UPDATE -eq 0 ]; then
  "$BIN" install
fi
systemctl daemon-reload
systemctl enable "$SVC" >/dev/null 2>&1 || true
systemctl restart "$SVC"
sleep 2
systemctl is-active --quiet "$SVC" || { journalctl -u "$SVC" -n 30 --no-pager; die "serviciul nu a pornit"; }

# porturi din config
RTMP=$(grep -o '"rtmp_port": *[0-9]*' "$DIR/config.json" | grep -o '[0-9]*$' || echo 1935)
HTTP=$(grep -o '"http_port": *[0-9]*' "$DIR/config.json" | grep -o '[0-9]*$' || echo 8080)
if command -v ufw >/dev/null && ufw status | grep -q "Status: active"; then
  ufw allow "$RTMP"/tcp >/dev/null; ufw allow "$HTTP"/tcp >/dev/null
  say "UFW: deschis $RTMP/tcp și $HTTP/tcp"
fi

IP=$(curl -fs4 --max-time 3 https://api.ipify.org 2>/dev/null || hostname -I | awk '{print $1}')
echo
say "RTMP Media Server $("$BIN" version) rulează"
echo -e "   Panou:     ${G}http://$IP:$HTTP/panel/${N}   (prima dată: admin / admin)"
echo -e "   Publicare: rtmp://$IP:$RTMP/live   cheie: <nume>?key=<cheie>"
echo -e "   Config:    $DIR/config.json    Log: $DIR/rtmp-media-server.log"
echo -e "   Comenzi:   systemctl status|restart $SVC   ·   $BIN passwd <parola>"
