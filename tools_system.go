package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// systemInfo is what system_info reports. It exists so a model does not have to
// infer the platform from path separators in error messages: whether to write
// PowerShell or sh, backslashes or forward slashes, and which of git, gh and go
// are actually installed are all things it would otherwise guess at.
type systemInfo struct {
	OS                   string        `json:"os"`
	OSName               string        `json:"os_name"`
	OSVersion            string        `json:"os_version,omitempty"`
	Arch                 string        `json:"arch"`
	Hostname             string        `json:"hostname,omitempty"`
	CPUs                 int           `json:"cpus"`
	Shell                string        `json:"shell"`
	ShellFlavor          string        `json:"shell_flavor"`
	PathSeparator        string        `json:"path_separator"`
	LineEnding           string        `json:"line_ending"`
	WorkspaceLineEndings string        `json:"workspace_line_endings"`
	WorkspaceRoot        string        `json:"workspace_root"`
	WorkspaceScope       string        `json:"workspace_scope"`
	GoVersion            string        `json:"go_version"`
	Programs             []string      `json:"programs,omitempty"`
	Missing              []string      `json:"missing,omitempty"`
	Privileges           privilegeInfo `json:"privileges"`
}

// osDisplayName is the human name of a GOOS value, since "darwin" and "linux"
// are not what anyone calls the thing.
func osDisplayName(goos string) string {
	switch goos {
	case "windows":
		return "Windows"
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	case "freebsd":
		return "FreeBSD"
	case "openbsd":
		return "OpenBSD"
	case "netbsd":
		return "NetBSD"
	case "dragonfly":
		return "DragonFly BSD"
	case "solaris", "illumos":
		return "Solaris"
	case "aix":
		return "AIX"
	case "android":
		return "Android"
	case "ios":
		return "iOS"
	case "plan9":
		return "Plan 9"
	case "js", "wasip1":
		return "WebAssembly"
	default:
		return goos
	}
}

// shellCommandLine describes the shell that run_command and the project
// commands are executed through, which is the practical thing a model needs:
// the syntax it may use in a command line.
func shellCommandLine() (path, flavor string) {
	if runtime.GOOS == "windows" {
		shell := os.Getenv("COMSPEC")
		if shell == "" {
			shell = "cmd.exe"
		}
		return shell, "cmd"
	}
	return "/bin/sh", "sh"
}

// osVersion is a best-effort release string. Every branch is allowed to fail:
// the platform is already known from GOOS, and the version is a nicety.
func osVersion(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	switch runtime.GOOS {
	case "windows":
		// cmd's own builtin, so there is nothing extra to install.
		out, err := exec.CommandContext(ctx, os.Getenv("COMSPEC"), "/c", "ver").Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	case "darwin":
		out, err := exec.CommandContext(ctx, "sw_vers", "-productVersion").Output()
		if err != nil {
			return unameRelease(ctx)
		}
		return "macOS " + strings.TrimSpace(string(out))
	default:
		// A Linux distribution names itself in os-release; fall back to the
		// kernel version when there is no such file.
		if name := prettyNameFromOSRelease(); name != "" {
			return name
		}
		return unameRelease(ctx)
	}
}

func unameRelease(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "uname", "-sr").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// prettyNameFromOSRelease reads PRETTY_NAME out of /etc/os-release.
func prettyNameFromOSRelease() string {
	for _, path := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			name, value, found := strings.Cut(strings.TrimSpace(line), "=")
			if !found || name != "PRETTY_NAME" {
				continue
			}
			return strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return ""
}

