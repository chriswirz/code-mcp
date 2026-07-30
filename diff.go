package main

import (
	"fmt"
	"strconv"
	"strings"
)

// A self-contained unified-diff parser and applier. It does not shell out to
// git or patch: the workspace need not be a repository, git need not be
// installed, and applying a patch stays subject to the same workspace
// containment as every other file tool.

// fileDiff is one file's worth of a unified diff.
type fileDiff struct {
	OldPath  string
	NewPath  string
	IsNew    bool
	IsDelete bool
	Hunks    []hunk
}

// hunk is one @@ block.
type hunk struct {
	OldStart int
	OldCount int
	NewStart int
	NewCount int
	Lines    []hunkLine
	// Header is the text after the second @@, kept only for error messages.
	Header string
}

// hunkLine is one line of a hunk. Op is ' ' for context, '-' for a removal and
// '+' for an addition.
type hunkLine struct {
	Op   byte
	Text string
	// NoNewline records a "\ No newline at end of file" marker following this
	// line, which says the file it belongs to does not end in a newline.
	NoNewline bool
}

// parseUnifiedDiff splits a unified diff into per-file changes. strip is the
// number of leading path components to drop, as patch -pN does: 1 removes the
// a/ and b/ prefixes git produces.
func parseUnifiedDiff(text string, strip int) ([]fileDiff, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var files []fileDiff
	var current *fileDiff

	// Extended git headers that appear between "diff --git" and the ---/+++
	// pair. Only the rename ones carry information this applier needs.
	var pendingRenameFrom, pendingRenameTo string

	flush := func() {
		if current != nil && (len(current.Hunks) > 0 || current.IsNew || current.IsDelete) {
			files = append(files, *current)
		}
		current = nil
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]

		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			pendingRenameFrom, pendingRenameTo = "", ""
			continue

		// The rename headers carry bare repository paths, with none of the
		// a/ and b/ prefixes the ---/+++ lines use, so they are not stripped.
		case strings.HasPrefix(line, "rename from "):
			pendingRenameFrom = strings.TrimSpace(strings.TrimPrefix(line, "rename from "))
			continue
		case strings.HasPrefix(line, "rename to "):
			pendingRenameTo = strings.TrimSpace(strings.TrimPrefix(line, "rename to "))
			continue

		case strings.HasPrefix(line, "--- "):
			// The ---/+++ pair opens a file section. A bare "---" without a
			// following "+++" is not a diff header, so both are read together.
			if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "+++ ") {
				continue
			}
			flush()
			oldRaw := headerPath(strings.TrimPrefix(line, "--- "))
			newRaw := headerPath(strings.TrimPrefix(lines[i+1], "+++ "))
			i++

			current = &fileDiff{}
			current.IsNew = oldRaw == "/dev/null"
			current.IsDelete = newRaw == "/dev/null"
			if !current.IsNew {
				current.OldPath = stripPath(oldRaw, strip)
			}
			if !current.IsDelete {
				current.NewPath = stripPath(newRaw, strip)
			}
			// git writes /dev/null on one side of a rename only when the file
			// is also emptied, so the rename headers are the reliable source.
			if pendingRenameFrom != "" && pendingRenameTo != "" {
				current.OldPath, current.NewPath = pendingRenameFrom, pendingRenameTo
				current.IsNew, current.IsDelete = false, false
			}
			continue

		case strings.HasPrefix(line, "@@"):
			if current == nil {
				return nil, fmt.Errorf("hunk at line %d has no file header before it; "+
					"a unified diff needs --- and +++ lines", i+1)
			}
			h, err := parseHunkHeader(line)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
			// Read the hunk body: exactly the number of lines the header
			// promises on each side.
			oldSeen, newSeen := 0, 0
			for i+1 < len(lines) && (oldSeen < h.OldCount || newSeen < h.NewCount) {
				i++
				body := lines[i]
				if body == "" {
					// An empty line in a diff is a context line whose trailing
					// space was stripped, which many tools and editors do.
					if oldSeen < h.OldCount && newSeen < h.NewCount {
						h.Lines = append(h.Lines, hunkLine{Op: ' '})
						oldSeen++
						newSeen++
						continue
					}
					break
				}
				op, rest := body[0], body[1:]
				switch op {
				case ' ':
					h.Lines = append(h.Lines, hunkLine{Op: ' ', Text: rest})
					oldSeen++
					newSeen++
				case '-':
					h.Lines = append(h.Lines, hunkLine{Op: '-', Text: rest})
					oldSeen++
				case '+':
					h.Lines = append(h.Lines, hunkLine{Op: '+', Text: rest})
					newSeen++
				case '\\':
					// "\ No newline at end of file" describes the line above.
					if len(h.Lines) > 0 {
						h.Lines[len(h.Lines)-1].NoNewline = true
					}
				default:
					return nil, fmt.Errorf("line %d: unexpected %q in a hunk body; "+
						"every line must start with a space, - or +", i+1, body)
				}
			}
			if oldSeen != h.OldCount || newSeen != h.NewCount {
				return nil, fmt.Errorf("hunk %q is short: the header promises %d old and %d new lines, "+
					"the body has %d and %d", line, h.OldCount, h.NewCount, oldSeen, newSeen)
			}
			// A trailing "\ No newline" can follow the final line of the hunk.
			if i+1 < len(lines) && strings.HasPrefix(lines[i+1], "\\") {
				i++
				if len(h.Lines) > 0 {
					h.Lines[len(h.Lines)-1].NoNewline = true
				}
			}
			current.Hunks = append(current.Hunks, h)
			continue
		}
	}
	flush()

	if len(files) == 0 {
		return nil, fmt.Errorf("no file changes found; a unified diff needs --- and +++ header lines " +
			"followed by @@ hunks")
	}
	for _, f := range files {
		if f.Path() == "" {
			return nil, fmt.Errorf("a file section names no path")
		}
	}
	return files, nil
}

