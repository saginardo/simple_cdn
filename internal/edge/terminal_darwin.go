package edge

import (
	"golang.org/x/sys/unix"
	"os"
)

func terminalForegroundGroup(ptmx *os.File) (int, error) {
	return unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPGRP)
}
func killTerminalSessionMembers(int) {}
