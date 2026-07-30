package main

import (
	"context"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The workspace is exposed as resources as well as tools, so a client can
// browse the project without spending a tool call on it.

func (s *Server) listResources(ctx context.Context) (any, *RPCError) {
	var resources []Resource
	count := 0
	ws := s.workspace()
	err := ws.Walk(ws.Root, func(path string, d fs.DirEntry) bool {
		info, statErr := d.Info()
		if statErr != nil || info.Size() > ws.MaxFileBytes {
			return true
		}
		rel := ws.Rel(path)
		resources = append(resources, Resource{
			URI:      fileURI(path),
			Name:     rel,
			MimeType: mimeTypeFor(rel),
		})
		count++
		return count < ws.MaxResults
	})
	if err != nil {
		return nil, Errorf(CodeInternalError, "could not list the workspace: %v", err)
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].Name < resources[j].Name })
	return &ListResourcesResult{
		Result:    s.completeResult(ctx).cacheable(60000, CacheScopePrivate),
		Resources: resources,
	}, nil
}

func (s *Server) listResourceTemplates(ctx context.Context) any {
	return &ListResourceTemplatesResult{
		Result: s.completeResult(ctx).cacheable(3600000, CacheScopePrivate),
		ResourceTemplates: []ResourceTemplate{{
			URITemplate: "workspace:///{path}",
			Name:        "Workspace file",
			Description: "Any file under the workspace root, addressed by its relative path.",
		}},
	}
}

func (s *Server) readResource(ctx context.Context, req *Request) (any, *RPCError) {
	var params struct {
		URI string `json:"uri"`
	}
	if err := req.Bind(&params); err != nil {
		return nil, err
	}
	rel, err := s.uriToPath(params.URI)
	if err != nil {
		// The specification moved resource-not-found onto -32602 in this revision.
		return nil, Errorf(CodeInvalidParams, "%v", err)
	}
	content, readErr := s.workspace().ReadFile(rel)
	if readErr != nil {
		return nil, Errorf(CodeInvalidParams, "%v", readErr)
	}
	return &ReadResourceResult{
		Result: s.completeResult(ctx).cacheable(10000, CacheScopePrivate),
		Contents: []ResourceContents{{
			URI:      params.URI,
			MimeType: mimeTypeFor(rel),
			Text:     content,
		}},
	}, nil
}

// uriToPath accepts both file:// URIs and the workspace:///path form from the
// resource template, and returns a workspace-relative path.
func (s *Server) uriToPath(uri string) (string, error) {
	if uri == "" {
		return "", errString("uri is required")
	}
	u, err := url.Parse(uri)
	if err != nil {
		return "", errString("unreadable uri: " + uri)
	}
	switch u.Scheme {
	case "workspace":
		return strings.TrimPrefix(u.Path, "/"), nil
	case "file":
		p := u.Path
		// Windows file URIs come through as /C:/dir/file.
		if len(p) > 2 && p[0] == '/' && p[2] == ':' {
			p = p[1:]
		}
		return filepath.FromSlash(p), nil
	case "":
		return uri, nil
	}
	return "", errString("unsupported uri scheme: " + u.Scheme)
}

func fileURI(abs string) string {
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}

func mimeTypeFor(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go":
		return "text/x-go"
	case ".json":
		return "application/json"
	case ".yaml", ".yml":
		return "application/yaml"
	case ".md":
		return "text/markdown"
	case ".sql":
		return "application/sql"
	case ".html":
		return "text/html"
	case ".js", ".ts":
		return "text/javascript"
	case ".sh", ".bash":
		return "application/x-sh"
	default:
		return "text/plain"
	}
}

func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

type errString string

func (e errString) Error() string { return string(e) }
