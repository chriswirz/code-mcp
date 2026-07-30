package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// sanitizeReplacer maps the typographic characters models like to produce -
// dashes, arrows, curly quotes and the rest - onto plain ASCII spellings.
// Every key is a single rune, so text can be sanitized fragment by fragment
// as it streams in without a replacement ever straddling two fragments.
var sanitizeReplacer = strings.NewReplacer(
	// Dashes and minus signs.
	"\u2014", "-", // — em dash
	"\u2013", "-", // – en dash
	"\u2012", "-", // ‒ figure dash
	"\u2015", "-", // ― horizontal bar
	"\u2010", "-", // ‐ hyphen
	"\u2011", "-", // ‑ non-breaking hyphen
	"\u2212", "-", // − minus sign

	// Arrows.
	"\u2192", "->", // →
	"\u2190", "<-", // ←
	"\u2194", "<->", // ↔
	"\u27F6", "->", // ⟶
	"\u27F5", "<-", // ⟵
	"\u27F7", "<->", // ⟷
	"\u21D2", "=>", // ⇒
	"\u21D0", "<=", // ⇐
	"\u21D4", "<=>", // ⇔
	"\u27F9", "=>", // ⟹
	"\u27F8", "<=", // ⟸

	// Quotes.
	"\u201C", `"`, // “
	"\u201D", `"`, // ”
	"\u201E", `"`, // „
	"\u2033", `"`, // ″
	"\u2018", "'", // ‘
	"\u2019", "'", // ’
	"\u201A", "'", // ‚
	"\u2032", "'", // ′

	// Comparison and other symbols.
	"\u2264", "<=", // ≤
	"\u2265", ">=", // ≥
	"\u2260", "!=", // ≠
	"\u2026", "...", // …
	"\u2022", "*", // •

	// Invisible and unusual spaces.
	"\u00A0", " ", // no-break space
	"\u202F", " ", // narrow no-break space
	"\u2009", " ", // thin space
	"\u200A", " ", // hair space
	"\u2007", " ", // figure space
	"\u200B", "", // zero-width space
	"\u2060", "", // word joiner
	"\uFEFF", "", // zero-width no-break space / BOM
)

// sanitizeText replaces typographic characters with their ASCII spellings.
func sanitizeText(s string) string {
	return sanitizeReplacer.Replace(s)
}

// sanitizedArgKeys are the tool-argument keys holding text the model is
// writing - file contents, replacements, commit messages - which is what
// /sanitize cleans. Keys that must match what is already on disk (old_string,
// anchor, expected_text, pattern, paths) are deliberately absent: cleaning
// those would make an edit miss text that really does contain an em dash.
var sanitizedArgKeys = map[string]bool{
	"content": true, "text": true, "replacement": true, "message": true,
	"new_string": true, "newText": true, "new_text": true, "newString": true,
}

// sanitizeArgs cleans the written-text fields of a local tool call's
// arguments, at any depth (multi_edit's edits[].new_string, for one), and
// only the added lines of a diff. Arguments that are not a JSON object, or
// that hold nothing to change, are returned as they were.
func sanitizeArgs(raw json.RawMessage) json.RawMessage {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	changed := false
	v = sanitizeValue(v, "", &changed)
	if !changed {
		return raw
	}
	data, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return data
}

func sanitizeValue(v any, key string, changed *bool) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			t[k] = sanitizeValue(child, k, changed)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = sanitizeValue(child, key, changed)
		}
		return t
	case string:
		var out string
		switch {
		case key == "diff":
			out = sanitizeDiff(t)
		case sanitizedArgKeys[key]:
			out = sanitizeText(t)
		default:
			return t
		}
		if out != t {
			*changed = true
		}
		return out
	}
	return v
}

// sanitizeDiff cleans only a unified diff's added lines: context and removed
// lines have to match the file as it is.
func sanitizeDiff(diff string) string {
	lines := strings.SplitAfter(diff, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++ ") {
			lines[i] = sanitizeText(line)
		}
	}
	return strings.Join(lines, "")
}

// sanitizeReply cleans a reply in place: its text, its reasoning, and the
// arguments of each call to a local tool. External tools are left alone,
// since there is no telling which of their fields must match something
// exactly.
func (a *agent) sanitizeReply(reply *llmReply) {
	reply.Text = sanitizeText(reply.Text)
	reply.Reasoning = sanitizeText(reply.Reasoning)
	for i, call := range reply.ToolCalls {
		if tool, ok := a.toolIndex[call.Name]; ok && tool.source == "local" {
			reply.ToolCalls[i].Arguments = sanitizeArgs(call.Arguments)
		}
	}
}

// setSanitize is /sanitize's whole implementation, shared with hotCommand so
// it can be toggled while a turn is running.
func (a *agent) setSanitize(rest []string) {
	if len(rest) > 0 {
		switch strings.ToLower(rest[0]) {
		case "on":
			a.sanitize.Store(true)
		case "off":
			a.sanitize.Store(false)
		default:
			fmt.Fprintln(a.out, "usage: /sanitize [on|off]")
			return
		}
	} else {
		a.sanitize.Store(!a.sanitize.Load())
	}
	if a.sanitize.Load() {
		fmt.Fprintln(a.out, "sanitize: on - em/en dashes become -, arrows become -> and <-, curly quotes become "+
			"straight ones, and so on, in the model's replies and in the text it writes to files")
	} else {
		fmt.Fprintln(a.out, "sanitize: off - the model's output is used exactly as sent")
	}
}
