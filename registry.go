package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// AppConfig is the persistent definition of an app. Runtime state lives in memory.
type AppConfig struct {
	Name        string            `json:"name"`
	Port        int               `json:"port,omitempty"`
	PortAuto    bool              `json:"port_auto,omitempty"`
	Command     string            `json:"command,omitempty"`
	Cwd         string            `json:"cwd,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	RewriteHost bool              `json:"rewrite_host,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

type registryFile struct {
	Version int          `json:"version"`
	Apps    []*AppConfig `json:"apps"`
}

type paths struct{ config, state string }

func getPaths() paths {
	if h := os.Getenv("LOCALDEV_HOME"); h != "" {
		return paths{config: h, state: h}
	}
	home, _ := os.UserHomeDir()
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	st := os.Getenv("XDG_STATE_HOME")
	if st == "" {
		st = filepath.Join(home, ".local", "state")
	}
	return paths{config: filepath.Join(cfg, "localdev"), state: filepath.Join(st, "localdev")}
}

func (p paths) appsFile() string           { return filepath.Join(p.config, "apps.json") }
func (p paths) daemonFile() string         { return filepath.Join(p.state, "daemon.json") }
func (p paths) lockFile() string           { return filepath.Join(p.state, "daemon.lock") }
func (p paths) daemonLog() string          { return filepath.Join(p.state, "daemon.log") }
func (p paths) logFile(name string) string { return filepath.Join(p.state, "logs", name+".log") }

func loadRegistry(p paths) (map[string]*AppConfig, error) {
	apps := map[string]*AppConfig{}
	data, err := os.ReadFile(p.appsFile())
	if errors.Is(err, os.ErrNotExist) {
		return apps, nil
	}
	if err != nil {
		return nil, err
	}
	var f registryFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p.appsFile(), err)
	}
	for _, a := range f.Apps {
		if validName(a.Name) {
			apps[a.Name] = a
		}
	}
	return apps, nil
}

func saveRegistry(p paths, apps map[string]*AppConfig) error {
	f := registryFile{Version: 1, Apps: []*AppConfig{}}
	for _, a := range apps {
		f.Apps = append(f.Apps, a)
	}
	sort.Slice(f.Apps, func(i, j int) bool { return f.Apps[i].Name < f.Apps[j].Name })
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(p.config, 0o755); err != nil {
		return err
	}
	tmp := p.appsFile() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p.appsFile())
}

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validName(s string) bool { return nameRe.MatchString(s) }

// sanitizeName turns an arbitrary string (e.g. a directory name) into a valid app name.
func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	dash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	return out
}
