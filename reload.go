package main

import (
	"encoding/json"
	"log"
	"sync"
)

// configReloader re-reads config.json before each request cycle, so a change to
// the file takes effect on the next call rather than at the next restart.
//
// The rule that makes this safe to do on every request is that a failure never
// costs anything: an unreadable, half-written or invalid config leaves the
// server running on the values it already had, and the error is logged once
// rather than on every request until the file is fixed.
type configReloader struct {
	path      string
	explicit  bool
	workspace string
	logger    *log.Logger
	// overlay re-applies whatever the command line said, so a flag keeps
	// winning over the file however many times the file is re-read.
	overlay func(*Config)

	mu sync.Mutex
	// applied is the JSON form of the configuration currently in effect, used
	// to tell an edited file from an untouched one without diffing structs.
	applied []byte
	// lastError is the most recent failure text, kept so the same broken file
	// is not reported on every single request.
	lastError string
	// apply installs a new configuration. It is a field so the server can
	// supply the wiring without this type knowing about tools.
	apply func(Config) error
}

// newConfigReloader records the configuration currently in effect, so the first
// reload only does work if the file has actually changed since startup.
func newConfigReloader(path string, explicit bool, workspace string, cfg Config, logger *log.Logger, overlay func(*Config), apply func(Config) error) *configReloader {
	r := &configReloader{path: path, explicit: explicit, workspace: workspace, logger: logger, overlay: overlay, apply: apply}
	r.applied = fingerprint(cfg)
	return r
}

// Refresh re-reads the file and applies it when it differs from what is in
// effect. It never returns an error: the previous values are always a valid
// fallback, and a request should not fail because a config file is being
// edited at that moment.
func (r *configReloader) Refresh() {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	cfg, err := LoadConfig(r.path, r.explicit)
	if err == nil {
		if r.overlay != nil {
			r.overlay(&cfg)
		}
		err = cfg.Normalize(r.workspace)
	}
	if err != nil {
		r.report(err.Error())
		return
	}

	next := fingerprint(cfg)
	if next == nil {
		// The configuration could not be re-encoded, which says nothing about
		// the file; keeping what is in effect is the safe reading.
		r.report("could not compare the reloaded configuration with the current one")
		return
	}
	if string(next) == string(r.applied) {
		r.clearError()
		return
	}
	if err := r.apply(cfg); err != nil {
		r.report("keeping the previous configuration: " + err.Error())
		return
	}
	r.applied = next
	r.clearError()
	r.logf("reloaded %s", r.path)
}

// report logs a failure the first time it is seen. A config file left broken
// would otherwise produce one line per request.
func (r *configReloader) report(text string) {
	if r.lastError == text {
		return
	}
	r.lastError = text
	r.logf("could not reload %s (using the previous values): %s", r.path, text)
}

// clearError makes the next failure loggable again, and says so when the file
// has recovered.
func (r *configReloader) clearError() {
	if r.lastError == "" {
		return
	}
	r.lastError = ""
	r.logf("%s is readable again", r.path)
}

func (r *configReloader) logf(format string, args ...any) {
	if r.logger != nil {
		r.logger.Printf(format, args...)
	}
}

// fingerprint is the comparable form of a configuration. The unexported path
// field is not part of it, which is what makes two loads of the same file
// compare equal.
func fingerprint(cfg Config) []byte {
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil
	}
	return data
}