// Path is the path the change is written to: the new path, or the old one for
// a deletion.
func (f fileDiff) Path() string {
	if f.NewPath != "" {
		return f.NewPath
	}
	return f.OldPath
}

// IsRename reports whether the file also moves.
func (f fileDiff) IsRename() bool {
	return f.OldPath != "" && f.NewPath != "" && f.OldPath != f.NewPath
}

// headerPath takes the path out of a --- or +++ line, dropping the timestamp
// some diff tools append after a tab.
func headerPath(rest string) string {
	if tab := strings.IndexByte(rest, '\t'); tab >= 0 {
		rest = rest[:tab]
	}
	return strings.TrimSpace(rest)
}

// stripPath removes n leading path components, as patch -pN does.
func stripPath(path string, n int) string {
	path = strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
	if path == "/dev/null" {
		return path
	}
	// Quoted paths appear when a name has unusual characters in it.
	if len(path) > 1 && path[0] == '"' && path[len(path)-1] == '"' {
		if unquoted, err := strconv.Unquote(path); err == nil {
			path = unquoted
		}
	}
	for range n {
		slash := strings.IndexByte(path, '/')
		if slash < 0 {
			break
		}
		path = path[slash+1:]
	}
	return strings.TrimPrefix(path, "./")
}

// parseHunkHeader reads "@@ -oldStart,oldCount +newStart,newCount @@ heading".
func parseHunkHeader(line string) (hunk, error) {
	var h hunk
	rest, ok := strings.CutPrefix(line, "@@")
	if !ok {
		return h, fmt.Errorf("not a hunk header: %q", line)
	}
	end := strings.Index(rest, "@@")
	if end < 0 {
		return h, fmt.Errorf("malformed hunk header %q: no closing @@", line)
	}
	h.Header = strings.TrimSpace(rest[end+2:])
	ranges := strings.Fields(strings.TrimSpace(rest[:end]))
	if len(ranges) != 2 || !strings.HasPrefix(ranges[0], "-") || !strings.HasPrefix(ranges[1], "+") {
		return h, fmt.Errorf("malformed hunk header %q: want @@ -old,count +new,count @@", line)
	}
	var err error
	if h.OldStart, h.OldCount, err = parseRange(ranges[0][1:]); err != nil {
		return h, fmt.Errorf("malformed hunk header %q: %w", line, err)
	}
	if h.NewStart, h.NewCount, err = parseRange(ranges[1][1:]); err != nil {
		return h, fmt.Errorf("malformed hunk header %q: %w", line, err)
	}
	return h, nil
}

