package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/datarhei/joy4/av"
	"github.com/datarhei/joy4/format/flv"
	"github.com/datarhei/joy4/format/rtmp"
	"github.com/datarhei/joy4/format/ts"
)

// Puller keeps an external source (RTMP/HTTP/HLS/UDP/SRT/RTSP) running as a local live stream.
type Puller struct {
	App, Name, URL string

	mu     sync.Mutex
	Status string
	Err    string
	Since  time.Time
	Mode   string // "rtmp", "ffmpeg", "ffmpeg+aac", "ts"
	stop   chan struct{}
	cur    source
	stage  int // FFmpeg: 0 = TS copy, 1 = TS + audio AAC, 2 = FLV copy (HEVC/AV1, FFmpeg 7+)
}

type PullInfo struct {
	URL    string `json:"url"`
	Status string `json:"status"`
	Err    string `json:"err"`
	Mode   string `json:"mode"`
	Since  int64  `json:"since"`
}

var (
	pullMu  sync.Mutex
	pullers = map[string]*Puller{}
)

func (p *Puller) set(status, err string) {
	p.mu.Lock()
	p.Status, p.Err = status, err
	if status == "conectat" {
		p.Since = time.Now()
	}
	p.mu.Unlock()
}

func (p *Puller) info() PullInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	pi := PullInfo{URL: maskPull(p.URL), Status: p.Status, Err: p.Err, Mode: p.Mode}
	if p.Status == "conectat" {
		pi.Since = int64(time.Since(p.Since).Seconds())
	}
	return pi
}

func pullStatus() map[string]PullInfo {
	pullMu.Lock()
	defer pullMu.Unlock()
	out := map[string]PullInfo{}
	for k, p := range pullers {
		out[k] = p.info()
	}
	return out
}

// pullManager starts/stops pullers to match the config.
func pullManager() {
	for {
		c := getCfg()
		want := map[string]StreamConf{}
		for _, s := range c.Streams {
			if s.Enabled && s.Pull != "" {
				want[streamKey(s.App, s.Name)] = s
			}
		}
		pullMu.Lock()
		for k, p := range pullers {
			if w, ok := want[k]; !ok || w.Pull != p.URL {
				p.shutdown()
				delete(pullers, k)
				logf("PULL %s oprit", k)
			}
		}
		for k, s := range want {
			if _, ok := pullers[k]; !ok {
				p := &Puller{App: s.App, Name: s.Name, URL: s.Pull, Status: "pornire", stop: make(chan struct{})}
				pullers[k] = p
				go p.run()
			}
		}
		pullMu.Unlock()
		time.Sleep(3 * time.Second)
	}
}

func (p *Puller) shutdown() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	p.mu.Lock()
	if p.cur != nil {
		p.cur.Close()
	}
	p.mu.Unlock()
}

func (p *Puller) stopped() bool {
	select {
	case <-p.stop:
		return true
	default:
		return false
	}
}

func (p *Puller) wait(d time.Duration) bool {
	select {
	case <-p.stop:
		return false
	case <-time.After(d):
		return true
	}
}

func (p *Puller) run() {
	k := streamKey(p.App, p.Name)
	for !p.stopped() {
		p.set("conectare", "")
		src, codecs, mode, err := p.open()
		p.mu.Lock()
		p.Mode = mode
		p.mu.Unlock()
		if err != nil {
			p.set("eroare", err.Error())
			logf("PULL %s (%s): %v", k, maskPull(p.URL), err)
			if strings.HasPrefix(mode, "ffmpeg") && !isNetErr(err.Error()) {
				p.stage = (p.stage + 1) % 3 // altă variantă de remux la următoarea încercare
			}
			if !p.wait(5 * time.Second) {
				break
			}
			continue
		}
		p.mu.Lock()
		p.cur = src
		p.mu.Unlock()
		if p.stopped() {
			src.Close()
			break
		}
		p.set("conectat", "")
		sc := StreamConf{App: p.App, Name: p.Name, Enabled: true}
		if c := getCfg(); c.findStream(p.App, p.Name) != nil {
			sc = *c.findStream(p.App, p.Name)
		}
		started := time.Now()
		if err := hub.runStream(p.App, p.Name, &sc, src, codecs, maskPull(p.URL), "PULL"); err != nil {
			p.set("eroare", err.Error())
		} else if !p.stopped() {
			p.set("reconectare", "sursa s-a întrerupt")
		}
		src.Close()
		p.mu.Lock()
		p.cur = nil
		p.mu.Unlock()
		d := 5 * time.Second
		if time.Since(started) > time.Minute {
			d = 2 * time.Second
		}
		if !p.wait(d) {
			break
		}
	}
	p.set("oprit", "")
}

