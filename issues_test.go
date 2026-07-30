package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// issueServer stands in for the GitHub API: it records what was posted and
// answers with whatever status the test is about.
type issueServer struct {
	*httptest.Server
	path   string
	auth   string
	body   issueRequest
	status int
	reply  string
}

func newIssueServer(t *testing.T, status int, reply string) *issueServer {
	t.Helper()
	stub := &issueServer{status: status, reply: reply}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.path = r.URL.Path
		stub.auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&stub.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stub.status)
		_, _ = w.Write([]byte(stub.reply))
	}))
	t.Cleanup(stub.Close)
	return stub
}

// serverWithIssues builds a server whose report_issue tool talks to the stub.
func serverWithIssues(t *testing.T, adjust func(*IssuesConfig)) (*Server, *issueServer) {
	t.Helper()
	stub := newIssueServer(t, http.StatusCreated,
		`{"number":42,"html_url":"https://github.com/chriswirz/code-mcp/issues/42"}`)
	s := serverWith(t, func(c *Config) {
		c.Issues = IssuesConfig{
			Enabled: true,
			Repo:    "chriswirz/code-mcp",
			Token:   "test-token",
			APIBase: stub.URL,
		}
		if adjust != nil {
			adjust(&c.Issues)
		}
	})
	return s, stub
}

func TestReportIssuePostsTheReport(t *testing.T) {
	s, stub := serverWithIssues(t, nil)

	text := toolText(t, s, "report_issue", map[string]any{
		"title": "apply_diff rejects a valid patch",
		"body":  "The hunk counts were right and it still failed.",
	})
	if strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("report_issue failed: %s", text)
	}
	if stub.path != "/repos/chriswirz/code-mcp/issues" {
		t.Errorf("posted to %q", stub.path)
	}
	if stub.auth != "Bearer test-token" {
		t.Errorf("Authorization = %q", stub.auth)
	}
	if stub.body.Title != "apply_diff rejects a valid patch" {
		t.Errorf("title = %q", stub.body.Title)
	}
	if !strings.Contains(stub.body.Body, "The hunk counts were right") {
		t.Errorf("body lost the report: %q", stub.body.Body)
	}
	if !strings.Contains(stub.body.Body, "| platform |") {
		t.Errorf("body should carry the diagnostics block: %q", stub.body.Body)
	}
	if !strings.Contains(text, "issues/42") {
		t.Errorf("the answer should carry the issue URL: %s", text)
	}
}

// TestReportIssueDiagnosticsKeepLocalDetailOut: the issue is public, so the
// appended block must not carry the workspace path.
func TestReportIssueDiagnosticsKeepLocalDetailOut(t *testing.T) {
	s, stub := serverWithIssues(t, nil)

	toolText(t, s, "report_issue", map[string]any{"title": "t", "body": "b"})
	root := s.workspace().Root
	if strings.Contains(stub.body.Body, root) {
		t.Errorf("the diagnostics block leaked the workspace path: %q", stub.body.Body)
	}
	if strings.Contains(stub.body.Body, "test-token") {
		t.Errorf("the diagnostics block leaked the token: %q", stub.body.Body)
	}
}

func TestReportIssueDryRunPostsNothing(t *testing.T) {
	s, stub := serverWithIssues(t, nil)

	text := toolText(t, s, "report_issue", map[string]any{
		"title": "a fault", "body": "what happened", "dry_run": true,
	})
	if stub.path != "" {
		t.Errorf("a dry run must not call GitHub, but it posted to %q", stub.path)
	}
	for _, want := range []string{"Dry run", "a fault", "what happened"} {
		if !strings.Contains(text, want) {
			t.Errorf("dry run should show what would be posted (%q): %s", want, text)
		}
	}
}

func TestReportIssueRequiresTitleAndBody(t *testing.T) {
	s, stub := serverWithIssues(t, nil)

	if text := toolText(t, s, "report_issue", map[string]any{"title": "  ", "body": "x"}); !strings.HasPrefix(text, "ERROR:") {
		t.Errorf("an empty title should be refused: %s", text)
	}
	if stub.path != "" {
		t.Errorf("nothing should have been posted")
	}
}

// TestReportIssueErrorsAreActionable: every GitHub refusal has to come back as
// something the caller can do something about, with the token never in it.
func TestReportIssueErrorsAreActionable(t *testing.T) {
	cases := []struct {
		status int
		reply  string
		want   string
	}{
		{http.StatusUnauthorized, `{"message":"Bad credentials"}`, "expired or been revoked"},
		{http.StatusForbidden, `{"message":"Resource not accessible"}`, "Issues: read and write"},
		{http.StatusNotFound, `{"message":"Not Found"}`, "not scoped to it"},
		{http.StatusGone, `{"message":"Issues are disabled"}`, "issues are disabled"},
		{http.StatusUnprocessableEntity, `{"message":"Validation Failed"}`, "without labels"},
		{http.StatusTooManyRequests, `{"message":"rate limited"}`, "rate-limiting"},
		{http.StatusInternalServerError, `{"message":"boom"}`, "was not created"},
	}
	for _, c := range cases {
		stub := newIssueServer(t, c.status, c.reply)
		cfg := IssuesConfig{Enabled: true, Repo: "chriswirz/code-mcp", Token: "test-token", APIBase: stub.URL}
		_, err := postIssue(t.Context(), cfg, issueRequest{Title: "t", Body: "b"})
		if err == nil {
			t.Errorf("status %d should have been an error", c.status)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(c.want)) {
			t.Errorf("status %d says %q, want it to mention %q", c.status, err, c.want)
		}
		if strings.Contains(err.Error(), "test-token") {
			t.Errorf("status %d leaked the token: %v", c.status, err)
		}
	}
}

