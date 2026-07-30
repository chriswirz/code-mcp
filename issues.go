package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// report_issue is how a model or its user reports a fault in this server to
// the people who maintain it. It goes straight to the GitHub issue tracker
// rather than through the gh CLI: the report is about code-mcp itself, not
// about whatever repository happens to be open, and it must work on a machine
// where gh is not installed or is signed in to somebody else's account.

// defaultIssueRepo is where reports go when the configuration names no other.
const defaultIssueRepo = "chriswirz/code-mcp"

// defaultIssueTokenEnv is the environment variable the token is read from
// unless the configuration names another.
const defaultIssueTokenEnv = "CODEMCP_ISSUE_TOKEN"

// IssuesConfig governs report_issue.
//
// The token deliberately has no place in config.json worth using: that file
// sits inside the workspace, where this server's own read_file and grep_files
// can reach it, so a model on the other end of the connection can read the
// credential back out and put it in a transcript. Whether git tracks the file
// is a second question and depends on .gitignore. token_env and token_file
// keep the credential out of the workspace altogether.
type IssuesConfig struct {
	Enabled bool `json:"enabled"`
	// Repo is the owner/name the reports are filed against.
	Repo string `json:"repo,omitempty"`
	// Token is the literal credential. Discouraged, and named plainly so that
	// putting one here is a decision rather than an accident.
	Token string `json:"token,omitempty"`
	// TokenEnv names an environment variable holding a GitHub token with
	// permission to open issues on Repo. This is the usual way to supply it.
	TokenEnv string `json:"token_env,omitempty"`
	// TokenFile names a file whose first line is the token. It is read on each
	// use, so rotating the file needs no restart.
	TokenFile string `json:"token_file,omitempty"`
	// Labels are attached to every report, for example "from-mcp".
	Labels []string `json:"labels,omitempty"`
	// APIBase is the GitHub API root, for GitHub Enterprise or for a test.
	APIBase string `json:"api_base,omitempty"`
}

// Secret resolves the token: the file, then the environment, then the literal.
func (c IssuesConfig) Secret() string {
	if c.TokenFile != "" {
		if data, err := os.ReadFile(c.TokenFile); err == nil {
			if line := strings.SplitN(strings.TrimRight(string(data), "\r\n"), "\n", 2)[0]; line != "" {
				return strings.TrimSpace(line)
			}
		}
	}
	env := c.TokenEnv
	if env == "" {
		env = defaultIssueTokenEnv
	}
	if value := os.Getenv(env); value != "" {
		return strings.TrimSpace(value)
	}
	return c.Token
}

// Configured reports whether a token is available. The secret never leaves
// this type: only the answer to this question does.
func (c IssuesConfig) Configured() bool { return c.Secret() != "" }

// TokenSource names where the token would come from, for a message that has to
// tell an operator what to set without quoting the value.
func (c IssuesConfig) TokenSource() string {
	switch {
	case c.TokenFile != "":
		return "issues.token_file (" + c.TokenFile + ")"
	case c.TokenEnv != "":
		return "the " + c.TokenEnv + " environment variable"
	case c.Token != "":
		return "issues.token in config.json"
	default:
		return "the " + defaultIssueTokenEnv + " environment variable"
	}
}

// RepoOrDefault is the repository reports are filed against.
func (c IssuesConfig) RepoOrDefault() string {
	if c.Repo != "" {
		return c.Repo
	}
	return defaultIssueRepo
}

// apiBase is the API root, with any trailing slash removed.
func (c IssuesConfig) apiBase() string {
	if c.APIBase != "" {
		return strings.TrimRight(c.APIBase, "/")
	}
	return "https://api.github.com"
}

// issueRequest is the JSON body the GitHub issues API takes.
type issueRequest struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels,omitempty"`
}

// issueResponse is the part of the reply worth reporting back.
type issueResponse struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Message string `json:"message"`
}

