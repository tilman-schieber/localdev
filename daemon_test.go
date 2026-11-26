package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestDaemon returns a daemon with its state in a temp dir. It isn't listening anywhere:
// tests call its handler directly, or wrap it in httptest.NewServer for streaming.
func newTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	dir := t.TempDir()
	d := &Daemon{paths: paths{config: dir, state: dir}, apps: map[string]*App{}, port: 1,
		startedAt: time.Now(), shutdown: make(chan struct{}, 1)}
	d.routes()
	d.proxy = d.newProxy()
	t.Cleanup(func() {
		d.mu.Lock()
		var ps []*Proc
		for _, a := range d.apps {
			if a.proc != nil {
				ps = append(ps, a.proc)
			}
		}
		d.mu.Unlock()
		for _, p := range ps {
			d.stop(p)
		}
		// The exit goroutine still saves procs.json after p.done closes; wait for it so the
		// temp dir can be removed.
		d.mu.Lock()
		d.mu.Unlock()
	})
	return d
}

type apiCall struct {
	method, path string
	body         any
	headers      map[string]string
}

func (d *Daemon) call(t *testing.T, c apiCall) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if c.body != nil {
		b, _ := json.Marshal(c.body)
		rd = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(c.method, "http://localhost"+c.path, rd)
	if c.method != "GET" {
		req.Header.Set("X-Localdev", "test")
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	return rec
}

func (d *Daemon) mustView(t *testing.T, name string) AppView {
	t.Helper()
	v, ok := d.view(name)
	if !ok {
		t.Fatalf("no app %s", name)
	}
	return v
}

func waitStatus(t *testing.T, d *Daemon, name, want string) AppView {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		v := d.mustView(t, name)
		if v.Status == want {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: status %q, want %q; log:\n%s", name, v.Status, want, tailFile(d.paths.logFile(name), 30))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func needPython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	pyServer = pyCommand(t, "$PORT", "")
}

// pyHTTP starts a minimal HTTP server. Python's own HTTPServer.server_bind does a reverse DNS
// lookup (socket.getfqdn) before listening, which takes many seconds on macOS CI runners.
const pyHTTP = `import os, sys, socketserver, http.server
class S(http.server.HTTPServer):
    def server_bind(self):
        socketserver.TCPServer.server_bind(self)
        self.server_name, self.server_port = "localhost", self.server_address[1]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        ok = len(sys.argv) < 3 or os.path.exists(sys.argv[2])
        self.send_response(200 if ok else 503)
        self.send_header("Content-Length", "0")
        self.end_headers()
    def log_message(self, *a): pass
S(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
`

var pyServer string // command for an app that listens on $PORT; set by needPython

// pyCommand runs pyHTTP on port (a number, or "$PORT"). With readyFile, it answers 503 until
// that file exists.
func pyCommand(t *testing.T, port, readyFile string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "srv.py")
	if err := os.WriteFile(script, []byte(pyHTTP), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := "exec python3 " + script + " " + port
	if readyFile != "" {
		cmd += " " + readyFile
	}
	return cmd
}

func TestPutValidation(t *testing.T) {
	d := newTestDaemon(t)
	for _, c := range []struct {
		name string
		call apiCall
		want int
	}{
		{"bad name", apiCall{method: "PUT", path: "/api/apps/Bad_Name", body: map[string]any{"port": 3000}}, 400},
		{"no port or command", apiCall{method: "PUT", path: "/api/apps/x", body: map[string]any{}}, 400},
		{"relative cwd", apiCall{method: "PUT", path: "/api/apps/x", body: map[string]any{"port": 3000, "cwd": "rel"}}, 400},
		{"daemon port", apiCall{method: "PUT", path: "/api/apps/x", body: map[string]any{"port": 1}}, 400},
		{"bad idle_stop", apiCall{method: "PUT", path: "/api/apps/x", body: map[string]any{"port": 3000, "idle_stop": "soon"}}, 400},
		{"bad ready_path", apiCall{method: "PUT", path: "/api/apps/x", body: map[string]any{"port": 3000, "ready_path": "health"}}, 400},
		{"cross-origin", apiCall{method: "PUT", path: "/api/apps/x", body: map[string]any{"port": 3000},
			headers: map[string]string{"Origin": "http://evil.example"}}, 403},
		{"created", apiCall{method: "PUT", path: "/api/apps/x", body: map[string]any{"port": 3000, "idle_stop": "off"}}, 201},
		{"updated", apiCall{method: "PUT", path: "/api/apps/x", body: map[string]any{"port": 3001}}, 200},
	} {
		if rec := d.call(t, c.call); rec.Code != c.want {
			t.Errorf("%s: got %d, want %d: %s", c.name, rec.Code, c.want, rec.Body)
		}
	}
	// State-changing requests need X-Localdev.
	req := httptest.NewRequest("DELETE", "http://localhost/api/apps/x", nil)
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("missing header: got %d, want 403", rec.Code)
	}
	// Only localhost Host headers reach the API (DNS rebinding).
	req = httptest.NewRequest("GET", "http://evil.example/api/apps", nil)
	rec = httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("foreign host: got %d", rec.Code)
	}
}

