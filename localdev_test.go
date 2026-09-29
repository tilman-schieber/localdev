package main

import (
	"flag"
	"slices"
	"testing"
)

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"My App":        "my-app",
		"web_frontend!": "web-frontend",
		"--x--":         "x",
		"Ünïcode":       "n-code",
		"":              "",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
		if want != "" && !validName(want) {
			t.Errorf("sanitized %q is not valid", want)
		}
	}
}

func TestParseProcIP(t *testing.T) {
	if ip := parseProcIP("0100007F"); ip.String() != "127.0.0.1" {
		t.Errorf("v4: %v", ip)
	}
	if ip := parseProcIP("00000000000000000000000001000000"); ip.String() != "::1" {
		t.Errorf("v6: %v", ip)
	}
}

func TestParseArgsInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	port := fs.Int("port", 0, "")
	pos, cmd, err := parseArgs(fs, []string{"web", "--port", "3000", "--", "npm", "run", "dev", "--port", "x"})
	if err != nil || *port != 3000 || !slices.Equal(pos, []string{"web"}) || !slices.Equal(cmd, []string{"npm", "run", "dev", "--port", "x"}) {
		t.Fatalf("pos=%v cmd=%v port=%d err=%v", pos, cmd, *port, err)
	}
}

func TestHostOnly(t *testing.T) {
	for in, want := range map[string]string{"Web.Localhost:80": "web.localhost", "[::1]:7780": "::1", "localhost.": "localhost"} {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}