// issueDiagnostics is the block appended to a report. It is deliberately made
// of facts about the build and the platform and nothing else: an issue is
// public, and a path, a repository name or a configured host would put the
// reporter's own details on the internet.
func (s *Server) issueDiagnostics() string {
	name, version := s.identity()
	ws := s.workspace()
	var b strings.Builder
	b.WriteString("\n\n---\n\n<!-- added by report_issue -->\n\n")
	b.WriteString("| | |\n| --- | --- |\n")
	fmt.Fprintf(&b, "| server | %s %s |\n", name, version)
	fmt.Fprintf(&b, "| protocol | %s |\n", ProtocolVersion)
	fmt.Fprintf(&b, "| platform | %s/%s |\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "| go | %s |\n", runtime.Version())
	fmt.Fprintf(&b, "| tools | %d registered |\n", len(s.ToolNames()))
	fmt.Fprintf(&b, "| line endings | %s |\n", workspaceLineEndingName(ws))
	fmt.Fprintf(&b, "| workspace | %s, writes %s |\n", workspaceScopeName(ws), allowedOrNot(ws.AllowWrite))
	return b.String()
}

func allowedOrNot(allowed bool) string {
	if allowed {
		return "allowed"
	}
	return "disabled"
}

// registerIssueTool adds report_issue when a token is configured, and records
// why it is missing when there is none.
func (s *Server) registerIssueTool(cfg IssuesConfig) {
	if !cfg.Enabled {
		s.markUnavailable("report_issue", "issues.enabled is false",
			"Report the problem to the user instead, and let them decide where it goes.")
		return
	}
	if !cfg.Configured() {
		s.markUnavailable("report_issue",
			"no GitHub token is available, so there is nothing to authenticate with. "+
				"The operator sets one in "+cfg.TokenSource(),
			"Tell the user what went wrong and let them file it by hand at "+
				"https://github.com/"+cfg.RepoOrDefault()+"/issues/new.")
		return
	}

	repo := cfg.RepoOrDefault()
	s.RegisterTool(Tool{
		Name:  "report_issue",
		Title: "Report a problem with this server",
		Description: fmt.Sprintf(
			"Open an issue against %s, the repository of this MCP server itself. Use it when a tool "+
				"here is broken, misleading or missing - not for problems in the project being worked "+
				"on, which belong in that project's own tracker.\n\n"+
				"THIS PUBLISHES PUBLICLY: the issue is visible to anyone and cannot be quietly "+
				"withdrawn, so confirm the wording with the user before calling this, and keep "+
				"customer data, credentials and file contents out of it. Pass dry_run to see exactly "+
				"what would be posted first.\n\n"+
				"Write the body the way a maintainer needs it: what you called and with what "+
				"arguments, what came back, what you expected instead, and whether it happens every "+
				"time. Details of the build and platform are appended for you.", repo),
		Annotations: &ToolAnnotations{OpenWorldHint: true},
		InputSchema: schema([]string{"title", "body"}, map[string]any{
			"title": prop("string", "One line naming the fault, specific enough to tell two reports apart."),
			"body": prop("string",
				"The report, in Markdown: what happened, what was expected, and the smallest way to reproduce it."),
			"labels": map[string]any{
				"type":        "array",
				"description": "Labels to attach. Ones the repository does not define are refused by GitHub, so leave this out unless you know them.",
				"items":       map[string]any{"type": "string"},
			},
			"include_diagnostics": propDefault("boolean",
				"Append the server version, protocol version and platform. No paths or configured hosts are included.", true),
			"dry_run": propDefault("boolean",
				"Return the exact issue that would be posted, without posting it.", false),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Title              string   `json:"title"`
			Body               string   `json:"body"`
			Labels             []string `json:"labels"`
			IncludeDiagnostics *bool    `json:"include_diagnostics"`
			DryRun             bool     `json:"dry_run"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if strings.TrimSpace(args.Title) == "" || strings.TrimSpace(args.Body) == "" {
			return toolError("title and body are both required: a report with neither is not actionable"), nil
		}

		body := args.Body
		if args.IncludeDiagnostics == nil || *args.IncludeDiagnostics {
			body += s.issueDiagnostics()
		}
		labels := append(append([]string(nil), cfg.Labels...), args.Labels...)
		req := issueRequest{Title: strings.TrimSpace(args.Title), Body: body, Labels: labels}

		if args.DryRun {
			return toolResult(fmt.Sprintf(
				"Dry run: nothing was posted.\n\nWould open an issue on %s:\n\nTitle: %s\nLabels: %s\n\n%s",
				repo, req.Title, labelList(labels), req.Body)), nil
		}

		res, err := postIssue(ctx, cfg, req)
		if err != nil {
			return toolError("%v", err), nil
		}
		return &CallToolResult{
			Content: textContent(fmt.Sprintf("Opened %s#%d: %s\n%s",
				repo, res.Number, req.Title, res.HTMLURL)),
			StructuredContent: map[string]any{
				"repo": repo, "number": res.Number, "url": res.HTMLURL, "title": req.Title,
			},
		}, nil
	})
}

func labelList(labels []string) string {
	if len(labels) == 0 {
		return "(none)"
	}
	return strings.Join(labels, ", ")
}

// postIssue sends the report. Every failure is translated into something the
// caller can act on, and the token never appears in any of them.
func postIssue(ctx context.Context, cfg IssuesConfig, issue issueRequest) (*issueResponse, error) {
	token := cfg.Secret()
	if token == "" {
		return nil, fmt.Errorf("no GitHub token is available; the operator sets one in %s", cfg.TokenSource())
	}
	payload, err := json.Marshal(issue)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/repos/%s/issues", cfg.apiBase(), cfg.RepoOrDefault())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "code-mcp")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach %s: %v", cfg.apiBase(), redactToken(err.Error(), token))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var out issueResponse
	_ = json.Unmarshal(data, &out)
	if resp.StatusCode == http.StatusCreated {
		return &out, nil
	}
	return nil, issueError(resp.StatusCode, out.Message, cfg, token, data)
}

// issueError turns a GitHub status code into the sentence that says what to do
// about it. "401 Unauthorized" tells a model nothing it can use; "the token is
// rejected, ask the operator to check it" does.
func issueError(status int, message string, cfg IssuesConfig, token string, body []byte) error {
	detail := strings.TrimSpace(message)
	if detail == "" {
		detail = strings.TrimSpace(string(body))
	}
	detail = redactToken(detail, token)
	if len(detail) > 300 {
		detail = detail[:300] + "..."
	}
	repo := cfg.RepoOrDefault()
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("GitHub rejected the token (401). It may have expired or been revoked; "+
			"the operator sets a new one in %s. Nothing was posted. GitHub said: %s", cfg.TokenSource(), detail)
	case http.StatusForbidden:
		return fmt.Errorf("GitHub refused the request (403). A fine-grained token needs the "+
			"\"Issues: read and write\" permission on %s, and the repository must be one it is scoped to. "+
			"Nothing was posted. GitHub said: %s", repo, detail)
	case http.StatusNotFound:
		return fmt.Errorf("GitHub has no %s that this token can see (404). Either issues.repo names the "+
			"wrong repository or the token is not scoped to it. Nothing was posted", repo)
	case http.StatusGone:
		return fmt.Errorf("issues are disabled on %s (410), so there is nowhere to file this. "+
			"Tell the user what went wrong instead", repo)
	case http.StatusUnprocessableEntity:
		return fmt.Errorf("GitHub would not accept the issue (422) - usually a label %s does not define. "+
			"Try again without labels. Nothing was posted. GitHub said: %s", repo, detail)
	case http.StatusTooManyRequests:
		return fmt.Errorf("GitHub is rate-limiting this token (429). Wait before trying again; " +
			"nothing was posted")
	}
	return fmt.Errorf("GitHub returned %d and the issue was not created: %s", status, detail)
}

// redactToken keeps a credential out of an error, however it got in there.
func redactToken(text, token string) string {
	if token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "[redacted]")
}

// looksLikeAToken reports whether a string is a GitHub credential rather than
// the name of something. The prefixes are the ones GitHub documents, and are
// what its own secret scanning looks for.
func looksLikeAToken(value string) bool {
	for _, prefix := range []string{"github_pat_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_"} {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

// validate catches the two ways of supplying the token that look right and do
// nothing. token_env and token_file name where the credential lives; putting
// the credential itself in one is silent - the lookup simply finds nothing,
// the tool goes missing, and the secret is sitting in a file that git tracks.
func (c IssuesConfig) validate() error {
	if looksLikeAToken(c.TokenEnv) {
		return fmt.Errorf("issues.token_env holds what looks like a GitHub token, but it names the " +
			"environment variable to read the token from, so nothing will be found there. " +
			"Set issues.token_env back to a name such as \"" + defaultIssueTokenEnv + "\" and put the " +
			"token in that variable, or point issues.token_file at a file containing it. " +
			"Treat the token now in config.json as compromised: it is inside the workspace, where " +
			"this server's own file tools can read it back out")
	}
	if looksLikeAToken(c.TokenFile) {
		return fmt.Errorf("issues.token_file holds what looks like a GitHub token, but it names a " +
			"file to read the token from. Write the token to a file - .issue-token is in .gitignore - " +
			"and name that file here, or use issues.token_env instead")
	}
	if c.TokenFile != "" {
		info, err := os.Stat(c.TokenFile)
		if err != nil {
			return fmt.Errorf("issues.token_file: %w", err)
		}
		if info.IsDir() {
			return fmt.Errorf("issues.token_file: %s is a directory", c.TokenFile)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("issues.token_file: %s is readable by other accounts (mode %04o); chmod 600 it",
				c.TokenFile, info.Mode().Perm())
		}
	}
	return nil
}
