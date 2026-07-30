package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// newStaleReadTestAgent wires a real local server's file tools into an agent
// the way rebuildTools does at startup, so executeTool actually reads from
// disk rather than a stand-in.
func newStaleReadTestAgent(t *testing.T) (*agent, string) {
	t.Helper()
	srv, root := newTestServer(t)
	srv.registerCodeTools() // for read_files, not registered by newTestServer
	a := &agent{srv: srv, allowed: map[string]bool{}, out: io.Discard}
	a.rebuildTools()
	return a, root
}

// runToolAsATurnWould calls executeTool and appends the assistant/tool
// message pair runTurn would, so nudgeStaleReads has real history to walk.
func runToolAsATurnWould(t *testing.T, a *agent, call toolCall) string {
	t.Helper()
	result := a.executeTool(context.Background(), call)
	a.history = append(a.history,
		chatMessage{Role: "assistant", ToolCalls: []toolCall{call}},
		chatMessage{Role: "tool", ToolCallID: call.ID, ToolName: call.Name, Content: result},
	)
	return result
}

func TestNudgeStaleReadsReplacesEarlierReadFile(t *testing.T) {
	a, root := newStaleReadTestAgent(t)
	if err := os.WriteFile(filepath.Join(root, "foo.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	first := runToolAsATurnWould(t, a, toolCall{ID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"foo.txt"}`)})
	if a.history[1].Content != first {
		t.Fatalf("history[1] = %q before the second read, want the first result untouched", a.history[1].Content)
	}

	if err := os.WriteFile(filepath.Join(root, "foo.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := runToolAsATurnWould(t, a, toolCall{ID: "c2", Name: "read_file", Arguments: json.RawMessage(`{"path":"foo.txt"}`)})

	if a.history[1].Content == first {
		t.Errorf("the first read's result was not nudged: %q", a.history[1].Content)
	}
	if a.history[1].Content == "" || a.history[1].Content == second {
		t.Errorf("first result after nudging = %q, want a stale-read note", a.history[1].Content)
	}
	if a.history[3].Content != second {
		t.Errorf("history[3] = %q, want the second read's own result untouched", a.history[3].Content)
	}
}

func TestNudgeStaleReadsSkippedWithPreserveTrue(t *testing.T) {
	a, root := newStaleReadTestAgent(t)
	if err := os.WriteFile(filepath.Join(root, "foo.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	first := runToolAsATurnWould(t, a, toolCall{ID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"foo.txt"}`)})

	if err := os.WriteFile(filepath.Join(root, "foo.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runToolAsATurnWould(t, a, toolCall{ID: "c2", Name: "read_file", Arguments: json.RawMessage(`{"path":"foo.txt","preserve":true}`)})

	if a.history[1].Content != first {
		t.Errorf("history[1] = %q, want it left intact when preserve is true", a.history[1].Content)
	}
}

func TestNudgeStaleReadsMatchesNormalizedPaths(t *testing.T) {
	a, root := newStaleReadTestAgent(t)
	if err := os.WriteFile(filepath.Join(root, "foo.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	first := runToolAsATurnWould(t, a, toolCall{ID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"./foo.txt"}`)})
	runToolAsATurnWould(t, a, toolCall{ID: "c2", Name: "read_file", Arguments: json.RawMessage(`{"path":"foo.txt"}`)})

	if a.history[1].Content == first {
		t.Errorf("\"./foo.txt\" and \"foo.txt\" should be recognized as the same file")
	}
}

func TestNudgeStaleReadsLeavesOtherFilesAlone(t *testing.T) {
	a, root := newStaleReadTestAgent(t)
	for _, name := range []string{"foo.txt", "bar.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("content of "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	foo := runToolAsATurnWould(t, a, toolCall{ID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"foo.txt"}`)})
	runToolAsATurnWould(t, a, toolCall{ID: "c2", Name: "read_file", Arguments: json.RawMessage(`{"path":"bar.txt"}`)})

	if a.history[1].Content != foo {
		t.Errorf("reading bar.txt should not touch foo.txt's earlier result: %q", a.history[1].Content)
	}
}

// TestNudgeStaleReadsCrossesReadFileAndReadFiles: the two tools share one
// file identity, so a batch read_files call reading a path nudges an earlier
// plain read_file of the same path, and the reverse.
func TestNudgeStaleReadsCrossesReadFileAndReadFiles(t *testing.T) {
	a, root := newStaleReadTestAgent(t)
	if err := os.WriteFile(filepath.Join(root, "foo.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	first := runToolAsATurnWould(t, a, toolCall{ID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"foo.txt"}`)})
	runToolAsATurnWould(t, a, toolCall{ID: "c2", Name: "read_files", Arguments: json.RawMessage(`{"files":["foo.txt"]}`)})

	if a.history[1].Content == first {
		t.Errorf("a read_files call reading foo.txt should nudge the earlier read_file of it")
	}
}

func TestReadFileErrorDoesNotNudgeEarlierReads(t *testing.T) {
	a, root := newStaleReadTestAgent(t)
	if err := os.WriteFile(filepath.Join(root, "foo.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	first := runToolAsATurnWould(t, a, toolCall{ID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"foo.txt"}`)})
	// A second read of a file that does not exist: it should not invalidate
	// the earlier, still-valid read of foo.txt.
	runToolAsATurnWould(t, a, toolCall{ID: "c2", Name: "read_file", Arguments: json.RawMessage(`{"path":"nope.txt"}`)})

	if a.history[1].Content != first {
		t.Errorf("history[1] = %q, want the unrelated earlier read left alone", a.history[1].Content)
	}
}