// parseRange reads "start,count", where an absent count means one line.
func parseRange(text string) (start, count int, err error) {
	startText, countText, hasCount := strings.Cut(text, ",")
	start, err = strconv.Atoi(startText)
	if err != nil {
		return 0, 0, fmt.Errorf("bad line number %q", startText)
	}
	count = 1
	if hasCount {
		count, err = strconv.Atoi(countText)
		if err != nil {
			return 0, 0, fmt.Errorf("bad line count %q", countText)
		}
	}
	return start, count, nil
}

// splitLines breaks content into lines without their terminators, and reports
// whether the content ended with one.
func splitLines(content string) (lines []string, finalNewline bool) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if content == "" {
		return nil, false
	}
	finalNewline = strings.HasSuffix(content, "\n")
	if finalNewline {
		content = content[:len(content)-1]
	}
	return strings.Split(content, "\n"), finalNewline
}

// joinLines is the inverse of splitLines.
func joinLines(lines []string, finalNewline bool) string {
	if len(lines) == 0 {
		return ""
	}
	joined := strings.Join(lines, "\n")
	if finalNewline {
		joined += "\n"
	}
	return joined
}

// applyOptions tunes how strictly a hunk must match.
type applyOptions struct {
	// MaxOffset is how far from the line the hunk header names the applier will
	// search for the context. Line numbers in a model-authored diff are often
	// slightly wrong even when the context is exactly right.
	MaxOffset int
	// IgnoreWhitespace compares lines with leading and trailing whitespace
	// removed, for diffs whose indentation has been mangled in transit.
	IgnoreWhitespace bool
}

// hunkResult records where one hunk actually landed.
type hunkResult struct {
	Header string `json:"header"`
	Line   int    `json:"line"`
	Offset int    `json:"offset"`
}

// applyHunks applies a file's hunks to its old content.
func applyHunks(old string, hunks []hunk, opts applyOptions) (string, []hunkResult, error) {
	lines, finalNewline := splitLines(old)
	if old == "" {
		// A file being created ends with a newline unless the diff says
		// otherwise, since each + line stands for a terminated line.
		finalNewline = true
	}
	var out []string
	var results []hunkResult

	consumed := 0 // how much of lines has been copied into out
	offset := 0   // running drift between the diff's line numbers and reality

	for _, h := range hunks {
		want := h.OldStart - 1 + offset
		if h.OldCount == 0 {
			// A pure insertion names the line it goes after.
			want = h.OldStart + offset
		}
		pos, err := locateHunk(lines, h, want, consumed, opts)
		if err != nil {
			return "", nil, err
		}
		if pos < consumed {
			return "", nil, fmt.Errorf("hunk %q overlaps the previous one", h.headerText())
		}
		out = append(out, lines[consumed:pos]...)

		cursor := pos
		for _, hl := range h.Lines {
			switch hl.Op {
			case ' ':
				// Keep the file's own text rather than the diff's, so a match
				// found under IgnoreWhitespace does not rewrite indentation.
				out = append(out, lines[cursor])
				cursor++
			case '-':
				cursor++
			case '+':
				out = append(out, hl.Text)
			}
			if hl.NoNewline && hl.Op != '-' {
				finalNewline = false
			} else if hl.NoNewline && hl.Op == '-' && cursor >= len(lines) {
				// The old file lacked a final newline; unless the new side says
				// otherwise it gains one.
				finalNewline = true
			}
		}
		consumed = cursor
		results = append(results, hunkResult{
			Header: h.headerText(),
			Line:   pos + 1,
			Offset: pos - (h.OldStart - 1 + offset),
		})
		offset = len(out) - cursor
	}
	out = append(out, lines[consumed:]...)
	return joinLines(out, finalNewline), results, nil
}

