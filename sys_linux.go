package main

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func setProcAttrs(cmd *exec.Cmd) {
	// Own process group so the whole tree can be signalled; SIGTERM the shell if the daemon dies.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}

// groupRunsApp reports whether some process in the group was started by localdev for name.
func groupRunsApp(pgid int, name string) bool {
	entries, _ := os.ReadDir("/proc")
	marker := []byte("LOCALDEV_NAME=" + name + "\x00")
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(stat)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		if f := strings.Fields(s[i+1:]); len(f) < 3 || f[2] != strconv.Itoa(pgid) {
			continue
		}
		env, err := os.ReadFile("/proc/" + e.Name() + "/environ")
		if err == nil && bytes.Contains(append([]byte{0}, env...), append([]byte{0}, marker...)) {
			return true
		}
	}
	return false
}
