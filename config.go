package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// StreamConf defines a publishable stream (Wowza "stream" in an "application").
type StreamConf struct {
	App     string   `json:"app"`
	Name    string   `json:"name"`
	Key     string   `json:"key"`
	Enabled bool     `json:"enabled"`
	Record  bool     `json:"record"`
	Push    []string `json:"push"`
	Note    string   `json:"note"`
}

// Token grants playback access (HLS / HTTP-FLV / HTTP-TS / RTMP play).
type Token struct {
	Name    string `json:"name"`
	Token   string `json:"token"`
	Streams string `json:"streams"` // "*" or comma list "live/tv1,live/tv2"
	Expires string `json:"expires"` // "" or YYYY-MM-DD
	Enabled bool   `json:"enabled"`
}

type Config struct {
	RTMPPort  int    `json:"rtmp_port"`
	HTTPPort  int    `json:"http_port"`
	AdminUser string `json:"admin_user"`
	AdminHash string `json:"admin_hash"` // salt$sha256(salt+pass)

	PublishAuth  bool `json:"publish_auth"`  // require stream key
	AllowUnknown bool `json:"allow_unknown"` // allow publishing streams not in list (only when publish_auth=false)
	PlayAuth     bool `json:"play_auth"`     // require token for playback

	HLSEnabled  bool `json:"hls_enabled"`
	HLSSegment  int  `json:"hls_segment_sec"`
	HLSListSize int  `json:"hls_list_size"`

	RecordDir       string `json:"record_dir"`
	RecordRotateMin int    `json:"record_rotate_min"`
	RecordKeepDays  int    `json:"record_keep_days"`

	Streams []StreamConf `json:"streams"`
	Tokens  []Token      `json:"tokens"`
}

