package main

import (
	"fmt"
	"os"
	"os/user"
	"runtime"
	"strings"
)

// privilegeInfo describes the account this server's commands run as. On a Unix
// host a server started with sudo executes every run_command call as root, and
// a model that does not know that will happily write commands whose blast
// radius is the whole machine rather than the workspace.
type privilegeInfo struct {
	User       string `json:"user,omitempty"`
	UID        int    `json:"uid"`
	EUID       int    `json:"euid"`
	Privileged bool   `json:"privileged"`
	// ViaSudo is set when the process was started through sudo, in which case
	// SUDO_USER names the human behind it.
	ViaSudo  bool   `json:"via_sudo,omitempty"`
	SudoUser string `json:"sudo_user,omitempty"`
	Warning  string `json:"warning,omitempty"`
}

// currentPrivileges inspects the running process. On Windows Geteuid returns
// -1 and there is no root account to detect, so the result is simply not
// privileged and carries no warning.
func currentPrivileges() privilegeInfo {
	info := privilegeInfo{UID: os.Getuid(), EUID: os.Geteuid()}
	if runtime.GOOS == "windows" {
		if u, err := user.Current(); err == nil {
			info.User = u.Username
		}
		return info
	}
	info.Privileged = info.EUID == 0
	if u, err := user.Current(); err == nil {
		info.User = u.Username
	} else if info.Privileged {
		info.User = "root"
	}
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		info.ViaSudo = true
		info.SudoUser = sudoUser
	}
	if info.Privileged {
		info.Warning = rootWarning(info)
	}
	return info
}

// rootWarning is the sentence handed to the model. It is deliberately
// actionable: what is true, what it changes, and what to do instead.
func rootWarning(info privilegeInfo) string {
	var b strings.Builder
	b.WriteString("WARNING: this server is running as root (uid 0)")
	if info.ViaSudo {
		fmt.Fprintf(&b, ", started with sudo by %s", info.SudoUser)
	}
	b.WriteString(". Every run_command call, project command and file write executes with full " +
		"system privileges: there is no permission barrier protecting files outside the workspace, " +
		"system configuration or package state. Do not prefix commands with sudo, and do not run " +
		"anything destructive or system-wide (package installs, service restarts, writes under /etc, " +
		"/usr or /var, recursive deletes) unless the user has explicitly asked for it. Files created " +
		"by these commands will be owned by root and may be unwritable for the user afterwards.")
	return b.String()
}

// note is the one-line form used in tool descriptions, where the full warning
// would crowd out the rest of the text.
func (p privilegeInfo) note() string {
	if !p.Privileged {
		return ""
	}
	who := "as root (uid 0)"
	if p.ViaSudo {
		who = fmt.Sprintf("as root (uid 0, via sudo from %s)", p.SudoUser)
	}
	return "IMPORTANT: this server runs " + who + ", so commands are unsandboxed and system-wide. " +
		"Never add sudo, and keep changes inside the workspace unless the user explicitly asked " +
		"for a system-level change; files you create will be owned by root."
}

// privilegeDescriptionSuffix is appended to the run_command description, so the
// model sees the elevation up front rather than only if it calls system_info.
func privilegeDescriptionSuffix() string {
	note := currentPrivileges().note()
	if note == "" {
		return ""
	}
	return "\n\n" + note
}
