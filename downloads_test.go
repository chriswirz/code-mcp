package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testDownloadServer(t *testing.T) (*downloadServer, string) {
	t.Helper()
	dl := newDownloadServer(DownloadsConfig{
		Enabled:           true,
		DefaultTTLMinutes: 5,
		MaxTTLMinutes:     60,
		MaxLinks:          4,
		Addr:              "127.0.0.1:0",
	}, nil)
	dl.Attach("http://127.0.0.1:8765/mcp")
	t.Cleanup(dl.Close)

	dir := t.TempDir()
	file := filepath.Join(dir, "report.tar.gz")
	if err := os.WriteFile(file, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dl, file
}

func fetch(t *testing.T, dl *downloadServer, target string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	dl.Handle(rec, req)
	return rec.Result()
}

func TestDownloadLinkServesFile(t *testing.T) {
	dl, file := testDownloadServer(t)
	link, url, err := dl.Add(file, "report.tar.gz", 7, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	want := "http://127.0.0.1:8765/mcp/files/report.tar.gz?token=" + link.token
	if url != want {
		t.Fatalf("url = %q, want %q", url, want)
	}

	res := fetch(t, dl, url)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != "payload" {
		t.Fatalf("body = %q", body)
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, "report.tar.gz") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
}

func TestDownloadLinkRejectsBadTokenAndName(t *testing.T) {
	dl, file := testDownloadServer(t)
	link, _, err := dl.Add(file, "report.tar.gz", 7, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"no token":    "http://127.0.0.1:8765/mcp/files/report.tar.gz",
		"wrong token": "http://127.0.0.1:8765/mcp/files/report.tar.gz?token=nope",
		"wrong name":  "http://127.0.0.1:8765/mcp/files/secrets.txt?token=" + link.token,
	} {
		if res := fetch(t, dl, target); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", name, res.StatusCode)
		}
	}
}

func TestDownloadLinkExpires(t *testing.T) {
	dl, file := testDownloadServer(t)
	_, url, err := dl.Add(file, "report.tar.gz", 7, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if res := fetch(t, dl, url); res.StatusCode != http.StatusOK {
		t.Fatalf("before expiry: status = %d, want 200", res.StatusCode)
	}
	time.Sleep(40 * time.Millisecond)
	if res := fetch(t, dl, url); res.StatusCode != http.StatusNotFound {
		t.Fatalf("after expiry: status = %d, want 404", res.StatusCode)
	}
	if links := dl.List(); len(links) != 0 {
		t.Fatalf("expired link still listed: %d", len(links))
	}
}

func TestDownloadLinkRevoke(t *testing.T) {
	dl, file := testDownloadServer(t)
	link, url, err := dl.Add(file, "report.tar.gz", 7, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !dl.Revoke(tokenFromArg(url)) {
		t.Fatal("Revoke by URL did not find the link")
	}
	if dl.Revoke(link.token) {
		t.Fatal("Revoke succeeded twice")
	}
	if res := fetch(t, dl, url); res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
}

func TestDownloadMaxLinksEvictsSoonest(t *testing.T) {
	dl, file := testDownloadServer(t)
	var first string
	for i := range 5 {
		ttl := time.Duration(10+i) * time.Minute
		_, url, err := dl.Add(file, "report.tar.gz", 7, ttl)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = url
		}
	}
	if got := len(dl.List()); got != 4 {
		t.Fatalf("live links = %d, want 4", got)
	}
	if res := fetch(t, dl, first); res.StatusCode != http.StatusNotFound {
		t.Fatalf("evicted link: status = %d, want 404", res.StatusCode)
	}
}

func TestDownloadRouteFollowsEndpointPath(t *testing.T) {
	for path, want := range map[string]string{
		"/mcp":  "/mcp/files/",
		"/mcp/": "/mcp/files/",
		"/":     "/files/",
	} {
		if got := downloadRoute(path); got != want {
			t.Errorf("downloadRoute(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestDownloadTokensAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		token, err := newDownloadToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[token] {
			t.Fatalf("duplicate token %q", token)
		}
		seen[token] = true
	}
}
