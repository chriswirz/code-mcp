package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// defaultDownloadMaxBytes caps a download unless the caller raises it, so a
// wrong URL cannot fill the disk.
const defaultDownloadMaxBytes = 100 << 20

// registerFetchTools adds download_file: fetching a URL into the workspace,
// without a shell command that differs on every platform.
func (s *Server) registerFetchTools() {
	ws := s.workspace()

	s.RegisterTool(Tool{
		Name:  "download_file",
		Title: "Download a file from a URL",
		Description: "Download an http or https URL into a workspace file, byte for byte (binaries are safe). " +
			"Refuses to replace an existing file unless overwrite is set; a replaced file can be restored with rollback. " +
			"Set ignore_certificate_errors to accept a self-signed, expired or mismatched certificate - " +
			"only for hosts you trust, since it removes protection against interception.",
		Annotations: &ToolAnnotations{DestructiveHint: true, OpenWorldHint: true},
		InputSchema: schema([]string{"url", "path"}, map[string]any{
			"url":                       prop("string", "The http or https URL to download."),
			"path":                      prop("string", "Destination file, relative to the workspace root. Parent directories are created."),
			"overwrite":                 propDefault("boolean", "Replace the destination if it already exists.", false),
			"ignore_certificate_errors": propDefault("boolean", "Skip TLS certificate verification.", false),
			"headers": map[string]any{
				"type":                 "object",
				"description":          "Extra request headers, e.g. {\"Authorization\": \"Bearer ...\"}.",
				"additionalProperties": map[string]any{"type": "string"},
			},
			"timeout_seconds": propDefault("integer", "Give up after this many seconds.", 300),
			"max_bytes":       propDefault("integer", "Refuse a download larger than this.", defaultDownloadMaxBytes),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			URL                     string            `json:"url"`
			Path                    string            `json:"path"`
			Overwrite               bool              `json:"overwrite"`
			IgnoreCertificateErrors bool              `json:"ignore_certificate_errors"`
			Headers                 map[string]string `json:"headers"`
			TimeoutSeconds          int               `json:"timeout_seconds"`
			MaxBytes                int64             `json:"max_bytes"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.URL == "" || args.Path == "" {
			return toolError("url and path are required"), nil
		}
		parsed, err := url.Parse(args.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return toolError("url must be an absolute http or https URL, not %q", args.URL), nil
		}
		if !ws.AllowWrite {
			return toolError("writes are disabled (workspace.allow_write is false)"), nil
		}
		abs, err := ws.resolveNew(args.Path)
		if err != nil {
			return pathToolError(ws, args.Path, err), nil
		}
		name := ws.Canonical(abs)
		if info, statErr := os.Stat(abs); statErr == nil {
			if info.IsDir() {
				return toolError("%s is a directory", name), nil
			}
			if !args.Overwrite {
				return toolError("%s already exists; set overwrite to replace it (nothing was downloaded)", name), nil
			}
		}
		if args.TimeoutSeconds <= 0 {
			args.TimeoutSeconds = 300
		}
		if args.MaxBytes <= 0 {
			args.MaxBytes = defaultDownloadMaxBytes
		}

		transport := http.DefaultTransport.(*http.Transport).Clone()
		if args.IgnoreCertificateErrors {
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicitly requested by the caller
		}
		client := &http.Client{Transport: transport, Timeout: time.Duration(args.TimeoutSeconds) * time.Second}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, args.URL, nil)
		if err != nil {
			return toolError("%v", err), nil
		}
		for k, v := range args.Headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			var certErr *tls.CertificateVerificationError
			if errors.As(err, &certErr) && !args.IgnoreCertificateErrors {
				return toolError("%v\nThe server's certificate could not be verified. If you trust this host, "+
					"retry with ignore_certificate_errors set (nothing was downloaded)", err), nil
			}
			return toolError("%v (nothing was downloaded)", err), nil
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return toolError("%s answered %s (nothing was downloaded)", args.URL, resp.Status), nil
		}
		if resp.ContentLength > args.MaxBytes {
			return toolError("%s is %d bytes, over max_bytes %d (nothing was downloaded)", args.URL, resp.ContentLength, args.MaxBytes), nil
		}

		// Download beside the destination and rename into place, so a failure
		// part way leaves no truncated file and the existing one untouched.
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return toolError("%v", err), nil
		}
		tmp, err := os.CreateTemp(filepath.Dir(abs), ".download-*")
		if err != nil {
			return toolError("%v", err), nil
		}
		defer os.Remove(tmp.Name())
		written, copyErr := io.Copy(tmp, io.LimitReader(resp.Body, args.MaxBytes+1))
		closeErr := tmp.Close()
		switch {
		case copyErr != nil:
			return toolError("download failed after %d bytes: %v (nothing was written)", written, copyErr), nil
		case closeErr != nil:
			return toolError("%v (nothing was written)", closeErr), nil
		case written > args.MaxBytes:
			return toolError("%s is over max_bytes %d (nothing was written)", args.URL, args.MaxBytes), nil
		}
		ws.History.Record(abs)
		if err := os.Rename(tmp.Name(), abs); err != nil {
			return toolError("%v", err), nil
		}

		msg := fmt.Sprintf("Downloaded %d bytes from %s to %s", written, resp.Request.URL, name)
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			msg += fmt.Sprintf(" (%s)", ct)
		}
		if args.IgnoreCertificateErrors && parsed.Scheme == "https" {
			msg += "\nWarning: TLS certificate verification was skipped for this download."
		}
		return toolResult(msg), nil
	})
}