func (p *Puller) open() (source, []av.CodecData, string, error) {
	u := p.URL
	lu := strings.ToLower(u)
	if strings.HasPrefix(lu, "rtmp://") {
		conn, err := rtmp.DialTimeout(u, 10*time.Second, rtmp.DialOptions{})
		if err != nil {
			return nil, nil, "rtmp", err
		}
		conn.SetIdleTimeout(15 * time.Second)
		codecs, err := conn.Streams()
		if err != nil {
			conn.Close()
			return nil, nil, "rtmp", err
		}
		return conn, codecs, "rtmp", nil
	}
	if ff := ffmpegPath(); ff != "" {
		s, codecs, err := openFFmpeg(ff, u, p.stage)
		mode := []string{"ffmpeg", "ffmpeg+aac", "ffmpeg-flv"}[p.stage]
		return s, codecs, mode, err
	}
	if strings.HasPrefix(lu, "http://") || strings.HasPrefix(lu, "https://") || strings.HasPrefix(lu, "udp://") {
		s, codecs, err := openNativeTS(u)
		return s, codecs, "ts", err
	}
	return nil, nil, "ffmpeg", errors.New("FFmpeg nu a fost găsit (necesar pentru acest tip de sursă) — instalează-l sau setează calea în Setări")
}

// ---------- FFmpeg source ----------

