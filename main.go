// Rec-Martori — înregistrare "martori" TV cu FFmpeg + interfață web.
// Un singur executabil: pornește/oprește înregistrările, le repornește automat,
// arată starea live, fișierele, spațiul pe disc, logurile și un snapshot din sursă.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const version = "1.0.0"

//go:embed web/index.html
var indexHTML []byte

//go:embed web/login.html
var loginHTML []byte

// ───────────────────────────── config ─────────────────────────────

type RecCfg struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Input         string `json:"input"`
	OutDir        string `json:"out_dir"`
	Prefix        string `json:"prefix"`
	SegmentSec    int    `json:"segment_sec"`
	Container     string `json:"container"` // mp4 | mkv | ts
	RetentionDays int    `json:"retention_days"`
	AutoStart     bool   `json:"autostart"`
}

type Config struct {
	Listen          string   `json:"listen"`
	User            string   `json:"user"`
	Password        string   `json:"password"`
	FFmpeg          string   `json:"ffmpeg"`
	RestartDelaySec int      `json:"restart_delay_sec"`
	StallSec        int      `json:"stall_sec"`
	Recorders       []RecCfg `json:"recorders"`
}

func defaultConfig() *Config {
	ff := `C:\Users\produ\Documents\ffmpeg\bin\ffmpeg.exe`
	if runtime.GOOS != "windows" {
		ff = "ffmpeg"
	}
	return &Config{
		Listen: ":8090", User: "admin", Password: "admin", FFmpeg: ff,
		RestartDelaySec: 10, StallSec: 60,
		Recorders: []RecCfg{
			{ID: "m9tv", Name: "M9TV", Input: "rtmp://localhost/live/m9tv", OutDir: `D:\Martor`,
				Prefix: "Martor_M9Tv", SegmentSec: 7200, Container: "mp4", AutoStart: true},
			{ID: "m9plus", Name: "M9 Plus", Input: "rtmp://s2.netstreaming.eu/live/m9plus", OutDir: `D:\Martor_M9_Plus`,
				Prefix: "Martor_M9_Plus", SegmentSec: 7200, Container: "mp4", AutoStart: true},
		},
	}
}

var reBad = regexp.MustCompile(`[^A-Za-z0-9_\-]+`)

func slug(s string) string {
	s = strings.Trim(reBad.ReplaceAllString(strings.TrimSpace(s), "_"), "_")
	if s == "" {
		s = "rec"
	}
	return s
}

func (c *RecCfg) normalize() {
	c.Name = strings.TrimSpace(c.Name)
	c.Input = strings.TrimSpace(c.Input)
	c.OutDir = strings.TrimSpace(c.OutDir)
	c.Prefix = slug(c.Prefix)
	if c.Prefix == "rec" && c.Name != "" {
		c.Prefix = "Martor_" + slug(c.Name)
	}
	if c.SegmentSec < 60 {
		c.SegmentSec = 7200
	}
	switch c.Container {
	case "mp4", "mkv", "ts":
	default:
		c.Container = "mp4"
	}
	if c.RetentionDays < 0 {
		c.RetentionDays = 0
	}
}

func (c *RecCfg) ext() string { return "." + c.Container }

func (c *RecCfg) segFormat() string {
	switch c.Container {
	case "mkv":
		return "matroska"
	case "ts":
		return "mpegts"
	}
	return "mp4"
}

// ───────────────────────────── recorder ─────────────────────────────

type Event struct {
	T     time.Time `json:"t"`
	Level string    `json:"level"` // info | warn | err
	Msg   string    `json:"msg"`
}

type Recorder struct {
	app *App
	mu  sync.Mutex
	cfg RecCfg

	want         bool
	state        string // stopped | starting | recording | waiting
	pid          int
	procStart    time.Time
	sessionStart time.Time
	restarts     int
	lastProgress time.Time
	progTime     string
	bitrate      string
	speed        string
	size         string
	fps          string
	curFile      string
	lastErr      string
	logs         []string
	events       []Event
	stopCh       chan struct{}
	doneCh       chan struct{}

	// statistici disc (actualizate periodic)
	fileCount int
	fileBytes int64
	diskFree  uint64
	diskTotal uint64
}

