package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed dashboard.html
var dashboardHTML []byte

const (
	defaultPort  = 80
	fallbackPort = 7777
)

type Daemon struct {
	mu         sync.Mutex
	paths      paths
	port       int
	apps       map[string]*App
	startedAt  time.Time
	discoverOn bool
	discovered []Listener
	shutdown   chan struct{}
	mux        *http.ServeMux
	proxy      http.Handler
}

type daemonInfo struct {
	Service   string    `json:"service"`
	Version   string    `json:"version"`
	PID       int       `json:"pid"`
	Port      int       `json:"port"`
	URL       string    `json:"url"`
	StartedAt time.Time `json:"started_at"`
	Discover  bool      `json:"discover"`
	Config    string    `json:"config_file"`
}

// AppView is the API/CLI representation of an app: definition plus runtime state.
type AppView struct {
	Name        string            `json:"name"`
	URL         string            `json:"url"`
	Status      string            `json:"status"`
	Managed     bool              `json:"managed"`
	Port        int               `json:"port"`
	PortAuto    bool              `json:"port_auto"`
	Command     string            `json:"command,omitempty"`
	Cwd         string            `json:"cwd,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	RewriteHost bool              `json:"rewrite_host"`
	NoPreview   bool              `json:"no_preview"`
	PID         int               `json:"pid,omitempty"`
	ExitCode    *int              `json:"exit_code,omitempty"`
	StartedAt   *time.Time        `json:"started_at,omitempty"`
	Stale       bool              `json:"stale,omitempty"`
	LogFile     string            `json:"log_file,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

func (d *Daemon) baseURL(host string) string {
	if d.port == 80 {
		return "http://" + host
	}
	return fmt.Sprintf("http://%s:%d", host, d.port)
}

func (d *Daemon) appURL(name string) string { return d.baseURL(name + ".localhost") }

func (d *Daemon) info() daemonInfo {
	return daemonInfo{
		Service: "localdev", Version: version, PID: os.Getpid(), Port: d.port,
		URL: d.baseURL("localhost"), StartedAt: d.startedAt, Discover: d.discoverOn,
		Config: d.paths.appsFile(),
	}
}

func (d *Daemon) saveLocked() {
	cfgs := map[string]*AppConfig{}
	for n, a := range d.apps {
		cfgs[n] = a.cfg
	}
	if err := saveRegistry(d.paths, cfgs); err != nil {
		log.Printf("save registry: %v", err)
	}
}

func sameRunConfig(a, b *AppConfig) bool {
	if a.Command != b.Command || a.Cwd != b.Cwd || a.PortAuto != b.PortAuto || !maps.Equal(a.Env, b.Env) {
		return false
	}
	return a.PortAuto || a.Port == b.Port
}

// viewLocked snapshots an app; status probing happens later, outside the lock.
func (d *Daemon) viewLocked(a *App) AppView {
	c := a.cfg
	v := AppView{
		Name: c.Name, URL: d.appURL(c.Name), Managed: c.Command != "", Port: c.Port,
		PortAuto: c.PortAuto, Command: c.Command, Cwd: c.Cwd, Env: c.Env, RewriteHost: c.RewriteHost,
		NoPreview: c.NoPreview,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
	}
	if v.Managed {
		v.LogFile = d.paths.logFile(c.Name)
		v.Status = "stopped"
	}
	if p := a.proc; p != nil {
		if p.exited() {
			if !p.stopping {
				v.Status = "exited"
				code := p.exitCode
				v.ExitCode = &code
			}
		} else {
			v.Status = "starting"
			v.PID = p.pgid
			t := p.startedAt
			v.StartedAt = &t
			v.Stale = !sameRunConfig(&p.cfg, c)
		}
	}
	return v
}

func probe(v *AppView) {
	up := portOpen(v.Port)
	switch {
	case !v.Managed && up:
		v.Status = "running"
	case !v.Managed:
		v.Status = "down"
	case v.Status == "starting" && up:
		v.Status = "running"
	}
}

func (d *Daemon) views() []AppView {
	d.mu.Lock()
	vs := make([]AppView, 0, len(d.apps))
	for _, a := range d.apps {
		vs = append(vs, d.viewLocked(a))
	}
	d.mu.Unlock()
	var wg sync.WaitGroup
	for i := range vs {
		wg.Add(1)
		go func() { defer wg.Done(); probe(&vs[i]) }()
	}
	wg.Wait()
	sort.Slice(vs, func(i, j int) bool { return vs[i].Name < vs[j].Name })
	return vs
}

func (d *Daemon) view(name string) (AppView, bool) {
	d.mu.Lock()
	a := d.apps[name]
	if a == nil {
		d.mu.Unlock()
		return AppView{}, false
	}
	v := d.viewLocked(a)
	d.mu.Unlock()
	probe(&v)
	return v, true
}

// ---- HTTP plumbing ----

type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Error: msg, Code: code})
}

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.TrimSuffix(strings.ToLower(strings.Trim(h, "[]")), ".")
}