// registerSystemTools adds the tool that describes the machine this server is
// running on.
func (s *Server) registerSystemTools(cfg Config) {
	// The programs worth reporting on are the ones the other tools shell out
	// to, plus the toolchain of whatever is being built here.
	candidates := []string{cfg.Git.GitPath, cfg.GitHub.GhPath, "go", "make", "docker", "node", "python3", "python"}

	s.RegisterTool(Tool{
		Name:  "system_info",
		Title: "Operating system and environment",
		Description: "Report the operating system, architecture and shell this server is running on, " +
			"and which common developer programs are installed. Call this before writing any shell " +
			"command or path, so the syntax matches the platform rather than being guessed at. " +
			"Inferring the platform from a stray backslash in an error message, or assuming a Unix shell " +
			"because the project looks like one, is the usual way a run_command call is wasted. " +
			"The report also states which account the server runs as, including whether that is root.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema(nil, map[string]any{
			"check_programs": propDefault("boolean",
				"Also probe for git, gh, go and other common programs. Costs a few subprocesses.", true),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			CheckPrograms *bool `json:"check_programs"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		shell, flavor := shellCommandLine()
		info := systemInfo{
			OS:                   runtime.GOOS,
			OSName:               osDisplayName(runtime.GOOS),
			OSVersion:            osVersion(ctx),
			Arch:                 runtime.GOARCH,
			CPUs:                 runtime.NumCPU(),
			Shell:                shell,
			ShellFlavor:          flavor,
			PathSeparator:        string(filepath.Separator),
			LineEnding:           lineEndingName(),
			WorkspaceLineEndings: workspaceLineEndingName(s.ws),
			WorkspaceRoot:        s.ws.Root,
			WorkspaceScope:       workspaceScopeName(s.ws),
			GoVersion:            runtime.Version(),
			Privileges:           currentPrivileges(),
		}
		if hostname, err := os.Hostname(); err == nil {
			info.Hostname = hostname
		}
		if args.CheckPrograms == nil || *args.CheckPrograms {
			info.Programs, info.Missing = findPrograms(candidates)
		}
		return &CallToolResult{
			Content:           textContent(info.summarize()),
			StructuredContent: info,
		}, nil
	})
}

// workspaceScopeName names the containment rule in one word, since a model
// reading the report needs to know whether a path outside the root is an error
// or simply another file.
func workspaceScopeName(ws *Workspace) string {
	if ws.Unrestricted {
		return "unrestricted: the whole filesystem is reachable"
	}
	return "contained: paths outside the root are refused or anchored inside it"
}

func lineEndingName() string {
	if runtime.GOOS == "windows" {
		return "CRLF"
	}
	return "LF"
}

// workspaceLineEndingName says what the workspace does with line endings: the
// convention it writes, or that it leaves each file as it found it. This is a
// separate fact from the platform's own convention, which is what a command run
// through run_command produces.
func workspaceLineEndingName(w *Workspace) string {
	switch w.LineEndings {
	case "lf":
		return "LF (normalized on read and write)"
	case "crlf":
		return "CRLF (normalized on read and write)"
	}
	return "preserved per file"
}

// findPrograms splits the candidates into those on PATH and those that are not,
// keeping the order given and skipping duplicates.
func findPrograms(candidates []string) (found, missing []string) {
	seen := map[string]bool{}
	for _, name := range candidates {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if path, err := exec.LookPath(name); err == nil {
			found = append(found, name+" ("+path+")")
		} else {
			missing = append(missing, name)
		}
	}
	return found, missing
}

// summarize renders the information as the text block of the tool result.
func (i systemInfo) summarize() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s", i.OSName)
	if i.OSVersion != "" {
		fmt.Fprintf(&b, " - %s", i.OSVersion)
	}
	fmt.Fprintf(&b, "\n\n")
	fmt.Fprintf(&b, "  GOOS/GOARCH     %s/%s\n", i.OS, i.Arch)
	if i.Hostname != "" {
		fmt.Fprintf(&b, "  hostname        %s\n", i.Hostname)
	}
	fmt.Fprintf(&b, "  cpus            %d\n", i.CPUs)
	fmt.Fprintf(&b, "  shell           %s (%s syntax)\n", i.Shell, i.ShellFlavor)
	fmt.Fprintf(&b, "  path separator  %s\n", i.PathSeparator)
	fmt.Fprintf(&b, "  workspace eol   %s\n", i.WorkspaceLineEndings)
	fmt.Fprintf(&b, "  line endings    %s\n", i.LineEnding)
	fmt.Fprintf(&b, "  workspace       %s\n", i.WorkspaceRoot)
	fmt.Fprintf(&b, "  go              %s\n", i.GoVersion)
	if len(i.Programs) > 0 {
		fmt.Fprintf(&b, "\nInstalled: %s\n", strings.Join(i.Programs, ", "))
	}
	if len(i.Missing) > 0 {
		fmt.Fprintf(&b, "Not on PATH: %s\n", strings.Join(i.Missing, ", "))
	}
	return b.String()
}