func ffmpegPath() string {
	c := getCfg()
	if c.FFmpegPath != "" {
		if _, err := os.Stat(c.FFmpegPath); err == nil {
			return c.FFmpegPath
		}
		if p, err := exec.LookPath(c.FFmpegPath); err == nil {
			return p
		}
		return ""
	}
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name = "ffmpeg.exe"
	}
	if p := filepath.Join(baseDir, name); fileExists(p) {
		return p
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, p := range []string{"/usr/bin/ffmpeg", "/usr/local/bin/ffmpeg", `C:\ffmpeg\bin\ffmpeg.exe`} {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

type countReader struct {
	r io.Reader
	n uint64
}

func (c *countReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	atomic.AddUint64(&c.n, uint64(n))
	return n, err
}

type ffSource struct {
	cmd    *exec.Cmd
	dmx    av.Demuxer
	cr     *countReader
	once   sync.Once
	stderr *tailBuf
}

func (f *ffSource) ReadPacket() (av.Packet, error) { return f.dmx.ReadPacket() }
func (f *ffSource) RxBytes() uint64                { return atomic.LoadUint64(&f.cr.n) }
func (f *ffSource) Close() error {
	f.once.Do(func() {
		if f.cmd.Process != nil {
			f.cmd.Process.Kill()
		}
		go f.cmd.Wait()
	})
	return nil
}

type tailBuf struct {
	mu      sync.Mutex
	lines   []string
	inAudio bool // sursa are o pistă audio (din log-ul FFmpeg)
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	for _, l := range strings.Split(strings.TrimSpace(string(p)), "\n") {
		l = strings.TrimSpace(l)
		if strings.Contains(l, "[info]") {
			if strings.Contains(l, "Stream #0:") && strings.Contains(l, "Audio:") && !strings.Contains(l, "->") {
				t.inAudio = true
			}
			continue
		}
		for _, lv := range []string{"[error] ", "[fatal] ", "[panic] ", "[warning] "} {
			if i := strings.Index(l, lv); i >= 0 {
				l = strings.TrimSpace(l[i+len(lv):])
			}
		}
		if l != "" {
			t.lines = append(t.lines, l)
		}
	}
	if len(t.lines) > 5 {
		t.lines = t.lines[len(t.lines)-5:]
	}
	t.mu.Unlock()
	return len(p), nil
}
func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	msg := strings.Join(t.lines, " | ")
	if len(msg) > 220 {
		msg = msg[len(msg)-220:]
	}
	return msg
}

func openFFmpeg(ff, u string, stage int) (source, []av.CodecData, error) {
	lu := strings.ToLower(u)
	in := u
	args := []string{"-hide_banner", "-loglevel", "level+info", "-nostats", "-nostdin"}
	switch {
	case strings.HasPrefix(lu, "rtsp://"):
		args = append(args, "-rtsp_transport", "tcp", "-timeout", "10000000")
	case strings.HasPrefix(lu, "http://"), strings.HasPrefix(lu, "https://"), strings.HasPrefix(lu, "rtmps://"):
		args = append(args, "-rw_timeout", "15000000")
	case strings.HasPrefix(lu, "udp://"), strings.HasPrefix(lu, "rtp://"):
		sep := "?"
		if strings.Contains(in, "?") {
			sep = "&"
		}
		if !strings.Contains(lu, "fifo_size") {
			in += sep + "fifo_size=1000000&overrun_nonfatal=1"
			sep = "&"
		}
		if !strings.Contains(lu, "timeout") {
			in += sep + "timeout=10000000"
		}
	}
	args = append(args, "-fflags", "+genpts+discardcorrupt", "-i", in,
		"-map", "0:v:0?", "-map", "0:a:0?", "-c:v", "copy")
	switch stage {
	case 1:
		args = append(args, "-c:a", "aac", "-b:a", "160k", "-ar", "48000", "-f", "mpegts", "pipe:1")
	case 2:
		args = append(args, "-c:a", "copy", "-f", "flv", "-flvflags", "no_duration_filesize", "pipe:1")
	default:
		args = append(args, "-c:a", "copy", "-f", "mpegts", "pipe:1")
	}

	cmd := exec.Command(ff, args...)
	hideWindow(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	tb := &tailBuf{}
	cmd.Stderr = tb
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("ffmpeg: %v", err)
	}
	cr := &countReader{r: bufio.NewReaderSize(out, 256*1024)}
	s := &ffSource{cmd: cmd, cr: cr, stderr: tb}
	if stage == 2 {
		s.dmx = flv.NewDemuxer(cr)
	} else {
		s.dmx = ts.NewDemuxer(cr)
	}

	type res struct {
		c   []av.CodecData
		err error
	}
	ch := make(chan res, 1)
	go func() { c, err := s.dmx.Streams(); ch <- res{c, err} }()
	select {
	case r := <-ch:
		if r.err != nil || len(r.c) == 0 {
			s.Close()
			time.Sleep(200 * time.Millisecond)
			msg := tb.String()
			if msg == "" && r.err != nil {
				msg = r.err.Error()
			}
			if msg == "" {
				msg = "sursa nu a trimis date"
			}
			return nil, nil, errors.New(msg)
		}
		hasA := false
		for _, c := range r.c {
			if c.Type().IsAudio() {
				hasA = true
			}
		}
		tb.mu.Lock()
		lost := tb.inAudio && !hasA
		tb.mu.Unlock()
		if lost && stage != 1 {
			s.Close()
			return nil, nil, errors.New("audio sursă incompatibil (ex. MP2/AC3) — trec pe conversie AAC")
		}
		return s, r.c, nil
	case <-time.After(25 * time.Second):
		s.Close()
		msg := tb.String()
		if msg == "" {
			msg = "timeout la conectare (25s)"
		}
		return nil, nil, errors.New(msg)
	}
}

// ---------- native MPEG-TS source (fără FFmpeg): HTTP-TS / UDP multicast ----------

type tsSource struct {
	dmx   *ts.Demuxer
	cr    *countReader
	close func()
	once  sync.Once
}

func (t *tsSource) ReadPacket() (av.Packet, error) { return t.dmx.ReadPacket() }
func (t *tsSource) RxBytes() uint64                { return atomic.LoadUint64(&t.cr.n) }
func (t *tsSource) Close() error                   { t.once.Do(t.close); return nil }

type udpReader struct{ c *net.UDPConn }

func (u udpReader) Read(b []byte) (int, error) {
	u.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	return u.c.Read(b)
}

func openNativeTS(raw string) (source, []av.CodecData, error) {
	var r io.Reader
	var closer func()
	if strings.HasPrefix(strings.ToLower(raw), "udp://") {
		pu, err := url.Parse(raw)
		if err != nil {
			return nil, nil, err
		}
		host := strings.TrimPrefix(pu.Host, "@")
		addr, err := net.ResolveUDPAddr("udp4", host)
		if err != nil {
			return nil, nil, err
		}
		var c *net.UDPConn
		if addr.IP.IsMulticast() {
			var ifi *net.Interface
			if n := pu.Query().Get("localaddr"); n != "" {
				ifi = ifaceByIP(n)
			}
			c, err = net.ListenMulticastUDP("udp4", ifi, addr)
		} else {
			c, err = net.ListenUDP("udp4", addr)
		}
		if err != nil {
			return nil, nil, err
		}
		c.SetReadBuffer(4 << 20)
		r = bufio.NewReaderSize(udpReader{c}, 1<<20)
		closer = func() { c.Close() }
	} else {
		cl := &http.Client{Timeout: 0, Transport: &http.Transport{ResponseHeaderTimeout: 15 * time.Second}}
		resp, err := cl.Get(raw)
		if err != nil {
			return nil, nil, err
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return nil, nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "mpegurl") {
			resp.Body.Close()
			return nil, nil, errors.New("sursele HLS (.m3u8) necesită FFmpeg")
		}
		r = resp.Body
		closer = func() { resp.Body.Close() }
	}
	cr := &countReader{r: r}
	s := &tsSource{dmx: ts.NewDemuxer(cr), cr: cr, close: closer}
	ch := make(chan error, 1)
	var codecs []av.CodecData
	go func() {
		var err error
		codecs, err = s.dmx.Streams()
		ch <- err
	}()
	select {
	case err := <-ch:
		if err != nil {
			s.Close()
			return nil, nil, err
		}
	case <-time.After(15 * time.Second):
		s.Close()
		return nil, nil, errors.New("timeout: nu s-au primit date TS")
	}
	return s, codecs, nil
}

func ifaceByIP(ip string) *net.Interface {
	ifs, _ := net.Interfaces()
	for i := range ifs {
		addrs, _ := ifs[i].Addrs()
		for _, a := range addrs {
			if strings.HasPrefix(a.String(), ip+"/") {
				return &ifs[i]
			}
		}
	}
	return nil
}

// maskPull hides credentials and key-like query values in a URL for display.
func maskPull(raw string) string {
	pu, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if pu.User != nil {
		pu.User = url.User(pu.User.Username())
	}
	q := pu.Query()
	for k := range q {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "key") || strings.Contains(lk, "token") || strings.Contains(lk, "pass") || strings.Contains(lk, "auth") {
			q.Set(k, "****")
		}
	}
	if len(q) > 0 {
		pu.RawQuery = q.Encode()
	}
	return pu.String()
}

func isNetErr(m string) bool {
	m = strings.ToLower(m)
	for _, k := range []string{"connection", "404", "403", "401", "timeout", "timed out", "no route", "refused", "resolve", "not found", "unreachable", "sursa nu a trimis"} {
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
}
