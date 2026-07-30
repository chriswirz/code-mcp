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
  { "name": "lint",  "description": "Vet and lint.", "command": "go vet ./...", "read_only": true },
  { "name": "serve", "description": "Run the dev server.", "command": "npm run dev", "background": true }
]
```

Each entry becomes an MCP tool of the same name, listed with its description and the exact command line it runs.
`project_commands` returns the whole set in one call.

With `accepts_args`, the caller's arguments are appended to the command line.
A `{{args}}` placeholder puts them somewhere else instead, which is what a command like `go test` needs: appending `-run TestFoo ./pkg/...` to `go test ./...` produces a command line with two package lists, whereas `go test {{args}}` produces the right one.
`default_args` is used when the caller passes none, so the bare tool still runs the whole suite.

With `background`, the command starts through the process registry instead of blocking: the tool returns a process id immediately, and the output is read back with `check_process_id`, listed by `list_processes` and ended with `stop_process` - the same workflow as `run_process`.
Use it for dev servers, watchers and anything else that outlives a single request.
Such a tool also takes `wait_seconds`, which returns a fast-finishing process complete in one call.
`timeout_seconds` still applies, killing the process however far it has got.

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

### Updating

However it was installed, an installed binary can replace itself:

```sh
codemcp --update
```

It asks GitHub for the latest release, downloads the asset for this platform, checks it against that release's `SHA256SUMS`, and only then installs it - a download that fails its checksum is discarded and the running binary is left alone.
`--update-version 0.1.0007` pins a particular release instead, which is also how you go back to an earlier one.

The binary is replaced where it actually lives, following a symlink to its target, so a system-wide install needs the privileges that directory does: `sudo codemcp --update` for `/usr/local/bin`, or an elevated shell on Windows.
A server already running keeps running the old image until it is restarted.
Installs that came from the `.deb` or `.rpm` are better updated through the package manager, so the package database stays in step with what is on disk.

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

JSON has no comments, so `config.json` tolerates two kinds of annotation: any key whose name starts with an underscore is ignored wherever it appears, and an unknown top-level section is ignored too.
An unknown key *inside* a known section is still an error, since there it is nearly always a misspelt setting.

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

**Long-running processes** - `run_process`, `check_process_id`, `list_processes`, `stop_process`.
`run_command` has to answer inside the request it arrived on, so anything slower than the client's HTTP timeout is lost halfway through with nothing to show for it - which is exactly what a full build, a migration, a soak test or a dev server tends to be.
`run_process` starts the same kind of command line and returns immediately with a process id:

```
process proc-1: go test ./...
running for 41ms (pid 51204)
Still running. Call check_process_id with process_id "proc-1" for its output.
```

`check_process_id` then says whether it is still running and hands back the stdout and stderr collected so far, so polling a build shows its progress rather than an empty result until the end.
It is safe to call as often as you like; `tail_lines` returns only the last lines of each stream, and `wait_seconds` waits for the process to finish first, up to a bound you choose.
`run_process` takes `wait_seconds` too, so a command that turns out to be quick comes back complete in one call and only a slow one needs a second.

Output is collected continuously into a 1 MB per-stream buffer that keeps the most recent bytes; what fell off the front is counted rather than silently dropped.
A process is killed after `timeout_seconds` (an hour by default) however far it has got, and `stop_process` ends one early - on Linux and macOS the whole process group goes with it, and on Windows the tree is walked with `taskkill`, so a shell line that started a server does not leave the server behind.
Exit status, output and all stay readable through `check_process_id` afterwards.
`list_processes` shows every process of the session, running and finished, without their output.

Thirty-two processes are tracked at once; past that the oldest *finished* one is forgotten, and a running one is never dropped to make room.
Processes survive a config reload, and are killed when the server exits rather than orphaned. sudo works exactly as it does for `run_command`.

**Environment** - `system_info` reports the operating system, version, architecture, hostname, CPU count, the shell that `run_command` and the project commands actually execute through, the path separator and line ending, and which of git, gh, go, make, docker, node and python are on `PATH`.
Worth calling before writing any shell command or path, so the syntax matches the platform instead of being inferred from stray backslashes in an error message.
`run_command` states the essentials in its own description, so this is the fuller picture rather than the first thing to reach for.
Pass `check_programs: false` to skip the `PATH` probe.

**Code**, which answers the question grep answers badly - `find_method_definition`, `find_class_definition`.

**Files**, all confined to the workspace root (symlinks are resolved before the containment check; a rooted path such as `/README.md` is anchored inside the workspace rather than refused, so it means the README at the top of the project, not one at the root of the filesystem) - `read_file`, `write_file`, `edit_file`, `multi_edit`, `apply_diff`, `format_markdown`, `list_directory`, `grep_files`, `search_files`, `find_files`.

A path that is not there is answered with the paths that are.
`read_file`, `edit_file`, `multi_edit`, `apply_diff`, `format_markdown` and `fix_line_endings` all say `the path parameter was specified incorrectly: "internal/handler.go" does not exist in the workspace` and then offer the nearest files - a name in the wrong directory, a directory with the wrong name, a transposition, a difference of case:

```
ERROR: the path parameter was specified incorrectly: "internal/handler.go" does not exist in
the workspace. Did you mean one of these?
  internal/server/handler.go
  internal/server/handler_test.go
