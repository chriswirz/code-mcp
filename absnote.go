package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// workspaceRelativeMarker is the phrase every workspace-relative path
// parameter's description carries, consistently enough across the tool set
// to double as a machine-readable tag: absolutePathNote uses it to find
// which top-level string arguments name a workspace path, without every tool
// having to register itself for the feature separately. A "path" that is
// really a git pathspec or similar, such as git_log's, carries no such
// phrase and is left alone.
const workspaceRelativeMarker = "relative to the workspace root"

// absolutePathNote reports, for each top-level string argument whose schema
// description marks it as workspace-relative, the absolute path it actually
// resolved to. It is empty unless workspace.note_absolute_paths is on: the
// workspace-relative path already in the result is what a model normally
// wants, and this only adds something new when it was left to wonder where a
// file actually lives on disk.
//
// A value that is itself absolute or rooted is skipped - that case is what
// Workspace.AdjustmentNote already reports on the tools that call it - and so
// is a path a schema default is asked to supply. Only top-level properties
// are considered; a path nested inside an array item, such as multi_edit's
// per-edit path, is not.
func absolutePathNote(ws *Workspace, inputSchema map[string]any, raw json.RawMessage) string {
	if ws == nil || !ws.NoteAbsolutePaths {
		return ""
	}
	props, _ := inputSchema["properties"].(map[string]any)
	if len(props) == 0 {
		return ""
	}
	var args map[string]json.RawMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return ""
		}
	}

	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)

	var lines []string
	for _, name := range names {
		def, _ := props[name].(map[string]any)
		desc, _ := def["description"].(string)
		if !strings.Contains(desc, workspaceRelativeMarker) {
			continue
		}
		value, ok := stringArgOrDefault(def, args[name])
		if !ok || value == "" || rooted(value) {
			continue
		}
		abs, err := ws.Resolve(value)
		if err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("Note: %q resolved to the absolute path %s.", value, filepath.ToSlash(abs)))
	}
	return strings.Join(lines, "\n")
}

// stringArgOrDefault reads a string-typed argument, falling back to the
// schema's own default when the caller left it out - list_directory's bare
// call, for instance, is understood to mean "." even though nothing in the
// request says so. The second result is false for anything that is not a
// plain string, such as an omitted argument with no default.
func stringArgOrDefault(propDef map[string]any, raw json.RawMessage) (string, bool) {
	if len(raw) > 0 {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", false
		}
		return s, true
	}
	if def, ok := propDef["default"].(string); ok {
		return def, true
	}
	return "", false
}