func (r *Recorder) addLog(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	r.mu.Lock()
	r.logs = append(r.logs, time.Now().Format("15:04:05 ")+line)
	if len(r.logs) > 500 {
		r.logs = r.logs[len(r.logs)-500:]
	}
	r.mu.Unlock()
}

func (r *Recorder) event(level, msg string) {
	r.mu.Lock()
	r.events = append(r.events, Event{time.Now(), level, msg})
	if len(r.events) > 300 {
		r.events = r.events[len(r.events)-300:]
	}
	name := r.cfg.Name
	r.mu.Unlock()
	r.app.logLine(fmt.Sprintf("[%s] %s: %s", strings.ToUpper(level), name, msg))
}

func (r *Recorder) setState(s string) {
	r.mu.Lock()
	r.state = s
	r.mu.Unlock()
}

func (r *Recorder) Start() {
	r.mu.Lock()
	if r.want {
		r.mu.Unlock()
		return
	}
	r.want = true
	r.state = "starting"
	r.restarts = 0
	r.sessionStart = time.Now()
	r.stopCh = make(chan struct{})
	r.doneCh = make(chan struct{})
	stop, done := r.stopCh, r.doneCh
	r.mu.Unlock()
	r.event("info", "Înregistrare pornită")
	go r.loop(stop, done)
}

func (r *Recorder) Stop() {
	r.mu.Lock()
	if !r.want {
		r.mu.Unlock()
		return
	}
	r.want = false
	stop, done := r.stopCh, r.doneCh
	r.mu.Unlock()
	close(stop)
	<-done
	r.event("info", "Înregistrare oprită manual")
}

func (r *Recorder) Restart() {
	r.Stop()
	r.Start()
}

func (r *Recorder) loop(stop, done chan struct{}) {
	defer close(done)
	defer func() {
		r.mu.Lock()
		r.state, r.pid, r.curFile = "stopped", 0, ""
		r.progTime, r.bitrate, r.speed, r.size, r.fps = "", "", "", "", ""
		r.mu.Unlock()
	}()
	for {
		r.runOnce(stop)
		select {
		case <-stop:
			return
		default:
		}
		delay := time.Duration(r.app.settings().RestartDelaySec) * time.Second
		r.mu.Lock()
		r.state, r.pid = "waiting", 0
		r.mu.Unlock()
		select {
		case <-stop:
			return
		case <-time.After(delay):
		}
		r.mu.Lock()
		r.restarts++
		r.mu.Unlock()
	}
}

var (
	reTime    = regexp.MustCompile(`time=\s*(\S+)`)
	reBitrate = regexp.MustCompile(`bitrate=\s*(\S+)`)
	reSpeed   = regexp.MustCompile(`speed=\s*(\S+)`)
	reSize    = regexp.MustCompile(`size=\s*(\S+)`)
	reFps     = regexp.MustCompile(`fps=\s*(\S+)`)
	reOpening = regexp.MustCompile(`Opening '(.+)' for writing`)
)