var (
	cfgMu   sync.RWMutex
	cfg     *Config
	cfgPath string
	baseDir string
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,64}$`)

func defaultConfig() *Config {
	return &Config{
		RTMPPort:        1935,
		HTTPPort:        8080,
		AdminUser:       "admin",
		AdminHash:       hashPass("admin"),
		PublishAuth:     true,
		PlayAuth:        false,
		HLSEnabled:      true,
		HLSSegment:      2,
		HLSListSize:     6,
		RecordDir:       "recordings",
		RecordRotateMin: 60,
		RecordKeepDays:  7,
		Streams: []StreamConf{
			{App: "live", Name: "test", Key: randHex(8), Enabled: true},
		},
		Tokens: []Token{},
	}
}

func loadConfig() error {
	b, err := os.ReadFile(cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		c := defaultConfig()
		cfg = c
		logf("config nou creat: %s (login panou: admin / admin)", cfgPath)
		return saveConfigLocked()
	}
	if err != nil {
		return err
	}
	c := defaultConfig()
	c.Streams = nil
	if err := json.Unmarshal(b, c); err != nil {
		return fmt.Errorf("config invalid: %w", err)
	}
	normalize(c)
	cfg = c
	return nil
}

func normalize(c *Config) {
	if c.RTMPPort <= 0 {
		c.RTMPPort = 1935
	}
	if c.HTTPPort <= 0 {
		c.HTTPPort = 8080
	}
	if c.HLSSegment < 1 {
		c.HLSSegment = 2
	}
	if c.HLSListSize < 3 {
		c.HLSListSize = 3
	}
	if c.RecordRotateMin < 1 {
		c.RecordRotateMin = 60
	}
	if c.RecordDir == "" {
		c.RecordDir = "recordings"
	}
	if c.AdminUser == "" {
		c.AdminUser = "admin"
	}
	if c.AdminHash == "" {
		c.AdminHash = hashPass("admin")
	}
	if c.Streams == nil {
		c.Streams = []StreamConf{}
	}
	if c.Tokens == nil {
		c.Tokens = []Token{}
	}
	for i := range c.Streams {
		if c.Streams[i].Push == nil {
			c.Streams[i].Push = []string{}
		}
	}
}

func validate(c *Config) error {
	seen := map[string]bool{}
	for _, s := range c.Streams {
		if !nameRe.MatchString(s.App) || !nameRe.MatchString(s.Name) {
			return fmt.Errorf("nume invalid: %s/%s (doar litere, cifre, _ . -)", s.App, s.Name)
		}
		if s.App == "api" || s.App == "panel" || s.App == "rec" {
			return fmt.Errorf("aplicația nu poate fi numită %q", s.App)
		}
		k := s.App + "/" + s.Name
		if seen[k] {
			return fmt.Errorf("stream duplicat: %s", k)
		}
		seen[k] = true
		for _, p := range s.Push {
			if !strings.HasPrefix(p, "rtmp://") {
				return fmt.Errorf("push invalid (trebuie rtmp://): %s", p)
			}
		}
	}
	tk := map[string]bool{}
	for _, t := range c.Tokens {
		if len(t.Token) < 4 {
			return fmt.Errorf("token prea scurt pentru %q", t.Name)
		}
		if tk[t.Token] {
			return fmt.Errorf("token duplicat: %s", t.Token)
		}
		tk[t.Token] = true
		if t.Expires != "" {
			if _, err := time.Parse("2006-01-02", t.Expires); err != nil {
				return fmt.Errorf("dată expirare invalidă pentru %q (YYYY-MM-DD)", t.Name)
			}
		}
	}
	if c.RTMPPort > 65535 || c.HTTPPort > 65535 || c.RTMPPort == c.HTTPPort {
		return errors.New("porturi invalide")
	}
	return nil
}

func saveConfigLocked() error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := cfgPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, cfgPath)
}

func getCfg() Config {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	c := *cfg
	c.Streams = append([]StreamConf{}, cfg.Streams...)
	c.Tokens = append([]Token{}, cfg.Tokens...)
	return c
}

func (c *Config) findStream(app, name string) *StreamConf {
	for i := range c.Streams {
		if c.Streams[i].App == app && c.Streams[i].Name == name {
			return &c.Streams[i]
		}
	}
	return nil
}

func (c Config) recordDir() string {
	if filepath.IsAbs(c.RecordDir) {
		return c.RecordDir
	}
	return filepath.Join(baseDir, c.RecordDir)
}

// checkPublish validates a publish attempt.
func checkPublish(app, name, key string) (*StreamConf, error) {
	c := getCfg()
	sc := c.findStream(app, name)
	if sc == nil {
		if c.PublishAuth || !c.AllowUnknown {
			return nil, errors.New("stream necunoscut")
		}
		return &StreamConf{App: app, Name: name, Enabled: true}, nil
	}
	if !sc.Enabled {
		return nil, errors.New("stream dezactivat")
	}
	if c.PublishAuth && subtle.ConstantTimeCompare([]byte(sc.Key), []byte(key)) != 1 {
		return nil, errors.New("cheie greșită")
	}
	cp := *sc
	return &cp, nil
}

// checkPlay validates a playback token.
func checkPlay(app, name, token string) (string, bool) {
	c := getCfg()
	if !c.PlayAuth {
		return "", true
	}
	if token == "" {
		return "", false
	}
	today := time.Now().Format("2006-01-02")
	for _, t := range c.Tokens {
		if !t.Enabled || subtle.ConstantTimeCompare([]byte(t.Token), []byte(token)) != 1 {
			continue
		}
		if t.Expires != "" && today > t.Expires {
			return t.Name, false
		}
		if t.Streams == "" || strings.TrimSpace(t.Streams) == "*" {
			return t.Name, true
		}
		for _, s := range strings.Split(t.Streams, ",") {
			s = strings.TrimSpace(s)
			if s == app+"/"+name || s == app+"/*" {
				return t.Name, true
			}
		}
		return t.Name, false
	}
	return "", false
}

func hashPass(p string) string {
	salt := randHex(8)
	h := sha256.Sum256([]byte(salt + p))
	return salt + "$" + hex.EncodeToString(h[:])
}

func checkPass(stored, p string) bool {
	parts := strings.SplitN(stored, "$", 2)
	if len(parts) != 2 {
		return false
	}
	h := sha256.Sum256([]byte(parts[0] + p))
	return subtle.ConstantTimeCompare([]byte(parts[1]), []byte(hex.EncodeToString(h[:]))) == 1
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
