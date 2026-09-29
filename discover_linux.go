package main

import (
	"encoding/hex"
	"net"
	"os"
	"strconv"
	"strings"
)

// listListeners reads /proc/net/tcp{,6} and maps socket inodes to processes.
// Processes owned by other users are listed without pid information.
func listListeners() ([]Listener, error) {
	byInode := map[string]*Listener{}
	var order []string
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for _, line := range lines[1:] {
			f := strings.Fields(line)
			if len(f) < 10 || f[3] != "0A" { // 0A = LISTEN
				continue
			}
			hexIP, hexPort, ok := strings.Cut(f[1], ":")
			if !ok {
				continue
			}
			port, err := strconv.ParseInt(hexPort, 16, 32)
			if err != nil {
				continue
			}
			ip := parseProcIP(hexIP)
			if ip == nil || !(ip.IsLoopback() || ip.IsUnspecified()) {
				continue
			}
			byInode[f[9]] = &Listener{Port: int(port), Addr: ip.String()}
			order = append(order, f[9])
		}
	}
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := "/proc/" + e.Name()
		fds, err := os.ReadDir(dir + "/fd")
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(dir + "/fd/" + fd.Name())
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			l := byInode[link[8:len(link)-1]]
			if l == nil || l.PID != 0 {
				continue
			}
			l.PID = pid
			comm, _ := os.ReadFile(dir + "/comm")
			l.Process = strings.TrimSpace(string(comm))
			cmdline, _ := os.ReadFile(dir + "/cmdline")
			l.Command = strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " "))
			l.Cwd, _ = os.Readlink(dir + "/cwd")
			if stat, err := os.ReadFile(dir + "/stat"); err == nil {
				s := string(stat)
				if i := strings.LastIndexByte(s, ')'); i >= 0 {
					if fs := strings.Fields(s[i+1:]); len(fs) > 2 {
						l.PGID, _ = strconv.Atoi(fs[2])
					}
				}
			}
		}
	}
	var out []Listener
	for _, ino := range order {
		out = append(out, *byInode[ino])
	}
	return dedupeListeners(out), nil
}

// parseProcIP decodes the little-endian-per-32-bit-word hex addresses used by /proc/net/tcp.
func parseProcIP(s string) net.IP {
	b, err := hex.DecodeString(s)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return nil
	}
	for i := 0; i < len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	return net.IP(b)
}