func splitCRLF(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func sub(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

func (r *Recorder) readStderr(rd io.Reader) {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	sc.Split(splitCRLF)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.Contains(line, "time=") && strings.Contains(line, "bitrate=") {
			t := sub(reTime, line)
			r.mu.Lock()
			if t != "" && t != r.progTime && !strings.HasPrefix(t, "N/A") {
				r.lastProgress = time.Now()
				if r.state == "starting" {
					r.state = "recording"
				}
			}
			r.progTime = t
			r.bitrate = sub(reBitrate, line)
			r.speed = sub(reSpeed, line)
			r.size = sub(reSize, line)
			r.fps = sub(reFps, line)
			r.mu.Unlock()
			continue
		}
		if m := reOpening.FindStringSubmatch(line); m != nil {
			r.mu.Lock()
			r.curFile = filepath.Base(m[1])
			r.mu.Unlock()
			r.event("info", "Segment nou: "+filepath.Base(m[1]))
		}
		r.addLog(line)
		low := strings.ToLower(line)
		if strings.Contains(low, "error") || strings.Contains(low, "failed") || strings.Contains(low, "invalid") ||
			strings.Contains(low, "refused") || strings.Contains(low, "timed out") || strings.Contains(low, "not found") {
			r.mu.Lock()
			r.lastErr = line
			r.mu.Unlock()
		}
	}
}

func (r *Recorder) buildArgs() []string {
	r.mu.Lock()
	c := r.cfg
	r.mu.Unlock()
	out := filepath.Join(c.OutDir, c.Prefix+"_%Y-%m-%d_%H-%M-%S"+c.ext())
	return []string{
		"-hide_banner", "-loglevel", "info",
		"-rw_timeout", "15000000",
		"-i", c.Input,
		"-c:v", "copy", "-c:a", "copy",
		"-f", "segment",
		"-segment_time", fmt.Sprint(c.SegmentSec),
		"-segment_atclocktime", "1",
		"-reset_timestamps", "1",
		"-segment_format", c.segFormat(),
		"-break_non_keyframes", "1",
		"-strftime", "1",
		out,
	}
}

func (r *Recorder) runOnce(stop chan struct{}) {
	st := r.app.settings()
	r.mu.Lock()
	outDir := r.cfg.OutDir
	r.state = "starting"
	r.progTime, r.bitrate, r.speed, r.size, r.fps, r.curFile, r.lastErr = "", "", "", "", "", "", ""
	r.mu.Unlock()

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		r.mu.Lock()
		r.lastErr = "Nu pot crea folderul: " + err.Error()
		r.mu.Unlock()
		r.event("err", "Nu pot crea folderul "+outDir+": "+err.Error())
		return
	}

	args := r.buildArgs()
	cmd := exec.Command(st.FFmpeg, args...)
	prepareCmd(cmd)
	stdin, _ := cmd.StdinPipe()
	stderr, err := cmd.StderrPipe()
	if err != nil {
		r.event("err", "stderr pipe: "+err.Error())
		return
	}
	r.addLog("$ " + st.FFmpeg + " " + strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		r.mu.Lock()
		r.lastErr = "Nu pot porni ffmpeg: " + err.Error()
		r.mu.Unlock()
		r.event("err", "Nu pot porni ffmpeg: "+err.Error())
		return
	}
	attachJob(cmd)

	now := time.Now()
	r.mu.Lock()
	r.pid = cmd.Process.Pid
	r.procStart = now
	r.lastProgress = now
	r.mu.Unlock()

	readDone := make(chan struct{})
	go func() { r.readStderr(stderr); close(readDone) }()
	exitCh := make(chan error, 1)
	go func() { <-readDone; exitCh <- cmd.Wait() }()

	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	killed := false
	for {
		select {
		case err := <-exitCh:
			r.mu.Lock()
			le := r.lastErr
			dur := time.Since(r.procStart).Round(time.Second)
			r.mu.Unlock()
			msg := fmt.Sprintf("ffmpeg s-a oprit după %s", dur)
			if err != nil {
				msg += " (" + err.Error() + ")"
			}
			if le != "" {
				msg += " — " + le
			}
			r.event("warn", msg)
			return
		case <-stop:
			if stdin != nil {
				stdin.Write([]byte("q"))
			}
			select {
			case <-exitCh:
			case <-time.After(15 * time.Second):
				cmd.Process.Kill()
				<-exitCh
			}
			return
		case <-tick.C:
			if killed {
				continue
			}
			stall := time.Duration(r.app.settings().StallSec) * time.Second
			r.mu.Lock()
			idle := time.Since(r.lastProgress)
			r.mu.Unlock()
			if stall > 0 && idle > stall {
				killed = true
				r.event("warn", fmt.Sprintf("Fără date de %s — repornesc ffmpeg", idle.Round(time.Second)))
				cmd.Process.Kill()
			}
		}
	}
}

