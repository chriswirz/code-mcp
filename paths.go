package main

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// A wrong path is the most ordinary way a tool call fails, and "no such file
// or directory" is close to the least useful thing to say about it: the caller
// already believed the file was there, and the reply gives it nothing to go on
// but another guess. The workspace knows what is actually on disk, so a
// not-found answer carries the files the caller most likely meant.

// defaultPathSuggestions is how many candidates a not-found error offers when
// the configuration says nothing.
const defaultPathSuggestions = 5

// maxSuggestionScan bounds the walk behind a suggestion. A miss on a large
// tree must not cost more than the successful call would have.
const maxSuggestionScan = 20000

// pathCandidate is one file considered as what the caller meant.
type pathCandidate struct {
	rel   string
	score int
}

// SuggestPaths returns the workspace files closest to the path asked for, best
// first. It returns nothing when suggestions are switched off, when the
// workspace is unrestricted (the search space is then the whole machine, and a
// guess would be noise), or when nothing is close enough to be worth offering.
func (w *Workspace) SuggestPaths(rel string) []string {
	limit := w.PathSuggestions
	if limit <= 0 || w.Unrestricted || rel == "" {
		return nil
	}

	wanted := filepath.ToSlash(strings.TrimPrefix(filepath.ToSlash(filepath.Clean(rel)), "./"))
	wantedBase := strings.ToLower(path.Base(wanted))
	wantedDir := strings.ToLower(path.Dir(wanted))
	if wantedBase == "." || wantedBase == "/" {
		return nil
	}

	var candidates []pathCandidate
	scanned := 0
	_ = w.Walk(w.Root, func(abs string, d fs.DirEntry) bool {
		scanned++
		candidate := filepath.ToSlash(w.Rel(abs))
		if score := pathScore(wanted, wantedBase, wantedDir, candidate); score > 0 {
			candidates = append(candidates, pathCandidate{rel: candidate, score: score})
		}
		return scanned < maxSuggestionScan
	})

	// Best first, and alphabetically within a score so the answer is stable
	// between calls rather than following the order of the walk.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].rel < candidates[j].rel
	})

	out := make([]string, 0, limit)
	for _, c := range candidates {
		if len(out) == limit {
			break
		}
		out = append(out, c.rel)
	}
	return out
}

// pathScore rates a workspace file as what the caller meant. The file name
// carries most of the intent - a model that gets a path wrong usually has the
// name right and the directory wrong, or the other way round - so the name is
// scored first and the directory adjusts it.
func pathScore(wanted, wantedBase, wantedDir, candidate string) int {
	lower := strings.ToLower(candidate)
	base := path.Base(lower)
	dir := path.Dir(lower)

	var score int
	switch {
	case lower == strings.ToLower(wanted):
		// Same path in a different case, which on a case-sensitive
		// filesystem is exactly the mistake worth naming.
		return 1000
	case base == wantedBase:
		score = 500
	case strings.HasSuffix(base, wantedBase) || strings.HasPrefix(base, wantedBase):
		score = 300
	case strings.Contains(base, wantedBase) || strings.Contains(wantedBase, base):
		score = 200
	default:
		// Not obviously related by name: fall back to how many edits apart
		// the names are, which catches a typo or a transposition.
		distance := editDistance(base, wantedBase)
		longest := max(len(base), len(wantedBase))
		if longest == 0 || distance*3 > longest {
			return 0
		}
		score = 150 - distance*10
	}

	switch {
	case dir == wantedDir:
		score += 60
	case strings.HasSuffix(dir, wantedDir) || strings.HasSuffix(wantedDir, dir):
		score += 30
	case strings.Contains(lower, wantedDir) && wantedDir != ".":
		score += 15
	}
	// A file near the top of the tree is more likely to be the one meant than
	// a namesake buried deep in it.
	score -= strings.Count(candidate, "/")
	return score
}

// editDistance is Levenshtein, on two names that are both short. It is here
// rather than in a dependency because it is fifteen lines and this is the only
// place that wants it.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(min(current[j-1]+1, previous[j]+1), previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}

