package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// A process is a command that outlives the request that started it.
//
// run_command has to answer within the HTTP request it arrived on, so anything
// slower than the client's timeout - a long build, a migration, a dev server,
// a soak test - is lost halfway through with nothing to show for it. A process
// is started, detached from the request, and polled afterwards: run_process
// hands back an id immediately, and check_process_id says whether it is still
// running and returns whatever it has printed so far.
type managedProcess struct {
	id      string
	command string
	dir     string
	started time.Time
	timeout time.Duration

	cmd    *exec.Cmd
	stdout *outputBuffer
	stderr *outputBuffer

	// done is closed when the process has exited and every field below it has
	// its final value.
	done chan struct{}

	mu       sync.Mutex
	exited   bool
	exitCode int
	timedOut bool
	killed   bool
	failure  string
	ended    time.Time
}

// maxProcessOutputBytes is what is kept per stream for a running process. It is
// larger than maxOutputBytes because the buffer is not the tool result: a
// caller reads a window of it, so holding more history costs memory here rather
// than context in the model.
const maxProcessOutputBytes = 1 << 20

// defaultProcessTimeout bounds a process that is never checked on again, so a
// forgotten dev server does not outlive the session indefinitely.
const defaultProcessTimeout = time.Hour

// maxProcesses bounds how many are tracked at once. Past it, the oldest
// finished process is forgotten; a live one is never dropped to make room.
const maxProcesses = 32

// outputBuffer is a tail-keeping byte buffer. A process that prints megabytes
// keeps its most recent maxProcessOutputBytes, which is where a failure is, and
// counts what it dropped so a reader knows the window is not the whole story.
type outputBuffer struct {
	mu      sync.Mutex
	buf     []byte
	total   int64
	dropped int64
}

func (b *outputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += int64(len(p))
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - maxProcessOutputBytes; over > 0 {
		b.buf = b.buf[over:]
		b.dropped += int64(over)
	}
	return len(p), nil
}

// snapshot returns the retained text, how many bytes the process has written in
// total, and how many were dropped off the front.
func (b *outputBuffer) snapshot() (text string, total, dropped int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf), b.total, b.dropped
}

