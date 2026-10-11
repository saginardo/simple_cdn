package edge

import (
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func terminalForegroundGroup(ptmx *os.File) (int, error) {
	return unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPGRP)
}

func killTerminalSessionMembers(sessionID int) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			continue
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) < 4 {
			continue
		}
		sid, err := strconv.Atoi(fields[3])
		if err == nil && sid == sessionID {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}
