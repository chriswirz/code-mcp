package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	mdListRe   = regexp.MustCompile(`^(\s*)([-*+]|\d+[.)])(\s+)(.*)$`)
	mdRuleRe   = regexp.MustCompile(`^\s*(-{3,}|\*{3,}|_{3,})\s*$`)
	mdIndentRe = regexp.MustCompile(`^[ \t]*`)
	mdCodeSpan = regexp.MustCompile("`+[^`]*`+")
	mdMaskRe   = regexp.MustCompile("\x00(\\d+)\x00")
	mdBreakRe  = regexp.MustCompile(`([.!?]["')\]]?)(\s+)`)
	mdTailWord = regexp.MustCompile(`(\S+)$`)
	mdInitial  = regexp.MustCompile(`^\p{Lu}\.$`)
)

// mdAbbrev are tokens that end in a period without ending a sentence.
var mdAbbrev = map[string]bool{
	"e.g.": true, "i.e.": true, "etc.": true, "vs.": true, "cf.": true,
	"al.": true, "approx.": true, "no.": true, "Fig.": true, "ca.": true,
	"Mr.": true, "Mrs.": true, "Ms.": true, "Dr.": true, "St.": true,
	"Inc.": true, "Ltd.": true, "Co.": true,
}

// mdOpeners are the non-letter characters a sentence may legitimately start
// with: markdown emphasis, a link, a quote, or a masked code span.
const mdOpeners = "*_[(\"'\x00"

// maskCodeSpans hides inline code so its punctuation cannot be mistaken for a
// sentence break. Without this, `go test ./...` splits three ways.
func maskCodeSpans(text string) (string, []string) {
	var spans []string
	masked := mdCodeSpan.ReplaceAllStringFunc(text, func(m string) string {
		spans = append(spans, m)
		return "\x00" + strconv.Itoa(len(spans)-1) + "\x00"
	})
	return masked, spans
}

func unmaskCodeSpans(text string, spans []string) string {
	if len(spans) == 0 {
		return text
	}
	return mdMaskRe.ReplaceAllStringFunc(text, func(m string) string {
		n, err := strconv.Atoi(strings.Trim(m, "\x00"))
		if err != nil || n < 0 || n >= len(spans) {
			return m
		}
		return spans[n]
	})
}

// splitMarkdownSentences breaks a run of prose into sentences. A break needs
// terminal punctuation, whitespace, and something that can open a sentence, and
// the token before the punctuation must not be a known abbreviation.
func splitMarkdownSentences(text string) []string {
	masked, spans := maskCodeSpans(text)
	var parts []string
	start := 0

	for _, m := range mdBreakRe.FindAllStringSubmatchIndex(masked, -1) {
		punctEnd, afterSpace := m[3], m[1]
		if afterSpace >= len(masked) {
			continue
		}
		next, _ := utf8.DecodeRuneInString(masked[afterSpace:])
		if !unicode.IsUpper(next) && !unicode.IsDigit(next) && !strings.ContainsRune(mdOpeners, next) {
			continue
		}
		if word := mdTailWord.FindString(masked[:punctEnd]); word != "" {
			if mdAbbrev[word] || mdInitial.MatchString(word) {
				continue
			}
		}
		if punctEnd <= start {
			continue
		}
		if seg := strings.TrimSpace(masked[start:punctEnd]); seg != "" {
			parts = append(parts, seg)
		}
		start = afterSpace
	}
	if tail := strings.TrimSpace(masked[start:]); tail != "" {
		parts = append(parts, tail)
	}
	if len(parts) == 0 {
		return []string{strings.TrimSpace(text)}
	}
	for i := range parts {
		parts[i] = unmaskCodeSpans(parts[i], spans)
	}
	return parts
}

// isMarkdownBoundary reports whether a line ends the run of prose being
// gathered. These lines are structural and pass through untouched.
func isMarkdownBoundary(line string) bool {
	trimmed := strings.TrimSpace(line)
	switch {
	case trimmed == "":
		return true
	case strings.HasPrefix(trimmed, "#"):
		return true
	case strings.HasPrefix(trimmed, "|"):
		return true
	case strings.HasPrefix(trimmed, "```"), strings.HasPrefix(trimmed, "~~~"):
		return true
	case strings.HasPrefix(trimmed, ">"):
		return true
	case mdRuleRe.MatchString(line):
		return true
	case mdListRe.MatchString(line):
		return true
	}
	return false
}

func isFence(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
}