Paths are relative to the workspace root.
```

When the path is not there but its **file name matches exactly one file** in the workspace, the error names that file outright rather than burying it in a list, because it is almost certainly the one that was meant:

```
ERROR: the path parameter was specified incorrectly: "handler.go" does not exist in the
workspace. internal/server/handler.go is the only file with that name; call it by that path.
(workspace.exact_path_not_required makes a unique name enough on its own.)
```

Setting **`workspace.exact_path_not_required`** to `true` goes one step further and performs the operation on that file, since the caller had the name right and the directory wrong, there is nothing else it could have meant, and refusing would only produce the same call again with the path corrected.
The answer says so before its usual output:

```
Note: "internal/handler.go" does not exist. internal/server/handler.go is the only file named
"handler.go" in the workspace, so that is the file this acted on. Use that path next time.

Replaced 1 occurrence(s) in internal/server/handler.go
```

It is off by default: a correction that ends in a write is a decision an operator should make deliberately, and the error names the file either way.
Two files of that name make it ambiguous, and then nothing is touched whatever the setting says: the answer is the error and its suggestions.
A name that is only nearly right - `hadnler.go` - is not corrected either, since that would be a guess about intent landing as a write; it is suggested instead.
A path that does resolve is used exactly as given, whatever else shares its name.
`apply_diff` corrects a section's path the same way, except on a rename, where the new path is the caller's own choice and moving it would be a second guess.

`workspace.path_suggestions` sets how many suggestions are offered, defaulting to **5**; `0` switches them off and leaves the error itself.
Candidates are ranked on the file name first and the directory second, since a path that is wrong usually has one of the two right.
Nothing is offered when nothing is close, when the workspace is unrestricted (the search space would be the whole machine), or in a batch - where the answer names the entry instead, `edits[1]: the path parameter was specified incorrectly...`.

Recursive walks (`list_directory` with `recursive`, plus `grep_files`, `search_files` and `find_files`) skip directories named in `workspace.exclude` (defaulting to `.git`, `node_modules`, `vendor`, `dist`, `out`, `target` and `.venv`).
Pass one of those paths explicitly as the tool's `path` when you need to look inside it; nested copies of the same names are still skipped.

`find_method_definition` and `find_class_definition` answer "where is this defined?" directly.
A grep for the name returns every call site as well, one line at a time; these return the definition itself, whole:

```json
{
  "path": "internal/server/handler.go",
  "language": "go",
  "name": "Serve",
  "kind": "method",
  "declaration": "func (h *Handler) Serve(req string) error {",
  "start_line": 41,
  "definition_line": 44,
  "end_line": 58,
  "total_lines": 18,
  "has_body": true,
  "body": "// Serve handles one request.
..."
}
```

The answer is always a list, because one name defined in several places is the normal case: an interface and its implementations, a method on two types, the same helper in two packages.

- **`start_line` includes the doc comment** above the definition, along with any annotation, attribute or decorator attached to it, since that is what a reader of the definition needs.
  `definition_line` is the declaration itself, for pointing at rather than reading.
  `end_line` closes the body, and `total_lines` counts what is returned.
- **Declarations without a body are marked** and sorted last: a C prototype, an interface method, an abstract member.
  They are still returned - knowing a method is declared in a header is worth something - but the definition comes first.
- **Languages**: Go, C, C++, C#, Java, JavaScript, TypeScript, Rust, Python, Ruby, PHP, Swift, Kotlin, Scala, taken from each file's extension.
  `language` narrows the search; `path` narrows it to a directory or one file.
- **Comments and strings are masked before matching**, so a name mentioned in a comment or inside a string literal is not a definition, and a call site is not one either.
  The body's extent comes from the language: matched braces, Python's indentation, Ruby's `end`.
- **`include_body: false`** returns locations alone; `max_body_lines` (400 by default) caps a long one and says it did.
  `max_results` defaults to 20.

This is a lexical matcher, not a compiler: it reads the declaration and the block under it the way a careful person does, and it is wrong in the ways that approach is wrong - a name a macro assembles, a body whose braces live inside a construct it does not model.
Every result carries its file and line range, so a doubtful one costs one `read_file` to check, and `grep_files` is still there for what this cannot see.

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
  When the context is in the file but out of reach, the error says where it is instead: past `max_offset`, or behind a hunk that has already been applied, which is what a duplicated hunk looks like.
  `ignore_whitespace` relaxes indentation while keeping the file's own.
- **The hunk headers need not add up.** The counts after the `@@` are the part of a hand-written diff most often wrong, so the body is read to the start of the next section and trusted over them.
  The result carries a note saying which header was wrong, since applying it is not the same as it being right.
- **Hunks may be in any order, and a file may appear in more than one section.** Hunks are sorted before they are applied, and sections against the same file apply one after another rather than the last one silently winning.
  Deleting a file and recreating it in one patch works for the same reason.
- **The `---`/`+++` pair is optional when a `diff --git` line names the paths**, and `new file mode` or `deleted file mode` with no hunk at all creates or removes an empty file.
- **Context lines need their leading space.** A line without one cannot be told from a sentence of prose, so the patch is refused and the error says exactly that.
  Prose *after* a complete hunk is ignored, so an explanation appended to a patch does no harm.
- **All or nothing.** Every file is computed and checked before anything is written, so a patch that fails on its third file does not leave the first two applied.
  `dry_run` checks without writing.

A call this server will not serve comes back as a tool error the model can read, never as a JSON-RPC error: a protocol error reads to a model as the transport failing, and the usual reaction is to send the same call again.
A tool switched off by the configuration - `git_push` under `git.allow_push`, the database tools with no database, a project command that runs git - names the setting that governs it, says that retrying will not help, and points at what to do instead.
A name that never existed gets the closest tools this server does have, and a pointer to `tools/list`.

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

With `git.enabled` set to false the git tools are not registered, and git is blocked at the shell as well: `run_command`, `run_process` and any configured command whose line runs git are refused, wrappers and nested shells included, so `sudo git push` and `bash -c "git reset --hard"` are no way around the setting.
A configured command that runs git is not registered at all while git is off, and the server logs which one it skipped; calling it anyway answers with the reason rather than an unknown-tool error.
Only the program each command actually runs is inspected, so `grep git README.md` and a path like `.git` stay callable.

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
The rest of the section is described under [Downloads](#downloads).

**Line endings.** `workspace.line_endings` decides whether the workspace speaks one convention or leaves every file as it found it.
The default, `"preserve"`, changes nothing: each file keeps the endings it has, and an edit whose anchor does not match is retried against the file's own convention (`normalize_line_endings`, on by default per call).

Set it to `"lf"`, `"crlf"` or `"native"` (this machine's) and the workspace normalizes instead:

- every read hands back LF, whatever is on disk - including a file with mixed endings, or a lone ` `;
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

All three keys are optional, and leaving the section out entirely is a supported choice: nothing answers the prompt, and a command that asks for one hangs until its timeout.
Editing the section is picked up by the next request, so a rotated password needs no restart.

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

## OpenAPI tool server

The same tools are served as a plain REST API with an OpenAPI description, for clients that speak that rather than MCP - [Open WebUI's tool servers](https://github.com/open-webui/openapi-servers) being the convention this follows.
It rides on the same listener as the MCP endpoint, under `/api` by default:

```
  Connect your MCP client to:  http://127.0.0.1:8765/mcp
  OpenAPI tool server: http://127.0.0.1:8765/api/openapi.json
