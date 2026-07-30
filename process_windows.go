package main

import (
	"os/exec"
	"strconv"
)

// setProcessGroup has no counterpart here: the children are found at kill time
// by walking the tree with taskkill instead.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessTree ends the process and everything it started. cmd.exe spawns
// the real work as a child, so killing the shell alone would leave it running.
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	if err := kill.Run(); err == nil {
		return nil
	}
	return cmd.Process.Kill()
}
