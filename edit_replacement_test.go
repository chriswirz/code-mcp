package main

import (
	"strings"
	"testing"
)

// An edit says what to remove and what to put there. If the second half does
// not arrive - misspelled, dropped, never written - applying the first half
// alone deletes the code the caller meant to change, and reports success while
// doing it. That is the worst failure a file tool has, because the damage is
// silent and the report says otherwise.
//
// These tests hold the line: a replacement that is absent is refused, and one
// that is deliberately empty still deletes.

// editCall runs multi_edit with one edit and returns the tool's text.
func editCall(t *testing.T, s *Server, edit map[string]any) string {
	t.Helper()
	return toolText(t, s, "multi_edit", map[string]any{"edits": []any{edit}})
}

// TestReplacementUnderAnUnreadKeyIsRefused covers the spellings a model
// reaches for. Every one of these used to delete the anchor and report
// "1 replacement(s)".
func TestReplacementUnderAnUnreadKeyIsRefused(t *testing.T) {
	unread := []string{"newStr", "replacement", "new", "content", "new_value", "to"}
	for _, key := range unread {
		s, root := newTestServer(t)
		write(t, root, "a.go", "before\nOLD\nafter\n")

		text := editCall(t, s, map[string]any{"path": "a.go", "old_string": "OLD", key: "NEW"})
		if !strings.HasPrefix(text, "ERROR:") {
			t.Errorf("%s: the edit should have been refused: %s", key, text)
		}
		if !strings.Contains(text, key) {
			t.Errorf("%s: the error should name the key that was not read: %s", key, text)
		}
		if got := mustRead(t, root, "a.go"); got != "before\nOLD\nafter\n" {
			t.Errorf("%s: the file was changed: %q", key, got)
		}
	}
}

func TestEditWithNoReplacementAtAllIsRefused(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.go", "before\nOLD\nafter\n")

	text := editCall(t, s, map[string]any{"path": "a.go", "old_string": "OLD"})
	if !strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("the edit should have been refused: %s", text)
	}
	for _, want := range []string{"new_string", "would delete", "empty string"} {
		if !strings.Contains(text, want) {
			t.Errorf("the error should mention %q: %s", want, text)
		}
	}
	if got := mustRead(t, root, "a.go"); got != "before\nOLD\nafter\n" {
		t.Errorf("the file was changed: %q", got)
	}
}

// TestNullReplacementIsRefused: null is not an empty string, and a client that
// serialises a missing value as null has not supplied a replacement.
func TestNullReplacementIsRefused(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.go", "before\nOLD\nafter\n")

	if text := editCall(t, s, map[string]any{
		"path": "a.go", "old_string": "OLD", "new_string": nil,
	}); !strings.HasPrefix(text, "ERROR:") {
		t.Errorf("a null replacement should be refused: %s", text)
	}
	if got := mustRead(t, root, "a.go"); got != "before\nOLD\nafter\n" {
		t.Errorf("the file was changed: %q", got)
	}
}

// TestDeliberateDeletionStillWorks is the other side: an empty string is a
// replacement, and it deletes.
func TestDeliberateDeletionStillWorks(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.go", "before\nOLD\nafter\n")

	text := editCall(t, s, map[string]any{"path": "a.go", "old_string": "OLD\n", "new_string": ""})
	if strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("an explicit empty replacement should delete: %s", text)
	}
	if got := mustRead(t, root, "a.go"); got != "before\nafter\n" {
		t.Errorf("a.go = %q, want the line deleted", got)
	}
}

// TestReplacementSpellingsThatAreRead: the aliases this tool does accept, all
// of which must actually replace.
func TestReplacementSpellingsThatAreRead(t *testing.T) {
	spellings := []map[string]any{
		{"path": "a.go", "old_string": "OLD", "new_string": "NEW"},
		{"path": "a.go", "oldText": "OLD", "newText": "NEW"},
		{"path": "a.go", "old_text": "OLD", "new_text": "NEW"},
		{"path": "a.go", "oldString": "OLD", "newString": "NEW"},
		{"path": "a.go", "old_string": "OLD", "newText": "NEW"},
	}
	for i, edit := range spellings {
		s, root := newTestServer(t)
		write(t, root, "a.go", "before\nOLD\nafter\n")

		text := editCall(t, s, edit)
		if strings.HasPrefix(text, "ERROR:") {
			t.Errorf("spelling %d (%v) was refused: %s", i, edit, text)
			continue
		}
		if got := mustRead(t, root, "a.go"); got != "before\nNEW\nafter\n" {
			t.Errorf("spelling %d (%v): a.go = %q", i, edit, got)
		}
	}
}

// TestOneBadEditWritesNothing: the batch is checked before anything is
// written, so a lost replacement in the second edit must not leave the first
// applied.
func TestOneBadEditWritesNothing(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.go", "ONE\nTWO\n")

	text := toolText(t, s, "multi_edit", map[string]any{"edits": []any{
		map[string]any{"path": "a.go", "old_string": "ONE", "new_string": "1"},
		map[string]any{"path": "a.go", "old_string": "TWO", "replacement": "2"},
	}})
	if !strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("the batch should have been refused: %s", text)
	}
	if !strings.Contains(text, "edits[1]") {
		t.Errorf("the error should name the edit that was wrong: %s", text)
	}
	if got := mustRead(t, root, "a.go"); got != "ONE\nTWO\n" {
		t.Errorf("nothing should have been written, got %q", got)
	}
}

func TestEditFileRefusesAMissingReplacement(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.go", "before\nOLD\nafter\n")

	text := toolText(t, s, "edit_file", map[string]any{"path": "a.go", "old_string": "OLD"})
	if !strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("edit_file should refuse an edit with no replacement: %s", text)
	}
	if got := mustRead(t, root, "a.go"); got != "before\nOLD\nafter\n" {
		t.Errorf("the file was changed: %q", got)
	}
	// And the deliberate deletion still works through edit_file too.
	if text := toolText(t, s, "edit_file", map[string]any{
		"path": "a.go", "old_string": "OLD\n", "new_string": "",
	}); strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("an explicit empty replacement should delete: %s", text)
	}
	if got := mustRead(t, root, "a.go"); got != "before\nafter\n" {
		t.Errorf("a.go = %q, want the line deleted", got)
	}
}

// TestUnknownKeysAreCollected is the machinery behind the error message.
func TestUnknownKeysAreCollected(t *testing.T) {
	var op editOp
	if bad := decodeArgs([]byte(`{"path":"a.go","old_string":"A","replacement":"B","why":"because"}`), &op); bad != nil {
		t.Fatalf("decode failed: %v", bad.Content)
	}
	if len(op.Extra) != 2 || op.Extra[0] != "replacement" || op.Extra[1] != "why" {
		t.Errorf("Extra = %v, want the two unread keys in order", op.Extra)
	}
	if _, _, hasNew := op.anchor(); hasNew {
		t.Error("an edit with no replacement field should report none")
	}

	var good editOp
	if bad := decodeArgs([]byte(`{"path":"a.go","old_string":"A","new_string":"B","replace_all":true}`), &good); bad != nil {
		t.Fatalf("decode failed: %v", bad.Content)
	}
	if len(good.Extra) != 0 {
		t.Errorf("Extra = %v, want nothing: every key here is read", good.Extra)
	}
	if !good.ReplaceAll {
		t.Error("replace_all was lost")
	}
}
