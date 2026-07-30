package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// mcpClient is a connection to one external MCP server. It opens with the
// initialize handshake, which every revision in use still answers, and then
// lists and calls tools. Over HTTP it follows the session id the server issues
// and reads either a JSON body or an event stream; over stdio it launches the
// server and exchanges newline-delimited JSON-RPC.
type mcpClient struct {
	cfg    AgentMCPConfig
	nextID atomic.Int64

	// HTTP
	url      string
	http     *http.Client
	session  string
	protocol string

	// stdio
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	serverName string
	tools      []Tool
}

func newMCPClient(cfg AgentMCPConfig) (*mcpClient, error) {
	c := &mcpClient{cfg: cfg}
	if cfg.URL != "" {
		u, err := normalizeEndpointURL(cfg.URL)
		if err != nil {
			return nil, err
		}
		c.url = u
		c.http = newAgentHTTPClient(cfg.TimeoutSeconds, cfg.InsecureSkipVerify, 5*time.Minute)
	}
	return c, nil
}

func (c *mcpClient) target() string {
	if c.url != "" {
		return c.url
	}
	return strings.TrimSpace(c.cfg.Command + " " + strings.Join(c.cfg.Args, " "))
}

type rpcReply struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

// connect performs the handshake and loads the tool list.
func (c *mcpClient) connect(ctx context.Context) error {
	c.close()
	c.session, c.protocol = "", ""
	if c.url == "" {
		if err := c.startProcess(); err != nil {
			return err
		}
	}
	var init struct {
		ProtocolVersion string         `json:"protocolVersion"`
		ServerInfo      Implementation `json:"serverInfo"`
	}
	err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": LatestLegacyVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": appName + "-agent", "version": version},
	}, &init)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	c.protocol = init.ProtocolVersion
	c.serverName = strings.TrimSpace(init.ServerInfo.Name + " " + init.ServerInfo.Version)
	_ = c.notify(ctx, "notifications/initialized")

	c.tools = nil
	cursor := ""
	for page := 0; page < 50; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var list struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &list); err != nil {
			return fmt.Errorf("tools/list: %w", err)
		}
		c.tools = append(c.tools, list.Tools...)
		if list.NextCursor == "" {
			break
		}
		cursor = list.NextCursor
	}
	return nil
}

func (c *mcpClient) callTool(ctx context.Context, name string, args json.RawMessage) (*CallToolResult, error) {
	var res CallToolResult
	err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": argsObject(args)}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func (c *mcpClient) call(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	var reply *rpcReply
	var err error
	if c.url != "" {
		reply, err = c.postHTTP(ctx, msg, id)
	} else {
		reply, err = c.roundTripStdio(ctx, msg, id)
	}
	if err != nil {
		return err
	}
	if reply.Error != nil {
		return fmt.Errorf("server error %d: %s", reply.Error.Code, reply.Error.Message)
	}
	if out != nil && len(reply.Result) > 0 {
		return json.Unmarshal(reply.Result, out)
	}
	return nil
}

func (c *mcpClient) notify(ctx context.Context, method string) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if c.url != "" {
		_, err := c.postHTTP(ctx, msg, 0)
		return err
	}
	return c.writeStdio(msg)
}

// --- Streamable HTTP ---

