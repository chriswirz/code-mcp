package main

import (
	"os"
	"strings"
	"testing"
)

// TestReflowMarkdownOnREADME exercises the formatter against this repository's
// own README, which is the largest real document to hand and the one that
// surfaced the indentation and code-span cases in the first place.
func TestReflowMarkdownOnREADME(t *testing.T) {
	src, err := os.ReadFile("README.md")
	if err != nil {
		t.Skipf("no README to check: %v", err)
	}
	original := string(src)
	formatted := reflowMarkdown(original)

	if !sameWords(original, formatted) {
		t.Error("reflowing the README changed its words")
	}
	if formatted != original {
		t.Error("the README is checked in reflowed, so this should be a no-op")
	}
	if second := reflowMarkdown(formatted); second != formatted {
		t.Error("reflow is not idempotent on the README")
	}

	// No prose line should start lowercase: that is what a split landing in the
	// middle of a sentence looks like.
	inFence := false
	for n, line := range strings.Split(formatted, "\n") {
		if isFence(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "`") || strings.HasPrefix(trimmed, "|") {
			continue
		}
		if r := []rune(trimmed); len(r) > 0 && r[0] >= 'a' && r[0] <= 'z' {
			t.Errorf("line %d starts mid-sentence: %s", n+1, trimmed)
		}
	}
}