// reflowMarkdown rewrites prose to one sentence per line. The file's own line
// endings are preserved, so running this on a CRLF document does not silently
// convert it.
func reflowMarkdown(src string) string {
	ending := dominantLineEnding(src)
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")

	out := make([]string, 0, len(lines))
	i := 0

	// YAML front matter is not prose and must survive verbatim, so it is copied
	// across before the main walk begins.
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		out = append(out, lines[0])
		i = 1
		for i < len(lines) {
			out = append(out, lines[i])
			closed := strings.TrimSpace(lines[i]) == "---"
			i++
			if closed {
				break
			}
		}
	}

	inFence := false
	for i < len(lines) {
		line := lines[i]

		if isFence(line) {
			inFence = !inFence
			out = append(out, line)
			i++
			continue
		}
		if inFence || (isMarkdownBoundary(line) && !mdListRe.MatchString(line)) {
			out = append(out, line)
			i++
			continue
		}

		var lead, hang string
		var buf []string
		if m := mdListRe.FindStringSubmatch(line); m != nil {
			lead = m[1] + m[2] + m[3]
			hang = strings.Repeat(" ", len(lead))
			buf = []string{m[4]}
		} else {
			// A paragraph's own indentation is significant: it is what keeps a
			// continuation attached to the list item it belongs to.
			lead = mdIndentRe.FindString(line)
			hang = lead
			buf = []string{strings.TrimSpace(line)}
		}

		i++
		for i < len(lines) && !isMarkdownBoundary(lines[i]) && !isFence(lines[i]) {
			buf = append(buf, strings.TrimSpace(lines[i]))
			i++
		}

		sentences := splitMarkdownSentences(strings.Join(buf, " "))
		out = append(out, lead+sentences[0])
		for _, s := range sentences[1:] {
			out = append(out, hang+s)
		}
	}

	joined := strings.Join(out, "\n")
	if ending != "\n" {
		joined = strings.ReplaceAll(joined, "\n", ending)
	}
	return joined
}

// sameWords reports whether two documents carry the same words in the same
// order. Reflowing only ever moves line breaks, so this is the invariant that
// says the rewrite did not eat or reorder any text.
func sameWords(a, b string) bool {
	fa, fb := strings.Fields(a), strings.Fields(b)
	if len(fa) != len(fb) {
		return false
	}
	for i := range fa {
		if fa[i] != fb[i] {
			return false
		}
	}
	return true
}

// registerMarkdownTools adds the document formatter.
func (s *Server) registerMarkdownTools() {
	ws := s.ws

	s.RegisterTool(Tool{
		Name:  "format_markdown",
		Title: "Reflow markdown",
		Description: "Rewrite a markdown document so that each sentence starts on its own line. " +
			"Prose wrapped to a fixed column produces diffs in which one edited word reflows the whole paragraph; " +
			"one sentence per line keeps the diff to the sentences that actually changed. " +
			"Code fences, tables, headings, block quotes, YAML front matter and the blank-line structure pass through untouched, " +
			"list items keep their marker with later sentences hanging at the text column, and inline code is protected so a path " +
			"like `./...` is not mistaken for a sentence end. No word is added, removed or reordered, and the tool refuses to write " +
			"if that turns out not to hold. Pass path to rewrite a workspace file, or content to format text without touching disk.",
		Annotations: &ToolAnnotations{DestructiveHint: true, IdempotentHint: true},
		InputSchema: schema(nil, map[string]any{
			"path":    prop("string", "Markdown file relative to the workspace root. Paths are workspace-relative; an absolute path such as \"/README.md\" is interpreted relative to the workspace root, never the root of the filesystem."),
			"content": prop("string", "Markdown to format and return directly. Nothing is written, and path is not needed."),
			"dry_run": propDefault("boolean", "With path, report the diff that would be made without writing it.", false),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Path    string `json:"path"`
			Content string `json:"content"`
			DryRun  bool   `json:"dry_run"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}

		switch {
		case args.Path == "" && args.Content == "":
			return toolError("one of path or content is required"), nil
		case args.Path != "" && args.Content != "":
			return toolError("pass either path or content, not both"), nil
		}

		if args.Content != "" {
			formatted := reflowMarkdown(args.Content)
			if !sameWords(args.Content, formatted) {
				return toolError("refusing to return the result: reflowing changed the text, not just the line breaks (this is a bug)"), nil
			}
			return toolResult(formatted), nil
		}

		original, err := ws.ReadFile(args.Path)
		if err != nil {
			return toolError("%v", err), nil
		}
		formatted := reflowMarkdown(original)

		// The whole point of the tool is that it only moves line breaks. If that
		// is not what happened, the file is left alone rather than trusted.
		if !sameWords(original, formatted) {
			return toolError("refusing to write %s: reflowing changed the text, not just the line breaks (this is a bug)", args.Path), nil
		}
		name := ws.Canonical(args.Path)
		if formatted == original {
			return toolResult(fmt.Sprintf("%s is already one sentence per line", name)), nil
		}

		before := strings.Count(original, "\n") + 1
		after := strings.Count(formatted, "\n") + 1
		if args.DryRun {
			return toolResult(fmt.Sprintf("Dry run: nothing was written.\n%s: %d lines would become %d\n%s",
				name, before, after, changedHunk(name, original, formatted))), nil
		}
		if _, err := ws.WriteFile(args.Path, formatted); err != nil {
			return toolError("%v", err), nil
		}
		return toolResult(fmt.Sprintf("%s: %d lines became %d\n%s",
			name, before, after, changedHunk(name, original, formatted))), nil
	})
}
