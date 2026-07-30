package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// defaultRollbackDepth is how many earlier states of each file rollback keeps.
const defaultRollbackDepth = 5

// errNoEarlierState is what rollback reports once a file's history is spent.
var errNoEarlierState = errors.New("the file has already been rolled back to the earliest tracked state")

// fileState is one earlier version of a file: its bytes exactly as they were on
// disk, or the fact that it did not exist yet, and when it was replaced.
type fileState struct {
	data    []byte
	existed bool
	at      time.Time
}

// FileHistory keeps, in memory, the last few states of every file the server
// wrote, so a bad edit can be undone. It survives a configuration reload but
// not a restart.
type FileHistory struct {
	mu     sync.Mutex
	depth  int
	states map[string][]fileState
}

// NewFileHistory builds a history that keeps depth states per file. Zero
// switches tracking off.
func NewFileHistory(depth int) *FileHistory {
	return &FileHistory{depth: depth, states: map[string][]fileState{}}
}

// SetDepth changes how many states are kept, trimming any file over the limit.
func (h *FileHistory) SetDepth(depth int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.depth = depth
	for key, stack := range h.states {
		h.states[key] = trimStates(stack, depth)
		if len(h.states[key]) == 0 {
			delete(h.states, key)
		}
	}
}

// Record captures the file at abs as it is now, before a write replaces it.
func (h *FileHistory) Record(abs string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.depth <= 0 {
		return
	}
	state := fileState{at: time.Now()}
	if data, err := os.ReadFile(abs); err == nil {
		state.data, state.existed = data, true
	} else if !errors.Is(err, os.ErrNotExist) {
		// Unreadable is not the same as absent; restoring "absent" would
		// delete the file, so this write simply goes untracked.
		return
	}
	key := historyKey(abs)
	h.states[key] = trimStates(append(h.states[key], state), h.depth)
}

// Pop removes and returns the most recent state of the file at abs, and how
// many earlier states remain after it.
func (h *FileHistory) Pop(abs string) (fileState, int, error) {
	if h == nil {
		return fileState{}, 0, errNoEarlierState
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	key := historyKey(abs)
	stack := h.states[key]
	if len(stack) == 0 {
		return fileState{}, 0, errNoEarlierState
	}
	state := stack[len(stack)-1]
	h.states[key] = stack[:len(stack)-1]
	return state, len(stack) - 1, nil
}

// Peek returns what Pop would, without removing it.
func (h *FileHistory) Peek(abs string) (fileState, int, error) {
	if h == nil {
		return fileState{}, 0, errNoEarlierState
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	stack := h.states[historyKey(abs)]
	if len(stack) == 0 {
		return fileState{}, 0, errNoEarlierState
	}
	return stack[len(stack)-1], len(stack) - 1, nil
}

// States returns a copy of the tracked states of the file at abs, oldest first.
func (h *FileHistory) States(abs string) []fileState {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]fileState(nil), h.states[historyKey(abs)]...)
}

func trimStates(stack []fileState, depth int) []fileState {
	if depth <= 0 {
		return nil
	}
	if len(stack) > depth {
		stack = append([]fileState(nil), stack[len(stack)-depth:]...)
	}
	return stack
}

// historyKey names a file the same way however its path was spelled.
func historyKey(abs string) string {
	key := filepath.Clean(abs)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return key
}
