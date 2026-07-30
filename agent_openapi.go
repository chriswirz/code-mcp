package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// openAPIClient turns an external REST API's specification into tools: one per
// operation, its path, query and header parameters as properties, and a JSON
// request body as a "body" property.
type openAPIClient struct {
	cfg     AgentOpenAPIConfig
	http    *http.Client
	baseURL string
	title   string
	ops     []*apiOperation
}

type apiOperation struct {
	name        string
	method      string
	path        string
	description string
	params      []apiParam
	hasBody     bool
	schema      map[string]any
}

type apiParam struct {
	Name string
	In   string
}

func newOpenAPIClient(cfg AgentOpenAPIConfig) *openAPIClient {
	return &openAPIClient{cfg: cfg, http: newAgentHTTPClient(cfg.TimeoutSeconds, cfg.InsecureSkipVerify, 2*time.Minute)}
}

func (c *openAPIClient) target() string { return c.cfg.SpecURL }

func (c *openAPIClient) authorize(req *http.Request) {
	c.cfg.Auth.apply(req, "")
	applyHeaders(req, c.cfg.Headers)
}

// connect fetches and parses the specification.
func (c *openAPIClient) connect(ctx context.Context) error {
	data, specURL, err := c.fetchSpec(ctx)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}), &doc); err != nil {
		return fmt.Errorf("the specification is not JSON (only JSON specifications are supported): %v", err)
	}
	if _, ok := doc["paths"].(map[string]any); !ok {
		return fmt.Errorf("the document has no paths; is %s an OpenAPI specification?", c.cfg.SpecURL)
	}
	if info, ok := doc["info"].(map[string]any); ok {
		title, _ := info["title"].(string)
		ver, _ := info["version"].(string)
		c.title = strings.TrimSpace(title + " " + ver)
	}
	c.baseURL = c.resolveBase(doc, specURL)
	c.ops = parseOperations(doc, c.cfg.Include, c.cfg.Exclude)
	if len(c.ops) == 0 {
		return fmt.Errorf("the specification defines no operations")
	}
	// A reachable spec says nothing about whether the credentials work. Probe
	// the API root, and report an authorization refusal rather than hide it.
	if c.baseURL != "" {
		pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if req, err := http.NewRequestWithContext(pctx, http.MethodGet, c.baseURL, nil); err == nil {
			c.authorize(req)
			if resp, err := c.http.Do(req); err == nil {
				resp.Body.Close()
				if resp.StatusCode == 401 || resp.StatusCode == 403 {
					return fmt.Errorf("specification loaded, but %s refused the credentials (HTTP %d)", c.baseURL, resp.StatusCode)
				}
			}
		}
	}
	return nil
}

