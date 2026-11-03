package main

import (
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/datarhei/joy4/av"
	"github.com/datarhei/joy4/format/flv"
	"github.com/datarhei/joy4/format/ts"
)

//go:embed web
var webFS embed.FS

var (
	sessMu   sync.Mutex
	sessions = map[string]time.Time{}
	startAt  = time.Now()
)

const sessCookie = "rms_session"

func newSession() string {
	t := randHex(24)
	sessMu.Lock()
	sessions[t] = time.Now().Add(7 * 24 * time.Hour)
	sessMu.Unlock()
	return t
}

func isAuthed(r *http.Request) bool {
	c, err := r.Cookie(sessCookie)
	if err != nil {
		return false
	}
	sessMu.Lock()
	defer sessMu.Unlock()
	exp, ok := sessions[c.Value]
	if !ok || time.Now().After(exp) {
		delete(sessions, c.Value)
		return false
	}
	return true
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func newHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServer(http.FS(sub))

	mux.HandleFunc("/panel/", func(w http.ResponseWriter, r *http.Request) {
		http.StripPrefix("/panel/", static).ServeHTTP(w, r)
	})
	mux.HandleFunc("/api/", apiHandler)
	mux.HandleFunc("/rec/", recHandler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/panel/", http.StatusFound)
			return
		}
		playHandler(w, r)
	})
	return mux
}

// ---------------- playback ----------------

// Routes:
//
//	/<app>/<stream>/index.m3u8   /<app>/<stream>.m3u8   /<app>/<stream>/segN.ts
//	/<app>/<stream>.flv          /<app>/<stream>.ts
func playHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		return
	}
	p := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(p, "/")
	var app, name, file string
	switch len(parts) {
	case 2:
		app = parts[0]
		name, file = parts[1], ""
		for _, ext := range []string{".m3u8", ".flv", ".ts"} {
			if strings.HasSuffix(name, ext) {
				file = "live" + ext
				name = strings.TrimSuffix(name, ext)
			}
		}
	case 3:
		app, name, file = parts[0], parts[1], parts[2]
	}
	if app == "" || name == "" || file == "" {
		http.NotFound(w, r)
		return
	}
	tok := r.URL.Query().Get("token")
	if !isAuthed(r) {
		if _, ok := checkPlay(app, name, tok); !ok {
			http.Error(w, "acces interzis (token)", http.StatusForbidden)
			return
		}
	}
	st := hub.get(app, name)
	if st == nil {
		http.Error(w, "stream offline", http.StatusNotFound)
		return
	}
	ip, ua := clientIP(r), r.UserAgent()
	q := ""
	if tok != "" {
		q = "?token=" + tok
	}

	switch {
	case file == "live.m3u8" || file == "index.m3u8":
		if st.hls == nil {
			http.Error(w, "HLS indisponibil pentru acest codec (folosește .flv)", http.StatusNotFound)
			return
		}
		var pl string
		var ok bool
		for i := 0; i < 40; i++ { // wait up to ~10s for the first segment
			if pl, ok = st.hls.playlist(q); ok {
				break
			}
			select {
			case <-r.Context().Done():
				return
			case <-st.done:
				http.Error(w, "stream offline", http.StatusNotFound)
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
		if !ok {
			http.Error(w, "se pregătește", http.StatusServiceUnavailable)
			return
		}
		if len(parts) == 2 { // short URL: make segment URIs absolute
			pl = strings.ReplaceAll(pl, "\nseg", "\n/"+app+"/"+name+"/seg")
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-cache")
		io.WriteString(w, pl)
		st.touchHLS(ip, tok, ua, 0)
	case strings.HasPrefix(file, "seg") && strings.HasSuffix(file, ".ts"):
		if st.hls == nil {
			http.NotFound(w, r)
			return
		}
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(file, "seg"), ".ts"))
		data := st.hls.segment(n)
		if err != nil || data == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Cache-Control", "max-age=60")
		w.Write(data)
		st.touchHLS(ip, tok, ua, len(data))
	case file == "live.flv":
		streamHTTP(w, r, st, "HTTP-FLV", ip, tok, ua)
	case file == "live.ts":
		if !st.tsCapable {
			http.Error(w, "HTTP-TS indisponibil pentru acest codec (folosește .flv)", http.StatusNotFound)
			return
		}
		streamHTTP(w, r, st, "HTTP-TS", ip, tok, ua)
	default:
		http.NotFound(w, r)
	}
}

type httpFlushW struct {
	w  http.ResponseWriter
	f  http.Flusher
	cl *Client
}

func (h *httpFlushW) Write(p []byte) (int, error) {
	n, err := h.w.Write(p)
	atomic.AddInt64(&h.cl.Bytes, int64(n))
	return n, err
}
func (h *httpFlushW) Flush() error { h.f.Flush(); return nil }

