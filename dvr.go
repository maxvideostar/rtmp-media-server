package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/datarhei/joy4/av"
	"github.com/datarhei/joy4/format/flv"
	"github.com/datarhei/joy4/format/ts"
)

// Recorder writes the live stream to disk, rotating files every N minutes on a keyframe.
// H264/AAC -> .ts, anything else (HEVC/AV1/VP9...) -> .flv
type Recorder struct {
	mu    sync.Mutex
	File  string
	Bytes int64
	Err   string
}

type countW struct {
	w *bufio.Writer
	r *Recorder
}

func (c *countW) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.r.mu.Lock()
	c.r.Bytes += int64(n)
	c.r.mu.Unlock()
	return n, err
}
func (c *countW) Flush() error { return c.w.Flush() }

func (rec *Recorder) setErr(e string) {
	rec.mu.Lock()
	rec.Err = e
	rec.mu.Unlock()
}

func (rec *Recorder) run(st *Stream, c Config) {
	dir := filepath.Join(c.recordDir(), st.App, st.Name)
	rotate := int64(c.RecordRotateMin) * 60 * 1000
	ext := ".ts"
	if !st.tsCapable {
		ext = ".flv"
	}
	r := newLiveReader(st)

	var f *os.File
	var bw *bufio.Writer
	var cw *countW
	var muxer av.Muxer
	var fileStart int64 = -1

	closeFile := func() {
		if f == nil {
			return
		}
		if muxer != nil {
			muxer.WriteTrailer()
		}
		bw.Flush()
		f.Close()
		f = nil
	}
	open := func() bool {
		closeFile()
		now := time.Now()
		d := filepath.Join(dir, now.Format("2006-01-02"))
		if err := os.MkdirAll(d, 0755); err != nil {
			rec.setErr(err.Error())
			return false
		}
		name := filepath.Join(d, now.Format("15-04-05")+ext)
		var err error
		f, err = os.Create(name)
		if err != nil {
			rec.setErr(err.Error())
			return false
		}
		bw = bufio.NewWriterSize(f, 256*1024)
		cw = &countW{w: bw, r: rec}
		if ext == ".ts" {
			muxer = ts.NewMuxer(cw)
		} else {
			muxer = flv.NewMuxerWriteFlusher(cw)
		}
		if err := muxer.WriteHeader(st.codecs); err != nil {
			rec.setErr(err.Error())
			return false
		}
		rec.mu.Lock()
		rec.File = name
		rec.Err = ""
		rec.mu.Unlock()
		logf("DVR %s/%s -> %s", st.App, st.Name, name)
		return true
	}

	for {
		pkt, err := r.ReadPacket()
		if err != nil {
			break
		}
		isKey := st.vidx < 0 || (int(pkt.Idx) == st.vidx && pkt.IsKeyFrame)
		if fileStart < 0 || (isKey && pkt.Time-fileStart >= rotate) {
			if !open() {
				break
			}
			fileStart = pkt.Time
		}
		if ext == ".flv" {
			pkt.Time -= fileStart
			if pkt.Time < 0 {
				pkt.Time = 0
			}
		}
		if err := muxer.WritePacket(pkt); err != nil {
			rec.setErr(err.Error())
			break
		}
	}
	closeFile()
}

// cleanupLoop deletes recordings older than record_keep_days.
func cleanupLoop() {
	for {
		c := getCfg()
		if c.RecordKeepDays > 0 {
			limit := time.Now().AddDate(0, 0, -c.RecordKeepDays)
			root := c.recordDir()
			filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil
				}
				ext := strings.ToLower(filepath.Ext(p))
				if (ext == ".ts" || ext == ".flv") && info.ModTime().Before(limit) {
					os.Remove(p)
					logf("DVR șters (vechi): %s", p)
				}
				return nil
			})
			removeEmptyDirs(root)
		}
		time.Sleep(time.Hour)
	}
}

func removeEmptyDirs(root string) {
	var dirs []string
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() && p != root {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i]) // only succeeds if empty
	}
}