func (c *openAPIClient) fetchSpec(ctx context.Context) ([]byte, *url.URL, error) {
	raw := c.cfg.SpecURL
	if !strings.Contains(raw, "://") {
		if data, err := os.ReadFile(raw); err == nil {
			return data, nil, nil
		}
		normalized, err := normalizeEndpointURL(raw)
		if err != nil {
			return nil, nil, err
		}
		raw = normalized
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, nil, fmt.Errorf("HTTP %d fetching the specification: authorization refused", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, nil, &httpStatusError{Status: resp.StatusCode, Body: string(data)}
	}
	return data, u, nil
}

// resolveBase picks the API's base URL: the configured override, the
// specification's first server (relative ones resolved against the spec URL),
// Swagger 2.0's host and basePath, or the spec's own origin.
func (c *openAPIClient) resolveBase(doc map[string]any, specURL *url.URL) string {
	if c.cfg.BaseURL != "" {
		if u, err := normalizeEndpointURL(c.cfg.BaseURL); err == nil {
			return u
		}
	}
	if servers, ok := doc["servers"].([]any); ok && len(servers) > 0 {
		if first, ok := servers[0].(map[string]any); ok {
			if s, ok := first["url"].(string); ok && s != "" {
				s = substituteServerVariables(s, first)
				if ref, err := url.Parse(s); err == nil {
					if specURL != nil {
						return strings.TrimRight(specURL.ResolveReference(ref).String(), "/")
					}
					return strings.TrimRight(ref.String(), "/")
				}
			}
		}
	}
	if host, ok := doc["host"].(string); ok && host != "" {
		scheme := "https"
		if schemes, ok := doc["schemes"].([]any); ok && len(schemes) > 0 {
			scheme, _ = schemes[0].(string)
		}
		basePath, _ := doc["basePath"].(string)
		return strings.TrimRight(scheme+"://"+host+basePath, "/")
	}
	if specURL != nil {
		base := *specURL
		base.Path, base.RawQuery = "", ""
		if basePath, ok := doc["basePath"].(string); ok {
			base.Path = basePath
		}
		return strings.TrimRight(base.String(), "/")
	}
	return ""
}

func substituteServerVariables(s string, server map[string]any) string {
	vars, _ := server["variables"].(map[string]any)
	for name, v := range vars {
		if def, ok := v.(map[string]any)["default"]; ok {
			s = strings.ReplaceAll(s, "{"+name+"}", fmt.Sprint(def))
		}
	}
	return s
}

var nonToolChars = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func parseOperations(doc map[string]any, include, exclude []string) []*apiOperation {
	paths, _ := doc["paths"].(map[string]any)
	var keys []string
	for k := range paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var ops []*apiOperation
	for _, path := range keys {
		item, _ := paths[path].(map[string]any)
		shared, _ := item["parameters"].([]any)
		for _, method := range []string{"get", "post", "put", "patch", "delete", "head", "options"} {
			op, ok := item[method].(map[string]any)
			if !ok {
				continue
			}
			name, _ := op["operationId"].(string)
			if name == "" {
				name = method + "_" + strings.Trim(nonToolChars.ReplaceAllString(path, "_"), "_")
			}
			if len(include) > 0 && !slicesContains(include, name) || slicesContains(exclude, name) {
				continue
			}
			summary, _ := op["summary"].(string)
			desc, _ := op["description"].(string)
			ao := &apiOperation{
				name:        name,
				method:      strings.ToUpper(method),
				path:        path,
				description: strings.TrimSpace(fmt.Sprintf("%s %s\n%s\n%s", strings.ToUpper(method), path, summary, desc)),
			}
			props := map[string]any{}
			var required []string
			params := append(append([]any{}, shared...), asList(op["parameters"])...)
			for _, p := range params {
				pm := resolveRef(doc, p, 0)
				pname, _ := pm["name"].(string)
				in, _ := pm["in"].(string)
				if pname == "" || in == "cookie" {
					continue
				}
				if in == "body" { // Swagger 2.0
					ao.hasBody = true
					props["body"] = inlineRefs(doc, pm["schema"], 0)
					if req, _ := pm["required"].(bool); req {
						required = append(required, "body")
					}
					continue
				}
				if in == "formData" {
					continue
				}
				ps, ok := pm["schema"]
				if !ok {
					ps = map[string]any{"type": firstNonEmpty(fmt.Sprint(pm["type"]), "string")}
				}
				schemaMap, _ := inlineRefs(doc, ps, 0).(map[string]any)
				if schemaMap == nil {
					schemaMap = map[string]any{"type": "string"}
				}
				if d, _ := pm["description"].(string); d != "" {
					schemaMap["description"] = fmt.Sprintf("(%s) %s", in, d)
				} else {
					schemaMap["description"] = fmt.Sprintf("(%s parameter)", in)
				}
				props[pname] = schemaMap
				ao.params = append(ao.params, apiParam{Name: pname, In: in})
				if req, _ := pm["required"].(bool); req || in == "path" {
					required = append(required, pname)
				}
			}
			if rb := resolveRef(doc, op["requestBody"], 0); rb != nil {
				if content, ok := rb["content"].(map[string]any); ok {
					var media map[string]any
					for ct, m := range content {
						if strings.Contains(ct, "json") {
							media, _ = m.(map[string]any)
							break
						}
					}
					if media != nil {
						ao.hasBody = true
						body := inlineRefs(doc, media["schema"], 0)
						if body == nil {
							body = map[string]any{"type": "object"}
						}
						if bm, ok := body.(map[string]any); ok {
							bm = copyMap(bm)
							bm["description"] = "JSON request body. " + fmt.Sprint(firstNonEmpty(strOf(bm["description"])))
							body = bm
						}
						props["body"] = body
						if req, _ := rb["required"].(bool); req {
							required = append(required, "body")
						}
					}
				}
			}
			s := map[string]any{"type": "object", "properties": props}
			if len(required) > 0 {
				s["required"] = required
			}
			ao.schema = s
			ops = append(ops, ao)
		}
	}
	return ops
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

func asList(v any) []any {
	list, _ := v.([]any)
	return list
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// resolveRef follows a local $ref to the object it names.
func resolveRef(doc map[string]any, v any, depth int) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil || depth > 16 {
		return m
	}
	ref, ok := m["$ref"].(string)
	if !ok || !strings.HasPrefix(ref, "#/") {
		return m
	}
	var cur any = doc
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[part]
	}
	return resolveRef(doc, cur, depth+1)
}

// inlineRefs copies a schema with its local $refs replaced by what they name,
// since a tool schema has to stand alone. Recursion is cut off at a depth,
// where the schema degrades to a plain object.
func inlineRefs(doc map[string]any, v any, depth int) any {
	if depth > 8 {
		return map[string]any{"type": "object"}
	}
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t["$ref"]; ok {
			resolved := resolveRef(doc, t, 0)
			if resolved == nil {
				return map[string]any{"type": "object"}
			}
			return inlineRefs(doc, resolved, depth+1)
		}
		out := make(map[string]any, len(t))
		for k, val := range t {
			// Keywords tool-calling APIs commonly reject.
			if k == "example" || k == "examples" || k == "xml" || k == "discriminator" || k == "readOnly" || k == "writeOnly" || k == "nullable" || k == "externalDocs" {
				continue
			}
			out[k] = inlineRefs(doc, val, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = inlineRefs(doc, val, depth+1)
		}
		return out
	}
	return v
}

// invoke calls one operation with the model's arguments.
func (c *openAPIClient) invoke(ctx context.Context, op *apiOperation, raw json.RawMessage) (*CallToolResult, error) {
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil || args == nil {
		args = map[string]any{}
	}
	path := op.path
	query := url.Values{}
	headers := map[string]string{}
	for _, p := range op.params {
		v, ok := args[p.Name]
		if !ok || v == nil {
			continue
		}
		value := scalarString(v)
		switch p.In {
		case "path":
			path = strings.ReplaceAll(path, "{"+p.Name+"}", url.PathEscape(value))
		case "query":
			if list, ok := v.([]any); ok {
				for _, item := range list {
					query.Add(p.Name, scalarString(item))
				}
			} else {
				query.Set(p.Name, value)
			}
		case "header":
			headers[p.Name] = value
		}
	}
	if strings.Contains(path, "{") {
		return toolError("missing path parameter in %s", path), nil
	}
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var body io.Reader
	if op.hasBody {
		if b, ok := args["body"]; ok {
			data, _ := json.Marshal(b)
			body = bytes.NewReader(data)
		}
	}
	req, err := http.NewRequestWithContext(ctx, op.method, target, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	text := fmt.Sprintf("HTTP %d %s\n%s", resp.StatusCode, http.StatusText(resp.StatusCode), prettyJSON(data))
	return &CallToolResult{Content: textContent(text), IsError: resp.StatusCode >= 400}, nil
}

func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case float64, bool:
		return fmt.Sprint(t)
	}
	data, _ := json.Marshal(v)
	return string(data)
}

func prettyJSON(data []byte) string {
	var buf bytes.Buffer
	if json.Indent(&buf, data, "", "  ") == nil {
		return buf.String()
	}
	return string(data)
}
