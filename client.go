package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

type client struct {
	base string
	http *http.Client
}

// callError is an API error response.
type callError struct {
	Status int
	Code   string
	Msg    string
}

func (e *callError) Error() string { return e.Msg }

var errDaemonDown = errors.New("localdev daemon is not running")

func newClient(port int) *client {
	return &client{
		base: fmt.Sprintf("http://127.0.0.1:%d", port),
		http: &http.Client{Transport: &http.Transport{Proxy: nil}},
	}
}

func (c *client) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Host = "localhost"
	req.Header.Set("X-Localdev", "cli")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errDaemonDown, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var ae apiError
		if json.Unmarshal(data, &ae) == nil && ae.Error != "" {
			return &callError{Status: resp.StatusCode, Code: ae.Code, Msg: ae.Error}
		}
		return &callError{Status: resp.StatusCode, Code: "http_" + strconv.Itoa(resp.StatusCode), Msg: string(data)}
	}
	switch o := out.(type) {
	case nil:
	case *string:
		*o = string(data)
	default:
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *client) health() (daemonInfo, bool) {
	var info daemonInfo
	c.http.Timeout = time.Second
	defer func() { c.http.Timeout = 0 }()
	if err := c.do("GET", "/api/health", nil, &info); err != nil || info.Service != "localdev" {
		return info, false
	}
	return info, true
}

// daemonPort picks the port to talk to: $LOCALDEV_PORT, then the running daemon's state file, then 80.
func daemonPort() int {
	if p := envInt("LOCALDEV_PORT", 0); p != 0 {
		return p
	}
	var info daemonInfo
	if data, err := os.ReadFile(getPaths().daemonFile()); err == nil && json.Unmarshal(data, &info) == nil && info.Port != 0 {
		return info.Port
	}
	return defaultPort
}

func findDaemon() (*client, daemonInfo, bool) {
	c := newClient(daemonPort())
	info, ok := c.health()
	return c, info, ok
}

// connect returns a client for a running daemon, starting one in the background if needed.
func connect() (*client, error) {
	if c, _, ok := findDaemon(); ok {
		return c, nil
	}
	if os.Getenv("LOCALDEV_NO_AUTOSTART") != "" {
		return nil, errDaemonDown
	}
	info, err := spawnDaemon()
	if err != nil {
		return nil, err
	}
	logf("localdev daemon running at %s", info.URL)
	return newClient(info.Port), nil
}

func spawnDaemon() (daemonInfo, error) {
	p := getPaths()
	if err := os.MkdirAll(p.state, 0o755); err != nil {
		return daemonInfo{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return daemonInfo{}, err
	}
	lf, err := os.OpenFile(p.daemonLog(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return daemonInfo{}, err
	}
	defer lf.Close()
	cmd := exec.Command(exe, "daemon", "run")
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return daemonInfo{}, err
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, info, ok := findDaemon(); ok {
			return info, nil
		}
		select {
		case <-exited:
			// Lost a race with another starter? Give it a moment, else report failure.
			time.Sleep(300 * time.Millisecond)
			if _, info, ok := findDaemon(); ok {
				return info, nil
			}
			return daemonInfo{}, fmt.Errorf("daemon failed to start; see %s", p.daemonLog())
		case <-time.After(100 * time.Millisecond):
		}
	}
	return daemonInfo{}, fmt.Errorf("daemon did not become ready; see %s", p.daemonLog())
}
