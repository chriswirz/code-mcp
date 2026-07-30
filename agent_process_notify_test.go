package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestRunProcessCallWantsNotify(t *testing.T) {
	if runProcessCallWantsNotify(json.RawMessage(`{"command":"x"}`)) {
		t.Error("notify should default to false")
	}
	if !runProcessCallWantsNotify(json.RawMessage(`{"command":"x","notify":true}`)) {
		t.Error("notify: true should be read as true")
	}
	if runProcessCallWantsNotify(json.RawMessage(`{"command":"x","notify":false}`)) {
		t.Error("notify: false should be read as false")
	}
}

func TestWatchForProcessCompletionSkipsAlreadyFinishedResult(t *testing.T) {
	srv, _ := newTestServer(t)
	a := &agent{srv: srv, allowed: map[string]bool{}, out: io.Discard, lines: make(chan string), skipPerms: true}

	// A result that already says the process is not running - the
	// wait_seconds case - has nothing left to wait for.
	res := &CallToolResult{StructuredContent: processStatus{ID: "proc-x", Running: false}}
	a.watchForProcessCompletion(res)

	select {
	case note := <-a.lines:
		t.Fatalf("unexpected notification for an already-finished process: %q", note)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestWatchForProcessCompletionSkipsUnrecognizedStructuredContent(t *testing.T) {
	srv, _ := newTestServer(t)
	a := &agent{srv: srv, allowed: map[string]bool{}, out: io.Discard, lines: make(chan string), skipPerms: true}

	// A remote MCP tool that happened to also be named run_process would
	// come back with StructuredContent of some other shape - not a panic.
	res := &CallToolResult{StructuredContent: map[string]any{"running": true}}
	a.watchForProcessCompletion(res)

	select {
	case note := <-a.lines:
		t.Fatalf("unexpected notification: %q", note)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestWatchForProcessCompletionSkipsUnknownID(t *testing.T) {
	srv, _ := newTestServer(t)
	a := &agent{srv: srv, allowed: map[string]bool{}, out: io.Discard, lines: make(chan string), skipPerms: true}

	res := &CallToolResult{StructuredContent: processStatus{ID: "no-such-process", Running: true}}
	a.watchForProcessCompletion(res)

	select {
	case note := <-a.lines:
		t.Fatalf("unexpected notification: %q", note)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestExecuteToolNotifiesWhenProcessFinishes drives the real run_process
// tool end to end: notify: true on a process that is still running when
// run_process returns must deliver a notification, through a.lines, once
// that process actually exits later.
func TestExecuteToolNotifiesWhenProcessFinishes(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.processes.stopAll()
	a := &agent{srv: srv, allowed: map[string]bool{}, out: io.Discard, lines: make(chan string), skipPerms: true}
	a.rebuildTools()

	args, _ := json.Marshal(map[string]any{"command": echoLoop(t), "notify": true})
	result := a.executeTool(context.Background(), toolCall{ID: "c1", Name: "run_process", Arguments: args})
	if !strings.Contains(result, "running for") {
		t.Fatalf("expected the process to still be running right after starting it: %s", result)
	}

	select {
	case note := <-a.lines:
		if !strings.Contains(note, "finished") || !strings.Contains(note, "second") {
			t.Errorf("notification = %q, want it to say the process finished and include its output", note)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the completion notification")
	}
}

// TestExecuteToolDoesNotNotifyWithoutOptingIn is notify's default: off.
func TestExecuteToolDoesNotNotifyWithoutOptingIn(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.processes.stopAll()
	a := &agent{srv: srv, allowed: map[string]bool{}, out: io.Discard, lines: make(chan string), skipPerms: true}
	a.rebuildTools()

	args, _ := json.Marshal(map[string]any{"command": echoLoop(t)})
	result := a.executeTool(context.Background(), toolCall{ID: "c1", Name: "run_process", Arguments: args})
	if !strings.Contains(result, "running for") {
		t.Fatalf("expected the process to still be running right after starting it: %s", result)
	}

	select {
	case note := <-a.lines:
		t.Fatalf("unexpected notification with notify left at its default: %q", note)
	case <-time.After(3 * time.Second):
	}
}

// TestExecuteToolDoesNotNotifyWhenWaitSecondsAlreadyCaughtIt: nothing is left
// to wait for once the result already reports the process as finished.
func TestExecuteToolDoesNotNotifyWhenWaitSecondsAlreadyCaughtIt(t *testing.T) {
	srv, _ := newTestServer(t)
	defer srv.processes.stopAll()
	a := &agent{srv: srv, allowed: map[string]bool{}, out: io.Discard, lines: make(chan string), skipPerms: true}
	a.rebuildTools()

	args, _ := json.Marshal(map[string]any{"command": "echo hi", "notify": true, "wait_seconds": 5})
	result := a.executeTool(context.Background(), toolCall{ID: "c1", Name: "run_process", Arguments: args})
	if strings.Contains(result, "Still running") {
		t.Fatalf("expected a quick command to finish within wait_seconds: %s", result)
	}

	select {
	case note := <-a.lines:
		t.Fatalf("unexpected notification for a process wait_seconds already caught: %q", note)
	case <-time.After(300 * time.Millisecond):
	}
}