```

Point Open WebUI at `http://127.0.0.1:8765/api` (Settings → Tools → add a server) and every tool this server has becomes a tool the model can call.
`GET /api` lists them, `GET /api/openapi.json` is the specification, and each tool is a `POST /api/<tool name>` whose JSON body is the tool's arguments:

```sh
curl -sS -X POST http://127.0.0.1:8765/api/read_file   -H "Content-Type: application/json"   -d '{"path": "README.md", "start_line": 1, "end_line": 3}'
```
```json
{"result": "# codemcp

![appicon.png](appicon.png)"}
```

- **It is the same tools, not a second implementation.** A request is decoded, handed to the handler the MCP endpoint calls, and encoded back, so the two faces cannot drift apart.
  A project command is an endpoint like anything else.
- **The specification is generated from the tools themselves.** OpenAPI 3.1 takes JSON Schema as it is, so each tool's own input schema is carried across rather than translated, and every operation carries the `operationId` Open WebUI requires - the tool's own name, which is what the model is given.
- **Answers have one shape**: `result` (the text a model reads), `data` (structured output, when the tool produced any) and `is_error`.
  A tool that refuses answers **200** with `is_error` set, not a 4xx: the call reached the tool and the tool answered, and a model reads a transport error as the connection failing rather than as something it can act on.
  The guidance a refusal carries - a wrong path and its suggestions, a switched-off tool and the setting that governs it - arrives intact.
