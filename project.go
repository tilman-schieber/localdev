package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// projectFileName is the per-project file that `localdev run` reads when given no command.
const projectFileName = "localdev.json"

// projectApp is one app in localdev.json. Field names match the API's PUT body.
type projectApp struct {
	Command     string            `json:"command"`
	Port        int               `json:"port"`
	Cwd         string            `json:"cwd"` // relative to the file's directory
	Env         map[string]string `json:"env"`
	RewriteHost bool              `json:"rewrite_host"`
	IdleStop    string            `json:"idle_stop"`
	ReadyPath   string            `json:"ready_path"`
}

type project struct {
	path  string
	apps  map[string]putRequest
	names []string // sorted
}

func (p *project) has(name string) bool { _, ok := p.apps[name]; return ok }

// findProject looks for localdev.json in dir and its parents. It returns nil if there is none.
func findProject(dir string) (*project, error) {
	for {
		path := filepath.Join(dir, projectFileName)
		if _, err := os.Stat(path); err == nil {
			return loadProject(path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, nil
		}
		dir = parent
	}
}

// loadProject parses a localdev.json file: an object mapping app names to definitions.
func loadProject(path string) (*project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: %v (expected an object mapping app names to definitions)", path, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s defines no apps", path)
	}
	dir := filepath.Dir(path)
	p := &project{path: path, apps: map[string]putRequest{}}
	for name, msg := range raw {
		if !validName(name) {
			return nil, fmt.Errorf("%s: invalid app name %q (use lowercase letters, digits, dashes)", path, name)
		}
		var a projectApp
		dec := json.NewDecoder(bytes.NewReader(msg))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			return nil, fmt.Errorf("%s: app %s: %v", path, name, err)
		}
		if a.Command == "" && a.Port == 0 {
			return nil, fmt.Errorf("%s: app %s needs a command or a port", path, name)
		}
		if _, _, err := parseIdleStop(a.IdleStop); err != nil {
			return nil, fmt.Errorf("%s: app %s: %v", path, name, err)
		}
		if a.ReadyPath != "" && !strings.HasPrefix(a.ReadyPath, "/") {
			return nil, fmt.Errorf("%s: app %s: ready_path must start with /", path, name)
		}
		cwd := dir
		if a.Cwd != "" {
			cwd = a.Cwd
			if !filepath.IsAbs(cwd) {
				cwd = filepath.Join(dir, cwd)
			}
		}
		p.apps[name] = putRequest{Port: a.Port, Command: a.Command, Cwd: filepath.Clean(cwd), Env: a.Env,
			RewriteHost: a.RewriteHost, IdleStop: a.IdleStop, ReadyPath: a.ReadyPath}
		p.names = append(p.names, name)
	}
	sort.Strings(p.names)
	return p, nil
}
