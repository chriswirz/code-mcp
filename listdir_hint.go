package main

import "strings"

// listDirCommands are the program names run_command's result warns about:
// shell ways to list a directory that list_directory already does the same
// job for, portably and confined to the workspace.
var listDirCommands = map[string]bool{"dir": true, "ls": true}

// invokesListDir reports whether a command line's first real program, in any
// segment a shell operator separates, behind a wrapper like sudo or env, or
// inside a nested shell's own command string, is dir or ls. It reuses
// invokesGit's word- and shell-look-through, just swapping in listDirCommands
// for the name it is hunting.
func invokesListDir(line string) bool {
	for _, segment := range splitShellSegments(line) {
		if segmentInvokesListDir(segment) {
			return true
		}
	}
	return false
}

func segmentInvokesListDir(segment string) bool {
	words := splitShellWords(segment)
	for i := 0; i < len(words); i++ {
		word := words[i]
		switch {
		case word == "":
			continue
		case isEnvAssignment(word):
			continue
		case strings.HasPrefix(word, "-"):
			continue
		}
		name := programName(word)
		switch {
		case listDirCommands[name]:
			return true
		case gitWrappers[name]:
			continue
		case gitShells[name]:
			for _, rest := range words[i+1:] {
				if !strings.HasPrefix(rest, "-") && invokesListDir(rest) {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
	return false
}

// listDirWarning is appended to a run_command result when the line ran dir or
// ls: the command still executed, so a model already relying on its output
// keeps working, but the next call should reach for the dedicated tool.
const listDirWarning = "Warning: list_directory lists a directory the same way on every platform and stays " +
	"confined to the workspace. The command still ran, but prefer list_directory over dir or ls next time."