func (d *Daemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)
	switch {
	case host == "localhost" || host == "127.0.0.1" || host == "::1":
		d.mux.ServeHTTP(w, r)
	case strings.HasSuffix(host, ".localhost"):
		d.serveProxy(w, r, strings.TrimSuffix(host, ".localhost"))
	default:
		// Also rejects DNS-rebinding attempts against the API.
		http.Error(w, "localdev: unknown host "+host, http.StatusMisdirectedRequest)
	}
}

// guard protects state-changing endpoints from cross-site requests: browsers
// cannot send a custom header cross-origin without a CORS preflight, which we never grant.
func (d *Daemon) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Localdev") == "" {
			writeErr(w, http.StatusForbidden, "forbidden", "missing X-Localdev header")
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			if u := strings.TrimPrefix(o, "http://"); u == o || hostOnly(u) != "localhost" && hostOnly(u) != "127.0.0.1" {
				writeErr(w, http.StatusForbidden, "forbidden", "cross-origin request rejected")
				return
			}
		}
		h(w, r)
	}
}

func (d *Daemon) routes() {
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(dashboardHTML)
	})
	m.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, d.info())
	})
	m.HandleFunc("GET /api/docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write(agentDocs)
	})
	m.HandleFunc("GET /api/apps", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, d.views())
	})
	m.HandleFunc("GET /api/apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		if v, ok := d.view(r.PathValue("name")); ok {
			writeJSON(w, 200, v)
		} else {
			writeErr(w, 404, "not_found", "no app named "+r.PathValue("name"))
		}
	})
	m.HandleFunc("PUT /api/apps/{name}", d.guard(d.handlePut))
	m.HandleFunc("DELETE /api/apps/{name}", d.guard(d.handleDelete))
	m.HandleFunc("POST /api/apps/{name}/start", d.guard(d.handleStart(false)))
	m.HandleFunc("POST /api/apps/{name}/restart", d.guard(d.handleStart(true)))
	m.HandleFunc("POST /api/apps/{name}/stop", d.guard(d.handleStop))
	m.HandleFunc("GET /api/apps/{name}/logs", d.handleLogs)
	m.HandleFunc("GET /api/discover", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cached") != "" && d.discoverOn {
			d.mu.Lock()
			ls := d.discovered
			d.mu.Unlock()
			writeJSON(w, 200, nonNil(ls))
			return
		}
		ls, err := d.scan()
		if err != nil {
			writeErr(w, 500, "unsupported", err.Error())
			return
		}
		writeJSON(w, 200, nonNil(ls))
	})
	m.HandleFunc("POST /api/shutdown", d.guard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]bool{"ok": true})
		select {
		case d.shutdown <- struct{}{}:
		default:
		}
	}))
	m.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, 404, "not_found", "unknown endpoint "+r.Method+" "+r.URL.Path)
	})
	d.mux = m
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func (d *Daemon) scan() ([]Listener, error) {
	d.mu.Lock()
	registered := map[int]string{}
	for n, a := range d.apps {
		registered[a.cfg.Port] = n
	}
	d.mu.Unlock()
	return discover(map[int]bool{d.port: true}, registered)
}

type putRequest struct {
	Port        int               `json:"port"`
	Command     string            `json:"command"`
	Cwd         string            `json:"cwd"`
	Env         map[string]string `json:"env"`
	RewriteHost bool              `json:"rewrite_host"`
	NoPreview   bool              `json:"no_preview"`
}