// fișierele înregistrate de acest recorder
type FileInfo struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	Current bool      `json:"current"`
}

func (r *Recorder) listFiles() ([]FileInfo, error) {
	r.mu.Lock()
	c := r.cfg
	cur := r.curFile
	r.mu.Unlock()
	ents, err := os.ReadDir(c.OutDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []FileInfo{}, nil
		}
		return nil, err
	}
	out := []FileInfo{}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, c.Prefix+"_") {
			continue
		}
		ext := strings.ToLower(filepath.Ext(n))
		if ext != ".mp4" && ext != ".mkv" && ext != ".ts" {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, FileInfo{n, fi.Size(), fi.ModTime(), n == cur})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

func (r *Recorder) refreshStats() {
	files, _ := r.listFiles()
	var total int64
	for _, f := range files {
		total += f.Size
	}
	r.mu.Lock()
	dir := r.cfg.OutDir
	r.mu.Unlock()
	free, tot, _ := diskUsage(existingAncestor(dir))
	r.mu.Lock()
	r.fileCount, r.fileBytes, r.diskFree, r.diskTotal = len(files), total, free, tot
	r.mu.Unlock()
}

func (r *Recorder) cleanup() {
	r.mu.Lock()
	days := r.cfg.RetentionDays
	dir := r.cfg.OutDir
	r.mu.Unlock()
	if days <= 0 {
		return
	}
	limit := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	files, _ := r.listFiles()
	n := 0
	for _, f := range files {
		if !f.Current && f.ModTime.Before(limit) {
			if os.Remove(filepath.Join(dir, f.Name)) == nil {
				n++
			}
		}
	}
	if n > 0 {
		r.event("info", fmt.Sprintf("Retenție %d zile: șterse %d fișiere vechi", days, n))
	}
}

func existingAncestor(p string) string {
	p = filepath.Clean(p)
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// ───────────────────────────── app ─────────────────────────────

type snapCache struct {
	t    time.Time
	data []byte
}

type App struct {
	mu       sync.Mutex
	cfg      *Config
	cfgPath  string
	recs     map[string]*Recorder
	order    []string
	sessions map[string]time.Time
	logF     *os.File
	logMu    sync.Mutex
	ffVer    string

	snapMu sync.Mutex
	snaps  map[string]snapCache
}

func (a *App) settings() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := *a.cfg
	c.Recorders = nil
	return c
}

func (a *App) logLine(s string) {
	line := time.Now().Format("2006-01-02 15:04:05 ") + s
	log.Println(s)
	a.logMu.Lock()
	if a.logF != nil {
		a.logF.WriteString(line + "\n")
	}
	a.logMu.Unlock()
}

func (a *App) save() error {
	a.mu.Lock()
	a.cfg.Recorders = a.cfg.Recorders[:0]
	for _, id := range a.order {
		r := a.recs[id]
		r.mu.Lock()
		a.cfg.Recorders = append(a.cfg.Recorders, r.cfg)
		r.mu.Unlock()
	}
	b, err := json.MarshalIndent(a.cfg, "", "  ")
	a.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := a.cfgPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.cfgPath)
}

func (a *App) get(id string) *Recorder {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.recs[id]
}

func (a *App) list() []*Recorder {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*Recorder, 0, len(a.order))
	for _, id := range a.order {
		out = append(out, a.recs[id])
	}
	return out
}

func (a *App) addRecorder(c RecCfg) *Recorder {
	r := &Recorder{app: a, cfg: c, state: "stopped"}
	a.mu.Lock()
	a.recs[c.ID] = r
	a.order = append(a.order, c.ID)
	a.mu.Unlock()
	return r
}

