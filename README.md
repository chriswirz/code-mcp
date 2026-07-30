# codemcp

![appicon.png](appicon.png)

A Model Context Protocol server, written in Go, that hands a coding agent the directory you run it from.
Start it in a repository and that repository becomes the workspace: file tools are confined to it, the build/test/lint commands you declared in `config.json` become tools of their own, and the GitHub Actions workflows that deploy the project can be dispatched and followed from the same session.

It implements MCP revision **2026-07-28**.

## Install on Linux in one line

```sh
curl -fsSL https://raw.githubusercontent.com/chriswirz/code-mcp/main/install.sh | sh
```

That downloads the latest release binary for your architecture (amd64 or arm64), verifies it against the published `SHA256SUMS`, and installs it as `codemcp` in `/usr/local/bin`, mode 0755, so the command works both as your own user and under root or `sudo`.
Only when it cannot write there does it fall back to `~/.local/bin`, and it then prints the one command that makes it system-wide.
Then run `codemcp` in a project directory.
The [Install](#install) section below covers Windows, macOS, the `.deb` and `.rpm` packages, and installing by hand.

## Why the commands section

Most of what an agent wastes time on in an unfamiliar repository is guessing how to build it.
`config.json` answers that once:

```json
"commands": [
  { "name": "build", "description": "Build every package.", "command": "go build ./..." },
  { "name": "test",  "description": "Run the tests.", "command": "go test {{args}}", "accepts_args": true, "default_args": "./..." },
  { "name": "lint",  "description": "Vet and lint.", "command": "go vet ./...", "read_only": true }
]
```

Each entry becomes an MCP tool of the same name, listed with its description and the exact command line it runs.
`project_commands` returns the whole set in one call.

With `accepts_args`, the caller's arguments are appended to the command line.
A `{{args}}` placeholder puts them somewhere else instead, which is what a command like `go test` needs: appending `-run TestFoo ./pkg/...` to `go test ./...` produces a command line with two package lists, whereas `go test {{args}}` produces the right one.
`default_args` is used when the caller passes none, so the bare tool still runs the whole suite.

## Install

Every push to `main` publishes a rolling release at [github.com/chriswirz/code-mcp/releases](https://github.com/chriswirz/code-mcp/releases) with binaries for Linux, Windows and macOS on amd64 and arm64, `.deb` and `.rpm` packages, and a `SHA256SUMS` file.
The release is tagged with its version, `0.1.NNNN`, where NNNN is the build number padded to four places - so the tag, the package version and what `codemcp --version` prints are all the same string, and the tags sort in build order.
There is nothing to install alongside them: the binary is static and has no runtime dependencies.

### Linux

The install script is the short way, and does the download, the checksum and the install for you:

```sh
curl -fsSL https://raw.githubusercontent.com/chriswirz/code-mcp/main/install.sh | sh
```

It installs system-wide by escalating with `sudo` when it has to, because a binary in `/usr/local/bin` is on the default `PATH` for root and for ordinary users alike - and both matter, since a server started with `sudo` and one started as yourself are two different things to run.
It reads three environment variables: `CODEMCP_VERSION` pins a release tag instead of the latest, `CODEMCP_INSTALL_DIR` chooses where the binary goes, and `CODEMCP_NO_SUDO=1` keeps it from escalating, installing under `~/.local/bin` for your user only.
Piping a script into a shell is worth reading first; `curl -fsSL .../install.sh -o install.sh` and running it afterwards does the same thing.

By hand:

```sh
base=https://github.com/chriswirz/code-mcp/releases/latest/download
curl -fsSL -O $base/codemcp-linux-amd64
curl -fsSL -O $base/SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS     # codemcp-linux-amd64: OK

sudo install -m 755 codemcp-linux-amd64 /usr/local/bin/codemcp
codemcp --version
```

Keep the asset's own name until after the checksum runs — `SHA256SUMS` lists the published names, so renaming on download leaves nothing for it to match.
`install` does the rename, the mode and the move in one step.

On arm64 (a Raspberry Pi, an AWS Graviton box) substitute `codemcp-linux-arm64`.

Or install the package, which puts the binary in `/usr/bin` and the docs in `/usr/share/doc/codemcp`.
The file names carry the build number, so let `gh` find the current one:

```sh
gh release download --repo chriswirz/code-mcp --pattern '*_amd64.deb'
sudo dpkg -i codemcp_*_amd64.deb
```

```sh
gh release download --repo chriswirz/code-mcp --pattern '*.x86_64.rpm'
sudo rpm -i codemcp-*.x86_64.rpm
```

### Windows

In PowerShell:

```powershell
$base  = "https://github.com/chriswirz/code-mcp/releases/latest/download"
$asset = "codemcp-windows-amd64.exe"
$dir   = "$env:LOCALAPPDATA\Programs\codemcp"
New-Item -ItemType Directory -Force -Path $dir | Out-Null

Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset"       -OutFile "$env:TEMP\$asset"
Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS"   -OutFile "$env:TEMP\SHA256SUMS"

# Verify before installing: find this asset's line in SHA256SUMS and compare.
$want = (Select-String -Path "$env:TEMP\SHA256SUMS" -Pattern ([regex]::Escape($asset))
        ).Line.Split(' ')[0]
$got  = (Get-FileHash "$env:TEMP\$asset" -Algorithm SHA256).Hash.ToLower()
if ($want -ne $got) { throw "checksum mismatch: $got != $want" }

Move-Item -Force "$env:TEMP\$asset" "$dir\codemcp.exe"

# Put it on PATH for future shells, and for this one.
[Environment]::SetEnvironmentVariable("PATH",
  [Environment]::GetEnvironmentVariable("PATH", "User") + ";$dir", "User")
$env:PATH += ";$dir"

codemcp --version
```

On an arm64 machine (a Surface Pro X, a Snapdragon laptop) use `codemcp-windows-arm64.exe`.
The download is unsigned, so SmartScreen may warn on first run.

### From source

Go 1.25 or newer, and nothing else:

```sh
git clone https://github.com/chriswirz/code-mcp.git
cd code-mcp
./build.sh          # or build.cmd on Windows; --all cross-compiles every target
```

`go install github.com/chriswirz/code-mcp@latest` also works, but names the binary `code-mcp` after the module path rather than `codemcp`.
Rename it if you want the shorter name:

```sh
mv "$(go env GOPATH)/bin/code-mcp" "$(go env GOPATH)/bin/codemcp"
```

## Getting started

```sh
codemcp --example-config > config.json   # a complete, commented starting point
# edit the commands section for your project
codemcp                                  # serve the current directory
```

On startup it prints the URL to connect to:

```
codemcp dev  (MCP protocol 2026-07-28)
  workspace  D:\Source\code-mcp
  transport  http
  listening  127.0.0.1:8765

  Connect your MCP client to:  http://127.0.0.1:8765/mcp

  tools (33)  build, edit_file, find_files, fmt, git_add, git_branch
              ...
  commands   build, test, lint, fmt
  database   not configured
```

`--check` prints exactly that and exits, which is the quick way to confirm a config change before restarting a running server.

## Flags

| Flag | Meaning |
| --- | --- |
| `-c`, `--config <path>` | Configuration file. Default: `config.json` in the workspace; a missing default file is not an error. |
| `--workspace <dir>` | Directory to serve. Default: the current directory. `.` serves the whole system: paths still resolve against the current directory, but nothing is refused for being outside it. |
| `-u`, `--url <url>` | Full URL clients connect to. Its host and port are what the server binds to, its path is the MCP endpoint. Default `http://127.0.0.1:8765/mcp`, or `server.url` from the config. Comma-separate several to serve them all at once. |
| `--transport <name>` | `http` for Streamable HTTP, or `stdio` for a client that launches the server as a subprocess. |
| `--token <token>` | Require this bearer token on every HTTP request. |
| `--allow-origin <o>` | Comma-separated origins a browser may call the server from, replacing `server.allowed_origins`. `*` allows any origin. |
| `--allow-header <h>` | Comma-separated request headers a browser may send, replacing `server.allowed_headers`. `*` allows any header. |
| `--tls-cert <path>`, `--tls-key <path>` | Serve HTTPS with this certificate and key. |
| `--tls-self-signed` | Serve HTTPS with a certificate generated at startup. |
| `--tunnel <url>` | Expose the server through an https-tunnel server at this URL, on a public HTTPS address. Sets `tunnel.enabled`. |
| `--tunnel-key <key>` | API key for that tunnel server. Default: the `TUNNEL_API_KEY` environment variable. |
| `--tunnel-subdomain <label>` | Subdomain to ask for; a random one is issued if it is taken. |
| `--tunnel-session-file <path>` | Keep the issued session id in this file instead of in `config.json`. Naming it makes the file the only store. |
| `--tunnel-only` | Serve the tunnel alone, binding no local port. |
| `--no-legacy` | Serve only protocol version 2026-07-28, refusing the older initialize-based revisions. |
| `--db-url <url>` | PostgreSQL connection URL; enables the database tools without putting credentials in the config file. |
| `--example-config` | Write a complete example `config.json` to stdout and exit. |
| `--check` | Load the config, print what would be served, and exit without listening. |
| `-v`, `--version` | Print the version and exit. |
| `-h`, `--help` | Describe the program and its arguments. |

Command-line flags override the corresponding `config.json` settings.

## Tools

Every tool declares its parameters in snake_case, but camelCase is accepted for any of them: `replaceAll` reaches `replace_all`, `startLine` reaches `start_line`, and so on, at any depth including inside arrays of objects.
Models disagree about which convention JSON "should" use, and a call that fails on the spelling of a key costs a whole turn to fix something that carries no meaning.
Rewriting applies to keys only - string values keep their capitals - and an explicitly supplied snake_case key always wins over a camelCase sibling.

**Project commands** - one tool per entry in `commands`, plus `project_commands` to list them and `run_command` as an escape hatch.

`run_command`'s description is generated at startup and names the platform it is actually running on: the operating system and architecture, the shell the command line is handed to, the syntax traps of that particular shell, and the path separator.
A static description has to cover every platform and leave the caller to work out which half applies, which is how a model ends up spending its first command discovering that `grep` does not exist on Windows.
Each project command's description names the shell for the same reason, since that is the syntax its `args` must be written in.

**Environment** - `system_info` reports the operating system, version, architecture, hostname, CPU count, the shell that `run_command` and the project commands actually execute through, the path separator and line ending, and which of git, gh, go, make, docker, node and python are on `PATH`.
Worth calling before writing any shell command or path, so the syntax matches the platform instead of being inferred from stray backslashes in an error message.
`run_command` states the essentials in its own description, so this is the fuller picture rather than the first thing to reach for.
Pass `check_programs: false` to skip the `PATH` probe.

**Files**, all confined to the workspace root (symlinks are resolved before the containment check; a rooted path such as `/README.md` is anchored inside the workspace rather than refused, so it means the README at the top of the project, not one at the root of the filesystem) - `read_file`, `write_file`, `edit_file`, `multi_edit`, `apply_diff`, `format_markdown`, `list_directory`, `grep_files`, `search_files`, `find_files`.

Recursive walks (`list_directory` with `recursive`, plus `grep_files`, `search_files` and `find_files`) skip directories named in `workspace.exclude` (defaulting to `.git`, `node_modules`, `vendor`, `dist`, `out`, `target` and `.venv`).
Pass one of those paths explicitly as the tool's `path` when you need to look inside it; nested copies of the same names are still skipped.

`grep_files` is the search to reach for: it returns each match with the lines around it, so a hit is interpretable without a second call to read the file.
`context` defaults to **0** - just the matching line - and `before`/`after` give an uneven window.
It also takes `literal` (no regex escaping needed), `glob`, `ignore_case`, `files_only` and `max_matches`, and accepts a single file as its `path`.
Output follows grep's convention, `path:line:text` for a match and `path-line-text` for context, with `--` between non-adjacent blocks:

```
compat.go-139-// newSessionID returns a globally unique, cryptographically random id, which is
compat.go-140-// what the legacy specification requires of a session id.
compat.go:141:func newSessionID() string {
compat.go-142-	var buf [16]byte
compat.go-143-	if _, err := rand.Read(buf[:]); err != nil {
```

`edit_file` replaces an exact string, and `multi_edit` applies a list of such replacements across one or more files in a single call.
Both are the tool to reach for over `write_file` when a file already exists, because they change only what they name:

- **The anchor must be unambiguous.** `old_string` has to appear exactly once unless `replace_all` is set, so an edit cannot silently land on the wrong occurrence.
  When it appears more than once the error says how many times.
  `oldText`/`newText` are accepted as aliases for clients that emit the camelCase spelling; the snake_case names win if both are sent.
  (These two are separate from the general camelCase handling above, since they differ by a word rather than by case.)
- **The result is echoed back as a diff hunk**, with three lines of context either side, so there is no need to re-read the file to confirm what landed.
  Large rewrites are capped rather than allowed to flood the context.
- **`multi_edit` stages everything first.** Each edit is applied in memory, in order, and every destination is checked for writability before a single byte is written, so a bad anchor on the sixth edit leaves the workspace untouched.
  Later edits in the list see the result of earlier ones.
  `dry_run` reports the diff without writing.
- **`normalize_line_endings`** (default true) retries an anchor that does not match, re-encoded to the file's own line endings, which is what makes an LF-quoted anchor work against a CRLF file.
  It is a fallback rather than a rewrite: an exact match always takes precedence, so an edit that deliberately changes a line ending still does exactly what it says, and only the replaced region is touched.
  When the retry is what matched, the result says so.
  With the option off, a mismatch that normalising would have fixed says as much in the error.

`apply_diff` applies a unified diff, which is the efficient way to make a change that spans several files or several places in one file.
It is implemented in Go rather than by shelling out to `git apply`, so the workspace need not be a repository and git need not be installed, and patched paths are held to the same workspace containment as every other file tool.
It handles multiple files, creation, deletion and renames, and:

- **Line numbers need not be exact.** The hunk's context is searched for around the line the header names (`max_offset`, default 200 lines), so a diff written against a slightly stale copy still applies.
  Any hunk that lands away from its stated line is reported with its offset, since that usually means the diff was stale.
- **Context lines must match.** When they don't, the error names the closest near-match and quotes both sides - `at line 8 the file has "WRONG" but the hunk expects "EXPECTED"` - which is what lets a model correct the patch.
  `ignore_whitespace` relaxes indentation while keeping the file's own.
- **All or nothing.** Every file is computed and checked before anything is written, so a patch that fails on its third file does not leave the first two applied.
  `dry_run` checks without writing.

`format_markdown` rewrites a document so that each sentence starts on its own line.
Prose wrapped to a fixed column produces diffs in which one edited word reflows the whole paragraph; one sentence per line keeps the diff to the sentences that actually changed, which is what makes a documentation review readable.
Pass `path` to rewrite a workspace file, or `content` to format text without touching disk.

- **Only line breaks move.** The tool compares the word sequence before and after and refuses to write if anything else changed, so a bug in the reflow cannot quietly eat a paragraph.
- **Structure is preserved.** Code fences, tables, headings, block quotes, YAML front matter and the blank-line layout pass through untouched.
  List items keep their marker with later sentences hanging at the text column, and an indented paragraph keeps its indent, which is what holds a continuation to the bullet it belongs to.
- **Inline code is protected.** Spans are masked before the split, so a path like `./...` or an abbreviation such as e.g. does not read as the end of a sentence.
- **Line endings are kept.** A CRLF document stays CRLF rather than being silently converted.
- **Idempotent.** Running it twice changes nothing the second time, so it is safe to wire into a pre-commit hook or a `commands` entry.

This README is maintained in that style, and the test suite checks that reflowing it is a no-op.

**Git** - `git_status`, `git_diff`, `git_log`, `git_show`, `git_blame`, `git_branch`, `git_add`, `git_stash`, `git_commit`, `git_push` when `git.allow_push` is set, and `git_restore` when `git.allow_restore` is set.
Pushing is off by default, because on a repository wired up like this one a push *is* a deploy.

`git_blame` takes `start_line`/`end_line` so you can blame just the region you care about, and passes `-w` so a reformatting commit does not mask whoever actually wrote the line.

`git_diff` shapes its output as well as selecting it.
A full patch of a large working tree is usually more than the question needs, so `stat` gives a per-file count of changed lines, `name_only` gives just the paths, and `context` narrows the lines shown around each hunk (`0` for changed lines only).
Reach for one of those first and then take the full patch of the file that turns out to matter.
`context` is ignored alongside `stat` or `name_only`, where it has no meaning and git rejects the combination.

`git_stash` (`push`, `pop`, `apply`, `list`, `show`, `drop`) is the undo for a change that went wrong: nothing else here puts a modified working tree back the way it was.
`git_restore` discards uncommitted changes outright, which is why it sits behind its own flag and is off by default - it is the one git tool that can destroy work that exists nowhere else.
It requires explicit paths rather than allowing a blanket restore.

**GitHub Actions**, through the `gh` CLI so they inherit its authentication - `github_workflows`, `github_workflow_file`, `github_workflow_run`, `github_runs`, `github_run_view`, `github_run_logs`, `github_run_watch`, `github_run_rerun`, `github_run_cancel`, `github_releases`, `github_pr`.

**PostgreSQL**, registered only when `config.json` carries credentials - `db_query`, `db_tables`, `db_describe_table`, and `db_execute` when `database.allow_write` is set.
`db_query` refuses anything that is not a single read statement.

**Temporary downloads** - `get_download_link`, `list_download_links`, `revoke_download_link`.
`get_download_link` publishes one workspace file at an unguessable URL on the listener the MCP endpoint already has, under the endpoint path:

```
http://127.0.0.1:8765/mcp/files/build.tar.gz?token=e16e370c-723d-40ee-ac31-b03966039064
```

The link lasts `downloads.default_ttl_minutes` (5) unless the call passes `minutes`, and no longer than `downloads.max_ttl_minutes` (60).
After that the URL answers 404 - the same answer an unknown token gets, so probing tokens tells you nothing.
The token is the only credential: it is generated from `crypto/rand`, checked in constant time, and carried in the query rather than an `Authorization` header, because the point is to hand the link to a browser or a person and a browser has nowhere to put a bearer token.
Everything about the response comes from the link rather than the request, so the URL has no path to steer; the file name in it is checked against the link and cannot be used to reach a different file.
`revoke_download_link` ends a link early, and it accepts the whole URL as well as the bare token.

Behind a reverse proxy, set `downloads.base_url` to the prefix a client actually reaches (`https://example.com/mcp/files`).
On stdio, where there is no MCP listener to share, the first link opens one of its own on `downloads.addr`.
Set `downloads.enabled` to `false` and none of these tools are registered at all.

**Line endings.** `workspace.line_endings` decides whether the workspace speaks one convention or leaves every file as it found it.
The default, `"preserve"`, changes nothing: each file keeps the endings it has, and an edit whose anchor does not match is retried against the file's own convention (`normalize_line_endings`, on by default per call).

Set it to `"lf"`, `"crlf"` or `"native"` (this machine's) and the workspace normalizes instead:

- every read hands back LF, whatever is on disk - including a file with mixed endings, or a lone ``;
- every write re-encodes to the configured convention, so a file cannot come out of an edit with two conventions in it, even when the text supplied had both;
- `edit_file`, `multi_edit` and `apply_diff` fold their anchors, replacements and patch text to LF before comparing, so an anchor written with CRLF matches a file stored with LF and the other way round;
- `grep_files` and `search_files` match against the normalized text too.

`system_info` reports which of the two is in effect, and the server says so in its instructions, so the model knows it should write LF and let the server convert.
Under normalization an edit can no longer change a file's line endings deliberately - that is what the setting is for; change `workspace.line_endings` instead.

`fix_line_endings` is the bulk counterpart to that setting: the setting governs what this server writes from now on, and this tool brings the files already on disk into line.
`scope` chooses how far it reaches - `"file"` (one file), `"folder"` (the files directly in a directory), `"tree"` (a whole subtree) or `"workspace"` (everything under the root) - and `mask` narrows it to matching names, one pattern or several separated by commas (`"*.js,*.ts"`).
`ending` defaults to `workspace.line_endings`, or the platform's convention when line endings are preserved.

The effect is always worked out before anything is written.
Every candidate is read and compared, so the count is what the write would really do, and the whole operation is refused - with nothing written - when it comes to more files than `workspace.max_line_ending_files` (500 by default).
`dry_run` returns the same plan without writing.
Binary files are never rewritten, and neither are excluded paths or files over `workspace.max_file_bytes`; all three are named in the result.
The `"workspace"` scope is refused outright on an unrestricted workspace, where it would mean rewriting files across the whole machine.

## Prompts

`verify_change`, `ship_change`, `diagnose_deploy` and `explore_workspace` are the recurring workflows: check a change, take it through commit, push and the Actions run that publishes it, work out why a run failed, and get oriented in an unfamiliar repository.

## Resources

Every readable file under the workspace root is listed as a `file://` resource, and the `workspace:///{path}` template addresses any of them by relative path.

## Reloading the configuration

`config.json` is re-read before every request, so an edit takes effect on the next call instead of at the next restart.
Adding a command, changing a workspace rule, turning the git tools off or rotating the sudo password all apply live: the workspace, the sudo agent, the instructions and the whole tool set are rebuilt from the file, and a client that lists tools again sees the new set.

A reload that fails costs nothing.
An unreadable file, one deleted mid-session, or one caught half-written and invalid leaves the values already in effect exactly as they are, and the failure is logged once rather than on every request until it is fixed.
The server says so again when the file becomes readable, and an untouched file is not reapplied at all, so a normal request pays for one read and a comparison.

Command-line flags keep winning over the file on every reload: `--workspace`, `--token` and the rest are re-applied on top of what was just read, so an edit to `config.json` cannot quietly take back what you asked for on this run.

What a reload cannot change is anything fixed when the process started: the listener and its URL, the TLS material, the auth token, the tunnel, and the database connection.
Those are reported in the log as needing a restart rather than half-applied.

## sudo

A command that needs elevation stalls forever on a password prompt nothing is there to answer.
Configure the password once and the server answers it, without the model ever being told what it is:

```json
"sudo": {
  "password": "",
  "password_env": "CODEMCP_SUDO_PASSWORD",
  "password_file": ""
}
```

The password is resolved in that order of preference: `password_file` (first line, re-read on each use so rotating the file needs no restart), then `password_env`, then the literal `password`.
Prefer either of the first two.
An inline `password` sits in `config.json`, which is a file inside the workspace that `read_file` can open, which defeats the point; the server warns on startup when it finds one.
A `password_file` that other accounts can read is refused at startup - `chmod 600` it.

With a password configured, the model writes `sudo apt-get update` like anyone else and it works.
What happens underneath is that a directory holding a `sudo` shim goes on the front of the command's `PATH`, the shim execs the real sudo with `-S -p ''`, and the password is written to the command's stdin by this process.
So the secret is never in the command's environment, never written to disk, never in a tool result (results are scrubbed for it in case a command echoes its own input), and never in anything the model is shown - `system_info` reports only that a password *is* configured, and `run_command`'s description tells the model to use sudo normally and not to go looking for the password.

The honest limit: `run_command` runs as the same user as this server, so a command that deliberately went hunting could still read the password out of its own stdin.
This keeps the secret out of the model's context and out of everything it is handed; it is not a sandbox around a model that is actively trying to steal it.
If that matters, do not configure a password, and use `NOPASSWD` sudoers rules scoped to the exact commands you want to allow.

## Database

Enable the section and give it either a URL or the discrete fields:

```json
"database": {
  "enabled": true,
  "url": "postgres://app:secret@127.0.0.1:5432/prod?sslmode=require",
  "max_rows": 200,
  "statement_timeout_seconds": 30,
  "allow_write": false
}
```

If the section is disabled or incomplete the server starts normally without the database tools.
If it is configured but unreachable, startup fails rather than silently dropping them.
`--db-url` sets the same thing from the command line, so credentials need not live in a file.

## Protocol notes

Revision 2026-07-28 changed the shape of MCP substantially, and this server implements the new shape rather than emulating the old one:

- **No handshake, no sessions.** There is no `initialize` and no `Mcp-Session-Id`.
  Every request carries its protocol version, client identity and capabilities in `_meta`; every result carries `resultType` and this server's identity under `io.modelcontextprotocol/serverInfo`.
- **`server/discover`** advertises the supported versions, capabilities and instructions.
  A request declaring a version this server does not implement gets `UnsupportedProtocolVersionError` (`-32022`) listing what it does support.
- **Mirrored request headers.** `MCP-Protocol-Version` and `Mcp-Method` are required on every POST, `Mcp-Name` on `tools/call`, `resources/read` and `prompts/get`; each is validated against the request body, including the `=?base64?…?=` sentinel encoding, and a mismatch is `HeaderMismatch` (`-32020`) with HTTP 400.
  `Mcp-Param-*` headers are validated against tool arguments annotated with `x-mcp-header`.
- **POST only.** The GET stream and the DELETE teardown of earlier revisions return 405.
  `subscriptions/listen` opens the one long-lived stream, with SSE comment keep-alives.
- **Cache hints.** `server/discover`, the list methods and `resources/read` return `ttlMs` and `cacheScope`, and `tools/list` is ordered deterministically so clients can cache it.

`--transport stdio` speaks the same protocol as newline-delimited JSON-RPC on stdin and stdout, with all human-readable output on stderr.

## Backwards compatibility

The server is *dual-era*: it serves the current stateless revision and the older initialize-based ones, and picks which from how the client opens.
A request carrying per-request `_meta` is served statelessly; an `initialize` request selects legacy semantics for the session (HTTP) or the process (stdio).

| | Served |
| --- | --- |
| `2026-07-28` | Stateless, `server/discover`, mirrored headers, `resultType`, cache hints |
| `2025-11-25`, `2025-06-18`, `2025-03-26` | `initialize` handshake, `Mcp-Session-Id`, GET stream, DELETE teardown, `ping`, `logging/setLevel`, `resources/subscribe` |
| `2024-11-05` (HTTP+SSE) | Not implemented - deprecated since 2025-03-26 |

Practical consequences:

- A legacy client's results carry no `resultType` and no `ttlMs`/`cacheScope`; those fields did not exist in its revision and are omitted rather than sent as noise.
- The mirrored `Mcp-Method`/`Mcp-Name` header validation applies only to modern requests.
  A legacy client is not judged against rules its revision never defined.
- `initialize` negotiates a version: one this server implements is echoed back, anything else falls back to `2025-11-25`.
- Sessions are minted on `initialize`, echoed on every response, terminated by `DELETE`, and expire after `server.session_timeout_seconds` (default two hours) so a client that vanishes does not leak one.
  A request against an unknown session gets 404, telling the client to start a new one.
- `GET` with `Accept: text/event-stream` opens the legacy standalone stream and keeps it alive.
  This server pushes nothing on it, but a client that waits on one is not left with a failed connection.
- `subscriptions/listen` is modern-only; `logging/setLevel` and `resources/subscribe` are legacy-only.
  Each returns method-not-found to the other era.

`--no-legacy` (or `"legacy_compatibility": false`) turns all of this off: `initialize` is then refused with an error naming the versions the server does speak, since a legacy client has no way to fall forward on its own and that message may be the only diagnostic its user ever sees.

## Authentication

The HTTP transport takes a single shared secret and requires it in the `Authorization` header of every request.
Set it with `--token`:

```sh
codemcp --token "$(openssl rand -hex 32)"
```

To keep it out of the process list and your shell history, put it in `config.json` instead:

```json
{
  "server": {
    "transport": "http",
    "url": "http://127.0.0.1:8765/mcp",
    "auth_token": "a-long-random-secret"
  }
}
```

The startup banner confirms it is on:

```
  Connect your MCP client to:  http://127.0.0.1:8765/mcp
  Authentication: send Authorization: Bearer <token>
```

Clients send the token as a bearer credential:

```sh
curl -sS http://127.0.0.1:8765/mcp \
  -H "Authorization: Bearer a-long-random-secret" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "MCP-Protocol-Version: 2026-07-28" \
  -H "Mcp-Method: tools/list" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'
```

An MCP client that supports remote servers takes the same header in its own configuration - for Claude Code:

```sh
claude mcp add --transport http codemcp http://127.0.0.1:8765/mcp \
  --header "Authorization: Bearer a-long-random-secret"
```

A missing or wrong token gets 401 with `WWW-Authenticate: Bearer realm="mcp"` and a JSON-RPC error body, before any method dispatch:

```json
{"jsonrpc":"2.0","error":{"code":-32600,"message":"authentication required"}}
```

An empty `auth_token` - the default - disables the check entirely, which is the sane setting for a server bound to `127.0.0.1` and nothing else.

Three things to know:

- The check covers the HTTP transport only.
  `stdio` has no headers and no network exposure; the token is ignored there.
- Preflight `OPTIONS` requests skip it, because a browser never attaches credentials to one.
  The real request that follows is still checked.
- If you restrict `server.allowed_headers`, `Authorization` has to be in the list or a browser client cannot send it.
  The default echo, and `["*"]`, both cover it.

## CORS

`server.allowed_origins` drives both the DNS rebinding defence and CORS:

```json
"allowed_origins": ["*"]
"allowed_origins": ["https://www.chriswirz.com", "http://localhost:5173"]
```

An entry without a port matches any port on that host, so `http://localhost` covers whatever port your dev server happens to be on.
`*` allows any origin.

Preflight `OPTIONS` requests are answered for every allowed origin - including the server-wide `OPTIONS *` - with `Access-Control-Allow-Methods: POST, OPTIONS` and a 24-hour `Max-Age`.
Preflights skip the bearer-token check, because browsers never attach credentials to one.

Request headers are controlled by `server.allowed_headers`:

| Setting | Preflight answers with |
| --- | --- |
| unset (default) | The headers the browser asked for, echoed back |
| `["*"]` | The headers asked for, or `*` when it asked for none |
| `["Content-Type", "Authorization"]` | Exactly that list, and nothing else |

The default is already permissive, and deliberately so: a tool can mirror arguments into `Mcp-Param-*` headers whose names the server cannot know in advance, so a fixed list would silently break them.
Naming headers is how you *restrict* them.
Note that a literal `*` is ignored by browsers on a credentialed request, which is why the concrete echo is preferred whenever there is one to give.

Responses expose `Mcp-Session-Id`, `MCP-Protocol-Version`, `Mcp-Method`, `Mcp-Name` and `WWW-Authenticate`.
The session id has to be exposed or a legacy client running in a page cannot read the session its `initialize` just created.

An origin that is *not* allowed gets 403 with no CORS headers, which is what stops a web page from reaching your local server by DNS rebinding.
Credentials are granted only to explicitly listed origins, never under the wildcard.

### Reaching this server from a hosted web app

A browser applies two checks *before* CORS, and neither can be satisfied by a response header alone:

1. **Mixed content.** A page served over `https://` may not fetch an `http://` URL.
   The request is blocked before it is sent, so the server never sees it and no header it returns can help.
   The symptom is a bare `TypeError: Failed to fetch` with no status code.
2. **Private Network Access.** A page on a public address reaching a private one such as `127.0.0.1` must be granted permission.
   This one *is* answerable: the server replies `Access-Control-Allow-Private-Network: true` to the preflight, controlled by `server.allow_private_network` (on by default).
   Only an origin that already passed the origin check gets the grant.

So for a hosted app at `https://app.example` talking to codemcp on your machine, pick one of:

- **Use the app's own proxy** if it has one.
  The connection is then made by the app's server rather than your browser, and neither check applies.
  This is the simplest fix and needs nothing from codemcp.
- **Serve codemcp over HTTPS**, so mixed content no longer applies:

  ```sh
  codemcp --tls-self-signed --allow-origin https://app.example
  # -> https://127.0.0.1:8765/mcp
  ```

  A generated certificate is not trusted by any browser, so open the URL once and accept the warning, after which the app can connect.
  For a certificate browsers trust without the detour, use [mkcert](https://github.com/FiloSottile/mkcert) and pass `--tls-cert`/`--tls-key`.
- **Run the app over plain HTTP on localhost**, which makes both checks moot.

Passing `--tls-cert` or `--tls-self-signed` with an `http://` URL rewrites the scheme to `https://`, rather than quietly serving plaintext on a URL that says otherwise.

### Reaching this server through a tunnel

Neither TLS nor CORS helps when the client cannot route to your machine at all - a hosted agent, a phone, a colleague's laptop.
For that, codemcp can open an [https-tunnel](https://github.com/chriswirz/https-tunnel) itself and be served on a public HTTPS URL:

```sh
export TUNNEL_API_KEY=...
codemcp --tunnel https://tunnel.example.com --tunnel-subdomain my-mcp --token "$MCP_TOKEN"
```

The tunnel client runs inside this process and serves the MCP handler directly, so nothing is proxied through a local socket and no port is bound on your behalf.
The public URL is printed as soon as the tunnel comes up; the MCP endpoint is the same path as locally, so `https://my-mcp.tunnel.example.com/mcp`.

- By default the local listeners run too, which is convenient while developing.
  `--tunnel-only` drops them.
- The subdomain is a request: it is granted when free and a random label is issued otherwise, so read the URL that is printed rather than assuming it.
- The session id the tunnel server issues is written straight back into the `tunnel` section of `config.json`, so the next run reclaims the same URL automatically.
  Naming `--tunnel-session-file` moves that id into a file of its own and leaves the config untouched: one store either way, never two that can disagree.
  The file is written `0600`; treat it as a credential and keep it out of version control.
- **Set `--token`.** A tunnelled server is reachable by anyone who has the URL, and every tool - shell included - is behind it.
  The startup banner warns when no token is set.

The same settings live under `tunnel` in `config.json`:

```json
{
  "tunnel": {
    "enabled": true,
    "server_url": "https://tunnel.example.com",
    "api_key_env": "TUNNEL_API_KEY",
    "subdomain": "my-mcp",
    "session_file": ".codemcp-tunnel-session",
    "only": false
  }
}
```

`config.example.json` ships the same block, filled in and disabled, ready to be switched on.
JSON carries no comments and the loader rejects unknown keys, so each field is described here instead:

| Key | Meaning |
| --- | --- |
| `enabled` | Open the tunnel on startup. Everything else in this section is ignored while it is `false`. |
| `server_url` | The https-tunnel control plane, e.g. `https://tunnel.example.com`. Required. |
| `api_key` | Key for that server. Leave it empty to read `api_key_env` instead, which keeps the key out of the file. |
| `api_key_env` | Environment variable the key is read from. Default `TUNNEL_API_KEY`. |
| `subdomain` | Label to ask for. Granted when free, otherwise a random one is issued - read the URL that is printed. |
| `session_id` | Resume a specific session, keeping its URL. Normally left empty and managed through `session_file`. |
| `session_file` | Where to persist the issued session id, so a restart reclaims the same URL. Relative paths resolve against the workspace, and the file is written `0600`. Empty means do not persist. |
| `only` | Serve the tunnel alone, binding no local port. `false` serves both. |
| `client_info` | Free text recorded in the tunnel server's log. Defaults to the name and version of this server. |

Every key has a flag: `--tunnel`, `--tunnel-key`, `--tunnel-subdomain`, `--tunnel-session-file` and `--tunnel-only` override the file.
`--check` validates the section and reports what would be served without connecting.

#### A tunnel and no local port at all

With `"only": true` nothing is bound on this machine: the tunnel client serves the MCP handler in process, and the only way in is the public URL.
This is the configuration for a machine where binding a port is awkward, or where you would rather not have one open at all.

```json
{
  "server": {
    "name": "code-mcp",
    "instructions": "Coding agent for this repository. Call system_info before writing your first shell command or path.",
    "transport": "http",
    "url": "http://127.0.0.1:8765/mcp",
    "auth_token": "a-long-random-secret",
    "allowed_origins": ["https://app.example"]
  },
  "tunnel": {
    "enabled": true,
    "server_url": "https://tunnel.example.com",
    "api_key_env": "TUNNEL_API_KEY",
    "subdomain": "my-mcp",
    "session_file": ".codemcp-tunnel-session",
    "only": true
  }
}
```

```sh
export TUNNEL_API_KEY=...
codemcp
# tunnel up: https://my-mcp.tunnel.example.com
# -> connect your MCP client to https://my-mcp.tunnel.example.com/mcp
```

`server.url` still matters even though nothing listens: its **path** is the MCP endpoint the tunnel serves, so `/mcp` above is what appears on the public URL.
Its host and port are simply unused, and the startup banner omits the usual `listening` line to say so.

Two settings that are optional on a loopback server are not optional here:

- `server.auth_token`, because the URL is public and every tool sits behind it.
- `server.allowed_origins`, if a browser is the client.
  The origin is now a real one such as `https://app.example` rather than `http://localhost`, and an origin that is not listed gets 403.

`transport` must stay `http`; a tunnel has no handler to serve under `stdio`, and startup refuses the combination rather than ignoring it.

### Serving http and https at once

You rarely want to choose.
A browser page on an `https` origin needs the encrypted endpoint; a local client is happier without the certificate warning.
`server.urls` serves both from one process.

Put this in `config.json` next to the code you want to work on, and run `codemcp` in that directory:

```json
{
  "server": {
    "urls": [
      "http://127.0.0.1:8765/mcp",
      "https://127.0.0.1:8766/mcp"
    ],
    "tls_self_signed": true,
    "allowed_origins": [
      "http://localhost",
      "https://app.example"
    ]
  },
  "commands": [
    { "name": "build", "description": "Build every package.", "command": "go build ./..." },
    { "name": "test",  "description": "Run the test suite.",  "command": "go test {{args}}", "accepts_args": true, "default_args": "./..." },
    { "name": "lint",  "description": "Vet the code base.",   "command": "go vet ./...", "read_only": true }
  ]
}
```

Four lines are doing the work:

- **`urls`** — the two endpoints.
  Different ports, because one port carries one protocol.
  This replaces `server.url`, which you can delete.
- **`tls_self_signed`** — generates the certificate at startup, so there is nothing to create first.
  Swap it for `"tls_cert_file"` and `"tls_key_file"` once you have a real certificate; see above for mkcert.
- **`allowed_origins`** — every browser origin that may connect.
  The default covers only `127.0.0.1` and `localhost`, so a hosted app has to be named here or it gets 403.
  An entry without a port matches any port on that host.
- **`commands`** — optional here, but it is what stops the model guessing how your project builds.

Everything else keeps its default; `codemcp --example-config` prints the full set of settings if you want to see what you are inheriting.

Starting it prints both endpoints:

```
  listening  127.0.0.1:8765, 127.0.0.1:8766

  Connect your MCP client to any of:
      http   http://127.0.0.1:8765/mcp
      https  https://127.0.0.1:8766/mcp

  tls        self-signed, generated at startup for localhost, 127.0.0.1, ::1
             browsers will not trust it; open the URL once and accept the warning
  origins    http://localhost, https://app.example
```

Check it before committing to a running server — `codemcp --check` prints exactly that and exits without listening.

The same thing without a config file, comma-separating the URLs:

```sh
codemcp --url http://127.0.0.1:8765/mcp,https://127.0.0.1:8766/mcp \
        --tls-self-signed --allow-origin http://localhost,https://app.example
```

`server.urls` replaces `server.url` when present.
Details worth knowing:

- **One certificate covers every endpoint**, and a generated one is issued for every hostname across all of them plus the loopback names.
- **One process, one state.** All endpoints share a single tool registry, workspace and session store, so a legacy session opened on the http endpoint keeps working if the client switches to the https one.
- **Several paths may share a port** — `http://host:8765/mcp` and `http://host:8765/api/mcp` become one socket answering both.
- **One port cannot serve both schemes.** Listing the same address as `http` and `https` is a startup error telling you to give them different ports, rather than a server that half works.
- **All or nothing.** If any listener cannot bind, the whole server fails and the others are shut down.
  A server that is half up is worse than one that refuses to start, because a client reaching the surviving half has no way to tell.
  The error names the address that failed:

  ```
  codemcp: 127.0.0.1:8765: listen tcp 127.0.0.1:8765: bind: Only one usage of
  each socket address (protocol/network address/port) is normally permitted.
  ```

  Usually that means an older `codemcp` is still running on that port.

## Security

- The workspace root bounds every file tool.
  A rooted path (`/README.md`, `C:\README.md`) is re-anchored inside the root and the tool says where the file landed; `..` traversal out of the root is still refused.
- Setting `workspace.root` to `"."` (or `workspace.unrestricted`) deliberately removes that bound: the file tools may then read and write anywhere on the machine.
  The server states this in its instructions and in `system_info` so the model knows its paths are not fenced.
  Combined with `workspace.allow_write`, and especially with a server running as root, this is full system access - use it only when that is what you want.
- On Linux and macOS the server detects whether it is running as root or under `sudo` and warns the model in its instructions, in `system_info` and in the `run_command` description, since every command it runs would then be unsandboxed.
- The `Origin` header is validated against `server.allowed_origins` and a foreign origin gets 403, which is what keeps a web page from driving your local server through DNS rebinding.
- Bind to `127.0.0.1` unless you have a reason not to, and set `server.auth_token` (or `--token`) if you widen it.
- A tunnel makes the server public.
  Set `server.auth_token`, and remember that the tunnel session file is a credential for the URL it reclaims.
- `sudo.password` (and its `password_env` / `password_file` forms) lets the server answer a sudo prompt without the model seeing the secret, but it also means the model can elevate at will: configure it only when that is what you want, and prefer scoped `NOPASSWD` sudoers rules when it is not.
- `get_download_link` publishes a workspace file to anyone who has the URL, with no authentication beyond the token in it, for as long as the link lives.
  Keep `downloads.max_ttl_minutes` short, and turn `downloads.enabled` off on a server whose listener is public and whose files are not.
- `git.allow_push`, `git.allow_restore`, `database.allow_write` and `workspace.allow_write` gate the tools that change something outside this process, or that can destroy work irrecoverably.

## Building

```sh
./build.sh              # build ./codemcp for this machine
./build.sh --test       # gofmt, go vet, go test
./build.sh --all        # cross-compile every release target into ./dist
```

```bat
build.cmd
build.cmd --test
build.cmd --all
```

Both scripts stamp the version from `git describe` into the binary, so `codemcp --version` reports the release it was built from - the tag itself on a release commit, or `0.1.0042-3-gabc1234` a few commits past one.
Or use the toolchain directly:

```sh
go build ./...
go test ./...
```

`.github/workflows/release.yml` runs gofmt, vet and the tests, cross-compiles for Windows, Linux and macOS on amd64 and arm64, builds `.deb` and `.rpm` packages with nfpm, and publishes a rolling release with a `SHA256SUMS` file.
A `version` job computes `0.1.NNNN` once from the run number, and the build, the packages and the release tag all take it from there.