// handlePut creates or replaces an app definition. It never starts or restarts processes;
// a running process whose definition changed is reported as "stale".
func (d *Daemon) handlePut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validName(name) {
		writeErr(w, 400, "bad_request", "invalid app name "+strconv.Quote(name)+": use lowercase letters, digits and dashes")
		return
	}
	var req putRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	if req.Command == "" && req.Port == 0 {
		writeErr(w, 400, "bad_request", "either port or command is required")
		return
	}
	if req.Port < 0 || req.Port > 65535 {
		writeErr(w, 400, "bad_request", "invalid port")
		return
	}
	if req.Cwd != "" && !filepath.IsAbs(req.Cwd) {
		writeErr(w, 400, "bad_request", "cwd must be an absolute path")
		return
	}
	d.mu.Lock()
	if req.Port == d.port {
		d.mu.Unlock()
		writeErr(w, 400, "bad_request", "port is used by the localdev daemon itself")
		return
	}
	now := time.Now().UTC()
	a := d.apps[name]
	status := 200
	if a == nil {
		a = &App{cfg: &AppConfig{Name: name, CreatedAt: now}}
		d.apps[name] = a
		status = 201
	}
	c := a.cfg
	wasAuto, oldPort := c.PortAuto, c.Port
	c.Command, c.Cwd, c.Env, c.RewriteHost, c.NoPreview = req.Command, req.Cwd, req.Env, req.RewriteHost, req.NoPreview
	c.PortAuto = req.Port == 0
	c.Port = req.Port
	if c.PortAuto && wasAuto {
		c.Port = oldPort // keep the previously assigned port stable
	}
	if c.PortAuto && c.Port == 0 {
		p, err := d.allocPortLocked(name)
		if err != nil {
			d.mu.Unlock()
			writeErr(w, 500, "internal", err.Error())
			return
		}
		c.Port = p
	}
	c.UpdatedAt = now
	d.saveLocked()
	v := d.viewLocked(a)
	d.mu.Unlock()
	probe(&v)
	writeJSON(w, status, v)
}

func (d *Daemon) handleDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	d.mu.Lock()
	a := d.apps[name]
	if a == nil {
		d.mu.Unlock()
		writeErr(w, 404, "not_found", "no app named "+name)
		return
	}
	delete(d.apps, name)
	d.saveLocked()
	p := a.proc
	d.mu.Unlock()
	if p != nil {
		d.stop(p)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "name": name})
}

type startRequest struct {
	Env []string `json:"env"`
}

// handleStart ensures the app runs with its current definition. It is idempotent:
// a running, up-to-date process is left alone unless force (restart) is set.
func (d *Daemon) handleStart(force bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var req startRequest
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeErr(w, 400, "bad_request", "invalid JSON body: "+err.Error())
				return
			}
		}
		d.mu.Lock()
		a := d.apps[name]
		if a == nil {
			d.mu.Unlock()
			writeErr(w, 404, "not_found", "no app named "+name)
			return
		}
		if a.cfg.Command == "" {
			d.mu.Unlock()
			writeErr(w, 409, "not_managed", name+" has no command; localdev only proxies to port "+strconv.Itoa(a.cfg.Port))
			return
		}
		p := a.proc
		upToDate := p != nil && sameRunConfig(&p.cfg, a.cfg)
		d.mu.Unlock()
		if p != nil && !p.exited() {
			if !force && upToDate {
				v, _ := d.view(name)
				writeJSON(w, 200, v)
				return
			}
			d.stop(p)
		}
		d.mu.Lock()
		if a.proc != nil && !a.proc.exited() { // raced with a concurrent start
			d.mu.Unlock()
			v, _ := d.view(name)
			writeJSON(w, 200, v)
			return
		}
		err := d.startLocked(a, req.Env)
		d.mu.Unlock()
		if err != nil {
			writeErr(w, 409, "start_failed", err.Error())
			return
		}
		v, _ := d.view(name)
		writeJSON(w, 200, v)
	}
}

func (d *Daemon) handleStop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	d.mu.Lock()
	a := d.apps[name]
	if a == nil {
		d.mu.Unlock()
		writeErr(w, 404, "not_found", "no app named "+name)
		return
	}
	p := a.proc
	d.mu.Unlock()
	if p != nil {
		d.stop(p)
	}
	v, _ := d.view(name)
	writeJSON(w, 200, v)
}

func (d *Daemon) handleLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	d.mu.Lock()
	_, ok := d.apps[name]
	d.mu.Unlock()
	if !ok {
		writeErr(w, 404, "not_found", "no app named "+name)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	if n <= 0 {
		n = 200
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(tailFile(d.paths.logFile(name), n)))
}