func (h hunk) headerText() string {
	return fmt.Sprintf("@@ -%d,%d +%d,%d @@", h.OldStart, h.OldCount, h.NewStart, h.NewCount)
}

// locateHunk finds where a hunk's old side sits in lines, searching outward
// from want. Searching is what makes a diff with stale line numbers usable.
func locateHunk(lines []string, h hunk, want, min int, opts applyOptions) (int, error) {
	if want < min {
		want = min
	}
	if want > len(lines) {
		want = len(lines)
	}
	if hunkMatchesAt(lines, h, want, opts) {
		return want, nil
	}
	maxOffset := opts.MaxOffset
	if maxOffset <= 0 {
		maxOffset = len(lines)
	}
	for delta := 1; delta <= maxOffset; delta++ {
		if back := want - delta; back >= min && hunkMatchesAt(lines, h, back, opts) {
			return back, nil
		}
		if forward := want + delta; hunkMatchesAt(lines, h, forward, opts) {
			return forward, nil
		}
	}
	// Report against wherever the hunk came closest to matching rather than
	// wherever it was first tried: naming the one line that disagrees is what
	// lets the caller fix the patch, and that line is rarely at want.
	return 0, fmt.Errorf("hunk %q does not apply: %s", h.headerText(), describeMismatch(lines, h, bestMatchPos(lines, h, min)))
}

// hunkMatchesAt reports whether the hunk's context and removed lines are the
// file's contents at pos.
func hunkMatchesAt(lines []string, h hunk, pos int, opts applyOptions) bool {
	if pos < 0 {
		return false
	}
	i := pos
	for _, hl := range h.Lines {
		if hl.Op == '+' {
			continue
		}
		if i >= len(lines) || !linesEqual(lines[i], hl.Text, opts.IgnoreWhitespace) {
			return false
		}
		i++
	}
	return true
}

func linesEqual(a, b string, ignoreWhitespace bool) bool {
	if a == b {
		return true
	}
	if !ignoreWhitespace {
		return false
	}
	return strings.TrimSpace(a) == strings.TrimSpace(b)
}

// bestMatchPos finds where the hunk's old side agrees with the file for the
// longest run, which is the position a human would point at when asking why
// the patch did not apply.
func bestMatchPos(lines []string, h hunk, min int) int {
	best, bestRun := min, -1
	for pos := min; pos <= len(lines); pos++ {
		run := 0
		i := pos
		for _, hl := range h.Lines {
			if hl.Op == '+' {
				continue
			}
			if i >= len(lines) || lines[i] != hl.Text {
				break
			}
			run++
			i++
		}
		if run > bestRun {
			best, bestRun = pos, run
		}
	}
	return best
}

// describeMismatch explains which line first disagreed, which is far more
// actionable for a model than "patch failed".
func describeMismatch(lines []string, h hunk, pos int) string {
	i := pos
	for _, hl := range h.Lines {
		if hl.Op == '+' {
			continue
		}
		if i >= len(lines) {
			return fmt.Sprintf("the file ends at line %d, but the hunk expects %q", len(lines), hl.Text)
		}
		if lines[i] != hl.Text {
			return fmt.Sprintf("at line %d the file has %q but the hunk expects %q",
				i+1, lines[i], hl.Text)
		}
		i++
	}
	return "the surrounding context was not found anywhere in the file"
}
