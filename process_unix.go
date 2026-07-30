//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the shell and everything it spawns in one process group,
// so stopping a process stops its children too. A long-running command is
// usually a shell line that starts something else; killing only the shell would
// leave that something else running with nothing tracking it.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree signals the whole group, falling back to the process alone if
// the group is gone.
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err == nil {
			return nil
		}
	}
	return cmd.Process.Kill()
}
