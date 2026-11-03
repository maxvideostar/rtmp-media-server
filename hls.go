package main

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/datarhei/joy4/format/ts"
)

type segment struct {
	seq  int
	dur  float64
	data []byte
}

// HLS is an in-memory live HLS segmenter (MPEG-TS segments, cut on keyframes).
type HLS struct {
	mu       sync.RWMutex
	segs     []*segment
	nextSeq  int
	segMs    int64
	listSize int
}

func newHLS(segSec, listSize int) *HLS {
	return &HLS{segMs: int64(segSec) * 1000, listSize: listSize}
}

func (h *HLS) run(st *Stream) {
	r := newLiveReader(st)
	buf := &bytes.Buffer{}
	mux := ts.NewMuxer(buf)
	if err := mux.WriteHeader(st.codecs); err != nil {
		logf("HLS %s/%s: %v", st.App, st.Name, err)
		return
	}
	segStart := int64(-1)
	for {
		pkt, err := r.ReadPacket()
		if err != nil {
			break
		}
		if segStart < 0 {
			segStart = pkt.Time
		}
		el := pkt.Time - segStart
		cut := false
		if st.vidx >= 0 {
			cut = int(pkt.Idx) == st.vidx && pkt.IsKeyFrame && el >= h.segMs
		} else {
			cut = el >= h.segMs
		}
		if cut {
			h.add(buf.Bytes(), float64(el)/1000)
			buf = &bytes.Buffer{}
			mux.SetWriter(buf)
			mux.WritePATPMT()
			segStart = pkt.Time
		}
		if err := mux.WritePacket(pkt); err != nil {
			break
		}
	}
}

func (h *HLS) add(data []byte, dur float64) {
	h.mu.Lock()
	h.segs = append(h.segs, &segment{seq: h.nextSeq, dur: dur, data: data})
	h.nextSeq++
	keep := h.listSize + 4
	if len(h.segs) > keep {
		h.segs = h.segs[len(h.segs)-keep:]
	}
	h.mu.Unlock()
}

func (h *HLS) playlist(query string) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.segs) == 0 {
		return "", false
	}
	list := h.segs
	if len(list) > h.listSize {
		list = list[len(list)-h.listSize:]
	}
	maxd := 1.0
	for _, s := range list {
		if s.dur > maxd {
			maxd = s.dur
		}
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(maxd)))
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", list[0].seq)
	for _, s := range list {
		fmt.Fprintf(&b, "#EXTINF:%.3f,\nseg%d.ts%s\n", s.dur, s.seq, query)
	}
	return b.String(), true
}

func (h *HLS) segment(seq int) []byte {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, s := range h.segs {
		if s.seq == seq {
			return s.data
		}
	}
	return nil
}
