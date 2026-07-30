package main

import (
	"runtime"
	"strings"
	"testing"
)

func TestRootWarningNamesTheRisk(t *testing.T) {
	warn := rootWarning(privilegeInfo{EUID: 0, Privileged: true})
	for _, want := range []string{"root", "uid 0", "sudo"} {
		if !strings.Contains(warn, want) {
			t.Errorf("warning does not mention %q:\n%s", want, warn)
		}
	}
	viaSudo := rootWarning(privilegeInfo{EUID: 0, Privileged: true, ViaSudo: true, SudoUser: "alice"})
	if !strings.Contains(viaSudo, "alice") {
		t.Errorf("warning does not name the sudo user:\n%s", viaSudo)
	}
}

// An unprivileged account must produce no note at all, so the description of
// run_command is not padded with a warning that does not apply.
func TestNoteOnlyWhenPrivileged(t *testing.T) {
	if note := (privilegeInfo{EUID: 1000}).note(); note != "" {
		t.Errorf("unprivileged note should be empty, got %q", note)
	}
	if note := (privilegeInfo{EUID: 0, Privileged: true}).note(); !strings.Contains(note, "root") {
		t.Errorf("privileged note should warn about root: %q", note)
	}
}

// On Windows there is no euid, so detection must not claim privilege from the
// -1 that Geteuid returns there.
func TestCurrentPrivilegesOnHost(t *testing.T) {
	info := currentPrivileges()
	if runtime.GOOS == "windows" {
		if info.Privileged {
			t.Error("Windows host reported as privileged")
		}
		if info.Warning != "" {
			t.Errorf("Windows host carries a warning: %q", info.Warning)
		}
		return
	}
	if info.Privileged != (info.EUID == 0) {
		t.Errorf("privileged=%v does not match euid %d", info.Privileged, info.EUID)
	}
	if info.Privileged && info.Warning == "" {
		t.Error("root process carries no warning")
	}
}
