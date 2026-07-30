package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/chriswirz/https-tunnel/tunnelclient"
)

// Tunnel exposes the MCP handler on a public HTTPS URL through an https-tunnel
// server. The handler is served in process by the tunnel client, so no local
// port is involved: a client that cannot reach this machine's network still
// reaches the workspace.
type Tunnel struct {
	client *tunnelclient.Client
	cfg    TunnelConfig

	mu  sync.Mutex
	url string
}

// NewTunnel prepares the client. It opens no connection; call Run for that.
//
// It takes the whole configuration rather than the tunnel section alone
// because the session id the server issues is written back into the file the
// configuration came from, which is what makes a restart reclaim the same
// public URL instead of being handed a new one.
func NewTunnel(cfg *Config, handler http.Handler, logger *log.Logger) (*Tunnel, error) {
	t := &Tunnel{cfg: cfg.Tunnel}

	// The tunnel client logs through slog; route it to the same stream, with
	// the same prefix, as everything else this server prints.
	slogger := slog.New(slog.NewTextHandler(logWriter{logger}, &slog.HandlerOptions{Level: slog.LevelInfo}))

	client, err := tunnelclient.New(tunnelclient.Options{
		APIKey:           cfg.Tunnel.APIKeyValue(),
		ServerURL:        cfg.Tunnel.ServerURL,
		SessionID:        tunnelSession(cfg.Tunnel),
		SubdomainRequest: cfg.Tunnel.Subdomain,
		Handler:          handler,
		Logger:           slogger,
		ClientInfo:       tunnelClientInfo(cfg.Tunnel),
		OnSession:        sessionSaver(cfg, logger),
		OnConnect: func(tun tunnelclient.Tunnel) {
			t.mu.Lock()
			t.url = tun.URL
			t.mu.Unlock()
			logger.Printf("tunnel up: %s", tun.URL)
		},
	})
	if err != nil {
		return nil, err
	}
	t.client = client
	return t, nil
}

// Run keeps the tunnel up until ctx is cancelled, reconnecting on its own.
func (t *Tunnel) Run(ctx context.Context) error {
	if err := t.client.Run(ctx); err != nil {
		return fmt.Errorf("tunnel: %w", err)
	}
	return nil
}

// URL is the public address, or "" before the first connection succeeds.
func (t *Tunnel) URL() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.url
}

func tunnelClientInfo(cfg TunnelConfig) string {
	if cfg.ClientInfo != "" {
		return cfg.ClientInfo
	}
	return appName + " " + version
}

// sessionSaver builds the callback the tunnel client invokes with each session
// id it is issued. That id is what reclaims the same public URL after a
// restart, so it has to be written down somewhere.
//
// There is exactly one store. Naming a session file says where the id goes,
// and the configuration is then left alone: two copies that can disagree about
// which id is current is worse than either one on its own. With no file named,
// the id belongs in config.json alongside the rest of the tunnel settings.
//
// Nothing here fails the tunnel. A tunnel that works but will come back on a
// different URL is worth a warning; it is not worth refusing to serve, which
// is what returning an error from this callback would do.
func sessionSaver(cfg *Config, logger *log.Logger) func(string) error {
	return func(id string) error {
		if file := cfg.Tunnel.SessionFile; file != "" {
			if err := saveTunnelSession(file, id); err != nil {
				logger.Printf("tunnel: could not write the session id to %s: %v", file, err)
			} else {
				logger.Printf("tunnel: session id saved to %s; a restart will reclaim this URL", file)
			}
			return nil
		}
		switch err := cfg.SaveSessionID(id); {
		case err == nil:
			logger.Printf("tunnel: session id saved to %s; a restart will reclaim this URL", cfg.Path())
		case errors.Is(err, ErrNoConfigFile):
			logger.Printf("tunnel: this session cannot be remembered - there is no config file to write to, " +
				"so a restart will be given a new URL. Write one with --example-config, or set --tunnel-session-file")
		default:
			logger.Printf("tunnel: could not write the session id to %s: %v", cfg.Path(), err)
		}
		return nil
	}
}

// tunnelSession is the session id to resume: the configured one, or whatever
// the last run persisted.
//
// An explicitly configured id wins over the file, which is the documented
// override: someone who writes session_id into the config is naming the
// session they want resumed, and an automatic store must not silently
// override that.
func tunnelSession(cfg TunnelConfig) string {
	if cfg.SessionID != "" {
		return cfg.SessionID
	}
	if cfg.SessionFile == "" {
		return ""
	}
	data, err := os.ReadFile(cfg.SessionFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// saveTunnelSession persists a newly issued session id so a restart reclaims
// the same public URL. It is written 0600: the id is a credential for that URL.
func saveTunnelSession(path, id string) error {
	if path == "" {
		return nil
	}
	return os.WriteFile(path, []byte(id+"\n"), 0o600)
}

// logWriter adapts a *log.Logger to io.Writer for slog, dropping the trailing
// newline slog writes so lines are not doubled.
type logWriter struct{ logger *log.Logger }

func (w logWriter) Write(p []byte) (int, error) {
	w.logger.Print(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

var _ io.Writer = logWriter{}