// TestReportIssueWithoutATokenExplainsItself: the tool is not registered, and
// calling it says what to set rather than coming back as an unknown name.
func TestReportIssueWithoutATokenExplainsItself(t *testing.T) {
	t.Setenv(defaultIssueTokenEnv, "")
	s := serverWith(t, func(c *Config) {
		c.Issues = IssuesConfig{Enabled: true, Repo: "chriswirz/code-mcp"}
	})

	text := errorText(t, s, "report_issue")
	if !strings.Contains(text, defaultIssueTokenEnv) {
		t.Errorf("the answer should name the environment variable: %s", text)
	}
	if !strings.Contains(text, "issues/new") {
		t.Errorf("the answer should offer the manual route: %s", text)
	}
}

func TestIssuesSecretPrefersFileThenEnv(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEMCP_TEST_ISSUE_TOKEN", "from-env")

	cfg := IssuesConfig{TokenFile: file, TokenEnv: "CODEMCP_TEST_ISSUE_TOKEN", Token: "inline"}
	if got := cfg.Secret(); got != "from-file" {
		t.Errorf("Secret() = %q, want the file to win", got)
	}
	cfg.TokenFile = ""
	if got := cfg.Secret(); got != "from-env" {
		t.Errorf("Secret() = %q, want the environment next", got)
	}
	cfg.TokenEnv = ""
	t.Setenv(defaultIssueTokenEnv, "")
	if got := cfg.Secret(); got != "inline" {
		t.Errorf("Secret() = %q, want the literal last", got)
	}
	if got := (IssuesConfig{}).RepoOrDefault(); got != defaultIssueRepo {
		t.Errorf("RepoOrDefault() = %q, want %q", got, defaultIssueRepo)
	}
}

// TestNoTokenInATrackedConfig guards what actually matters: a credential in a
// file git ships. A local config.json that .gitignore excludes is the
// operator's own business - issues.token is a supported way to configure this,
// with its trade-off documented - so it is not policed here.
func TestNoTokenInATrackedConfig(t *testing.T) {
	for _, name := range []string{"config.json", "config.example.json"} {
		if gitIgnores(t, name) {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		var cfg Config
		if err := json.Unmarshal(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}), &cfg); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cfg.Issues.Token != "" {
			t.Errorf("%s is tracked by git and carries an issues token; "+
				"put it in token_env or token_file instead", name)
		}
		if looksLikeAToken(cfg.Issues.TokenEnv) || looksLikeAToken(cfg.Issues.TokenFile) {
			t.Errorf("%s has a token in a field that names where the token lives", name)
		}
		for _, prefix := range []string{"github_pat_", "ghp_"} {
			if strings.Contains(string(data), prefix) {
				t.Errorf("%s is tracked by git and looks like it contains a GitHub token", name)
			}
		}
	}
}

// gitIgnores reports whether .gitignore excludes this file, which is how a
// local configuration stays out of the repository.
func gitIgnores(t *testing.T, name string) bool {
	t.Helper()
	data, err := os.ReadFile(".gitignore")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(strings.TrimPrefix(line, "/")) == name {
			return true
		}
	}
	return false
}

// TestIssuesConfigCatchesATokenInTheWrongField: token_env and token_file name
// where the credential lives. A token in one of them finds nothing, so the
// tool silently goes missing - and the secret is sitting in a tracked file.
func TestIssuesConfigCatchesATokenInTheWrongField(t *testing.T) {
	err := IssuesConfig{TokenEnv: "github_pat_11ABCDEFG_notarealtoken"}.validate()
	if err == nil {
		t.Fatal("a token in token_env should be refused")
	}
	for _, want := range []string{"names the environment variable", defaultIssueTokenEnv, "compromised"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
	if err := (IssuesConfig{TokenFile: "ghp_notarealtoken"}).validate(); err == nil {
		t.Error("a token in token_file should be refused")
	}
	if err := (IssuesConfig{TokenEnv: defaultIssueTokenEnv}).validate(); err != nil {
		t.Errorf("a plain variable name is fine: %v", err)
	}
}

func TestLooksLikeAToken(t *testing.T) {
	for _, value := range []string{"github_pat_x", "ghp_x", "gho_x", "ghu_x", "ghs_x", "ghr_x"} {
		if !looksLikeAToken(value) {
			t.Errorf("looksLikeAToken(%q) = false", value)
		}
	}
	for _, value := range []string{"", "CODEMCP_ISSUE_TOKEN", ".issue-token", "github-token"} {
		if looksLikeAToken(value) {
			t.Errorf("looksLikeAToken(%q) = true", value)
		}
	}
}
