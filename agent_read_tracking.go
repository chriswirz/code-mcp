package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// The agent keeps its own conversation history, and with it every file the
// model has read. Two things make an old read dangerous to leave in place:
// a newer read of the same lines (two copies to reason from), and a write to
// the file (the copy, and every line number in it, is now wrong). Both are
// handled here by replacing the old read's result with a note saying why and
// what to do, so the model re-reads rather than writing from a stale or
// partial picture - which is what produced garbled and duplicated sections.

// readFileTools names the tools whose results are tracked: read_file and its
// batch form, read_files. Both declare the same preserve parameter - see
// preserveReadDescription in tools_files.go.
var readFileTools = map[string]bool{"read_file": true, "read_files": true}

// readTarget is one file a read call read, and which lines. Start and End are
// as the call gave them: 0 for Start means the first line, 0 for End means
// to the end of the file.
type readTarget struct {
	Path       string
	Start, End int
}

// covers reports whether a read of t returned every line a read of older did.
func (t readTarget) covers(older readTarget) bool {
	if max(t.Start, 1) > max(older.Start, 1) {
		return false
	}
	if t.End == 0 {
		return true
	}
	return older.End != 0 && t.End >= older.End
}

// readCallWantsPreserve reports a read_file or read_files call's preserve
// argument, false (the default) for anything that did not set it.
func readCallWantsPreserve(args json.RawMessage) bool {
	var a struct {
		Preserve bool `json:"preserve"`
	}
	json.Unmarshal(args, &a)
	return a.Preserve
}

// readCallTargets extracts what a read_file or read_files call read. A
// read_files entry is either a bare path string or an object carrying one;
// an entry that fails to parse contributes nothing rather than aborting the
// others.
func readCallTargets(name string, args json.RawMessage) []readTarget {
	type ranged struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	switch name {
	case "read_file":
		var r ranged
		if json.Unmarshal(args, &r) == nil && r.Path != "" {
			return []readTarget{{r.Path, r.StartLine, r.EndLine}}
		}
	case "read_files":
		var a struct {
			Files []json.RawMessage `json:"files"`
		}
		if json.Unmarshal(args, &a) != nil {
			return nil
		}
		var out []readTarget
		for _, entry := range a.Files {
			var path string
			if json.Unmarshal(entry, &path) == nil && path != "" {
				out = append(out, readTarget{Path: path})
				continue
			}
			var r ranged
			if json.Unmarshal(entry, &r) == nil && r.Path != "" {
				out = append(out, readTarget{r.Path, r.StartLine, r.EndLine})
			}
		}
		return out
	}
	return nil
}

// readCallPaths is readCallTargets without the line ranges.
func readCallPaths(name string, args json.RawMessage) []string {
	var paths []string
	for _, t := range readCallTargets(name, args) {
		paths = append(paths, t.Path)
	}
	return paths
}

// normalizeReadPath makes two spellings of the same path compare equal -
// "foo.txt" and "./foo.txt" - without touching the filesystem. It is the
// fallback when there is no local workspace to ask.
func normalizeReadPath(p string) string {
	if p == "" {
		return p
	}
	return filepath.ToSlash(filepath.Clean(p))
}

// canonicalPath is the workspace's own name for p when a local server is
// attached - so "/README.md", "README.md" and an absolute path inside the
// root all agree - and normalizeReadPath's otherwise.
func (a *agent) canonicalPath(p string) string {
	if a.srv != nil {
		if ws := a.srv.workspace(); ws != nil {
			return strings.TrimSuffix(ws.Canonical(p), "/")
		}
	}
	return normalizeReadPath(p)
}

// pathWithin reports whether path is target or lies under it as a directory.
func pathWithin(path, target string) bool {
	if target == "." || target == "" {
		return true
	}
	return path == target || strings.HasPrefix(path, target+"/")
}

// nudgeStaleReads replaces the result of an earlier read that the newer
// read (keepCallID, never itself nudged) has made redundant: one of the same
// file whose lines the newer read all returned again. A read of a different
// range is left alone - reading a long file in chunks must not erase the
// chunks already read.
//
// A read_files result is replaced whole once any file in it is superseded,
// and the note names the other files it held so the model knows to read
// those again if it still needs them.
func (a *agent) nudgeStaleReads(newer []readTarget, keepCallID string) {
	if len(newer) == 0 {
		return
	}
	for i := range newer {
		newer[i].Path = a.canonicalPath(newer[i].Path)
	}
	a.replaceReads(keepCallID, func(old readTarget) bool {
		for _, n := range newer {
			if n.Path == old.Path && n.covers(old) {
				return true
			}
		}
		return false
	}, func(tool string, hit, others []string) string {
		return staleReadNudge(tool, hit, others)
	})
}

// invalidateReadsAfterWrite replaces the result of every earlier read of a
// file a write tool has just changed, whatever lines it covered: its
// contents and its line numbers no longer describe the file.
func (a *agent) invalidateReadsAfterWrite(writer string, targets []string) {
	if len(targets) == 0 {
		return
	}
	canon := make([]string, len(targets))
	for i, t := range targets {
		canon[i] = a.canonicalPath(t)
	}
	a.replaceReads("", func(old readTarget) bool {
		for _, t := range canon {
			if pathWithin(old.Path, t) {
				return true
			}
		}
		return false
	}, func(tool string, hit, others []string) string {
		return changedReadNudge(tool, writer, hit, others)
	})
}

