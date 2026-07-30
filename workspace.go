package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Workspace is the directory the server was started in. Every path a tool
// accepts is resolved against it and refused if it escapes.
type Workspace struct {
	Root         string
	MaxFileBytes int64
	MaxResults   int
	Exclude      []string
	AllowWrite   bool
	// Unrestricted turns off containment entirely: paths may name anything on
	// the machine. Relative paths are still resolved against Root, so ordinary
	// use is unchanged.
	Unrestricted bool

	// LineEndings is the convention the workspace normalises to: "preserve"
	// (leave every file as it is), "lf" or "crlf". When it is not "preserve",
	// a read hands back LF whatever is on disk, a write re-encodes to the
	// configured convention, and a find-and-replace compares in LF - so the
	// model never has to reason about  at all, and a file cannot come out of
	// an edit with two conventions in it.
	LineEndings string
}

// NewWorkspace builds a workspace from the config section.
func NewWorkspace(cfg WorkspaceConfig) *Workspace {
	return &Workspace{
		Root:         cfg.Root,
		MaxFileBytes: cfg.MaxFileBytes,
		MaxResults:   cfg.MaxResults,
		Exclude:      cfg.Exclude,
		AllowWrite:   cfg.AllowWrite,
		Unrestricted: cfg.Unrestricted,
		LineEndings:  cfg.LineEndings,
	}
}

// NormalizesLineEndings reports whether this workspace rewrites line endings
// rather than preserving what each file already uses.
func (w *Workspace) NormalizesLineEndings() bool {
	return w.LineEndings == "lf" || w.LineEndings == "crlf"
}

// DiskLineEnding is the sequence writes end their lines with. It is empty when
// the workspace preserves what it finds.
func (w *Workspace) DiskLineEnding() string {
	switch w.LineEndings {
	case "lf":
		return "\n"
	case "crlf":
		return "\r\n"
	}
	return ""
}

// ToLF is the read side of normalisation: whatever the file or the model
// supplied, the text that circulates inside the server uses LF. Lone carriage
// returns are folded too, since a classic-Mac file would otherwise arrive as
// one enormous line.
func (w *Workspace) ToLF(s string) string {
	if !w.NormalizesLineEndings() {
		return s
	}
	return normalizeToLF(s)
}

// ToDisk is the write side: LF text is re-encoded to the configured
// convention, so every line in the file ends the same way even when the model
// supplied a mixture.
func (w *Workspace) ToDisk(s string) string {
	ending := w.DiskLineEnding()
	if ending == "" {
		return s
	}
	return toLineEnding(normalizeToLF(s), ending)
}

// LineEndingNote tells the model what the workspace does with line endings,
// for the server instructions. It is empty while they are preserved, which is
// the behaviour anyone would assume without being told.
func (w *Workspace) LineEndingNote() string {
	if !w.NormalizesLineEndings() {
		return ""
	}
	name := strings.ToUpper(w.LineEndings)
	return "This workspace normalises line endings: every file is written with " + name +
		" endings and read back with LF, and edits match their anchors with line endings " +
		"reconciled. Write LF in the text you pass to write_file, edit_file and apply_diff; " +
		"the server converts on the way to disk. An edit cannot change a file's line endings " +
		"on its own - change workspace.line_endings for that."
}

// nativeLineEnding is this machine's own convention, which is what "native"
// resolves to wherever it is accepted.
func nativeLineEnding() string {
	if runtime.GOOS == "windows" {
		return "\r\n"
	}
	return "\n"
}

