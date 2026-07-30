package main

import (
	"strings"
	"unicode"
)

// The git tools are gated by git.enabled, but a shell is a way around any
// gate: "run_command git push" reaches the same repository the disabled tools
// would have. When git is off, this file blocks git at the shell too, so the
// setting means what an operator reading it would expect.

// gitWrappers are programs that run another command line, and so must be
// looked through rather than treated as the command being run.
var gitWrappers = map[string]bool{
	"sudo": true, "doas": true, "env": true, "command": true, "exec": true,
	"nice": true, "ionice": true, "nohup": true, "setsid": true, "stdbuf": true,
	"time": true, "winpty": true, "xargs": true,
}

// gitShells are shells whose -c argument is another command line, checked in
// turn so "bash -c 'git push'" is not a hole in the block.
var gitShells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
	"cmd": true, "cmd.exe": true, "powershell": true, "powershell.exe": true,
	"pwsh": true, "pwsh.exe": true,
}

// invokesGit reports whether a command line runs git anywhere in it: in any of
// the segments a shell operator separates, behind a wrapper like sudo or env,
// or inside a nested shell's command string.
//
// It looks at the program each segment actually runs, not at every word, so
// "grep git README.md" and a path like .git stay callable while "git push",
// "sudo git commit" and "/usr/bin/git status" do not.
func invokesGit(line string) bool {
	for _, segment := range splitShellSegments(line) {
		if segmentInvokesGit(segment) {
			return true
		}
	}
	return false
}

func segmentInvokesGit(segment string) bool {
	words := splitShellWords(segment)
	for i := 0; i < len(words); i++ {
		word := words[i]
		switch {
		case word == "":
			continue
		case isEnvAssignment(word):
			// NAME=value before the program name.
			continue
		case strings.HasPrefix(word, "-"):
			// A wrapper's own flags, such as sudo -u someone.
			continue
		}
		name := programName(word)
		switch {
		case name == "git":
			return true
		case gitWrappers[name]:
			continue
		case gitShells[name]:
			// Whatever this shell was handed is itself a command line.
			for _, rest := range words[i+1:] {
				if !strings.HasPrefix(rest, "-") && invokesGit(rest) {
					return true
				}
			}
			return false
		default:
			// The program is something else - but an unquoted Windows path
			// splits on its spaces, so the git in a path like
			// C:\Program Files\Git\cmd\git.exe arrives as a later word. Any
			// word that looks like a path to git still counts; a bare word,
			// like the one in "grep git", does not.
			return hasGitPathWord(words[i:])
		}
	}
	return false
}

// hasGitPathWord reports whether any of these words is a path ending in git.
func hasGitPathWord(words []string) bool {
	for _, word := range words {
		if strings.ContainsAny(word, `/\`) && programName(word) == "git" {
			return true
		}
	}
	return false
}

// programName is the lower-cased base name of a program, without its directory
// or a Windows .exe suffix, so /usr/bin/git and C:\Git\cmd\git.exe both read as
// "git".
func programName(word string) string {
	word = strings.Trim(word, `"'`)
	if i := strings.LastIndexAny(word, `/\`); i >= 0 {
		word = word[i+1:]
	}
	word = strings.ToLower(word)
	return strings.TrimSuffix(word, ".exe")
}

func isEnvAssignment(word string) bool {
	i := strings.Index(word, "=")
	if i <= 0 {
		return false
	}
	for _, r := range word[:i] {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return true
}

// splitShellSegments breaks a command line at the operators that start a new
// command - ; && || | & and a newline - ignoring any inside quotes.
func splitShellSegments(line string) []string {
	var segments []string
	var current strings.Builder
	var quote rune
	flush := func() {
		if s := strings.TrimSpace(current.String()); s != "" {
			segments = append(segments, s)
		}
		current.Reset()
	}
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			current.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			current.WriteRune(r)
		case r == ';' || r == '\n' || r == '|' || r == '&':
			// && and || are two runes for one operator; either way the effect
			// on the split is the same.
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return segments
}

// splitShellWords splits on whitespace outside quotes and unquotes each word,
// so a nested command string comes back as one word ready to be re-parsed.
func splitShellWords(segment string) []string {
	var words []string
	var current strings.Builder
	var quote rune
	quoted := false
	flush := func() {
		if current.Len() > 0 || quoted {
			words = append(words, current.String())
		}
		current.Reset()
		quoted = false
	}
	for _, r := range segment {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			quoted = true
		case unicode.IsSpace(r):
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return words
}

// gitDisabledMessage is what a blocked caller is told. It names the setting, so
// the answer is to change the configuration rather than to look for another
// way to spell the command.
const gitDisabledMessage = "git is disabled for this server (git.enabled is false in config.json), " +
	"so shelling out to git is blocked as well. Ask the operator to enable it if this repository's history is meant to be touched."

// checkGitAllowed returns a tool error when a command line runs git and the
// configuration has git switched off, and nil when the line may run.
func (s *Server) checkGitAllowed(line string) *CallToolResult {
	s.mu.RLock()
	enabled := s.cfg.Git.Enabled
	s.mu.RUnlock()
	if enabled || !invokesGit(line) {
		return nil
	}
	return toolError("%s", gitDisabledMessage)
}

// gitDescriptionSuffix warns the model off git before it writes the command,
// rather than only rejecting it afterwards.
func (s *Server) gitDescriptionSuffix() string {
	s.mu.RLock()
	enabled := s.cfg.Git.Enabled
	s.mu.RUnlock()
	if enabled {
		return ""
	}
	return "\n\nGit is disabled on this server: a command line that runs git is refused, however it is spelled."
}
