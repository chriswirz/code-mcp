package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUpdateAssetName(t *testing.T) {
	got := updateAssetName()
	want := fmt.Sprintf("codemcp-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	if got != want {
		t.Errorf("updateAssetName() = %q, want %q", got, want)
	}
}

func TestChecksumFor(t *testing.T) {
	sums := "aaaa  codemcp-linux-amd64\n" +
		"bbbb *codemcp-windows-amd64.exe\n" +
		"\n" +
		"CCCC  codemcp_0.1.0007_amd64.deb\n"
	cases := map[string]string{
		"codemcp-linux-amd64":        "aaaa",
		"codemcp-windows-amd64.exe":  "bbbb",
		"codemcp_0.1.0007_amd64.deb": "cccc",
	}
	for asset, want := range cases {
		got, err := checksumFor(sums, asset)
		if err != nil {
			t.Fatalf("checksumFor(%q): %v", asset, err)
		}
		if got != want {
			t.Errorf("checksumFor(%q) = %q, want %q", asset, got, want)
		}
	}
	if _, err := checksumFor(sums, "codemcp-plan9-mips"); err == nil {
		t.Error("expected an error for an asset the release does not carry")
	}
}

func TestReplaceExecutable(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "codemcp")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceExecutable(exe, []byte("new")); err != nil {
		t.Fatalf("replaceExecutable: %v", err)
	}
	content, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "new" {
		t.Errorf("binary = %q, want %q", content, "new")
	}
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		t.Errorf("mode = %v, want the executable bits set", info.Mode().Perm())
	}
	// Nothing but the binary itself is left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "codemcp" {
			t.Errorf("leftover file %s", e.Name())
		}
	}
}

func TestSelfUpdateVerifiesChecksum(t *testing.T) {
	asset := updateAssetName()
	payload := []byte("the new binary")
	sum := sha256.Sum256(payload)

	var sums string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			fmt.Fprint(w, sums)
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	exe := filepath.Join(dir, "codemcp")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("mismatch leaves the old binary in place", func(t *testing.T) {
		sums = "0000  " + asset + "\n"
		err := downloadAndReplace(srv.Client(), srv.URL, exe, "0.1.0009", os.Stderr)
		if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
			t.Fatalf("err = %v, want a checksum mismatch", err)
		}
		if content, _ := os.ReadFile(exe); string(content) != "old" {
			t.Errorf("binary = %q, want the original left untouched", content)
		}
	})

	t.Run("match installs the download", func(t *testing.T) {
		sums = hex.EncodeToString(sum[:]) + "  " + asset + "\n"
		if err := downloadAndReplace(srv.Client(), srv.URL, exe, "0.1.0009", os.Stderr); err != nil {
			t.Fatalf("downloadAndReplace: %v", err)
		}
		if content, _ := os.ReadFile(exe); string(content) != string(payload) {
			t.Errorf("binary = %q, want %q", content, payload)
		}
	})
}