func TestAutoPortStable(t *testing.T) {
	d := newTestDaemon(t)
	d.call(t, apiCall{method: "PUT", path: "/api/apps/web", body: map[string]any{"command": "true"}})
	p1 := d.mustView(t, "web").Port
	d.call(t, apiCall{method: "PUT", path: "/api/apps/web", body: map[string]any{"command": "true --changed"}})
	if p2 := d.mustView(t, "web").Port; p1 != p2 || p1 < autoPortMin || p1 > autoPortMax {
		t.Fatalf("auto port changed or out of range: %d then %d", p1, p2)
	}
	d.call(t, apiCall{method: "PUT", path: "/api/apps/api", body: map[string]any{"command": "true"}})
	if d.mustView(t, "api").Port == p1 {
		t.Fatal("two apps got the same auto port")
	}
}

func TestLifecycle(t *testing.T) {
	needPython(t)
	d := newTestDaemon(t)
	d.call(t, apiCall{method: "PUT", path: "/api/apps/web", body: map[string]any{"command": pyServer, "cwd": t.TempDir()}})
	if v := d.mustView(t, "web"); v.Status != "idle" {
		t.Fatalf("never-started app: status %q, want idle", v.Status)
	}

	d.call(t, apiCall{method: "POST", path: "/api/apps/web/start"})
	pid := waitStatus(t, d, "web", "running").PID

	// start is idempotent.
	d.call(t, apiCall{method: "POST", path: "/api/apps/web/start"})
	if v := d.mustView(t, "web"); v.PID != pid {
		t.Fatalf("idempotent start restarted the app (pid %d -> %d)", pid, v.PID)
	}

	// Changing the definition makes it stale; start then restarts it.
	d.call(t, apiCall{method: "PUT", path: "/api/apps/web", body: map[string]any{"command": pyServer, "cwd": t.TempDir(), "env": map[string]string{"X": "1"}}})
	if v := d.mustView(t, "web"); !v.Stale {
		t.Fatal("changed definition not reported as stale")
	}
	d.call(t, apiCall{method: "POST", path: "/api/apps/web/start"})
	v := waitStatus(t, d, "web", "running")
	if v.PID == pid || v.Stale {
		t.Fatalf("stale app not restarted: pid %d -> %d, stale %v", pid, v.PID, v.Stale)
	}

	// Explicitly stopped apps stay down: a request doesn't start them.
	d.call(t, apiCall{method: "POST", path: "/api/apps/web/stop"})
	waitStatus(t, d, "web", "stopped")
	rec := proxyGet(d, "web", "/")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("request to stopped app: got %d", rec.Code)
	}
	if v := d.mustView(t, "web"); v.Status != "stopped" {
		t.Fatalf("request started a stopped app (status %s)", v.Status)
	}
}

func proxyGet(d *Daemon, app, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "http://"+app+".localhost"+path, nil)
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	return rec
}

func TestCrashedAppStaysDown(t *testing.T) {
	d := newTestDaemon(t)
	d.call(t, apiCall{method: "PUT", path: "/api/apps/crash", body: map[string]any{"command": "exit 3"}})
	d.call(t, apiCall{method: "POST", path: "/api/apps/crash/start"})
	v := waitStatus(t, d, "crash", "exited")
	if v.ExitCode == nil || *v.ExitCode != 3 {
		t.Fatalf("exit code %v", v.ExitCode)
	}
	proxyGet(d, "crash", "/")
	if v := d.mustView(t, "crash"); v.Status != "exited" {
		t.Fatalf("request restarted a crashed app (status %s)", v.Status)
	}
}

