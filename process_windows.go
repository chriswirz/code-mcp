package main

import (
	"os/exec"
	"strconv"
	"syscall"
)

// setProcessGroup has no counterpart here: the children are found at kill time
// by walking the tree with taskkill instead.
func setProcessGroup(cmd *exec.Cmd) {}

// setShellCmdLine works around cmd.exe's /c not parsing the way Go's default
// argument-to-command-line join assumes. Go escapes each argument for the
// standard CommandLineToArgvW rules, but cmd.exe re-reads the raw command
// line itself and only ever strips one matching pair of outer quotes -  it
// does not undo the backslash-doubling Go adds before a quote. The result:
// any shell line containing a quote, or a trailing backslash next to a
// space-triggered quote, comes out corrupted (`dir "C:\Program Files\"` and
// even a bare `dir C:\` both fail with "syntax is incorrect"). Setting
// CmdLine directly bypasses that escaping and hands cmd.exe the line exactly
// as shellArgv built it, which is what /c actually expects.
func setShellCmdLine(cmd *exec.Cmd, args []string) {
	if len(args) == 2 && args[0] == "/c" {
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: "/c " + args[1]}
	}
}

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
