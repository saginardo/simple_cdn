//go:build linux || darwin

package edge

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
	"simple_cdn/internal/terminal"
)

type edgeTerminalSession struct {
	id     string
	input  chan terminal.Message
	cancel context.CancelFunc
	done   chan struct{}
}

func (session *edgeTerminalSession) close() { session.cancel(); <-session.done }

func terminalIdentity() (terminal.Message, error) {
	account, err := user.Current()
	if err != nil {
		return terminal.Message{}, err
	}
	shell := "/bin/bash"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}
	return terminal.Message{Type: "hello", User: account.Username, Shell: shell}, nil
}

func startEdgeTerminal(parent context.Context, conn *websocket.Conn, identity, request terminal.Message, idle, maximum time.Duration) (*edgeTerminalSession, error) {
	ctx, cancel := context.WithCancel(parent)
	command := exec.Command(identity.Shell, "-i")
	// Do not pass enrollment tokens, mTLS paths, or service configuration into
	// the shell's environment. The service sandbox (including ProtectHome) applies.
	command.Env = []string{"TERM=xterm-256color", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/", "USER=" + identity.User, "LOGNAME=" + identity.User, "LANG=C.UTF-8", "HISTFILE=/dev/null"}
	command.Dir = "/"
	ptmx, err := pty.StartWithSize(command, &pty.Winsize{Cols: request.Cols, Rows: request.Rows})
	if err != nil {
		cancel()
		return nil, err
	}
	session := &edgeTerminalSession{id: request.SessionID, input: make(chan terminal.Message, 16), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(session.done)
		defer cancel()
		// A PTY gives job-control commands a separate foreground process group.
		// Close both that group and the shell group when the session ends.
		defer func() {
			killTerminalProcesses(ptmx, command.Process.Pid)
			_ = ptmx.Close()
			_ = command.Wait()
		}()
		if err := terminal.Write(ctx, conn, terminal.Message{Type: "ready", SessionID: session.id, User: identity.User, Shell: identity.Shell}); err != nil {
			return
		}
		outputDone := make(chan error, 1)
		go func() {
			buffer := make([]byte, terminal.MaxData)
			for {
				count, err := ptmx.Read(buffer)
				if count > 0 {
					if writeErr := terminal.Write(ctx, conn, terminal.Message{Type: "output", SessionID: session.id, Data: buffer[:count]}); writeErr != nil {
						outputDone <- writeErr
						return
					}
				}
				if err != nil {
					outputDone <- err
					return
				}
			}
		}()
		// A separate writer keeps timeout and disconnect handling responsive even
		// if a command stops reading its terminal input.
		writeQueue := make(chan []byte, 16)
		writeFailed := make(chan error, 1)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case data := <-writeQueue:
					if _, err := ptmx.Write(data); err != nil {
						writeFailed <- err
						return
					}
				}
			}
		}()
		idleTimer := time.NewTimer(idle)
		defer idleTimer.Stop()
		maxTimer := time.NewTimer(maximum)
		defer maxTimer.Stop()
		reason := "shell_exited"
	loop:
		for {
			select {
			case <-ctx.Done():
				return
			case <-idleTimer.C:
				reason = "idle_timeout"
				break loop
			case <-maxTimer.C:
				reason = "max_duration"
				break loop
			case <-outputDone:
				break loop
			case <-writeFailed:
				reason = "io_error"
				break loop
			case message := <-session.input:
				if message.Type == "resize" {
					if err := pty.Setsize(ptmx, &pty.Winsize{Cols: message.Cols, Rows: message.Rows}); err != nil {
						reason = "io_error"
						break loop
					}
				} else {
					idleTimer.Reset(idle)
					select {
					case writeQueue <- message.Data:
					default:
						reason = "io_error"
						break loop
					}
				}
			}
		}
		_ = terminal.Write(ctx, conn, terminal.Message{Type: "exit", SessionID: session.id, Reason: reason})
	}()
	return session, nil
}

func killTerminalProcesses(ptmx *os.File, pid int) {
	if foreground, err := terminalForegroundGroup(ptmx); err == nil && foreground > 1 && foreground != syscall.Getpgrp() {
		_ = syscall.Kill(-foreground, syscall.SIGKILL)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	// Linux job-control background jobs may have their own process groups.
	killTerminalSessionMembers(pid)
}