- **`server.auth_token` applies here too**, as `bearerAuth` in the specification: a second door into the same tools must not be an easier one.
  Without a token the spec declares no security scheme rather than claiming one it does not enforce.
- **The browser origin must be allowed.** Open WebUI calls the tool server from the page, so its origin has to be in `server.allowed_origins` (`["*"]` for anywhere) or the preflight is refused - the origin check is this server's DNS-rebinding defence and the REST face does not get to skip it.
- **`servers[0].url` is the address the request arrived on**, `X-Forwarded-Proto`/`X-Forwarded-Host` included, so the specification is correct behind a tunnel or a reverse proxy without being told about either.
  `openapi.public_url` overrides it when the answer still needs to be something else.
- **`include`/`exclude` decide what is exposed**, by tool name; `exclude` wins.
  A tool left out is not callable either, rather than merely undocumented.
- It needs the HTTP transport, since it is served over HTTP.
  On stdio the setting is reported and otherwise ignored.

Set `"enabled": false` to turn the whole thing off, or `"path"` to mount it somewhere other than `/api`.
Those two decide the routes and so need a restart, which a reload says; the rest of the section - `include`, `exclude`, the title and the public URL - is re-read on every request.

## Reporting a problem with the server

`report_issue` opens an issue on this server's own repository, for when a tool here is broken, misleading or missing.
It posts straight to the GitHub API rather than through the `gh` CLI, so it works on a machine where `gh` is not installed or is signed in to somebody else's account, and it never touches the repository being worked on.

```json
{
  "issues": {
    "enabled": true,
    "repo": "chriswirz/code-mcp",
    "token": "",
    "token_env": "CODEMCP_ISSUE_TOKEN",
    "token_file": "",
    "labels": []
  }
}
```

The token is a GitHub fine-grained personal access token with **Issues: read and write** on that one repository, and nothing else.
There are three places to put it, and it is read from the first that has one: `token_file` (a file whose first line is the token), then `token_env` (an environment variable), then `token` (the literal, straight in `config.json`).

```sh
export CODEMCP_ISSUE_TOKEN="github_pat_..."
```

`token` works and is the least trouble to set up, so it is worth being clear about what you are choosing: a credential in `config.json` is a credential inside the workspace, which this server's own `read_file` and `grep_files` can reach - so the model on the other end of the connection can read it back out and put it in a transcript.
The environment and `token_file` (with the file outside the workspace, or ignored by git) keep it out of that reach.
Note also that a set `token_env` variable wins over `token`, which is how a stale literal ends up looking like it is being used when it is not.

There is a `token` field too, and using it is a mistake worth naming: `config.json` lives inside the workspace, where this server's own `read_file` and `grep_files` can reach it, so the model on the other end of the connection can read the credential back out and put it in a transcript.
Whether git tracks the file is a second question - this repository ignores it - and a token that does reach a public repository is one GitHub's secret scanning revokes and a stranger can use first.
`.issue-token` and `*.token` are in `.gitignore` for the file route.
`token_env` and `token_file` name *where the token lives*, not the token: putting the credential in one of them finds nothing, the tool quietly goes missing, and the secret ends up in a tracked file - so the server refuses to start and says so.

- **Nothing is posted without the call succeeding first.** Every refusal from GitHub comes back as a sentence saying what to change: an expired token, a token without `Issues: read and write`, a repository the token is not scoped to, issues turned off on the repository, or a label it does not define.
  The token never appears in an error, however GitHub phrased it.