// normalizeToLF folds CRLF and lone CR onto LF. The two passes are ordered so
// that a CRLF is not first turned into two newlines by the CR pass.
func normalizeToLF(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// ErrOutsideWorkspace is returned for any path that resolves outside the root.
var ErrOutsideWorkspace = errors.New("path is outside the workspace")

// Resolve turns a workspace-relative (or absolute, but contained) path into an
// absolute one. Symlinks in the resolved path are followed before the
// containment check so a link cannot be used to step out.
//
// A path that points outside the workspace is not refused outright: it is
// first re-anchored inside the root, so a model asking to write "/README.md"
// or "../README.md" gets the README at the top of the workspace rather than an
// error or, worse, a file at the root of the filesystem. Only a path that
// cannot be re-anchored at all is refused.
func (w *Workspace) Resolve(rel string) (string, error) {
	abs, _, err := w.ResolveAdjusted(rel)
	return abs, err
}

// ResolveAdjusted is Resolve, additionally reporting whether the path had to be
// re-anchored, so a tool can tell the model where the file actually landed.
func (w *Workspace) ResolveAdjusted(rel string) (abs string, adjusted bool, err error) {
	if rel == "" || rel == "." {
		return w.Root, false, nil
	}
	if w.Unrestricted {
		return w.absolute(rel), false, nil
	}
	root := w.evalRoot()
	if p, ok := w.contained(rel, root); ok {
		return p, false, nil
	}
	// Second chance: treat the path as if it had been written relative to the
	// workspace all along.
	anchored := w.reanchor(rel)
	if p, ok := w.contained(anchored, root); ok {
		return p, true, nil
	}
	return "", false, fmt.Errorf("%w: %s", ErrOutsideWorkspace, rel)
}

// absolute is the no-containment resolution: relative paths are still taken
// against the workspace root, absolute ones are used as written.
func (w *Workspace) absolute(rel string) string {
	p := rel
	if !filepath.IsAbs(p) {
		p = filepath.Join(w.Root, p)
	}
	p = filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p
}

// contained joins a path against the root when it is relative, follows any
// symlinks, and reports whether the result stays inside the workspace.
func (w *Workspace) contained(p, root string) (string, bool) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(w.Root, p)
	}
	p = filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	if !within(root, p) && !within(w.Root, p) {
		return "", false
	}
	return p, true
}