func (a *App) checkFFmpeg() {
	ff := a.settings().FFmpeg
	cmd := exec.Command(ff, "-hide_banner", "-version")
	prepareCmd(cmd)
	out, err := cmd.Output()
	v := ""
	if err != nil {
		v = "EROARE: ffmpeg nu poate fi rulat (" + err.Error() + ")"
	} else {
		v = strings.SplitN(string(out), "\n", 2)[0]
		if i := strings.Index(v, " Copyright"); i > 0 {
			v = v[:i]
		}
	}
	a.mu.Lock()
	a.ffVer = strings.TrimSpace(v)
	a.mu.Unlock()
}

func (a *App) background() {
	stats := time.NewTicker(30 * time.Second)
	clean := time.NewTicker(30 * time.Minute)
	for _, r := range a.list() {
		r.refreshStats()
		r.cleanup()
	}
	for {
		select {
		case <-stats.C:
			for _, r := range a.list() {
				r.refreshStats()
			}
		case <-clean.C:
			for _, r := range a.list() {
				r.cleanup()
			}
		}
	}
}

// ───────────────────────────── HTTP ─────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func newToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *App) authed(r *http.Request) bool {
	c, err := r.Cookie("rm_session")
	if err != nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[c.Value]
	if !ok || time.Now().After(exp) {
		delete(a.sessions, c.Value)
		return false
	}
	return true
}

func (a *App) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authed(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "neautentificat", http.StatusUnauthorized)
			} else {
				http.Redirect(w, r, "/login", http.StatusFound)
			}
			return
		}
		h(w, r)
	}
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		u, p := r.FormValue("user"), r.FormValue("pass")
		st := a.settings()
		if subtle.ConstantTimeCompare([]byte(u), []byte(st.User)) == 1 &&
			subtle.ConstantTimeCompare([]byte(p), []byte(st.Password)) == 1 {
			t := newToken()
			a.mu.Lock()
			a.sessions[t] = time.Now().Add(30 * 24 * time.Hour)
			a.mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: "rm_session", Value: t, Path: "/", HttpOnly: true,
				SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		a.logLine("Login eșuat de la " + r.RemoteAddr)
		time.Sleep(time.Second)
		http.Redirect(w, r, "/login?err=1", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(loginHTML)
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("rm_session"); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "rm_session", Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
}

type RecStatus struct {
	RecCfg
	State     string  `json:"state"`
	PID       int     `json:"pid"`
	Uptime    int64   `json:"uptime"`
	Session   int64   `json:"session"`
	Restarts  int     `json:"restarts"`
	ProgTime  string  `json:"prog_time"`
	Bitrate   string  `json:"bitrate"`
	Speed     string  `json:"speed"`
	Size      string  `json:"size"`
	FPS       string  `json:"fps"`
	CurFile   string  `json:"cur_file"`
	LastErr   string  `json:"last_err"`
	IdleSec   float64 `json:"idle_sec"`
	FileCount int     `json:"file_count"`
	FileBytes int64   `json:"file_bytes"`
	DiskFree  uint64  `json:"disk_free"`
	DiskTotal uint64  `json:"disk_total"`
	Events    []Event `json:"events"`
}

func (r *Recorder) status() RecStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := RecStatus{RecCfg: r.cfg, State: r.state, PID: r.pid, Restarts: r.restarts,
		ProgTime: r.progTime, Bitrate: r.bitrate, Speed: r.speed, Size: r.size, FPS: r.fps,
		CurFile: r.curFile, LastErr: r.lastErr, FileCount: r.fileCount, FileBytes: r.fileBytes,
		DiskFree: r.diskFree, DiskTotal: r.diskTotal}
	if r.pid != 0 {
		s.Uptime = int64(time.Since(r.procStart).Seconds())
		s.IdleSec = time.Since(r.lastProgress).Seconds()
	}
	if r.want {
		s.Session = int64(time.Since(r.sessionStart).Seconds())
	}
	n := len(r.events)
	k := n - 8
	if k < 0 {
		k = 0
	}
	s.Events = append([]Event{}, r.events[k:]...)
	return s
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	recs := a.list()
	list := make([]RecStatus, 0, len(recs))
	for _, rc := range recs {
		list = append(list, rc.status())
	}
	a.mu.Lock()
	fv := a.ffVer
	a.mu.Unlock()
	host, _ := os.Hostname()
	writeJSON(w, map[string]any{"recorders": list, "ffmpeg": fv, "version": version,
		"host": host, "now": time.Now(), "os": runtime.GOOS})
}

