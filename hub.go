package main

import (
	"errors"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/datarhei/joy4/av"
	"github.com/datarhei/joy4/av/pubsub"
	"github.com/datarhei/joy4/codec/aacparser"
	"github.com/datarhei/joy4/codec/h264parser"
	"github.com/datarhei/joy4/format/rtmp"
	"github.com/datarhei/joy4/format/ts"
)

// Client is one viewer connection.
type Client struct {
	ID       int64     `json:"id"`
	Proto    string    `json:"proto"`
	IP       string    `json:"ip"`
	Token    string    `json:"token"`
	UA       string    `json:"ua"`
	Start    time.Time `json:"start"`
	Bytes    int64     `json:"bytes"`
	Kbps     float64   `json:"kbps"`
	lastSeen int64     // unix nano (HLS)
	lastB    int64
	kick     func()
}

type Stream struct {
	App, Name string
	Conf      StreamConf
	q         *pubsub.Queue
	codecs    []av.CodecData
	vidx      int
	conn      *rtmp.Conn
	PubIP     string
	Start     time.Time
	done      chan struct{}
	hls       *HLS
	rec       *Recorder
	pushers   []*Pusher

	mu      sync.Mutex
	clients map[int64]*Client
	hlsKeys map[string]int64

	vframes   int64
	lastRx    uint64
	lastVF    int64
	kbpsIn    float64
	fps       float64
	tsCapable bool
}

type Hub struct {
	mu      sync.RWMutex
	streams map[string]*Stream
}

var hub = &Hub{streams: map[string]*Stream{}}
var clientSeq int64

func streamKey(app, name string) string { return app + "/" + name }

func (h *Hub) get(app, name string) *Stream {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.streams[streamKey(app, name)]
}

func (h *Hub) list() []*Stream {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Stream, 0, len(h.streams))
	for _, s := range h.streams {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return streamKey(out[i].App, out[i].Name) < streamKey(out[j].App, out[j].Name) })
	return out
}

func ipOf(a net.Addr) string {
	if a == nil {
		return ""
	}
	h, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return h
}

// parseRTMP extracts app, stream name and query from an RTMP URL path like /live/tv1?key=x
func parseRTMP(u *url.URL) (app, name string, q url.Values) {
	q = u.Query()
	p := strings.Trim(u.Path, "/")
	parts := strings.SplitN(p, "/", 2)
	if len(parts) == 2 {
		app, name = parts[0], parts[1]
	} else if len(parts) == 1 {
		app = parts[0]
	}
	// OBS sometimes puts "?key=" into the path segment
	if i := strings.Index(name, "?"); i >= 0 {
		if extra, err := url.ParseQuery(name[i+1:]); err == nil {
			for k, v := range extra {
				q[k] = v
			}
		}
		name = name[:i]
	}
	return
}

func (h *Hub) handlePublish(conn *rtmp.Conn) {
	app, name, q := parseRTMP(conn.URL)
	ip := ipOf(conn.RemoteAddr())
	key := q.Get("key")
	if key == "" {
		key = q.Get("k")
	}
	sc, err := checkPublish(app, name, key)
	if err != nil {
		logf("PUBLISH respins %s/%s de la %s: %v", app, name, ip, err)
		return
	}
	codecs, err := conn.Streams()
	if err != nil {
		logf("PUBLISH %s/%s: eroare la citirea codecurilor: %v", app, name, err)
		return
	}

	st := &Stream{
		App: app, Name: name, Conf: *sc, conn: conn, PubIP: ip, Start: time.Now(),
		done: make(chan struct{}), clients: map[int64]*Client{}, hlsKeys: map[string]int64{},
		codecs: codecs, vidx: -1, tsCapable: true,
	}
	for i, c := range codecs {
		if c.Type().IsVideo() {
			st.vidx = i
		}
		ok := false
		for _, t := range ts.CodecTypes {
			if c.Type() == t {
				ok = true
			}
		}
		if !ok {
			st.tsCapable = false
		}
	}

	k := streamKey(app, name)
	h.mu.Lock()
	if _, exists := h.streams[k]; exists {
		h.mu.Unlock()
		logf("PUBLISH respins %s de la %s: deja activ", k, ip)
		return
	}
	st.q = pubsub.NewQueue()
	st.q.SetMaxGopCount(2)
	st.q.WriteHeader(codecs)
	h.streams[k] = st
	h.mu.Unlock()

	logf("PUBLISH start %s de la %s [%s]", k, ip, st.codecString())
	c := getCfg()
	if c.HLSEnabled && st.tsCapable {
		st.hls = newHLS(c.HLSSegment, c.HLSListSize)
		go st.hls.run(st)
	}
	if sc.Record {
		st.rec = &Recorder{}
		go st.rec.run(st, c)
	}
	for _, u := range sc.Push {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		p := &Pusher{URL: u, Status: "pornire"}
		st.pushers = append(st.pushers, p)
		go p.run(st)
	}

	for {
		pkt, err := conn.ReadPacket()
		if err != nil {
			break
		}
		if int(pkt.Idx) == st.vidx {
			atomic.AddInt64(&st.vframes, 1)
		}
		st.q.WritePacket(pkt)
	}

	st.q.Close()
	close(st.done)
	h.mu.Lock()
	delete(h.streams, k)
	h.mu.Unlock()
	st.mu.Lock()
	for _, cl := range st.clients {
		if cl.kick != nil {
			cl.kick()
		}
	}
	st.mu.Unlock()
	logf("PUBLISH stop %s (durată %s)", k, time.Since(st.Start).Round(time.Second))
}

