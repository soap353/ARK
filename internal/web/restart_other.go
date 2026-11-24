//go:build !windows

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
