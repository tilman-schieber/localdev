package main

import (
	"net"
	"os/exec"
	"strconv"
	"strings"
)

// listListeners uses lsof, which ships with macOS.
func listListeners() ([]Listener, error) {
	out, err := exec.Command("lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-F", "pgcn").Output()
	if err != nil && len(out) == 0 {
		return nil, nil // lsof exits 1 when nothing matches
	}
	var ls []Listener
	var pid, pgid int
	var comm string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		v := line[1:]
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(v)
		case 'g':
			pgid, _ = strconv.Atoi(v)
		case 'c':
			comm = v
		case 'n':
			i := strings.LastIndexByte(v, ':')
			if i < 0 {
				continue
			}
			port, err := strconv.Atoi(v[i+1:])
			if err != nil {
				continue
			}
			host := strings.Trim(v[:i], "[]")
			if host == "*" {
				host = "0.0.0.0"
			}
			ip := net.ParseIP(host)
			if ip == nil || !(ip.IsLoopback() || ip.IsUnspecified()) {
				continue
			}
			ls = append(ls, Listener{Port: port, Addr: ip.String(), PID: pid, PGID: pgid, Process: comm})
		}
	}
	return dedupeListeners(ls), nil
}
