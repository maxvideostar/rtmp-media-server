package main

import (
	"sync"
	"time"

	"github.com/datarhei/joy4/format/rtmp"
)

// Pusher re-streams a live stream to an external RTMP target, reconnecting automatically.
type Pusher struct {
	mu     sync.Mutex
	URL    string
	Status string
	Err    string
	Bytes  uint64
	Since  time.Time
}

func (p *Pusher) set(status, err string) {
	p.mu.Lock()
	p.Status, p.Err = status, err
	if status == "conectat" {
		p.Since = time.Now()
	}
	p.mu.Unlock()
}

func (p *Pusher) run(st *Stream) {
	for {
		select {
		case <-st.done:
			p.set("oprit", "")
			return
		default:
		}
		p.set("conectare", "")
		conn, err := rtmp.DialTimeout(p.URL, 10*time.Second, rtmp.DialOptions{})
		if err == nil {
			conn.SetIdleTimeout(15 * time.Second)
			err = conn.WriteHeader(st.codecs)
		}
		if err != nil {
			if conn != nil {
				conn.Close()
			}
			p.set("eroare", err.Error())
			logf("PUSH %s/%s -> %s: %v", st.App, st.Name, maskURL(p.URL), err)
			if !waitOrDone(st, 5*time.Second) {
				p.set("oprit", "")
				return
			}
			continue
		}
		p.set("conectat", "")
		logf("PUSH %s/%s -> %s conectat", st.App, st.Name, maskURL(p.URL))
		r := newLiveReader(st)
		var werr error
		ended := false
		for {
			pkt, err := r.ReadPacket()
			if err != nil {
				ended = true
				break
			}
			if werr = conn.WritePacket(pkt); werr != nil {
				break
			}
			p.mu.Lock()
			p.Bytes = conn.TxBytes()
			p.mu.Unlock()
		}
		if ended {
			conn.WriteTrailer()
		}
		conn.Close()
		if ended {
			p.set("oprit", "")
			return
		}
		p.set("eroare", werr.Error())
		logf("PUSH %s/%s -> %s întrerupt: %v", st.App, st.Name, maskURL(p.URL), werr)
		if !waitOrDone(st, 3*time.Second) {
			p.set("oprit", "")
			return
		}
	}
}

func waitOrDone(st *Stream, d time.Duration) bool {
	select {
	case <-st.done:
		return false
	case <-time.After(d):
		return true
	}
}

// maskURL hides the stream key (last path segment) in logs/UI.
func maskURL(u string) string {
	i := lastIndex(u, '/')
	if i < 0 || i+1 >= len(u) {
		return u
	}
	k := u[i+1:]
	if len(k) <= 4 {
		return u
	}
	return u[:i+1] + k[:2] + "****" + k[len(k)-2:]
}

func lastIndex(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}