func streamHTTP(w http.ResponseWriter, r *http.Request, st *Stream, proto, ip, tok, ua string) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming nesuportat", 500)
		return
	}
	rc := http.NewResponseController(w)
	ctxDone := make(chan struct{})
	cl := st.addClient(proto, ip, tok, ua, func() {
		select {
		case <-ctxDone:
		default:
			close(ctxDone)
		}
	})
	defer st.removeClient(cl.ID)
	fw := &httpFlushW{w: w, f: fl, cl: cl}

	var m av.Muxer
	if proto == "HTTP-FLV" {
		w.Header().Set("Content-Type", "video/x-flv")
		m = flv.NewMuxerWriteFlusher(fw)
	} else {
		w.Header().Set("Content-Type", "video/mp2t")
		m = ts.NewMuxer(fw)
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	if err := m.WriteHeader(st.codecs); err != nil {
		return
	}
	fl.Flush()

	pr := newLiveReader(st)
	pkts := make(chan av.Packet, 512)
	go func() {
		defer close(pkts)
		for {
			pkt, err := pr.ReadPacket()
			if err != nil {
				return
			}
			select {
			case pkts <- pkt:
			case <-r.Context().Done():
				return
			case <-ctxDone:
				return
			}
		}
	}()
	last := time.Now()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ctxDone:
			return
		case pkt, ok := <-pkts:
			if !ok {
				return
			}
			rc.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if err := m.WritePacket(pkt); err != nil {
				return
			}
			if time.Since(last) > 100*time.Millisecond || len(pkts) == 0 {
				fl.Flush()
				last = time.Now()
			}
		}
	}
}

// ---------------- recordings ----------------

func safeRecPath(rel string) (string, bool) {
	root := getCfg().recordDir()
	full := filepath.Join(root, filepath.FromSlash(rel))
	rr, err := filepath.Rel(root, full)
	if err != nil || strings.HasPrefix(rr, "..") || rr == "." {
		return "", false
	}
	return full, true
}

func recHandler(w http.ResponseWriter, r *http.Request) {
	if !isAuthed(r) {
		http.Error(w, "neautorizat", http.StatusUnauthorized)
		return
	}
	full, ok := safeRecPath(strings.TrimPrefix(r.URL.Path, "/rec/"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get("dl") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(full)+`"`)
	}
	http.ServeFile(w, r, full)
}

type recFile struct {
	Path  string    `json:"path"`
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
	Live  bool      `json:"live"`
}

func listRecordings() []recFile {
	root := getCfg().recordDir()
	live := map[string]bool{}
	for _, s := range hub.list() {
		if s.rec != nil {
			s.rec.mu.Lock()
			live[s.rec.File] = true
			s.rec.mu.Unlock()
		}
	}
	out := []recFile{}
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if ext != ".ts" && ext != ".flv" {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, recFile{Path: filepath.ToSlash(rel), Size: info.Size(), MTime: info.ModTime(), Live: live[p]})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].MTime.After(out[j].MTime) })
	return out
}

// ---------------- API ----------------

type streamInfo struct {
	App     string     `json:"app"`
	Name    string     `json:"name"`
	PubIP   string     `json:"pub_ip"`
	Source  string     `json:"source"`
	Uptime  int64      `json:"uptime"`
	Codecs  string     `json:"codecs"`
	KbpsIn  float64    `json:"kbps_in"`
	FPS     float64    `json:"fps"`
	HLS     bool       `json:"hls"`
	TS      bool       `json:"ts"`
	Rec     *recInfo   `json:"rec"`
	Push    []pushInfo `json:"push"`
	Clients []Client   `json:"clients"`
	KbpsOut float64    `json:"kbps_out"`
}
type recInfo struct {
	File  string `json:"file"`
	Bytes int64  `json:"bytes"`
	Err   string `json:"err"`
}
type pushInfo struct {
	URL    string `json:"url"`
	Status string `json:"status"`
	Err    string `json:"err"`
	Bytes  uint64 `json:"bytes"`
	Since  int64  `json:"since"`
}

func statusInfo() []streamInfo {
	out := []streamInfo{}
	for _, s := range hub.list() {
		si := streamInfo{App: s.App, Name: s.Name, PubIP: s.PubIP, Source: s.Source, Uptime: int64(time.Since(s.Start).Seconds()),
			Codecs: s.codecString(), HLS: s.hls != nil, TS: s.tsCapable, Push: []pushInfo{}, Clients: []Client{}}
		s.mu.Lock()
		si.KbpsIn, si.FPS = s.kbpsIn, s.fps
		for _, c := range s.clients {
			cc := *c
			cc.Bytes = atomic.LoadInt64(&c.Bytes)
			si.Clients = append(si.Clients, cc)
			si.KbpsOut += c.Kbps
		}
		s.mu.Unlock()
		sort.Slice(si.Clients, func(i, j int) bool { return si.Clients[i].ID < si.Clients[j].ID })
		if s.rec != nil {
			s.rec.mu.Lock()
			si.Rec = &recInfo{File: filepath.Base(s.rec.File), Bytes: s.rec.Bytes, Err: s.rec.Err}
			s.rec.mu.Unlock()
		}
		for _, p := range s.pushers {
			p.mu.Lock()
			pi := pushInfo{URL: maskURL(p.URL), Status: p.Status, Err: p.Err, Bytes: p.Bytes}
			if p.Status == "conectat" {
				pi.Since = int64(time.Since(p.Since).Seconds())
			}
			p.mu.Unlock()
			si.Push = append(si.Push, pi)
		}
		out = append(out, si)
	}
	return out
}

func apiHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/")
	if path == "login" && r.Method == http.MethodPost {
		var in struct{ User, Pass string }
		json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
		c := getCfg()
		if in.User != c.AdminUser || !checkPass(c.AdminHash, in.Pass) {
			time.Sleep(700 * time.Millisecond)
			logf("LOGIN eșuat de la %s (user %q)", clientIP(r), in.User)
			writeJSON(w, 401, map[string]string{"error": "utilizator sau parolă greșită"})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: sessCookie, Value: newSession(), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 7 * 24 * 3600})
		writeJSON(w, 200, map[string]any{"ok": true, "default_pass": checkPass(c.AdminHash, "admin")})
		return
	}
	if !isAuthed(r) {
		writeJSON(w, 401, map[string]string{"error": "neautorizat"})
		return
	}
	switch {
	case path == "logout":
		if c, err := r.Cookie(sessCookie); err == nil {
			sessMu.Lock()
			delete(sessions, c.Value)
			sessMu.Unlock()
		}
		http.SetCookie(w, &http.Cookie{Name: sessCookie, Value: "", Path: "/", MaxAge: -1})
		writeJSON(w, 200, map[string]bool{"ok": true})

	case path == "status":
		c := getCfg()
		host, _ := os.Hostname()
		writeJSON(w, 200, map[string]any{
			"version": version, "uptime": int64(time.Since(startAt).Seconds()), "hostname": host,
			"rtmp_port": c.RTMPPort, "http_port": c.HTTPPort, "streams": statusInfo(), "pulls": pullStatus(), "ffmpeg": ffmpegPath(),
			"default_pass": checkPass(c.AdminHash, "admin"),
		})

	case path == "config" && r.Method == http.MethodGet:
		c := getCfg()
		c.AdminHash = ""
		writeJSON(w, 200, c)

	case path == "config" && r.Method == http.MethodPut:
		var in Config
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		normalize(&in)
		if err := validate(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		cfgMu.Lock()
		restart := in.RTMPPort != cfg.RTMPPort || in.HTTPPort != cfg.HTTPPort
		in.AdminHash = cfg.AdminHash
		if in.AdminUser == "" {
			in.AdminUser = cfg.AdminUser
		}
		cfg = &in
		err := saveConfigLocked()
		cfgMu.Unlock()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		logf("CONFIG salvat de %s", clientIP(r))
		writeJSON(w, 200, map[string]any{"ok": true, "restart_needed": restart})

	case path == "password" && r.Method == http.MethodPost:
		var in struct{ User, Old, New string }
		json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
		cfgMu.Lock()
		defer cfgMu.Unlock()
		if !checkPass(cfg.AdminHash, in.Old) {
			writeJSON(w, 400, map[string]string{"error": "parola curentă e greșită"})
			return
		}
		if len(in.New) < 6 {
			writeJSON(w, 400, map[string]string{"error": "parola nouă: minim 6 caractere"})
			return
		}
		if in.User != "" {
			cfg.AdminUser = in.User
		}
		cfg.AdminHash = hashPass(in.New)
		if err := saveConfigLocked(); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})

	case path == "kick" && r.Method == http.MethodPost:
		var in struct {
			App, Name string
			Client    int64
		}
		json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
		st := hub.get(in.App, in.Name)
		if st == nil {
			writeJSON(w, 404, map[string]string{"error": "stream offline"})
			return
		}
		if in.Client == 0 {
			logf("KICK publisher %s/%s de %s", in.App, in.Name, clientIP(r))
			st.kickPublisher()
		} else {
			st.mu.Lock()
			if cl := st.clients[in.Client]; cl != nil {
				if cl.kick != nil {
					cl.kick()
				}
				delete(st.clients, in.Client)
			}
			st.mu.Unlock()
		}
		writeJSON(w, 200, map[string]bool{"ok": true})

	case path == "recordings" && r.Method == http.MethodGet:
		writeJSON(w, 200, listRecordings())

	case path == "recordings" && r.Method == http.MethodDelete:
		full, ok := safeRecPath(r.URL.Query().Get("path"))
		if !ok {
			writeJSON(w, 400, map[string]string{"error": "cale invalidă"})
			return
		}
		if err := os.Remove(full); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		logf("DVR șters manual: %s", full)
		writeJSON(w, 200, map[string]bool{"ok": true})

	case path == "logs":
		writeJSON(w, 200, getLogs())

	case path == "genkey":
		writeJSON(w, 200, map[string]string{"key": randHex(10)})

	default:
		writeJSON(w, 404, map[string]string{"error": "necunoscut"})
	}
}
