package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// SudoConfig holds the password used to answer sudo's prompt, so a command
// that needs elevation can run without the model ever being shown the secret.
// Prefer password_env or password_file: an inline password sits in config.json,
// which is a file inside the workspace that the file tools can read.
type SudoConfig struct {
	// Password is the literal secret. Discouraged, but honest about what it is.
	Password string `json:"password,omitempty"`
	// PasswordEnv names an environment variable of this server's process to
	// read the password from, and is the usual way to supply it.
	PasswordEnv string `json:"password_env,omitempty"`
	// PasswordFile names a file whose first line is the password. It is read
	// on each use, so rotating the file needs no restart.
	PasswordFile string `json:"password_file,omitempty"`
}

// Secret resolves the password, in order of preference: the file, the
// environment, then the literal. An unreadable file is treated as no password
// rather than as a fatal error, since the alternative is refusing to run every
// command because one of them might need sudo.
func (c SudoConfig) Secret() string {
	if c.PasswordFile != "" {
		if data, err := os.ReadFile(c.PasswordFile); err == nil {
			if line := strings.SplitN(strings.TrimRight(string(data), "\r\n"), "\n", 2)[0]; line != "" {
				return line
			}
		}
	}
	if c.PasswordEnv != "" {
		if value := os.Getenv(c.PasswordEnv); value != "" {
			return value
		}
	}
	return c.Password
}

// Configured reports whether a password was set at all, which is what the tool
// descriptions and system_info may say. The secret itself never leaves here.
func (c SudoConfig) Configured() bool {
	return c.Secret() != ""
}

// validate checks what can be checked at startup: that a named password file
// exists and is not world-readable. A missing environment variable is not an
// error, since the server may be started before it is set.
func (c SudoConfig) validate() error {
	if c.PasswordFile == "" {
		return nil
	}
	info, err := os.Stat(c.PasswordFile)
	if err != nil {
		return fmt.Errorf("sudo.password_file: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("sudo.password_file: %s is a directory", c.PasswordFile)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("sudo.password_file: %s is readable by other accounts (mode %04o); chmod 600 it",
			c.PasswordFile, info.Mode().Perm())
	}
	return nil
}

// sudoAgent answers sudo's password prompt on behalf of the model.
//
// The secret is never put in the environment of a spawned command, never
// written to disk, and never included in a tool result: it is written to the
// command's stdin, where sudo -S expects to read it. What makes that work for
// any command line - "sudo apt-get update", but also "make || sudo make
// install" - is a directory placed first on PATH holding a `sudo` shim that
// execs the real sudo with -S and an empty prompt. The shim contains no
// secret; only the pipe does.
//
// This narrows the exposure rather than eliminating it: run_command executes as
// the same user as this server, so a command that went looking could still read
// the password out of its own stdin. The point is that the model is never told
// it and never has to be.
type sudoAgent struct {
	password string
	shimDir  string
	realSudo string
}

// newSudoAgent prepares the shim directory. It returns (nil, nil) when there is
// nothing to do: no password configured, a platform without sudo, or no sudo on
// PATH.
func newSudoAgent(cfg SudoConfig) (*sudoAgent, error) {
	if runtime.GOOS == "windows" || !cfg.Configured() {
		return nil, nil
	}
	real, err := exec.LookPath("sudo")
	if err != nil {
		return nil, nil
	}
	return newSudoAgentWithPath(cfg, real)
}

// newSudoAgentWithPath builds the agent around a specific sudo binary, which is
// what lets the tests point it at a stand-in.
func newSudoAgentWithPath(cfg SudoConfig, real string) (*sudoAgent, error) {
	dir, err := os.MkdirTemp("", "codemcp-sudo-")
	if err != nil {
		return nil, fmt.Errorf("sudo: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("sudo: %w", err)
	}
	// -S reads the password from stdin, -p '' keeps the prompt out of stderr
	// so it never reaches the model as noise in a tool result.
	shim := "#!/bin/sh\n# Written by codemcp: answer sudo's prompt from stdin.\nexec " +
		shellQuote(real) + " -S -p '' \"$@\"\n"
	path := filepath.Join(dir, "sudo")
	if err := os.WriteFile(path, []byte(shim), 0o700); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("sudo: %w", err)
	}
	return &sudoAgent{password: cfg.Secret(), shimDir: dir, realSudo: real}, nil
}

// Close removes the shim directory.
func (a *sudoAgent) Close() {
	if a == nil {
		return
	}
	os.RemoveAll(a.shimDir)
}

// sudoPattern matches sudo used as a command word, so "sudo apt-get" and
// "make && sudo make install" are recognised while "sudoku" and a mention in a
// quoted string are not.
var sudoPattern = regexp.MustCompile(`(^|[\s;&|(])sudo(\s|$)`)

// wants reports whether this command line would invoke sudo.
func (a *sudoAgent) wants(line string) bool {
	return a != nil && sudoPattern.MatchString(line)
}

// env is what has to be added to a command's environment for the shim to be
// found: the shim directory ahead of everything else.
func (a *sudoAgent) env() map[string]string {
	if a == nil {
		return nil
	}
	return map[string]string{"PATH": a.shimDir + string(os.PathListSeparator) + os.Getenv("PATH")}
}

// redact removes the password from text on its way back to the model. Nothing
// should echo it, but a command that logs its own stdin would, and a leak here
// is unrecoverable.
func (a *sudoAgent) redact(s string) string {
	if a == nil || a.password == "" {
		return s
	}
	return strings.ReplaceAll(s, a.password, "[sudo password redacted]")
}

// shellQuote wraps a path in single quotes for the shim, doubling any quote it
// contains. Paths with quotes in them are not expected; correctness is cheap.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// note is the sentence the model is told about sudo. It says the capability
// exists and that the password is handled elsewhere, so the model neither asks
// the user for it nor tries to read it out of a file.
func (a *sudoAgent) note() string {
	if a == nil {
		return ""
	}
	return "sudo: a password is configured on the server and is supplied automatically when a command " +
		"needs it. Write \"sudo <command>\" normally. Never ask the user for the password, never echo " +
		"it, and do not try to read it from the configuration: you are not given it, and commands that " +
		"try are a misuse of this server."
}

// sudoDescriptionSuffix is appended to the run_command description so the model
// knows elevation is available and that the password is not its business.
func sudoDescriptionSuffix(agent *sudoAgent) string {
	if note := agent.note(); note != "" {
		return "\n\n" + note
	}
	return ""
}