func (h *Hub) handlePlay(conn *rtmp.Conn) {
	app, name, q := parseRTMP(conn.URL)
	ip := ipOf(conn.RemoteAddr())
	tok := q.Get("token")
	if _, ok := checkPlay(app, name, tok); !ok {
		logf("PLAY rtmp respins %s/%s de la %s (token)", app, name, ip)
		return
	}
	st := h.get(app, name)
	if st == nil {
		return
	}
	cl := st.addClient("RTMP", ip, tok, "", func() { conn.Close() })
	defer st.removeClient(cl.ID)
	conn.SetWriteIdleTimeout(15 * time.Second)
	if err := conn.WriteHeader(st.codecs); err != nil {
		return
	}
	r := newLiveReader(st)
	for {
		pkt, err := r.ReadPacket()
		if err != nil {
			break
		}
		if err := conn.WritePacket(pkt); err != nil {
			break
		}
		atomic.StoreInt64(&cl.Bytes, int64(conn.TxBytes()))
	}
}

func (s *Stream) addClient(proto, ip, tok, ua string, kick func()) *Client {
	cl := &Client{ID: atomic.AddInt64(&clientSeq, 1), Proto: proto, IP: ip, Token: tok, UA: ua, Start: time.Now(), kick: kick}
	atomic.StoreInt64(&cl.lastSeen, time.Now().UnixNano())
	s.mu.Lock()
	s.clients[cl.ID] = cl
	s.mu.Unlock()
	return cl
}

func (s *Stream) removeClient(id int64) {
	s.mu.Lock()
	delete(s.clients, id)
	s.mu.Unlock()
}

// touchHLS tracks HLS viewers (stateless HTTP) by ip+token+ua.
func (s *Stream) touchHLS(ip, tok, ua string, n int) {
	k := ip + "|" + tok + "|" + ua
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.hlsKeys[k]
	cl := s.clients[id]
	if !ok || cl == nil {
		cl = &Client{ID: atomic.AddInt64(&clientSeq, 1), Proto: "HLS", IP: ip, Token: tok, UA: ua, Start: time.Now()}
		s.clients[cl.ID] = cl
		s.hlsKeys[k] = cl.ID
	}
	atomic.StoreInt64(&cl.lastSeen, time.Now().UnixNano())
	atomic.AddInt64(&cl.Bytes, int64(n))
}

func (s *Stream) codecString() string {
	var parts []string
	for _, c := range s.codecs {
		switch v := c.(type) {
		case h264parser.CodecData:
			parts = append(parts, "H264 "+itoa(v.Width())+"x"+itoa(v.Height()))
		case aacparser.CodecData:
			parts = append(parts, "AAC "+itoa(v.SampleRate())+"Hz "+itoa(v.ChannelLayout().Count())+"ch")
		default:
			parts = append(parts, c.Type().String())
		}
	}
	return strings.Join(parts, ", ")
}

func (s *Stream) kickPublisher() {
	if s.conn != nil {
		s.conn.Close()
	}
}

// statsLoop computes bitrates/fps every 2 seconds and expires idle HLS viewers.
func statsLoop() {
	const iv = 2.0
	for range time.Tick(2 * time.Second) {
		now := time.Now().UnixNano()
		for _, s := range hub.list() {
			rx := s.conn.RxBytes()
			s.mu.Lock()
			s.kbpsIn = float64(rx-s.lastRx) * 8 / 1000 / iv
			s.lastRx = rx
			vf := atomic.LoadInt64(&s.vframes)
			s.fps = float64(vf-s.lastVF) / iv
			s.lastVF = vf
			for id, cl := range s.clients {
				b := atomic.LoadInt64(&cl.Bytes)
				cl.Kbps = float64(b-cl.lastB) * 8 / 1000 / iv
				cl.lastB = b
				if cl.Proto == "HLS" && now-atomic.LoadInt64(&cl.lastSeen) > int64(30*time.Second) {
					delete(s.clients, id)
					delete(s.hlsKeys, cl.IP+"|"+cl.Token+"|"+cl.UA)
				}
			}
			s.mu.Unlock()
		}
	}
}

// liveReader reads from the GOP cache starting at a keyframe, with timestamps rebased to 0.
type liveReader struct {
	c       *pubsub.QueueCursor
	vidx    int
	started bool
	base    int64
}

func newLiveReader(s *Stream) *liveReader {
	return &liveReader{c: s.q.DelayedGopCount(1), vidx: s.vidx}
}

var errEOF = errors.New("eof")

func (r *liveReader) ReadPacket() (av.Packet, error) {
	for {
		pkt, err := r.c.ReadPacket()
		if err != nil {
			return pkt, err
		}
		if !r.started {
			if r.vidx >= 0 && !(int(pkt.Idx) == r.vidx && pkt.IsKeyFrame) {
				continue
			}
			r.started = true
			r.base = pkt.Time
		}
		pkt.Time -= r.base
		if pkt.Time < 0 {
			pkt.Time = 0
		}
		return pkt, nil
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}