func (a *App) recFromReq(w http.ResponseWriter, r *http.Request) *Recorder {
	rc := a.get(r.URL.Query().Get("id"))
	if rc == nil {
		http.Error(w, "canal inexistent", http.StatusNotFound)
	}
	return rc
}

func (a *App) handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	act := strings.TrimPrefix(r.URL.Path, "/api/")
	switch act {
	case "startall":
		for _, rc := range a.list() {
			rc.Start()
		}
	case "stopall":
		var wg sync.WaitGroup
		for _, rc := range a.list() {
			wg.Add(1)
			go func(x *Recorder) { defer wg.Done(); x.Stop() }(rc)
		}
		wg.Wait()
	default:
		rc := a.recFromReq(w, r)
		if rc == nil {
			return
		}
		switch act {
		case "start":
			rc.Start()
		case "stop":
			rc.Stop()
		case "restart":
			rc.Restart()
		}
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleLogs(w http.ResponseWriter, r *http.Request) {
	rc := a.recFromReq(w, r)
	if rc == nil {
		return
	}
	rc.mu.Lock()
	logs := append([]string{}, rc.logs...)
	ev := append([]Event{}, rc.events...)
	rc.mu.Unlock()
	args := rc.buildArgs()
	writeJSON(w, map[string]any{"logs": logs, "events": ev,
		"cmd": a.settings().FFmpeg + " " + strings.Join(args, " ")})
}

func (a *App) handleFiles(w http.ResponseWriter, r *http.Request) {
	rc := a.recFromReq(w, r)
	if rc == nil {
		return
	}
	files, err := rc.listFiles()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, files)
}

func (a *App) resolveFile(w http.ResponseWriter, r *http.Request) (*Recorder, string, bool) {
	rc := a.recFromReq(w, r)
	if rc == nil {
		return nil, "", false
	}
	name := filepath.Base(r.URL.Query().Get("name"))
	files, _ := rc.listFiles()
	for _, f := range files {
		if f.Name == name {
			rc.mu.Lock()
			dir := rc.cfg.OutDir
			rc.mu.Unlock()
			return rc, filepath.Join(dir, name), f.Current
		}
	}
	http.Error(w, "fișier inexistent", http.StatusNotFound)
	return nil, "", false
}

func (a *App) handleFile(w http.ResponseWriter, r *http.Request) {
	_, path, _ := a.resolveFile(w, r)
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4":
		w.Header().Set("Content-Type", "video/mp4")
	case ".ts":
		w.Header().Set("Content-Type", "video/mp2t")
	case ".mkv":
		w.Header().Set("Content-Type", "video/x-matroska")
	}
	if r.URL.Query().Get("dl") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(path)+`"`)
	}
	http.ServeContent(w, r, filepath.Base(path), fi.ModTime(), f)
}