- **`dry_run` shows the exact issue first.** An issue is public and cannot be quietly withdrawn, so the tool description tells the model to confirm the wording with its user before posting.
- **The appended diagnostics are deliberately thin**: server and protocol version, `GOOS/GOARCH`, Go version, how many tools are registered, and the workspace's line-ending and write settings.
  No paths, no repository names, no hosts - the report is going somewhere public.
- **With no token configured the tool is not registered**, and calling it says which environment variable to set and offers the manual `issues/new` link rather than coming back as an unknown tool.

## Downloads

Some results are not text: a build artifact, a heap profile, a screenshot, a 40 MB log the model has no business reading into its context.
The download tools hand those to a person as a URL instead.
The section is on by default and looks like this in full:

```json
"downloads": {
  "enabled": true,
  "default_ttl_minutes": 5,
  "max_ttl_minutes": 60,
  "max_file_bytes": 536870912,
  "max_links": 64,
  "base_url": "",
  "addr": "127.0.0.1:0"
}
```

`default_ttl_minutes` is how long a link lives when `get_download_link` is called without `minutes`, and `max_ttl_minutes` is the longest any call may ask for - a request for more is clamped, not refused.
Keep both short.
A link is a bearer credential in a URL, so its lifetime is the whole of its security: anyone who has it, or who finds it in a chat log or a browser history, is the owner of that file until it expires.

`max_file_bytes` (512 MB by default, `0` for no limit) bounds what may be published, and is deliberately separate from `workspace.max_file_bytes`.
That one bounds what the model may read into its context and is measured in kilobytes; this one bounds what a browser may fetch and is measured in hundreds of megabytes.
Publishing a large file costs nothing in context - the model gets back a URL and a size, never the bytes.

`max_links` (64) bounds how many links are live at once; past it the link closest to expiring is dropped to make room, so a long session cannot accumulate an unbounded set of live URLs.

`base_url` and `addr` are about where the link points.
By default links are built from the MCP listener's own URL and served under the endpoint path, which is right whenever the client reaches this process directly.
Behind a reverse proxy it is not: set `base_url` to the prefix a client actually reaches, up to and including the files segment (`https://example.com/mcp/files`), and that replaces the host and path the server would have guessed.
On stdio there is no MCP listener to share, so the first link opens a listener of its own on `addr`; port `0` picks a free one.

`"enabled": false` unregisters `get_download_link`, `list_download_links` and `revoke_download_link` entirely, which is the right setting on a server whose listener is public and whose files are not.

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
- Sessions are minted on `initialize`, echoed on every response, terminated by `DELETE`, and expire after `server.session_timeout_seconds` (default two hours) of **idleness** - the clock restarts on every request, so an active conversation never ages out.
- A request against a session this process does not know gets **404**, which is what the specification requires; the client is then required to start a new session by sending a fresh `initialize` without a session id.
  Sessions live in memory, so a restart forgets all of them.
- **`server.session_recovery`** (off unless set) adopts an unknown session id instead of refusing it.
  It is for the client that does not do its half of the exchange above and leaves its user with a dead conversation and a "session terminated" error - which is what a restarted server, or an overnight idle, produces with such a client.
  A legacy session here holds an id, a version, the client's name and two timestamps: no workspace position, no open handles, no authorisation, since that is the bearer token's job and is checked on every request regardless.
  Adopting the id therefore restores everything the session held, and each adoption is logged.
- **A session the client ended with `DELETE` stays ended**, whatever `session_recovery` says.
  Forgetting a session is this server's failing to make good; ending one is the client's decision.
  The id is remembered as terminated and answers 404 saying so.
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

The scheme is matched case-insensitively, as HTTP requires; the token itself is matched exactly, and in constant time, so neither its case nor how long the comparison took gives anything away.

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
- A path whose file does not exist yet is contained by where its deepest existing directory really is, links followed, so a linked directory inside the workspace is not a way to create a file outside it.
  On Windows that includes directory junctions, which `filepath.EvalSymlinks` does not follow and which an ordinary account can create without the privilege a symlink needs.
- A panic inside one tool is turned into an error result for that call rather than being allowed to take the process down.
- `git.enabled` set to false removes the git tools and blocks git at the shell too, so the setting cannot be sidestepped with `run_command`.
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