// pathToolError is what a file tool returns when a path does not resolve to a
// file. It says plainly that the path parameter is what was wrong - a model
// reading "no such file" often concludes the file needs creating - and offers
// the nearest things that do exist.
func pathToolError(w *Workspace, rel string, err error) *CallToolResult {
	if !isNotFound(err) {
		return toolError("%v", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "the path parameter was specified incorrectly: %q does not exist in the workspace.", rel)
	// When exactly one file carries that name, say so outright rather than
	// burying it in a list: it is almost certainly the file that was meant,
	// and naming the setting says why it was not simply used.
	if !w.ExactPathNotRequired {
		if found, ok := w.UniqueByName(rel); ok && found != rel {
			fmt.Fprintf(&b, " %s is the only file with that name; call it by that path. "+
				"(workspace.exact_path_not_required makes a unique name enough on its own.)", found)
			return toolError("%s", b.String())
		}
	}
	if suggestions := w.SuggestPaths(rel); len(suggestions) > 0 {
		if len(suggestions) == 1 {
			fmt.Fprintf(&b, " Did you mean %s?", suggestions[0])
		} else {
			fmt.Fprintf(&b, " Did you mean one of these?\n  %s", strings.Join(suggestions, "\n  "))
		}
		b.WriteString("\nPaths are relative to the workspace root.")
	} else {
		b.WriteString(" Nothing in the workspace has a similar name. Paths are relative to the " +
			"workspace root; list_directory or find_files will show what is there.")
	}
	return toolError("%s", b.String())
}

// isNotFound reports whether an error is a missing file, whatever wrapped it.
func isNotFound(err error) bool {
	return err != nil && os.IsNotExist(err)
}

// prefixToolError puts a caller's own label in front of a tool error, so a
// suggestion produced deep in a batch still says which entry it belongs to.
func prefixToolError(res *CallToolResult, prefix string) *CallToolResult {
	if res == nil || len(res.Content) == 0 {
		return res
	}
	res.Content[0].Text = prefix + ": " + res.Content[0].Text
	return res
}

// UniqueByName returns the one workspace file whose name is exactly the name
// asked for, and whether there was exactly one. Two files of that name make it
// a guess, and a guess is not something to act on.
func (w *Workspace) UniqueByName(rel string) (string, bool) {
	if w.Unrestricted || rel == "" {
		return "", false
	}
	wanted := path.Base(filepath.ToSlash(filepath.Clean(rel)))
	if wanted == "." || wanted == "/" || wanted == "" {
		return "", false
	}
	var found string
	matches, scanned := 0, 0
	_ = w.Walk(w.Root, func(abs string, d fs.DirEntry) bool {
		scanned++
		if d.Name() == wanted {
			matches++
			found = filepath.ToSlash(w.Rel(abs))
			if matches > 1 {
				return false
			}
		}
		return scanned < maxSuggestionScan
	})
	if matches == 1 {
		return found, true
	}
	return "", false
}

// ResolveForRead is what a tool calls instead of using its path argument
// directly. It returns the path to act on, and a note when that is not the
// path that was asked for.
//
// A path that does not exist, whose file name matches exactly one file in the
// workspace, is taken to mean that file: the caller had the name right and the
// directory wrong, there is nothing else it could have meant, and failing the
// call would only produce the same call again with the path corrected. The
// note says what happened, so the correction is visible rather than silent and
// the caller can use the real path next time.
func (w *Workspace) ResolveForRead(rel string) (actual, note string, err error) {
	if _, statErr := w.Stat(rel); statErr == nil {
		return rel, "", nil
	} else if !isNotFound(statErr) {
		return rel, "", statErr
	} else {
		err = statErr
	}
	if !w.ExactPathNotRequired {
		return rel, "", err
	}
	if found, ok := w.UniqueByName(rel); ok && found != rel {
		return found, fmt.Sprintf(
			"Note: %q does not exist. %s is the only file named %q in the workspace, so that is "+
				"the file this acted on. Use that path next time.",
			rel, found, path.Base(filepath.ToSlash(filepath.Clean(rel)))), nil
	}
	return rel, "", err
}

// withNote puts a correction note in front of a successful result, so the
// answer carries both what happened and the path it happened to.
func withNote(res *CallToolResult, note string) *CallToolResult {
	if note == "" || res == nil || len(res.Content) == 0 {
		return res
	}
	res.Content[0].Text = note + "\n\n" + res.Content[0].Text
	return res
}