func (a *App) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	rc, path, cur := a.resolveFile(w, r)
	if path == "" {
		return
	}
	if cur {
		http.Error(w, "fișierul se înregistrează acum", http.StatusConflict)
		return
	}
	if err := os.Remove(path); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	rc.event("info", "Fișier șters din interfață: "+filepath.Base(path))
	go rc.refreshStats()
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleSnap(w http.ResponseWriter, r *http.Request) {
	rc := a.recFromReq(w, r)
	if rc == nil {
		return
	}
	rc.mu.Lock()
	id, input := rc.cfg.ID, rc.cfg.Input
	rc.mu.Unlock()

	a.snapMu.Lock()
	if c, ok := a.snaps[id]; ok && time.Since(c.t) < 15*time.Second && r.URL.Query().Get("force") == "" {
		a.snapMu.Unlock()
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(c.data)
		return
	}
	a.snapMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.settings().FFmpeg, "-hide_banner", "-loglevel", "error",
		"-rw_timeout", "10000000", "-i", input, "-frames:v", "1", "-an",
		"-vf", "scale=640:-2", "-q:v", "4", "-f", "image2", "-c:v", "mjpeg", "pipe:1")
	prepareCmd(cmd)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil || out.Len() == 0 {
		msg := strings.TrimSpace(errb.String())
		if msg == "" && err != nil {
			msg = err.Error()
		}
		http.Error(w, "snapshot eșuat: "+msg, http.StatusBadGateway)
		return
	}
	a.snapMu.Lock()
	a.snaps[id] = snapCache{time.Now(), out.Bytes()}
	a.snapMu.Unlock()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(out.Bytes())
}

