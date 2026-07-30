package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSudoSecretPrefersFileThenEnv(t *testing.T) {
	file := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEMCP_TEST_SUDO_PW", "from-env")

	all := SudoConfig{Password: "inline", PasswordEnv: "CODEMCP_TEST_SUDO_PW", PasswordFile: file}
	if got := all.Secret(); got != "from-file" {
		t.Errorf("Secret() = %q, want the file to win", got)
	}
	noFile := SudoConfig{Password: "inline", PasswordEnv: "CODEMCP_TEST_SUDO_PW"}
	if got := noFile.Secret(); got != "from-env" {
		t.Errorf("Secret() = %q, want the environment to win over the literal", got)
	}
	if got := (SudoConfig{Password: "inline"}).Secret(); got != "inline" {
		t.Errorf("Secret() = %q, want the literal", got)
	}
	if (SudoConfig{}).Configured() {
		t.Error("an empty config should not be configured")
	}
}

// A password file other accounts can read is a misconfiguration worth failing
// at startup, not a warning to be missed in a log.
func TestSudoPasswordFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix permissions")
	}
	file := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(file, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (SudoConfig{PasswordFile: file}).validate(); err == nil {
		t.Error("a world-readable password file should be refused")
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (SudoConfig{PasswordFile: file}).validate(); err != nil {
		t.Errorf("a 0600 password file should be accepted: %v", err)
	}
	if err := (SudoConfig{PasswordFile: filepath.Join(t.TempDir(), "missing")}).validate(); err == nil {
		t.Error("a missing password file should be refused")
	}
}

func TestSudoWantsMatchesCommandWord(t *testing.T) {
	agent := &sudoAgent{password: "x"}
	for _, line := range []string{"sudo apt-get update", "make && sudo make install", "a; sudo b", "(sudo b)", "sudo"} {
		if !agent.wants(line) {
			t.Errorf("wants(%q) = false", line)
		}
	}
	for _, line := range []string{"sudoku", "echo nosudo", "go test ./..."} {
		if agent.wants(line) {
			t.Errorf("wants(%q) = true", line)
		}
	}
	var none *sudoAgent
	if none.wants("sudo apt-get update") {
		t.Error("a nil agent should never claim a command")
	}
}

// The password must not reach the model: not through the environment of the
// command, and not through its output.
func TestSudoPasswordIsRedactedAndNotInEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shim is a POSIX shell script")
	}
	agent := newTestSudoAgent(t, "hunter2")
	if got := agent.redact("prompt: hunter2 done"); strings.Contains(got, "hunter2") {
		t.Errorf("redact left the password in place: %q", got)
	}

	// A command that dumps its environment must not show the secret, even
	// though the shim directory is on its PATH.
	res, err := runShellSudo(context.Background(), agent, "sudo true; env", t.TempDir(), nil, 10*time.Second)
	if err != nil {
		t.Fatalf("runShellSudo: %v", err)
	}
	if strings.Contains(res.Stdout+res.Stderr, "hunter2") {
		t.Errorf("the password appeared in the command environment:\n%s", res.Stdout)
	}
}

// End to end against a stand-in sudo: the shim must be found first on PATH and
// the password must arrive on its stdin.
func TestSudoAgentAnswersThePrompt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shim is a POSIX shell script")
	}
	agent := newTestSudoAgent(t, "hunter2")

	res, err := runShellSudo(context.Background(), agent, "sudo whoami", t.TempDir(), nil, 10*time.Second)
	if err != nil {
		t.Fatalf("runShellSudo: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "authenticated") {
		t.Fatalf("sudo was not answered: exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
}

// With no password configured there is no agent at all, so nothing changes for
// a server that does not want this.
func TestNoAgentWithoutAPassword(t *testing.T) {
	agent, err := newSudoAgent(SudoConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if agent != nil {
		t.Error("an unconfigured sudo section should produce no agent")
	}
	if agent.note() != "" {
		t.Error("a nil agent should have no note")
	}
	if sudoDescriptionSuffix(nil) != "" {
		t.Error("a nil agent should add nothing to the tool description")
	}
}

// newTestSudoAgent builds an agent around a stand-in sudo that reads the
// password the way sudo -S does, then runs the command it was given. Tests must
// never reach the real sudo: with no shim in front of it, it blocks on the
// terminal waiting for a password nobody is there to type.
func newTestSudoAgent(t *testing.T, password string) *sudoAgent {
	t.Helper()
	fake := filepath.Join(t.TempDir(), "sudo")
	// $1 $2 $3 are "-S" "-p" "" as the shim passes them; the rest is the
	// command the caller actually wanted to run.
	script := `#!/bin/sh
[ "$1" = "-S" ] || { echo "shim did not pass -S" >&2; exit 2; }
shift 3
read -r pw
[ "$pw" = "` + password + `" ] || { echo "wrong password" >&2; exit 1; }
echo authenticated
exec "$@"
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	agent, err := newSudoAgentWithPath(SudoConfig{Password: password}, fake)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(agent.Close)
	return agent
}