// processStatus is what check_process_id reports.
type processStatus struct {
	ID       string `json:"process_id"`
	Command  string `json:"command"`
	Dir      string `json:"dir"`
	Running  bool   `json:"running"`
	PID      int    `json:"pid,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Killed   bool   `json:"killed,omitempty"`
	Failure  string `json:"failure,omitempty"`
	Started  string `json:"started"`
	Elapsed  string `json:"elapsed"`

	Stdout        string `json:"stdout"`
	Stderr        string `json:"stderr"`
	StdoutBytes   int64  `json:"stdout_bytes"`
	StderrBytes   int64  `json:"stderr_bytes"`
	StdoutDropped int64  `json:"stdout_dropped_bytes,omitempty"`
	StderrDropped int64  `json:"stderr_dropped_bytes,omitempty"`
}

// status builds a snapshot of the process. tailLines > 0 returns only the last
// that many lines of each stream, which is the usual way to poll a build.
func (p *managedProcess) status(tailLines int, agent *sudoAgent) processStatus {
	p.mu.Lock()
	st := processStatus{
		ID:       p.id,
		Command:  p.command,
		Dir:      p.dir,
		Running:  !p.exited,
		TimedOut: p.timedOut,
		Killed:   p.killed,
		Failure:  p.failure,
		Started:  p.started.Format(time.RFC3339),
	}
	if p.exited {
		code := p.exitCode
		st.ExitCode = &code
		st.Elapsed = p.ended.Sub(p.started).Round(time.Millisecond).String()
	} else {
		st.Elapsed = time.Since(p.started).Round(time.Millisecond).String()
	}
	p.mu.Unlock()

	if st.Running && p.cmd.Process != nil {
		st.PID = p.cmd.Process.Pid
	}
	out, outTotal, outDropped := p.stdout.snapshot()
	errText, errTotal, errDropped := p.stderr.snapshot()
	st.Stdout = agent.redact(tail(out, tailLines))
	st.Stderr = agent.redact(tail(errText, tailLines))
	st.StdoutBytes, st.StdoutDropped = outTotal, outDropped
	st.StderrBytes, st.StderrDropped = errTotal, errDropped
	return st
}

// tail keeps the last n lines of s. n <= 0 keeps everything.
func tail(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n") + "\n"
}

// summarize renders a status as the text block of a tool result.
func (st processStatus) summarize() string {
	var b strings.Builder
	fmt.Fprintf(&b, "process %s: %s\n", st.ID, st.Command)
	switch {
	case st.Running:
		fmt.Fprintf(&b, "running for %s", st.Elapsed)
		if st.PID != 0 {
			fmt.Fprintf(&b, " (pid %d)", st.PID)
		}
		b.WriteString("\n")
	case st.TimedOut:
		fmt.Fprintf(&b, "timed out after %s\n", st.Elapsed)
	case st.Killed:
		fmt.Fprintf(&b, "stopped after %s\n", st.Elapsed)
	case st.Failure != "":
		fmt.Fprintf(&b, "could not run: %s\n", st.Failure)
	default:
		fmt.Fprintf(&b, "exited with code %d after %s\n", *st.ExitCode, st.Elapsed)
	}
	writeStream(&b, "stdout", st.Stdout, st.StdoutDropped)
	writeStream(&b, "stderr", st.Stderr, st.StderrDropped)
	if st.Stdout == "" && st.Stderr == "" {
		b.WriteString("\n(no output yet)\n")
	}
	return b.String()
}

func writeStream(b *strings.Builder, name, text string, dropped int64) {
	if text == "" {
		return
	}
	fmt.Fprintf(b, "\n--- %s ---\n", name)
	if dropped > 0 {
		fmt.Fprintf(b, "[... %d earlier bytes dropped ...]\n", dropped)
	}
	b.WriteString(text)
	if !strings.HasSuffix(text, "\n") {
		b.WriteString("\n")
	}
}

// stop ends a running process. On Unix the whole process group is signalled, so
// a shell's children go with it rather than being orphaned.
func (p *managedProcess) stop() error {
	p.mu.Lock()
	if p.exited {
		p.mu.Unlock()
		return nil
	}
	p.killed = true
	p.mu.Unlock()
	if p.cmd.Process == nil {
		return nil
	}
	return killProcessTree(p.cmd)
}

// processRegistry holds the processes started in this session. It lives on the
// server across configuration reloads: a rebuild of the tool set must not lose
// track of something that is still running.
type processRegistry struct {
	mu   sync.Mutex
	next int
	all  map[string]*managedProcess
}

func newProcessRegistry() *processRegistry {
	return &processRegistry{all: map[string]*managedProcess{}}
}

// start launches a command line through the platform shell and returns as soon
// as it is running. The sudo agent is honoured exactly as it is for
// run_command: the shim goes on PATH and the password is written to stdin.
func (r *processRegistry) start(line, dir string, env map[string]string, timeout time.Duration, agent *sudoAgent) (*managedProcess, error) {
	if timeout <= 0 {
		timeout = defaultProcessTimeout
	}
	name, args := shellArgv(line)
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if merged := processEnv(env, line, agent); len(merged) > 0 {
		cmd.Env = os.Environ()
		for k, v := range merged {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	if agent.wants(line) {
		cmd.Stdin = strings.NewReader(agent.password + "\n")
	}
	setProcessGroup(cmd)

	p := &managedProcess{
		command: line,
		dir:     dir,
		started: time.Now(),
		timeout: timeout,
		cmd:     cmd,
		stdout:  &outputBuffer{},
		stderr:  &outputBuffer{},
		done:    make(chan struct{}),
	}
	cmd.Stdout = p.stdout
	cmd.Stderr = p.stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("could not run %s: %w", name, err)
	}

	r.mu.Lock()
	r.next++
	p.id = fmt.Sprintf("proc-%d", r.next)
	r.all[p.id] = p
	r.evictLocked()
	r.mu.Unlock()

	go p.wait()
	return p, nil
}

// wait reaps the process and enforces its timeout. Both live in one goroutine
// so the exit fields are written once, by whichever happens first.
func (p *managedProcess) wait() {
	waited := make(chan error, 1)
	go func() { waited <- p.cmd.Wait() }()

	timer := time.NewTimer(p.timeout)
	defer timer.Stop()

	var err error
	select {
	case err = <-waited:
	case <-timer.C:
		p.mu.Lock()
		p.timedOut = true
		p.mu.Unlock()
		if p.cmd.Process != nil {
			_ = killProcessTree(p.cmd)
		}
		err = <-waited
	}

	p.mu.Lock()
	p.exited = true
	p.ended = time.Now()
	switch {
	case p.timedOut || p.killed:
		p.exitCode = -1
	case err == nil:
		p.exitCode = 0
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			p.exitCode = exitErr.ExitCode()
		} else {
			p.exitCode = -1
			p.failure = err.Error()
		}
	}
	p.mu.Unlock()
	close(p.done)
}

// get looks a process up by id.
func (r *processRegistry) get(id string) (*managedProcess, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.all[strings.TrimSpace(id)]
	return p, ok
}

// list returns every tracked process, oldest first.
func (r *processRegistry) list() []*managedProcess {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*managedProcess, 0, len(r.all))
	for _, p := range r.all {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].started.Before(out[j].started) })
	return out
}

// evictLocked forgets finished processes, oldest first, once there are more
// than maxProcesses. A running one is never dropped: its output is the only
// record of what it is doing.
func (r *processRegistry) evictLocked() {
	for len(r.all) > maxProcesses {
		var oldest *managedProcess
		for _, p := range r.all {
			p.mu.Lock()
			finished := p.exited
			p.mu.Unlock()
			if !finished {
				continue
			}
			if oldest == nil || p.started.Before(oldest.started) {
				oldest = p
			}
		}
		if oldest == nil {
			return
		}
		delete(r.all, oldest.id)
	}
}

// stopAll ends every running process, which is what shutdown does rather than
// leaving orphans behind.
func (r *processRegistry) stopAll() {
	for _, p := range r.list() {
		_ = p.stop()
	}
}

// shellArgv is how a command line is handed to the platform shell. It is shared
// with runShellSudo so a process and a command interpret a line identically.
func shellArgv(line string) (string, []string) {
	if runtime.GOOS == "windows" {
		if shell := os.Getenv("COMSPEC"); shell != "" {
			return shell, []string{"/c", line}
		}
		return "cmd.exe", []string{"/c", line}
	}
	return "/bin/sh", []string{"-c", line}
}

// processEnv merges the caller's environment with the sudo shim's, keeping the
// shim ahead on PATH so sudo is answered at all.
func processEnv(env map[string]string, line string, agent *sudoAgent) map[string]string {
	if !agent.wants(line) {
		return env
	}
	merged := map[string]string{}
	for k, v := range agent.env() {
		merged[k] = v
	}
	for k, v := range env {
		if k == "PATH" {
			merged[k] = agent.shimDir + string(os.PathListSeparator) + v
			continue
		}
		merged[k] = v
	}
	return merged
}
