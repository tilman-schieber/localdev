package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Proc is one run of a managed app's command.
type Proc struct {
	cmd       *exec.Cmd
	pgid      int
	cfg       AppConfig // definition the process was started with
	env       []string
	startedAt time.Time
	done      chan struct{}
	exitCode  int
	stopping  bool
}

func (p *Proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// App couples a persistent definition with its (optional) running process.
type App struct {
	cfg     *AppConfig
	proc    *Proc
	lastEnv []string // caller environment from the most recent start request
}

const (
	autoPortMin = 4100
	autoPortMax = 4999
)

func portOpen(port int) bool {
	for _, host := range []string{"127.0.0.1", "::1"} {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(port)), 200*time.Millisecond)
		if err == nil {
			c.Close()
			return true
		}
	}
	return false
}

func portFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return !portOpen(port)
}

// allocPortLocked picks the first free port in the auto range not claimed by another app.
func (d *Daemon) allocPortLocked(name string) (int, error) {
	used := map[int]bool{d.port: true}
	for n, a := range d.apps {
		if n != name {
			used[a.cfg.Port] = true
		}
	}
	for p := autoPortMin; p <= autoPortMax; p++ {
		if !used[p] && portFree(p) {
			return p, nil
		}
	}
	return 0, errors.New("no free port in auto range")
}

func buildEnv(base []string, cfg *AppConfig, url string) []string {
	if base == nil {
		base = os.Environ()
	}
	m := map[string]string{}
	var order []string
	set := func(k, v string) {
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = v
	}
	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok {
			set(k, v)
		}
	}
	for k, v := range cfg.Env {
		set(k, v)
	}
	set("PORT", fmt.Sprint(cfg.Port))
	set("LOCALDEV_NAME", cfg.Name)
	set("LOCALDEV_URL", url)
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+m[k])
	}
	return out
}

// startLocked launches the app's command. Caller holds d.mu.
func (d *Daemon) startLocked(a *App, env []string) error {
	cfg := a.cfg
	if cfg.Command == "" {
		return errors.New("app has no command; it is not managed by localdev")
	}
	if cfg.PortAuto {
		if cfg.Port == 0 || !portFree(cfg.Port) {
			p, err := d.allocPortLocked(cfg.Name)
			if err != nil {
				return err
			}
			cfg.Port = p
			d.saveLocked()
		}
	} else if !portFree(cfg.Port) {
		return fmt.Errorf("port %d is already in use%s", cfg.Port, describePortOwner(cfg.Port))
	}
	if env != nil {
		a.lastEnv = env
	}

	logPath := d.paths.logFile(cfg.Name)
	os.MkdirAll(filepath.Dir(logPath), 0o755)
	os.Rename(logPath, logPath+".1")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fmt.Fprintf(f, "[localdev] %s starting: %s (cwd %s, PORT=%d)\n",
		time.Now().Format(time.RFC3339), cfg.Command, cfg.Cwd, cfg.Port)

	cmd := exec.Command("/bin/sh", "-c", cfg.Command)
	cmd.Dir = cfg.Cwd
	cmd.Env = buildEnv(a.lastEnv, cfg, d.appURL(cfg.Name))
	cmd.Stdout, cmd.Stderr = f, f
	setProcAttrs(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(f, "[localdev] failed to start: %v\n", err)
		f.Close()
		return err
	}
	snapshot := *cfg
	snapshot.Env = maps.Clone(cfg.Env)
	p := &Proc{cmd: cmd, pgid: cmd.Process.Pid, cfg: snapshot, env: cmd.Env, startedAt: time.Now(), done: make(chan struct{})}
	a.proc = p

	go func() {
		err := cmd.Wait()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			code = -1
		}
		// Clean up anything the shell left behind in its process group.
		syscall.Kill(-p.pgid, syscall.SIGTERM)
		fmt.Fprintf(f, "[localdev] %s process exited (code %d)\n", time.Now().Format(time.RFC3339), code)
		f.Close()
		d.mu.Lock()
		p.exitCode = code
		close(p.done)
		d.mu.Unlock()
	}()
	if cfg.PortAuto {
		go d.watchForeignPort(a, p, f)
	}
	return nil
}

// watchForeignPort handles commands that ignore $PORT: if the process group
// listens on some other port, the proxy target is switched to that port.
func (d *Daemon) watchForeignPort(a *App, p *Proc, logw io.Writer) {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(700 * time.Millisecond)
		d.mu.Lock()
		port, current := a.cfg.Port, a.proc == p
		d.mu.Unlock()
		if !current || p.exited() || portOpen(port) {
			return
		}
		ls, err := listListeners()
		if err != nil {
			return
		}
		best := 0
		for _, l := range ls {
			if l.PGID == p.pgid && (best == 0 || l.Port < best) {
				best = l.Port
			}
		}
		if best != 0 {
			d.mu.Lock()
			if a.proc == p {
				a.cfg.Port = best
				p.cfg.Port = best
				d.saveLocked()
				fmt.Fprintf(logw, "[localdev] command ignored PORT=%d; detected it listening on %d, proxying there\n", port, best)
			}
			d.mu.Unlock()
			return
		}
	}
}

// stop terminates the process group: SIGTERM, then SIGKILL after a grace period.
// Must be called without holding d.mu.
func (d *Daemon) stop(p *Proc) {
	d.mu.Lock()
	p.stopping = true
	d.mu.Unlock()
	if p.exited() {
		return
	}
	syscall.Kill(-p.pgid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		syscall.Kill(-p.pgid, syscall.SIGKILL)
		<-p.done
	}
}

func describePortOwner(port int) string {
	ls, err := listListeners()
	if err != nil {
		return ""
	}
	for _, l := range ls {
		if l.Port == port && l.PID != 0 {
			return fmt.Sprintf(" (by pid %d, %s)", l.PID, l.Process)
		}
	}
	return ""
}
