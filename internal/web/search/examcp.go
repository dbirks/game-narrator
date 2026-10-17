package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/bootstrap"
)

// Exa MCP streamable-HTTP defaults (ADR 0018). ExaMCP is the DEFAULT search
// provider: it is Exa's keyless public endpoint, the same backend the
// operator's pi agent uses, so web search works out of the box. Note it
// sends queries to a third party; use SearXNG for private self-hosting or
// Disabled to turn search off entirely.
const (
	// DefaultExaMCPEndpoint is Exa's public streamable-HTTP MCP endpoint.
	DefaultExaMCPEndpoint = "https://mcp.exa.ai/mcp"
	// DefaultExaSearchTool is the documented default tool name; the actual
	// tool is discovered via tools/list at runtime (this is only a
	// preference, not a hardcode of behavior).
	DefaultExaSearchTool = "web_search_exa"

	exaMCPMaxBody     = 4 << 20 // per MCP response
	exaMCPMaxSnippet  = 2000    // per-result snippet cap (chars)
	exaMCPProtocolVer = "2025-06-18"
	exaMCPDeadline    = 30 * time.Second // docs/spec/06: 15-30 s per search
)

// ExaMCP implements Provider over the Model Context Protocol
// (streamable-HTTP transport, JSON-RPC 2.0): initialize →
// notifications/initialized → tools/list (search tool discovered at
// runtime) → tools/call. Both application/json and text/event-stream
// response encodings are handled, and the Mcp-Session-Id header is
// carried across the request sequence.
type ExaMCP struct {
	// Endpoint; empty means DefaultExaMCPEndpoint.
	Endpoint string
	// ToolName is a preference for tools/list discovery; empty means
	// DefaultExaSearchTool, then best-effort matching (see pickSearchTool).
	ToolName string
	// Client is optional; default is a bounded http.Client.
	Client *http.Client
	// Timeout bounds each MCP request; 0 means DefaultTimeout.
	Timeout time.Duration
	// UserAgent; empty means DefaultSearchUserAgent.
	UserAgent string
	// Logger is optional; default slog.Default().
	Logger *slog.Logger
}

// Name implements Provider.
func (e *ExaMCP) Name() string { return "exa-mcp" }

// rpcError is a JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (r *rpcError) Error() string {
	return fmt.Sprintf("mcp JSON-RPC error %d: %s", r.Code, r.Message)
}