func (a *App) handleSaveRec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	var c RecCfg
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, "JSON invalid", 400)
		return
	}
	c.normalize()
	if c.Name == "" || c.Input == "" || c.OutDir == "" {
		http.Error(w, "Nume, sursă și folder sunt obligatorii", 400)
		return
	}
	running := false
	if rc := a.get(c.ID); c.ID != "" && rc != nil {
		rc.mu.Lock()
		rc.cfg = c
		running = rc.want
		rc.mu.Unlock()
		rc.event("info", "Configurație modificată")
		go rc.refreshStats()
	} else {
		base := strings.ToLower(slug(c.Name))
		id := base
		for i := 2; a.get(id) != nil; i++ {
			id = fmt.Sprintf("%s%d", base, i)
		}
		c.ID = id
		rc := a.addRecorder(c)
		rc.event("info", "Canal adăugat")
		go rc.refreshStats()
		if c.AutoStart {
			rc.Start()
		}
	}
	if err := a.save(); err != nil {
		http.Error(w, "salvare eșuată: "+err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": c.ID, "restart_needed": running})
}

func (a *App) handleDeleteRec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	rc := a.recFromReq(w, r)
	if rc == nil {
		return
	}
	rc.Stop()
	id := r.URL.Query().Get("id")
	a.mu.Lock()
	delete(a.recs, id)
	for i, x := range a.order {
		if x == id {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
	a.mu.Unlock()
	a.logLine("Canal șters: " + id + " (fișierele rămân pe disc)")
	if err := a.save(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var in struct {
			Listen          string `json:"listen"`
			User            string `json:"user"`
			Password        string `json:"password"`
			FFmpeg          string `json:"ffmpeg"`
			RestartDelaySec int    `json:"restart_delay_sec"`
			StallSec        int    `json:"stall_sec"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "JSON invalid", 400)
			return
		}
		a.mu.Lock()
		if s := strings.TrimSpace(in.Listen); s != "" {
			a.cfg.Listen = s
		}
		if s := strings.TrimSpace(in.User); s != "" {
			a.cfg.User = s
		}
		if in.Password != "" {
			a.cfg.Password = in.Password
		}
		if s := strings.TrimSpace(in.FFmpeg); s != "" {
			a.cfg.FFmpeg = s
		}
		if in.RestartDelaySec >= 1 {
			a.cfg.RestartDelaySec = in.RestartDelaySec
		}
		if in.StallSec >= 0 {
			a.cfg.StallSec = in.StallSec
		}
		a.mu.Unlock()
		if err := a.save(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		a.checkFFmpeg()
		a.logLine("Setări modificate")
	}
	st := a.settings()
	writeJSON(w, map[string]any{"listen": st.Listen, "user": st.User, "ffmpeg": st.FFmpeg,
		"restart_delay_sec": st.RestartDelaySec, "stall_sec": st.StallSec, "config_path": a.cfgPath})
}

// ───────────────────────────── main ─────────────────────────────

func main() {
	exe, _ := os.Executable()
	baseDir := filepath.Dir(exe)
	cfgPath := flag.String("config", filepath.Join(baseDir, "config.json"), "fișier configurare")
	flag.Parse()

	a := &App{cfgPath: *cfgPath, recs: map[string]*Recorder{}, sessions: map[string]time.Time{},
		snaps: map[string]snapCache{}}

	cfg := defaultConfig()
	if b, err := os.ReadFile(*cfgPath); err == nil {
		cfg = &Config{}
		if err := json.Unmarshal(b, cfg); err != nil {
			log.Fatalf("config.json invalid: %v", err)
		}
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8090"
	}
	if cfg.User == "" {
		cfg.User = "admin"
	}
	if cfg.Password == "" {
		cfg.Password = "admin"
	}
	if cfg.FFmpeg == "" {
		cfg.FFmpeg = defaultConfig().FFmpeg
	}
	if cfg.RestartDelaySec < 1 {
		cfg.RestartDelaySec = 10
	}
	a.cfg = cfg
	recs := cfg.Recorders
	cfg.Recorders = nil
	seen := map[string]bool{}
	for _, c := range recs {
		c.normalize()
		if c.ID == "" || seen[c.ID] {
			c.ID = strings.ToLower(slug(c.Name))
			for i := 2; seen[c.ID]; i++ {
				c.ID = fmt.Sprintf("%s%d", strings.ToLower(slug(c.Name)), i)
			}
		}
		seen[c.ID] = true
		a.addRecorder(c)
	}
	if err := a.save(); err != nil {
		log.Printf("Atenție: nu pot salva %s: %v", *cfgPath, err)
	}

	if f, err := os.OpenFile(filepath.Join(filepath.Dir(*cfgPath), "rec-martori.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		a.logF = f
	}
	a.logLine("Rec-Martori " + version + " pornit")
	a.checkFFmpeg()
	a.logLine("FFmpeg: " + a.ffVer)

	for _, r := range a.list() {
		if r.cfg.AutoStart {
			r.Start()
		}
	}
	go a.background()

	mux := http.NewServeMux()
	mux.HandleFunc("/login", a.handleLogin)
	mux.HandleFunc("/logout", a.handleLogout)
	mux.HandleFunc("/", a.guard(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	}))
	mux.HandleFunc("/api/status", a.guard(a.handleStatus))
	for _, p := range []string{"start", "stop", "restart", "startall", "stopall"} {
		mux.HandleFunc("/api/"+p, a.guard(a.handleAction))
	}
	mux.HandleFunc("/api/logs", a.guard(a.handleLogs))
	mux.HandleFunc("/api/files", a.guard(a.handleFiles))
	mux.HandleFunc("/api/file", a.guard(a.handleFile))
	mux.HandleFunc("/api/file/delete", a.guard(a.handleFileDelete))
	mux.HandleFunc("/api/snap", a.guard(a.handleSnap))
	mux.HandleFunc("/api/rec/save", a.guard(a.handleSaveRec))
	mux.HandleFunc("/api/rec/delete", a.guard(a.handleDeleteRec))
	mux.HandleFunc("/api/settings", a.guard(a.handleSettings))

	srv := &http.Server{Addr: a.settings().Listen, Handler: mux}
	go func() {
		a.logLine("Interfață web: http://localhost" + displayPort(srv.Addr) + "  (utilizator: " + a.settings().User + ")")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	a.logLine("Oprire... închid înregistrările curat")
	var wg sync.WaitGroup
	for _, r := range a.list() {
		wg.Add(1)
		go func(x *Recorder) { defer wg.Done(); x.Stop() }(r)
	}
	wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	a.logLine("Oprit.")
}

func displayPort(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return ":" + addr
}