// tailFile returns the last n lines of a file (reading at most the final 1 MiB).
func tailFile(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, _ := f.Stat()
	const max = 1 << 20
	off := int64(0)
	if st.Size() > max {
		off = st.Size() - max
	}
	buf := make([]byte, st.Size()-off)
	n2, _ := f.ReadAt(buf, off)
	lines := strings.SplitAfter(string(buf[:n2]), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "")
}

// ---- daemon lifecycle ----

func listenLoopback(port int) ([]net.Listener, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	lns := []net.Listener{ln}
	if ln6, err := net.Listen("tcp", fmt.Sprintf("[::1]:%d", port)); err == nil {
		lns = append(lns, ln6)
	}
	return lns, nil
}

func cmdDaemonRun(args []string) int {
	fs := flag.NewFlagSet("daemon run", flag.ContinueOnError)
	port := fs.Int("port", envInt("LOCALDEV_PORT", 0), "port to listen on (default 80, falling back to 7777 if 80 is unavailable)")
	disc := fs.Bool("discover", os.Getenv("LOCALDEV_DISCOVER") == "1", "continuously scan for unregistered localhost services")
	interval := fs.Duration("discover-interval", 5*time.Second, "scan interval for --discover")
	if _, _, err := parseArgs(fs, args); err != nil {
		return fail(err)
	}
	p := getPaths()
	if err := os.MkdirAll(p.state, 0o755); err != nil {
		log.Print(err)
		return exitError
	}
	lock, err := os.OpenFile(p.lockFile(), os.O_CREATE|os.O_RDWR, 0o644)
	if err == nil {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err != nil {
		log.Printf("localdev: another daemon is already running (lock %s)", p.lockFile())
		return exitError
	}

	reapOrphans(p)
	cfgs, err := loadRegistry(p)
	if err != nil {
		log.Print(err)
		return exitError
	}
	d := &Daemon{paths: p, apps: map[string]*App{}, startedAt: time.Now().UTC(), discoverOn: *disc, shutdown: make(chan struct{}, 1)}
	for n, c := range cfgs {
		d.apps[n] = &App{cfg: c}
	}

	var lns []net.Listener
	if *port != 0 {
		lns, err = listenLoopback(*port)
		d.port = *port
	} else {
		lns, err = listenLoopback(defaultPort)
		d.port = defaultPort
		if err != nil {
			log.Printf("localdev: cannot listen on port %d (%v); using %d. See README for enabling port 80.", defaultPort, err, fallbackPort)
			lns, err = listenLoopback(fallbackPort)
			d.port = fallbackPort
		}
	}
	if err != nil {
		log.Printf("localdev: %v", err)
		return exitError
	}
	d.routes()
	d.proxy = d.newProxy()

	info, _ := json.MarshalIndent(d.info(), "", "  ")
	os.WriteFile(p.daemonFile(), append(info, '\n'), 0o644)
	log.Printf("localdev %s listening on %s (pid %d)", version, d.baseURL("localhost"), os.Getpid())

	srv := &http.Server{Handler: d, ReadHeaderTimeout: 30 * time.Second}
	for _, ln := range lns {
		go srv.Serve(ln)
	}
	if d.discoverOn {
		go d.discoverLoop(*interval)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-sig:
		log.Printf("localdev: received %v, shutting down", s)
	case <-d.shutdown:
		log.Printf("localdev: shutdown requested")
	}
	d.mu.Lock()
	var procs []*Proc
	for _, a := range d.apps {
		if a.proc != nil && !a.proc.exited() {
			procs = append(procs, a.proc)
		}
	}
	d.mu.Unlock()
	var wg sync.WaitGroup
	for _, pr := range procs {
		wg.Add(1)
		go func() { defer wg.Done(); d.stop(pr) }()
	}
	wg.Wait()
	os.Remove(p.daemonFile())
	os.Remove(p.procsFile())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	return exitOK
}

func (d *Daemon) discoverLoop(interval time.Duration) {
	for {
		ls, err := d.scan()
		if err == nil {
			d.mu.Lock()
			d.discovered = ls
			d.mu.Unlock()
		} else if errors.Is(err, errors.ErrUnsupported) {
			return
		}
		time.Sleep(interval)
	}
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}
