//go:build windows

package web

import (
	"os/exec"
	"syscall"
)

func prepareRestartCmd(cmd *exec.Cmd) {
	const (
		createNoWindow        = 0x08000000
		detachedProcess       = 0x00000008
		createNewProcessGroup = 0x00000200
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNoWindow | detachedProcess | createNewProcessGroup,
		HideWindow:    true,
	}
}
