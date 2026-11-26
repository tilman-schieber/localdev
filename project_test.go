package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProject(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, projectFileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectFile(t *testing.T) {
	root := t.TempDir()
	writeProject(t, root, `{
  "web": {"command": "npm run dev -- --port $PORT", "env": {"A": "1"}, "idle_stop": "2h", "ready_path": "/"},
  "api": {"command": "go run .", "cwd": "api"},
  "db":  {"port": 5432}
}`)
	sub := filepath.Join(root, "api", "internal")
	os.MkdirAll(sub, 0o755)

	p, err := findProject(sub) // found from a subdirectory
	if err != nil || p == nil {
		t.Fatalf("findProject: %v, %v", p, err)
	}
	if strings.Join(p.names, ",") != "api,db,web" {
		t.Fatalf("names %v", p.names)
	}
	if got := p.apps["api"].Cwd; got != filepath.Join(root, "api") {
		t.Errorf("api cwd %s", got)
	}
	if w := p.apps["web"]; w.Cwd != root || w.Env["A"] != "1" || w.IdleStop != "2h" || w.ReadyPath != "/" {
		t.Errorf("web %+v", w)
	}
	if p.apps["db"].Port != 5432 || !p.has("db") || p.has("nope") {
		t.Errorf("db %+v", p.apps["db"])
	}
}

func TestProjectFileNone(t *testing.T) {
	if p, err := findProject(t.TempDir()); p != nil || err != nil {
		// A localdev.json in a parent of the temp dir would also match; that is not expected on CI.
		t.Fatalf("got %v, %v", p, err)
	}
}

func TestProjectFileErrors(t *testing.T) {
	for body, want := range map[string]string{
		`[]`:                                     "expected an object",
		`{}`:                                     "defines no apps",
		`{"Web": {"command": "x"}}`:              "invalid app name",
		`{"web": {"comand": "x"}}`:               "unknown field",
		`{"web": {}}`:                            "needs a command or a port",
		`{"web": {"port": 1, "idle_stop": "x"}}`: "invalid idle_stop",
		`{"web": {"port": 1, "ready_path": "x"}}`: "must start with /",
	} {
		dir := t.TempDir()
		writeProject(t, dir, body)
		_, err := loadProject(filepath.Join(dir, projectFileName))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want %q", body, err, want)
		}
	}
}
