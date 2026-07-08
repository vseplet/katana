//go:build !windows

package main

// Unix-псевдотерминал для общего терминала (terminal.go): login-шелл под
// creack/pty. TERM=xterm-256color, рабочая директория — домашняя.

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

type unixPTY struct {
	ptmx *os.File
	cmd  *exec.Cmd
}

// startPTY поднимает login-шелл ($SHELL или /bin/zsh) в PTY заданного размера.
func startPTY(cols, rows uint16) (ptySession, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	c := exec.Command(shell, "-l") // login-шелл: подхватит профиль пользователя
	c.Env = append(os.Environ(), "TERM=xterm-256color")
	if home, err := os.UserHomeDir(); err == nil {
		c.Dir = home
	}
	ptmx, err := pty.StartWithSize(c, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	return &unixPTY{ptmx: ptmx, cmd: c}, nil
}

func (u *unixPTY) Read(p []byte) (int, error)  { return u.ptmx.Read(p) }
func (u *unixPTY) Write(p []byte) (int, error) { return u.ptmx.Write(p) }

func (u *unixPTY) Resize(cols, rows uint16) error {
	return pty.Setsize(u.ptmx, &pty.Winsize{Cols: cols, Rows: rows})
}

func (u *unixPTY) Close() error {
	if u.cmd != nil && u.cmd.Process != nil {
		_ = u.cmd.Process.Kill()
	}
	return u.ptmx.Close()
}