// rpcMessage is one JSON-RPC 2.0 message (request or response).
type rpcMessage struct {
	ID     json.Number     `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// mcpTool is one entry of tools/list.
type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Search implements Provider with the full per-call MCP handshake.
func (e *ExaMCP) Search(ctx context.Context, query string, opts Options) ([]Result, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, exaMCPDeadline)
		defer cancel()
	}
	log := e.logger()
	session := ""

	// 1. initialize.
	initParams := map[string]any{
		"protocolVersion": exaMCPProtocolVer,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "game-narrator", "version": bootstrap.Version().Version},
	}
	init, sess, err := e.post(ctx, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": initParams}, "")
	if err != nil {
		return nil, fmt.Errorf("search: exa mcp initialize: %w", err)
	}
	session = sess
	if init == nil || init.Error != nil {
		return nil, fmt.Errorf("search: exa mcp initialize: %s", rpcErrMsg(init))
	}

	// 2. notifications/initialized (required by the MCP handshake).
	if _, sess, err = e.post(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, session); err != nil {
		return nil, fmt.Errorf("search: exa mcp initialized notification: %w", err)
	}
	session = sess

	// 3. tools/list — discover the search tool and its argument schema.
	listMsg, sess, err := e.post(ctx, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}, session)
	if err != nil {
		return nil, fmt.Errorf("search: exa mcp tools/list: %w", err)
	}
	session = sess
	var toolList struct {
		Tools []mcpTool `json:"tools"`
	}
	if listMsg == nil || listMsg.Error != nil || json.Unmarshal(listMsg.Result, &toolList) != nil {
		return nil, fmt.Errorf("search: exa mcp tools/list: %s", rpcErrMsg(listMsg))
	}
	toolName, schema := pickSearchTool(toolList.Tools, e.ToolName)
	if toolName == "" {
		return nil, fmt.Errorf("search: exa mcp tools/list: no search tool found")
	}
	log.Debug("exa mcp tool discovered", "tool", toolName, "query", query)

	// 4. tools/call — pass only arguments the discovered schema declares.
	args := map[string]any{"query": query}
	if schemaHasProperty(schema, "numResults") {
		args["numResults"] = opts.max()
	}
	callMsg, _, err := e.post(ctx, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": toolName, "arguments": args},
	}, session)
	if err != nil {
		return nil, fmt.Errorf("search: exa mcp tools/call: %w", err)
	}
	if callMsg == nil || callMsg.Error != nil {
		return nil, fmt.Errorf("search: exa mcp tools/call: %s", rpcErrMsg(callMsg))
	}
	var callResult struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(callMsg.Result, &callResult); err != nil {
		return nil, fmt.Errorf("search: exa mcp tools/call: decode result: %w", err)
	}
	if callResult.IsError {
		return nil, fmt.Errorf("search: exa mcp tools/call: %s", toolText(callResult.Content))
	}
	text := toolText(callResult.Content)
	return parseExaToolText(e.Name(), toolName, text, opts.max(), time.Now().UTC())
}

func rpcErrMsg(m *rpcMessage) string {
	if m == nil {
		return "empty MCP response"
	}
	if m.Error != nil {
		return m.Error.Error()
	}
	return "no result"
}

func toolText(content []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) string {
	var b strings.Builder
	for _, c := range content {
		if c.Type != "text" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(c.Text)
	}
	return b.String()
}

// pickSearchTool selects the web-search tool: an explicit preference first,
// then names containing "web_search", then "search", then the first tool.
// Returns the tool name and its input schema (nil when unknown).
func pickSearchTool(tools []mcpTool, prefer string) (string, json.RawMessage) {
	if prefer == "" {
		prefer = DefaultExaSearchTool
	}
	for _, t := range tools {
		if t.Name == prefer {
			return t.Name, t.InputSchema
		}
	}
	for _, t := range tools {
		if strings.Contains(strings.ToLower(t.Name), "web_search") {
			return t.Name, t.InputSchema
		}
	}
	for _, t := range tools {
		if strings.Contains(strings.ToLower(t.Name), "search") {
			return t.Name, t.InputSchema
		}
	}
	if len(tools) > 0 {
		return tools[0].Name, tools[0].InputSchema
	}
	return "", nil
}

// schemaHasProperty reports whether the discovered input schema declares
// the given property (in properties or required).
func schemaHasProperty(schema json.RawMessage, prop string) bool {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return false
	}
	if _, ok := s.Properties[prop]; ok {
		return true
	}
	for _, r := range s.Required {
		if r == prop {
			return true
		}
	}
	return false
}

// post performs one JSON-RPC POST to the MCP endpoint. For requests with an
// ID it returns the matching response; notifications (no ID) return a nil
// message on 2xx/202. wantID empty matches any message. The session header
// is returned for the caller to carry forward.
func (e *ExaMCP) post(ctx context.Context, payload map[string]any, session string) (*rpcMessage, string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, session, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, session, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", e.userAgent())
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := e.client().Do(req)
	if err != nil {
		return nil, session, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, exaMCPMaxBody+1))
	if err != nil {
		return nil, session, fmt.Errorf("read body: %w", err)
	}
	if int64(len(raw)) > exaMCPMaxBody {
		return nil, session, fmt.Errorf("mcp response exceeds %d bytes", exaMCPMaxBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, session, fmt.Errorf("mcp HTTP %d: %s", resp.StatusCode, snippet(string(raw), 300))
	}
	next := resp.Header.Get("Mcp-Session-Id")
	if next == "" {
		next = session
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, next, nil // e.g. 202 Accepted for notifications
	}
	ct := resp.Header.Get("Content-Type")
	var wantID json.Number
	if id, ok := payload["id"]; ok {
		if n, ok := id.(int); ok {
			wantID = json.Number(fmt.Sprintf("%d", n))
		}
	}
	if strings.Contains(ct, "text/event-stream") {
		msg, serr := sseExtractMessage(raw, wantID)
		return msg, next, serr
	}
	var msg rpcMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, next, fmt.Errorf("decode mcp json: %w (body: %s)", err, snippet(string(raw), 300))
	}
	return &msg, next, nil
}

// sseExtractMessage scans a text/event-stream body for the JSON-RPC data
// payload whose id matches wantID (any when wantID is zero). Multiple data
// lines within one event are joined with newlines per the SSE spec.
func sseExtractMessage(body []byte, wantID json.Number) (*rpcMessage, error) {
	norm := strings.ReplaceAll(string(body), "\r\n", "\n")
	for _, block := range strings.Split(norm, "\n\n") {
		var data strings.Builder
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		if data.Len() == 0 {
			continue
		}
		var msg rpcMessage
		if json.Unmarshal([]byte(data.String()), &msg) != nil {
			continue
		}
		if wantID == "" || msg.ID == "" || wantID.String() == msg.ID.String() {
			return &msg, nil
		}
	}
	return nil, fmt.Errorf("no JSON-RPC response for id %q in event stream", wantID)
}

func snippet(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (e *ExaMCP) endpoint() string {
	if e.Endpoint != "" {
		return e.Endpoint
	}
	return DefaultExaMCPEndpoint
}

func (e *ExaMCP) userAgent() string {
	if e.UserAgent != "" {
		return e.UserAgent
	}
	return DefaultSearchUserAgent
}

func (e *ExaMCP) client() *http.Client {
	if e.Client != nil {
		return e.Client
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &http.Client{Timeout: timeout}
}

func (e *ExaMCP) logger() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}