// replaceReads walks history for read calls (other than skipID) with a
// target matching stale, and rewrites each one's tool result with the note
// built from the matching paths and the call's other paths.
func (a *agent) replaceReads(skipID string, stale func(readTarget) bool, note func(tool string, hit, others []string) string) {
	for i := range a.history {
		msg := &a.history[i]
		if msg.Role != "assistant" {
			continue
		}
		for _, tc := range msg.ToolCalls {
			if tc.ID == skipID || !readFileTools[tc.Name] {
				continue
			}
			var hit, others []string
			for _, t := range readCallTargets(tc.Name, tc.Arguments) {
				t.Path = a.canonicalPath(t.Path)
				if stale(t) {
					hit = appendUnique(hit, t.Path)
				} else {
					others = appendUnique(others, t.Path)
				}
			}
			if len(hit) == 0 {
				continue
			}
			result := a.findToolResult(tc.ID)
			if result == nil || isReadNudge(result.Content) {
				continue
			}
			result.Content = note(tc.Name, hit, without(others, hit))
		}
	}
}

func appendUnique(list []string, s string) []string {
	for _, have := range list {
		if have == s {
			return list
		}
	}
	return append(list, s)
}

func without(list, drop []string) []string {
	var out []string
	for _, s := range list {
		if !slicesContains(drop, s) {
			out = append(out, s)
		}
	}
	return out
}

// findToolResult finds the "tool" role message answering a given call, so a
// read's result can be rewritten in place.
func (a *agent) findToolResult(callID string) *chatMessage {
	for i := range a.history {
		if a.history[i].Role == "tool" && a.history[i].ToolCallID == callID {
			return &a.history[i]
		}
	}
	return nil
}

const readNudgePrefix = "[This "

// isReadNudge reports whether a result has already been replaced, so a
// second replacement does not overwrite the first note's file list.
func isReadNudge(content string) bool {
	return strings.HasPrefix(content, readNudgePrefix) && strings.HasSuffix(content, "]") &&
		strings.Contains(content, " result was removed: ")
}

// staleReadNudge is what an old read becomes once a newer read has returned
// the same lines of the same file again.
func staleReadNudge(toolName string, paths, others []string) string {
	return fmt.Sprintf("%s%s result was removed: %s has been read again more recently in this conversation, "+
		"and that newer read covers these lines. Use the newer result instead.%s]",
		readNudgePrefix, toolName, quoteNames(paths), alsoHeld(others))
}

// changedReadNudge is what an old read becomes once a write tool has changed
// the file it read.
func changedReadNudge(toolName, writer string, paths, others []string) string {
	return fmt.Sprintf("%s%s result was removed: %s was changed by %s after this read, so its contents and "+
		"line numbers no longer match the file. Read it again before editing it further, rather than "+
		"working from memory of this copy.%s]",
		readNudgePrefix, toolName, quoteNames(paths), writer, alsoHeld(others))
}

func alsoHeld(others []string) string {
	if len(others) == 0 {
		return ""
	}
	return fmt.Sprintf(" This result also held %s; read it again if you still need it.", quoteNames(others))
}

func quoteNames(paths []string) string {
	return strings.Join(paths, ", ")
}

// writeCallTargets names the files or directories a successful call to one
// of the local write tools may have changed, or nil for any other tool or a
// dry run. A directory means everything under it.
func writeCallTargets(name string, args json.RawMessage) []string {
	var a struct {
		Path   string `json:"path"`
		DryRun bool   `json:"dry_run"`
		Scope  string `json:"scope"`
		Diff   string `json:"diff"`
		Strip  *int   `json:"strip"`
		Edits  []struct {
			Path string `json:"path"`
		} `json:"edits"`
		Sections []struct {
			Path string `json:"path"`
		} `json:"sections"`
	}
	if json.Unmarshal(args, &a) != nil || a.DryRun {
		return nil
	}
	var out []string
	add := func(p string) {
		if p != "" {
			out = appendUnique(out, p)
		}
	}
	switch name {
	case "write_file", "edit_file", "edit_lines", "insert_text", "rollback", "download_file":
		add(a.Path)
	case "multi_edit":
		for _, e := range a.Edits {
			add(e.Path)
		}
	case "remove_sections":
		for _, s := range a.Sections {
			add(firstNonEmpty(s.Path, a.Path))
		}
	case "replace_in_files":
		add(firstNonEmpty(a.Path, "."))
	case "fix_line_endings":
		if a.Scope == string(scopeWorkspace) {
			add(".")
		} else {
			add(firstNonEmpty(a.Path, "."))
		}
	case "apply_diff":
		strip := 1
		if a.Strip != nil {
			strip = *a.Strip
		}
		for _, p := range diffPaths(a.Diff, strip) {
			add(p)
		}
	}
	return out
}

// diffPaths lists the file names a unified diff's headers mention, with
// strip leading components removed as patch -pN would. A removed line that
// happens to look like a header only adds a name nothing has read, which
// costs nothing.
func diffPaths(diff string, strip int) []string {
	var out []string
	for _, line := range strings.Split(diff, "\n") {
		line = strings.TrimRight(line, "\r")
		var names []string
		switch {
		case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "+++ "):
			name, _, _ := strings.Cut(line[4:], "\t")
			names = []string{strings.TrimSpace(name)}
		case strings.HasPrefix(line, "diff --git "):
			names = strings.Fields(line[len("diff --git "):])
		}
		for _, n := range names {
			if n == "" || n == "/dev/null" {
				continue
			}
			parts := strings.Split(filepath.ToSlash(n), "/")
			if strip > 0 && strip < len(parts) {
				parts = parts[strip:]
			}
			out = appendUnique(out, strings.Join(parts, "/"))
		}
	}
	return out
}