func (c *mcpClient) postHTTP(ctx context.Context, msg map[string]any, id int64) (*rpcReply, error) {
	data, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.session != "" {
		req.Header.Set(sessionHeader, c.session)
	}
	if c.protocol != "" {
		req.Header.Set("MCP-Protocol-Version", c.protocol)
	}
	c.cfg.Auth.apply(req, "")
	applyHeaders(req, c.cfg.Headers)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if s := resp.Header.Get(sessionHeader); s != "" {
		c.session = s
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		hint := ""
		if challenge := resp.Header.Get("WWW-Authenticate"); challenge != "" {
			hint = " (server asks for: " + challenge + ")"
		}
		return nil, fmt.Errorf("HTTP %d: authorization refused%s %s", resp.StatusCode, hint, truncateText(strings.TrimSpace(string(body)), 300))
	}
	if id == 0 {
		// A notification: 202 Accepted with no body is the expected answer.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &httpStatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return readSSEReply(resp.Body, id)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	return matchReply(body, id)
}

// readSSEReply reads events until the response to id arrives; progress
// notifications and other messages on the same stream are skipped.
func readSSEReply(r io.Reader, id int64) (*rpcReply, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 64<<20)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			data.WriteString("\n")
		case line == "":
			if data.Len() > 0 {
				if reply, err := matchReply([]byte(data.String()), id); err == nil && reply != nil {
					return reply, nil
				}
				data.Reset()
			}
		}
	}
	if data.Len() > 0 {
		if reply, err := matchReply([]byte(data.String()), id); err == nil && reply != nil {
			return reply, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("the event stream ended without a response")
}

// matchReply finds the response to id in a single message or a batch.
func matchReply(body []byte, id int64) (*rpcReply, error) {
	body = bytes.TrimSpace(body)
	want := fmt.Sprint(id)
	if len(body) > 0 && body[0] == '[' {
		var batch []rpcReply
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, err
		}
		for i := range batch {
			if strings.Trim(string(batch[i].ID), `"`) == want {
				return &batch[i], nil
			}
		}
		return nil, fmt.Errorf("no response to request %d in the batch", id)
	}
	var reply rpcReply
	if err := json.Unmarshal(body, &reply); err != nil {
		return nil, fmt.Errorf("not a JSON-RPC response: %s", truncateText(string(body), 200))
	}
	if strings.Trim(string(reply.ID), `"`) != want {
		return nil, fmt.Errorf("response id %s does not match request %d", reply.ID, id)
	}
	return &reply, nil
}

// --- stdio ---

func (c *mcpClient) startProcess() error {
	cmd := exec.Command(c.cfg.Command, c.cfg.Args...)
	cmd.Env = os.Environ()
	for k, v := range c.cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+expandEnvRefs(v))
	}
	// A token for a stdio server has nowhere to go but the environment.
	if token := secretValue(c.cfg.Auth.Token, c.cfg.Auth.TokenEnv); token != "" && c.cfg.Auth.Name != "" {
		cmd.Env = append(cmd.Env, c.cfg.Auth.Name+"="+token)
	}
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", c.cfg.Command, err)
	}
	c.cmd, c.stdin, c.stdout = cmd, stdin, bufio.NewReaderSize(stdout, 1<<20)
	return nil
}

func (c *mcpClient) writeStdio(msg any) error {
	if c.stdin == nil {
		return fmt.Errorf("not connected")
	}
	data, _ := json.Marshal(msg)
	_, err := c.stdin.Write(append(data, '\n'))
	return err
}

func (c *mcpClient) roundTripStdio(ctx context.Context, msg map[string]any, id int64) (*rpcReply, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.writeStdio(msg); err != nil {
		return nil, err
	}
	type result struct {
		reply *rpcReply
		err   error
	}
	done := make(chan result, 1)
	go func() {
		for {
			line, err := c.stdout.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				var reply rpcReply
				if json.Unmarshal(line, &reply) == nil {
					if reply.Method != "" && len(reply.ID) > 0 {
						// A request from the server (sampling, roots, ...):
						// decline it so the server is not left waiting.
						_ = c.writeStdio(map[string]any{"jsonrpc": "2.0", "id": reply.ID,
							"error": map[string]any{"code": -32601, "message": "not supported by this client"}})
						continue
					}
					if strings.Trim(string(reply.ID), `"`) == fmt.Sprint(id) {
						done <- result{reply: &reply}
						return
					}
				}
			}
			if err != nil {
				done <- result{err: fmt.Errorf("the server process closed its output: %w", err)}
				return
			}
		}
	}()
	select {
	case r := <-done:
		return r.reply, r.err
	case <-ctx.Done():
		// The reader goroutine is left with a pipe that is about to close.
		c.closeLocked()
		return nil, ctx.Err()
	}
}

func (c *mcpClient) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

func (c *mcpClient) closeLocked() {
	if c.cmd != nil {
		if c.stdin != nil {
			c.stdin.Close()
		}
		if c.cmd.Process != nil {
			c.cmd.Process.Kill()
		}
		c.cmd.Wait()
		c.cmd, c.stdin, c.stdout = nil, nil, nil
	}
	if c.url != "" && c.session != "" {
		// Ending the session is a courtesy; a server that does not support
		// DELETE simply lets it expire.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url, nil); err == nil {
			req.Header.Set(sessionHeader, c.session)
			c.cfg.Auth.apply(req, "")
			applyHeaders(req, c.cfg.Headers)
			if resp, err := c.http.Do(req); err == nil {
				resp.Body.Close()
			}
		}
		c.session = ""
	}
}