func TestIdleStop(t *testing.T) {
	needPython(t)
	d := newTestDaemon(t)
	d.call(t, apiCall{method: "PUT", path: "/api/apps/web", body: map[string]any{"command": pyServer, "cwd": t.TempDir(), "idle_stop": "1h"}})
	d.call(t, apiCall{method: "PUT", path: "/api/apps/keep", body: map[string]any{"command": pyServer, "cwd": t.TempDir(), "idle_stop": "off"}})
	d.call(t, apiCall{method: "POST", path: "/api/apps/web/start"})
	d.call(t, apiCall{method: "POST", path: "/api/apps/keep/start"})
	waitStatus(t, d, "web", "running")
	waitStatus(t, d, "keep", "running")

	// A request in flight (an open WebSocket, say) keeps the app up.
	d.mu.Lock()
	d.apps["web"].active = 1
	d.mu.Unlock()
	d.idleCheck(time.Now().Add(2 * time.Hour))
	if v := d.mustView(t, "web"); v.Status != "running" {
		t.Fatalf("app with an active request was stopped (status %s)", v.Status)
	}
	d.mu.Lock()
	d.apps["web"].active = 0
	d.mu.Unlock()

	d.idleCheck(time.Now().Add(30 * time.Minute))
	if v := d.mustView(t, "web"); v.Status != "running" {
		t.Fatalf("app stopped before its idle timeout (status %s)", v.Status)
	}
	d.idleCheck(time.Now().Add(2 * time.Hour))
	waitStatus(t, d, "web", "idle")
	if v := d.mustView(t, "keep"); v.Status != "running" {
		t.Fatalf("idle_stop off app was stopped (status %s)", v.Status)
	}

	// The next request starts it again and is answered once it listens.
	rec := proxyGet(d, "web", "/")
	if rec.Code != 200 {
		t.Fatalf("request to idle app: got %d: %s", rec.Code, rec.Body)
	}
	v := waitStatus(t, d, "web", "running")
	if v.LastRequest == nil {
		t.Fatal("last_request_at not recorded")
	}

	// The daemon-wide default applies to apps without their own setting.
	d.call(t, apiCall{method: "PUT", path: "/api/apps/keep", body: map[string]any{"command": pyServer, "cwd": v.Cwd}})
	d.idleDefault = time.Minute
	d.idleCheck(time.Now().Add(2 * time.Minute))
	waitStatus(t, d, "keep", "idle")
}

func TestDashboardFaviconDoesNotCount(t *testing.T) {
	needPython(t)
	d := newTestDaemon(t)
	d.call(t, apiCall{method: "PUT", path: "/api/apps/web", body: map[string]any{"command": pyServer, "cwd": t.TempDir()}})
	req := httptest.NewRequest("GET", "http://web.localhost/favicon.ico", nil)
	req.Header.Set("Referer", "http://localhost/")
	d.ServeHTTP(httptest.NewRecorder(), req)
	if v := d.mustView(t, "web"); v.Status != "idle" || v.LastRequest != nil {
		t.Fatalf("dashboard favicon fetch started or touched the app: %+v", v)
	}
}

func TestReadyPath(t *testing.T) {
	needPython(t)
	dir := t.TempDir()
	flag := filepath.Join(dir, "ready")
	d := newTestDaemon(t)
	d.call(t, apiCall{method: "PUT", path: "/api/apps/web", body: map[string]any{"command": pyCommand(t, "$PORT", flag), "cwd": dir, "ready_path": "/health"}})
	d.call(t, apiCall{method: "POST", path: "/api/apps/web/start"})
	v := d.mustView(t, "web")
	for i := 0; i < 100 && !portOpen(v.Port); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	if v := d.mustView(t, "web"); v.Status != "starting" {
		t.Fatalf("app answering 503 has status %q, want starting", v.Status)
	}
	os.WriteFile(flag, nil, 0o644)
	waitStatus(t, d, "web", "running")
}

func TestForeignPortDetected(t *testing.T) {
	needPython(t)
	if _, err := listListeners(); err != nil {
		t.Skip("listener discovery unsupported here")
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	d := newTestDaemon(t)
	d.call(t, apiCall{method: "PUT", path: "/api/apps/web", body: map[string]any{
		"command": pyCommand(t, fmt.Sprint(port), ""), "cwd": t.TempDir()}})
	d.call(t, apiCall{method: "POST", path: "/api/apps/web/start"})
	if v := waitStatus(t, d, "web", "running"); v.Port != port {
		t.Fatalf("proxy port %d, want detected %d", v.Port, port)
	}
}

func TestLogsFollow(t *testing.T) {
	d := newTestDaemon(t)
	srv := httptest.NewServer(d)
	defer srv.Close()
	d.call(t, apiCall{method: "PUT", path: "/api/apps/talk", body: map[string]any{"command": "echo one; sleep 0.4; echo two; exec sleep 30"}})
	d.call(t, apiCall{method: "POST", path: "/api/apps/talk/start"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/apps/talk/logs?follow=1", nil)
	req.Host = "localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	seen := map[string]bool{}
	sc := bufio.NewScanner(resp.Body)
	for !(seen["one"] && seen["two"]) && sc.Scan() {
		seen[sc.Text()] = true
	}
	if !seen["one"] || !seen["two"] {
		t.Fatalf("follow missed lines: %v (err %v)", seen, sc.Err())
	}
}
