package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/datarhei/joy4/format"
	"github.com/datarhei/joy4/format/rtmp"
	"github.com/kardianos/service"
)

var version = "1.1.1"

// ---------------- logging ----------------

var (
	logMu   sync.Mutex
	logRing []string
	logOut  io.Writer = os.Stdout
)

func logf(f string, a ...any) {
	line := time.Now().Format("2006-01-02 15:04:05") + "  " + fmt.Sprintf(f, a...)
	logMu.Lock()
	logRing = append(logRing, line)
	if len(logRing) > 500 {
		logRing = logRing[len(logRing)-500:]
	}
	fmt.Fprintln(logOut, line)
	logMu.Unlock()
}

func getLogs() []string {
	logMu.Lock()
	defer logMu.Unlock()
	out := make([]string, len(logRing))
	copy(out, logRing)
	return out
}

func setupLogFile() {
	p := filepath.Join(baseDir, "rtmp-media-server.log")
	if fi, err := os.Stat(p); err == nil && fi.Size() > 20<<20 {
		os.Rename(p, p+".old")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		logOut = io.MultiWriter(os.Stdout, f)
	}
}

// ---------------- service ----------------

type program struct {
	rtmpLn net.Listener
	httpSv *http.Server
}

func (p *program) Start(s service.Service) error {
	go p.run()
	return nil
}

func (p *program) run() {
	c := getCfg()
	srv := &rtmp.Server{
		HandlePublish:         hub.handlePublish,
		HandlePlay:            hub.handlePlay,
		ConnectionIdleTimeout: 20 * time.Second,
		SkipInvalidMessages:   true,
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(c.RTMPPort))
	if err != nil {
		logf("EROARE: nu pot asculta RTMP pe %d: %v", c.RTMPPort, err)
		os.Exit(1)
	}
	p.rtmpLn = ln
	go func() {
		if err := srv.Serve(ln); err != nil && err != rtmp.ErrServerClosed {
			logf("RTMP server: %v", err)
		}
	}()

	p.httpSv = &http.Server{Addr: ":" + strconv.Itoa(c.HTTPPort), Handler: newHTTPHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := p.httpSv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logf("EROARE HTTP pe %d: %v", c.HTTPPort, err)
			os.Exit(1)
		}
	}()
	go statsLoop()
	go cleanupLoop()
	go pullManager()
	logf("RTMP Media Server %s pornit — RTMP :%d, HTTP/panou :%d (http://IP:%d/panel/)", version, c.RTMPPort, c.HTTPPort, c.HTTPPort)
}

func (p *program) Stop(s service.Service) error {
	logf("oprire...")
	if p.rtmpLn != nil {
		p.rtmpLn.Close()
	}
	for _, st := range hub.list() {
		st.kickPublisher()
	}
	if p.httpSv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		p.httpSv.Shutdown(ctx)
		cancel()
	}
	time.Sleep(300 * time.Millisecond)
	return nil
}

func exeDir() string {
	e, err := os.Executable()
	if err != nil {
		return "."
	}
	if r, err := filepath.EvalSymlinks(e); err == nil {
		e = r
	}
	return filepath.Dir(e)
}

func main() {
	format.RegisterAll()
	cfgFlag := flag.String("config", "", "cale config.json (implicit: lângă executabil)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `RTMP Media Server %s

Utilizare:
  rtmp-media-server [-config config.json]        pornește serverul
  rtmp-media-server install|uninstall|start|stop|restart|status
  rtmp-media-server passwd <parola_noua>          resetează parola admin
  rtmp-media-server version
`, version)
	}
	flag.Parse()

	baseDir = exeDir()
	cfgPath = filepath.Join(baseDir, "config.json")
	if *cfgFlag != "" {
		cfgPath, _ = filepath.Abs(*cfgFlag)
		baseDir = filepath.Dir(cfgPath)
	}

	args := []string{}
	if *cfgFlag != "" {
		args = []string{"-config", cfgPath}
	}
	svcConf := &service.Config{
		Name:        "rtmp-media-server",
		DisplayName: "RTMP Media Server",
		Description: "Server media RTMP/HLS/HTTP-FLV cu DVR și restream (github.com/maxvideostar/rtmp-media-server)",
		Arguments:   args,
		Option: service.KeyValue{
			"Restart":                "always",
			"OnFailure":              "restart",
			"OnFailureDelayDuration": "5s",
			"LimitNOFILE":            65535,
		},
	}
	prg := &program{}
	s, err := service.New(prg, svcConf)
	if err != nil {
		fmt.Println("eroare serviciu:", err)
		os.Exit(1)
	}

	if cmd := flag.Arg(0); cmd != "" {
		switch cmd {
		case "version":
			fmt.Println(version)
			return
		case "passwd":
			if flag.Arg(1) == "" {
				fmt.Println("utilizare: rtmp-media-server passwd <parola_noua>")
				os.Exit(1)
			}
			if err := loadConfig(); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			cfgMu.Lock()
			cfg.AdminHash = hashPass(flag.Arg(1))
			err := saveConfigLocked()
			cfgMu.Unlock()
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			fmt.Println("parola admin schimbată (repornește serviciul dacă rulează)")
			return
		case "status":
			st, err := s.Status()
			if err != nil {
				fmt.Println("status:", err)
				os.Exit(1)
			}
			fmt.Println(map[service.Status]string{service.StatusRunning: "pornit", service.StatusStopped: "oprit"}[st])
			return
		case "install":
			if err := loadConfig(); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
		}
		if err := service.Control(s, cmd); err != nil {
			fmt.Printf("%s: %v\n", cmd, err)
			os.Exit(1)
		}
		fmt.Printf("%s: OK\n", cmd)
		return
	}

	setupLogFile()
	if err := loadConfig(); err != nil {
		logf("EROARE config: %v", err)
		os.Exit(1)
	}
	if err := s.Run(); err != nil {
		logf("EROARE: %v", err)
		os.Exit(1)
	}
}
