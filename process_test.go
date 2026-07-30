package main

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// echoLoop is a shell line that prints a marker, waits, then prints another, so
// a test can observe a process both mid-flight and after it exits.
func echoLoop(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		// ping is the portable sleep on cmd.exe: no timeout /t, which needs a
		// console this process does not give a child.
		return "echo first& ping -n 3 127.0.0.1 >nul& echo second"
	}
	return "echo first; sleep 1; echo second"
}

// waitFor polls until cond holds, so a test never sleeps longer than it must
// and never fails merely because a machine was slow.
func waitFor(t *testing.T, why string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

func TestProcessRunsToCompletionAndCollectsOutput(t *testing.T) {
	r := newProcessRegistry()
	p, err := r.start(echoLoop(t), t.TempDir(), nil, 30*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.id == "" {
		t.Fatal("want a process id immediately")
	}

	// Mid-flight: running, and the first line is already collected.
	waitFor(t, "the first line of output", func() bool {
		return strings.Contains(p.status(0, nil).Stdout, "first")
	})
	if st := p.status(0, nil); !st.Running {
		t.Error("the process should still be running while it sleeps")
	} else if st.ExitCode != nil {
		t.Errorf("a running process has no exit code, got %d", *st.ExitCode)
	}

	<-p.done
	st := p.status(0, nil)
	if st.Running {
		t.Error("the process should be finished")
	}
	if st.ExitCode == nil || *st.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", st.ExitCode)
	}
	if !strings.Contains(st.Stdout, "second") {
		t.Errorf("stdout = %q, want the line printed after the wait", st.Stdout)
	}
	if _, ok := r.get(p.id); !ok {
		t.Error("a finished process should stay readable by id")
	}
}

func TestProcessReportsExitCode(t *testing.T) {
	r := newProcessRegistry()
	p, err := r.start("exit 3", t.TempDir(), nil, 30*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	<-p.done
	st := p.status(0, nil)
	if st.ExitCode == nil || *st.ExitCode != 3 {
		t.Errorf("exit code = %v, want 3", st.ExitCode)
	}
	if st.Failure != "" {
		t.Errorf("a non-zero exit is not a failure to run, got %q", st.Failure)
	}
}

func TestProcessTimeoutKills(t *testing.T) {
	r := newProcessRegistry()
	line := "sleep 30"
	if runtime.GOOS == "windows" {
		line = "ping -n 30 127.0.0.1 >nul"
	}
	p, err := r.start(line, t.TempDir(), nil, 300*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	case <-time.After(20 * time.Second):
		t.Fatal("the timeout did not kill the process")
	}
	if st := p.status(0, nil); !st.TimedOut {
		t.Errorf("status = %+v, want timed_out", st)
	}
}

func TestProcessStop(t *testing.T) {
	r := newProcessRegistry()
	line := "sleep 30"
	if runtime.GOOS == "windows" {
		line = "ping -n 30 127.0.0.1 >nul"
	}
	p, err := r.start(line, t.TempDir(), nil, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	case <-time.After(20 * time.Second):
		t.Fatal("stop did not end the process")
	}
	if st := p.status(0, nil); st.Running || !st.Killed {
		t.Errorf("status = %+v, want a stopped process", st)
	}
	// Stopping twice is what a model retrying a call does; it must not error.
	if err := p.stop(); err != nil {
		t.Errorf("stopping a finished process: %v", err)
	}
}

func TestProcessUnknownID(t *testing.T) {
	r := newProcessRegistry()
	if _, ok := r.get("proc-404"); ok {
		t.Error("an unknown id should not resolve")
	}
}

func TestOutputBufferKeepsTailAndCounts(t *testing.T) {
	b := &outputBuffer{}
	chunk := strings.Repeat("x", maxProcessOutputBytes)
	b.Write([]byte(chunk))
	b.Write([]byte("tail"))
	text, total, dropped := b.snapshot()
	if len(text) != maxProcessOutputBytes {
		t.Errorf("retained %d bytes, want the buffer capped at %d", len(text), maxProcessOutputBytes)
	}
	if !strings.HasSuffix(text, "tail") {
		t.Error("the most recent output should be the part kept")
	}
	if want := int64(maxProcessOutputBytes + 4); total != want {
		t.Errorf("total = %d, want %d", total, want)
	}
	if dropped != 4 {
		t.Errorf("dropped = %d, want 4", dropped)
	}
}

func TestTailLines(t *testing.T) {
	in := "one\ntwo\nthree\n"
	if got := tail(in, 0); got != in {
		t.Errorf("tail(_, 0) = %q, want everything", got)
	}
	if got := tail(in, 2); got != "two\nthree\n" {
		t.Errorf("tail(_, 2) = %q", got)
	}
	if got := tail(in, 9); got != in {
		t.Errorf("asking for more lines than exist should return everything, got %q", got)
	}
}

// A finished process may be forgotten once the registry is full; one that is
// still running never is, because its output is the only record of it.
func TestProcessEvictionKeepsRunning(t *testing.T) {
	r := newProcessRegistry()
	line := "sleep 30"
	if runtime.GOOS == "windows" {
		line = "ping -n 30 127.0.0.1 >nul"
	}
	live, err := r.start(line, t.TempDir(), nil, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer live.stop()

	for i := 0; i < maxProcesses+4; i++ {
		p, err := r.start("exit 0", t.TempDir(), nil, 30*time.Second, nil)
		if err != nil {
			t.Fatal(err)
		}
		<-p.done
	}
	if _, ok := r.get(live.id); !ok {
		t.Error("the running process was evicted")
	}
	if n := len(r.list()); n > maxProcesses+1 {
		t.Errorf("tracking %d processes, want the finished ones evicted down to %d", n, maxProcesses)
	}
}

// The tools are what a model actually calls, so exercise the full round trip:
// start something slow, get an id back, poll it, and stop it.
func TestProcessToolsRoundTrip(t *testing.T) {
	s, _ := newTestServer(t)
	defer s.processes.stopAll()

	line := "echo started; sleep 30"
	if runtime.GOOS == "windows" {
		line = "echo started& ping -n 30 127.0.0.1 >nul"
	}
	got := call(t, s, "tools/call", map[string]any{
		"name":      "run_process",
		"arguments": map[string]any{"command": line},
	})
	if got["isError"] == true {
		t.Fatalf("run_process failed: %v", got)
	}
	structured, _ := got["structuredContent"].(map[string]any)
	id, _ := structured["process_id"].(string)
	if id == "" {
		t.Fatalf("no process id in %v", got)
	}
	if structured["running"] != true {
		t.Errorf("a sleeping process should be reported as running: %v", structured)
	}

	// Polling shows the output collected so far, while it is still running.
	waitFor(t, "the first line through check_process_id", func() bool {
		res := call(t, s, "tools/call", map[string]any{
			"name":      "check_process_id",
			"arguments": map[string]any{"process_id": id},
		})
		st, _ := res["structuredContent"].(map[string]any)
		out, _ := st["stdout"].(string)
		return strings.Contains(out, "started")
	})

	stopped := call(t, s, "tools/call", map[string]any{
		"name":      "stop_process",
		"arguments": map[string]any{"process_id": id},
	})
	if stopped["isError"] == true {
		t.Fatalf("stop_process failed: %v", stopped)
	}
	after := call(t, s, "tools/call", map[string]any{
		"name":      "check_process_id",
		"arguments": map[string]any{"process_id": id},
	})
	st, _ := after["structuredContent"].(map[string]any)
	if st["running"] != false {
		t.Errorf("the process should be stopped: %v", st)
	}

	listed := call(t, s, "tools/call", map[string]any{"name": "list_processes"})
	body, _ := listed["content"].([]any)
	block, _ := body[0].(map[string]any)
	if !strings.Contains(block["text"].(string), id) {
		t.Errorf("list_processes does not mention %s: %v", id, block["text"])
	}

	unknown := call(t, s, "tools/call", map[string]any{
		"name":      "check_process_id",
		"arguments": map[string]any{"process_id": "proc-404"},
	})
	if unknown["isError"] != true {
		t.Error("checking an unknown id should be an error")
	}
}

// TestBackgroundCommandStartsAsProcess covers a config.json command marked
// background: it must come back with a process id instead of blocking, and be
// pollable through the ordinary process tools.
func TestBackgroundCommandStartsAsProcess(t *testing.T) {
	s, _ := newTestServer(t)
	defer s.processes.stopAll()

	got := call(t, s, "tools/call", map[string]any{"name": "serve"})
	if got["isError"] == true {
		t.Fatalf("serve failed: %v", got)
	}
	st, _ := got["structuredContent"].(map[string]any)
	id, _ := st["process_id"].(string)
	if id == "" {
		t.Fatalf("a background command should return a process id: %v", got)
	}
	if st["running"] != true {
		t.Errorf("a sleeping process should be reported as running: %v", st)
	}

	waitFor(t, "the background command's output through check_process_id", func() bool {
		res := call(t, s, "tools/call", map[string]any{
			"name":      "check_process_id",
			"arguments": map[string]any{"process_id": id},
		})
		poll, _ := res["structuredContent"].(map[string]any)
		out, _ := poll["stdout"].(string)
		return strings.Contains(out, "first")
	})

	if stopped := call(t, s, "tools/call", map[string]any{
		"name":      "stop_process",
		"arguments": map[string]any{"process_id": id},
	}); stopped["isError"] == true {
		t.Fatalf("stop_process failed: %v", stopped)
	}
}

// TestBlockingCommandStaysBlocking guards the default: a command without the
// flag still returns its output inline and registers no process.
func TestBlockingCommandStaysBlocking(t *testing.T) {
	s, _ := newTestServer(t)
	defer s.processes.stopAll()

	got := call(t, s, "tools/call", map[string]any{"name": "build"})
	if got["isError"] == true {
		t.Fatalf("build failed: %v", got)
	}
	if st, ok := got["structuredContent"].(map[string]any); ok && st["process_id"] != nil {
		t.Errorf("a plain command should not start a process: %v", st)
	}
	if n := len(s.processes.list()); n != 0 {
		t.Errorf("a plain command registered %d processes, want 0", n)
	}
}
