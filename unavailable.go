package main

import (
	"fmt"
	"sort"
	"strings"
)

// A tool that the configuration switches off is simply not registered, which
// is right for tools/list but wrong for the model that calls it anyway - from
// a remembered name, from a system prompt, or because another server has one.
// "unknown tool: git_push" arriving as a JSON-RPC error reads to a model like
// the connection broke, and the usual reaction is to try the same call again.
//
// So every tool this build can serve but this configuration does not is
// recorded here with the reason, and calling one answers with an ordinary tool
// result that says what is off, what would turn it on, and what to do instead.

// unavailableTool is one switched-off tool and the guidance for it.
type unavailableTool struct {
	// Reason names the setting, in the words config.json uses.
	Reason string
	// Instead is the next thing to try: another tool, or another route to the
	// same end. Empty when there genuinely is one.
	Instead string
}

// markUnavailable records a tool as switched off. It is called while the tool
// set is being built, alongside the decision not to register it.
func (s *Server) markUnavailable(name, reason, instead string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable == nil {
		s.unavailable = map[string]unavailableTool{}
	}
	s.unavailable[name] = unavailableTool{Reason: reason, Instead: instead}
}

// unavailableFor returns the guidance for a switched-off tool.
func (s *Server) unavailableFor(name string) (unavailableTool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info, ok := s.unavailable[name]
	return info, ok
}

// message is what the caller is told: the state of things, the setting that
// governs it, and where to go next. It is deliberately a statement about the
// server's configuration rather than about the call, so a model does not read
// it as something to retry.
func (u unavailableTool) message(name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The %s tool is switched off on this server: %s. ", name, u.Reason)
	b.WriteString("Retrying will not help, and nothing was changed. ")
	if u.Instead != "" {
		b.WriteString(u.Instead + " ")
	}
	b.WriteString("Ask the operator to change config.json if this is meant to be available.")
	return b.String()
}

// suggestTools names the registered tools closest to what was asked for, so a
// call to a name that never existed still points somewhere. A model that meant
// git_log and typed gitlog gets the answer rather than a dead end.
func (s *Server) suggestTools(name string) []string {
	s.mu.RLock()
	names := append([]string(nil), s.order...)
	s.mu.RUnlock()

	wanted := normalizeToolName(name)
	type scored struct {
		name  string
		score int
	}
	var near []scored
	for _, candidate := range names {
		if score := toolNameScore(wanted, normalizeToolName(candidate)); score > 0 {
			near = append(near, scored{candidate, score})
		}
	}
	sort.SliceStable(near, func(i, j int) bool { return near[i].score > near[j].score })
	out := make([]string, 0, 3)
	for _, n := range near {
		if len(out) == 3 {
			break
		}
		out = append(out, n.name)
	}
	return out
}

// normalizeToolName folds the spellings that differ only in punctuation or
// case, which is most of what a guessed name gets wrong.
func normalizeToolName(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r == '.' || r == ' ' {
			return -1
		}
		return r
	}, strings.ToLower(name))
}

// toolNameScore rates how alike two normalised names are: an exact match, one
// containing the other, or a shared prefix long enough to be meant.
func toolNameScore(wanted, candidate string) int {
	switch {
	case wanted == candidate:
		return 100
	case strings.Contains(candidate, wanted), strings.Contains(wanted, candidate):
		return 50
	}
	shared := 0
	for shared < len(wanted) && shared < len(candidate) && wanted[shared] == candidate[shared] {
		shared++
	}
	if shared >= 4 {
		return shared
	}
	return 0
}

// unknownToolMessage is the answer for a name this server has never had. It
// still tries to be useful: the nearest names, and where the full list is.
func (s *Server) unknownToolMessage(name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "There is no %s tool on this server, and nothing was changed. ", name)
	if suggestions := s.suggestTools(name); len(suggestions) > 0 {
		fmt.Fprintf(&b, "The closest tools it does have are %s. ", strings.Join(suggestions, ", "))
	}
	b.WriteString("Call tools/list for the full set, or project_commands for this project's own commands, " +
		"and use run_command for anything that has no tool of its own.")
	return b.String()
}
