//go:build !windows

// Starts self-restarted Ark processes in a fresh POSIX session.
package web

import (
	"os/exec"
	"syscall"
)

func prepareRestartCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
}
