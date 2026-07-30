package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadFile(t *testing.T) {
	payload := "binary\r\n\x00data"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()
	s, root := codeServer(t)

	text := toolText(t, s, "download_file", map[string]any{"url": srv.URL + "/f", "path": "dl/f.bin"})
	if !strings.HasPrefix(text, "ERROR:") || !strings.Contains(text, "ignore_certificate_errors") {
		t.Fatalf("a self-signed certificate should be refused with a hint: %s", text)
	}

	text = toolText(t, s, "download_file", map[string]any{
		"url": srv.URL + "/f", "path": "dl/f.bin", "ignore_certificate_errors": true,
	})
	mustSucceed(t, text)
	if !strings.Contains(text, "verification was skipped") {
		t.Errorf("missing insecure warning: %s", text)
	}
	if got := mustReadRaw(t, root, "dl/f.bin"); got != payload {
		t.Fatalf("content = %q", got)
	}

	text = toolText(t, s, "download_file", map[string]any{
		"url": srv.URL + "/f", "path": "dl/f.bin", "ignore_certificate_errors": true,
	})
	if !strings.Contains(text, "overwrite") {
		t.Errorf("existing file should need overwrite: %s", text)
	}

	text = toolText(t, s, "download_file", map[string]any{
		"url": srv.URL + "/missing", "path": "dl/m.bin", "ignore_certificate_errors": true,
	})
	if !strings.Contains(text, "404") {
		t.Errorf("expected a 404 error: %s", text)
	}

	text = toolText(t, s, "download_file", map[string]any{
		"url": srv.URL + "/f", "path": "dl/big.bin", "ignore_certificate_errors": true, "max_bytes": 3,
	})
	if !strings.Contains(text, "max_bytes") {
		t.Errorf("expected a size refusal: %s", text)
	}

	if text := toolText(t, s, "download_file", map[string]any{"url": "file:///etc/passwd", "path": "x"}); !strings.HasPrefix(text, "ERROR:") {
		t.Errorf("non-http schemes should be refused: %s", text)
	}
}

func mustReadRaw(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
