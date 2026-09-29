//go:build unix && !linux

package main

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// groupRunsApp reports whether some process in the group was started by localdev for name.
// ps -E appends each process's initial environment to its command.
func groupRunsApp(pgid int, name string) bool {
	out, err := exec.Command("ps", "-A", "-ww", "-E", "-o", "pgid=,command=").Output()
	if err != nil {
		return false
	}
	want := strconv.Itoa(pgid)
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != want {
			continue
		}
		for _, field := range f[1:] {
			if field == "LOCALDEV_NAME="+name {
				return true
			}
		}
	}
	return false
}
