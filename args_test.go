package main

import (
	"encoding/json"
	"testing"
)

func TestCamelToSnake(t *testing.T) {
	cases := map[string]string{
		"replaceAll":           "replace_all",
		"startLine":            "start_line",
		"normalizeLineEndings": "normalize_line_endings",
		"oldText":              "old_text",
		"path":                 "path",
		"already_snake":        "already_snake",
		"maxHTTPRetries":       "max_http_retries",
		"HTTPPath":             "http_path",
		"ID":                   "id",
		"dryRun":               "dry_run",
		"":                     "",
	}
	for in, want := range cases {
		if got := camelToSnake(in); got != want {
			t.Errorf("camelToSnake(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeArgsAcceptsCamelCase(t *testing.T) {
	t.Run("top level keys", func(t *testing.T) {
		var args struct {
			Path       string `json:"path"`
			ReplaceAll bool   `json:"replace_all"`
			StartLine  int    `json:"start_line"`
		}
		raw := json.RawMessage(`{"path":"f.go","replaceAll":true,"startLine":12}`)
		if bad := decodeArgs(raw, &args); bad != nil {
			t.Fatalf("unexpected error: %v", bad.Content)
		}
		if args.Path != "f.go" || !args.ReplaceAll || args.StartLine != 12 {
			t.Fatalf("got %+v", args)
		}
	})

	t.Run("explicit snake_case wins", func(t *testing.T) {
		var args struct {
			ReplaceAll bool `json:"replace_all"`
		}
		raw := json.RawMessage(`{"replace_all":false,"replaceAll":true}`)
		if bad := decodeArgs(raw, &args); bad != nil {
			t.Fatalf("unexpected error: %v", bad.Content)
		}
		if args.ReplaceAll {
			t.Fatal("the explicitly supplied snake_case key should take precedence")
		}
	})

	t.Run("nested inside an array of objects", func(t *testing.T) {
		var args struct {
			Edits  []editOp `json:"edits"`
			DryRun bool     `json:"dry_run"`
		}
		raw := json.RawMessage(`{"dryRun":true,"edits":[{"path":"f.go","oldText":"a","newText":"b","replaceAll":true}]}`)
		if bad := decodeArgs(raw, &args); bad != nil {
			t.Fatalf("unexpected error: %v", bad.Content)
		}
		if !args.DryRun {
			t.Error("dryRun did not reach dry_run")
		}
		if len(args.Edits) != 1 {
			t.Fatalf("got %d edits", len(args.Edits))
		}
		if !args.Edits[0].ReplaceAll {
			t.Error("replaceAll did not reach replace_all inside the array")
		}
		// oldText/newText are their own aliases, resolved by anchor().
		gotOld, gotNew := args.Edits[0].anchor()
		if gotOld != "a" || gotNew != "b" {
			t.Errorf("anchor = (%q, %q), want (a, b)", gotOld, gotNew)
		}
	})

	t.Run("snake_case payloads are untouched", func(t *testing.T) {
		var args struct {
			Path      string `json:"path"`
			StartLine int    `json:"start_line"`
		}
		raw := json.RawMessage(`{"path":"f.go","start_line":3}`)
		if bad := decodeArgs(raw, &args); bad != nil {
			t.Fatalf("unexpected error: %v", bad.Content)
		}
		if args.Path != "f.go" || args.StartLine != 3 {
			t.Fatalf("got %+v", args)
		}
	})

	t.Run("string values keep their capitals", func(t *testing.T) {
		// The rewrite must apply to keys only: content is data, not identifiers.
		var args struct {
			OldString string `json:"old_string"`
		}
		raw := json.RawMessage(`{"oldString":"func NewServer(Name string) {"}`)
		if bad := decodeArgs(raw, &args); bad != nil {
			t.Fatalf("unexpected error: %v", bad.Content)
		}
		if args.OldString != "func NewServer(Name string) {" {
			t.Fatalf("value was altered: %q", args.OldString)
		}
	})

	t.Run("large integers survive the round trip", func(t *testing.T) {
		var args struct {
			MaxBytes int64 `json:"max_bytes"`
		}
		const big = int64(9007199254740993) // 2^53 + 1, unrepresentable as float64
		raw := json.RawMessage(`{"maxBytes":9007199254740993}`)
		if bad := decodeArgs(raw, &args); bad != nil {
			t.Fatalf("unexpected error: %v", bad.Content)
		}
		if args.MaxBytes != big {
			t.Fatalf("got %d, want %d", args.MaxBytes, big)
		}
	})

	t.Run("malformed payloads still report a decode error", func(t *testing.T) {
		var args struct {
			Path string `json:"path"`
		}
		if bad := decodeArgs(json.RawMessage(`{"badJSON":`), &args); bad == nil || !bad.IsError {
			t.Fatal("expected a decode error")
		}
	})

	t.Run("empty and null arguments", func(t *testing.T) {
		var args struct {
			Path string `json:"path"`
		}
		if bad := decodeArgs(json.RawMessage(``), &args); bad != nil {
			t.Errorf("empty: %v", bad.Content)
		}
		if bad := decodeArgs(json.RawMessage(`null`), &args); bad != nil {
			t.Errorf("null: %v", bad.Content)
		}
	})
}
