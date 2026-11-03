# RTMP Media Server

Server media tip Wowza, un singur executabil pentru **Ubuntu** și **Windows**, cu panou web.

- **Ingest RTMP** (OBS, vMix, FFmpeg, encodere hardware) — H.264/AAC, plus HEVC/AV1/VP9 prin Enhanced RTMP
- **Ieșiri**: HLS, HTTP-FLV, HTTP-TS (pentru IPTV/VLC), redare RTMP
- **Restream / push** automat către YouTube, alte servere RTMP etc., cu reconectare
- **DVR**: înregistrare pe disc (`.ts`, rotire la N minute, ștergere automată după X zile)
- **Securitate**: cheie de publicare per stream, tokenuri de redare (pe stream, cu expirare), login pe panou
- **Panou web**: streamuri live (bitrate, fps, uptime, clienți cu IP și viteză), preview HLS, kick publisher/client, configurare streamuri/tokenuri, înregistrări, jurnal
- Rulează ca **serviciu** (systemd / Windows Service), repornire automată

## Instalare

### Ubuntu / Debian
```bash
curl -fsSL https://raw.githubusercontent.com/maxvideostar/rtmp-media-server/main/install.sh | sudo bash
```
Aceeași comandă face și **update**. Dezinstalare: `... | sudo bash -s uninstall`.
Instalează în `/opt/rtmp-media-server`, serviciu `rtmp-media-server`, deschide porturile în UFW.

### Windows
PowerShell **ca Administrator**:
```powershell
irm https://raw.githubusercontent.com/maxvideostar/rtmp-media-server/main/install.ps1 | iex
```
Sau descarcă `rtmp-media-server-windows-amd64.zip` din [Releases](https://github.com/maxvideostar/rtmp-media-server/releases), dezarhivează și dublu-click pe `install.bat`.
Instalează în `C:\rtmp-media-server`, serviciu Windows, regulă de firewall.

Panou: `http://IP:8080/panel/` — prima autentificare **admin / admin** (schimb-o din Setări).

## Utilizare

| Ce | URL |
|---|---|
| Publicare OBS | Server `rtmp://IP:1935/live` · Cheie `tv1?key=CHEIA` |
| Publicare FFmpeg | `rtmp://IP:1935/live/tv1?key=CHEIA` |
| HLS | `http://IP:8080/live/tv1/index.m3u8` sau `http://IP:8080/live/tv1.m3u8` |
| HTTP-FLV | `http://IP:8080/live/tv1.flv` |
| HTTP-TS | `http://IP:8080/live/tv1.ts` |
| RTMP redare | `rtmp://IP:1935/live/tv1` |

Cu „Cere token la redare” activ, adaugă `?token=TOKEN` la URL-urile de redare.

Exemplu publicare FFmpeg (relay dintr-o sursă existentă):
```bash
ffmpeg -re -i "http://sursa/canal.ts" -c copy -f flv "rtmp://IP:1935/live/tv1?key=CHEIA"
```

## Comenzi
```
rtmp-media-server                      pornire în consolă
rtmp-media-server install|uninstall    serviciu
rtmp-media-server start|stop|restart|status
rtmp-media-server passwd PAROLA        resetare parolă admin
rtmp-media-server -config cale.json    config alternativ
```
Fișiere lângă executabil: `config.json`, `rtmp-media-server.log`, `recordings/`.

## Note
- HLS / HTTP-TS / DVR `.ts` necesită H.264 + AAC. Pentru HEVC/AV1 folosește HTTP-FLV sau RTMP; DVR scrie atunci `.flv`.
- Restream doar `rtmp://` (nu `rtmps://`). Modificările de push/DVR se aplică la următoarea publicare a streamului.
- Schimbarea porturilor necesită repornirea serviciului.
- Fără transcodare: fluxul e livrat exact cum vine (zero CPU).

## Compilare din sursă
```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o rtmp-media-server .
CGO_ENABLED=0 GOOS=windows go build -trimpath -ldflags "-s -w" -o rtmp-media-server.exe .
```