// rooted reports whether a path names a location from the root of a
// filesystem rather than from the current directory: "/README.md",
// "\src\main.go" or "C:\README.md". Those are the paths worth re-anchoring;
// a relative path that climbs out with ".." is a genuine escape and stays an
// error.
func rooted(p string) bool {
	if filepath.IsAbs(p) || filepath.VolumeName(p) != "" {
		return true
	}
	return strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`)
}

// reanchor rewrites a rooted path so it names the same location inside the
// workspace: the volume and leading separators are dropped and what is left is
// joined onto the root, so "/README.md" becomes "<root>/README.md". A path
// that is not rooted is returned unchanged, and will fail containment on its
// own merits.
func (w *Workspace) reanchor(p string) string {
	if !rooted(p) {
		return p
	}
	p = strings.TrimPrefix(p, filepath.VolumeName(p))
	p = strings.TrimLeft(p, `/\`)
	p = filepath.Clean(filepath.FromSlash(p))
	segments := strings.Split(p, string(filepath.Separator))
	for len(segments) > 0 && (segments[0] == ".." || segments[0] == "." || segments[0] == "") {
		segments = segments[1:]
	}
	if len(segments) == 0 {
		return w.Root
	}
	return filepath.Join(append([]string{w.Root}, segments...)...)
}

// Canonical is the name a tool should report a file by: the path as the
// workspace sees it, workspace-relative where it can be, so the model is told
// where its change actually landed rather than having its own input echoed
// back at it. A path that cannot be resolved is returned as given, since the
// caller is about to report an error about it anyway.
func (w *Workspace) Canonical(rel string) string {
	abs, err := w.Resolve(rel)
	if err != nil {
		abs, err = w.resolveNew(rel)
		if err != nil {
			return filepath.ToSlash(rel)
		}
	}
	return w.Rel(abs)
}

// ScopeNote tells the model how far the file tools reach. An unrestricted
// server is the surprising case and the one worth stating: the model must know
// that a path it writes is not fenced into a project directory.
func (w *Workspace) ScopeNote() string {
	if !w.Unrestricted {
		return ""
	}
	note := "IMPORTANT: this server's workspace root is \".\", which means the file tools are NOT " +
		"confined to a project directory: read_file, write_file, edit_file and the search tools may " +
		"reach any path on this machine, and an absolute path is used exactly as written rather than " +
		"being anchored inside a workspace. Relative paths are still resolved against " + w.Root + ". " +
		"Prefer paths under that directory, and touch anything outside it only when the user has asked " +
		"for it explicitly."
	if !w.AllowWrite {
		note += " Writes are disabled, so this reach is read-only."
	}
	return note
}

// AdjustmentNote describes a re-anchoring in one sentence, for tools that want
// to tell the model the file did not land where its path literally said. It is
// empty when the path was taken as given.
func (w *Workspace) AdjustmentNote(rel, abs string) string {
	if w.Unrestricted || !rooted(rel) {
		return ""
	}
	landed := w.Rel(abs)
	if landed == filepath.ToSlash(filepath.Clean(rel)) {
		return ""
	}
	return fmt.Sprintf("Note: %q is an absolute path; it was interpreted relative to the workspace root and written to %s.", rel, landed)
}

// evalRoot is the workspace root with symlinks followed, which is what a
// resolved path has to be compared against.
func (w *Workspace) evalRoot() string {
	if resolved, err := filepath.EvalSymlinks(w.Root); err == nil {
		return resolved
	}
	return w.Root
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Rel renders an absolute path relative to the workspace root, with forward
// slashes, which is how every tool reports paths back to the model.
func (w *Workspace) Rel(abs string) string {
	rel, err := filepath.Rel(w.Root, abs)
	if err != nil || !within(w.Root, abs) {
		// Outside the root a relative path would be a pile of "..", which is
		// no use to the reader; name the file outright instead.
		return filepath.ToSlash(abs)
	}
	return filepath.ToSlash(rel)
}

// IsExcluded reports whether any path segment matches an exclude pattern.
func (w *Workspace) IsExcluded(abs string) bool {
	return w.isExcludedRel(w.Rel(abs))
}

// IsExcludedUnder reports whether any path segment of abs beneath under matches
// an exclude pattern. Segments that belong to under itself are ignored, so a
// walk that starts at an excluded directory (e.g. .git or node_modules) still
// visits its contents. Nested copies of those directories are still skipped.
func (w *Workspace) IsExcludedUnder(abs, under string) bool {
	rel, err := filepath.Rel(under, abs)
	if err != nil {
		return w.IsExcluded(abs)
	}
	return w.isExcludedRel(filepath.ToSlash(rel))
}

func (w *Workspace) isExcludedRel(rel string) bool {
	if rel == "" || rel == "." {
		return false
	}
	for _, segment := range strings.Split(rel, "/") {
		for _, pattern := range w.Exclude {
			if segment == pattern {
				return true
			}
			if ok, err := filepath.Match(pattern, segment); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// Walk visits every non-excluded file under dir, stopping when fn returns
// false. Excluded names such as .git and node_modules are skipped unless dir
// itself is that path (or is inside it).
func (w *Workspace) Walk(dir string, fn func(path string, d fs.DirEntry) bool) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree should not abort the whole walk.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path != dir && w.IsExcludedUnder(path, dir) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !fn(path, d) {
			return fs.SkipAll
		}
		return nil
	})
}

// ReadFile reads a file, refusing anything larger than the configured limit.
func (w *Workspace) ReadFile(rel string) (string, error) {
	abs, err := w.Resolve(rel)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", rel)
	}
	if info.Size() > w.MaxFileBytes {
		return "", fmt.Errorf("%s is %d bytes, over the %d byte limit; read it in ranges instead",
			rel, info.Size(), w.MaxFileBytes)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	return w.ToLF(string(data)), nil
}

// WriteFile writes a file, creating parent directories as needed.
func (w *Workspace) WriteFile(rel, content string) (string, error) {
	if !w.AllowWrite {
		return "", errors.New("writes are disabled (workspace.allow_write is false)")
	}
	abs, err := w.Resolve(rel)
	if err != nil {
		// A file that does not exist yet cannot be symlink-resolved, so fall
		// back to a lexical containment check on the parent.
		abs, err = w.resolveNew(rel)
		if err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(abs, []byte(w.ToDisk(content)), 0o644); err != nil {
		return "", err
	}
	return abs, nil
}

// WriteFileCheck resolves a path that need not exist yet, for callers that
// want to validate a destination before producing its contents.
func (w *Workspace) WriteFileCheck(rel string) (string, error) {
	return w.resolveNew(rel)
}

// Stat reports on a workspace path, resolving it first so the containment
// rules apply.
func (w *Workspace) Stat(rel string) (os.FileInfo, error) {
	abs, err := w.Resolve(rel)
	if err != nil {
		abs, err = w.resolveNew(rel)
		if err != nil {
			return nil, err
		}
	}
	return os.Stat(abs)
}

// resolveNew resolves a path that need not exist yet. It cannot follow
// symlinks through a missing leaf, so containment is checked lexically, with
// the same re-anchoring as Resolve.
func (w *Workspace) resolveNew(rel string) (string, error) {
	abs, _, err := w.resolveNewAdjusted(rel)
	return abs, err
}

func (w *Workspace) resolveNewAdjusted(rel string) (abs string, adjusted bool, err error) {
	if rel == "" || rel == "." {
		return w.Root, false, nil
	}
	if w.Unrestricted {
		return w.absolute(rel), false, nil
	}
	root := w.evalRoot()
	for attempt, p := range []string{rel, w.reanchor(rel)} {
		if !filepath.IsAbs(p) {
			p = filepath.Join(w.Root, p)
		}
		p = filepath.Clean(p)
		if within(root, p) || within(w.Root, p) {
			return p, attempt > 0, nil
		}
	}
	return "", false, fmt.Errorf("%w: %s", ErrOutsideWorkspace, rel)
}
