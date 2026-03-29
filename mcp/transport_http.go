package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// HTTPTransport implements the MCP 2025-03-26 Streamable HTTP transport.
//
// Protocol summary:
//   - Client sends JSON-RPC request as HTTP POST with Content-Type: application/json
//   - Server responds with Content-Type: application/json (single) or text/event-stream (streaming)
//   - Session affinity via Mcp-Session-Id header
//   - Session termination via HTTP DELETE
type HTTPTransport struct {
	endpoint   string
	httpClient *http.Client
	headers    map[string]string
	authToken  string
	sessionID  string

	mu    sync.Mutex
	idSeq atomic.Int64
}

// HTTPOption configures the HTTPTransport.
type HTTPOption func(*HTTPTransport)

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(c *http.Client) HTTPOption {
	return func(t *HTTPTransport) { t.httpClient = c }
}

// WithHeaders adds custom headers to every request.
func WithHeaders(h map[string]string) HTTPOption {
	return func(t *HTTPTransport) { t.headers = h }
}

// WithAuth sets a Bearer token for authentication.
func WithAuth(token string) HTTPOption {
	return func(t *HTTPTransport) { t.authToken = token }
}

// NewHTTPTransport creates a new Streamable HTTP transport.
func NewHTTPTransport(endpoint string, opts ...HTTPOption) *HTTPTransport {
	t := &HTTPTransport{
		endpoint:   strings.TrimRight(endpoint, "/"),
		httpClient: http.DefaultClient,
		headers:    make(map[string]string),
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Send sends a JSON-RPC request and returns the result.
func (t *HTTPTransport) Send(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	resp, err := t.doPost(ctx, method, params)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	t.captureSessionID(resp)

	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		return t.collectSSEResult(resp.Body)
	}
	return t.readJSONResponse(resp.Body)
}

// SendStream sends a JSON-RPC request and streams SSE events to the handler.
func (t *HTTPTransport) SendStream(ctx context.Context, method string, params json.RawMessage, handler func(json.RawMessage)) error {
	resp, err := t.doPost(ctx, method, params)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	t.captureSessionID(resp)

	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		return t.streamSSE(resp.Body, handler)
	}

	// Fallback: single JSON response
	result, err := t.readJSONResponse(resp.Body)
	if err != nil {
		return err
	}
	handler(result)
	return nil
}

// Close terminates the MCP session by sending HTTP DELETE.
func (t *HTTPTransport) Close() error {
	t.mu.Lock()
	sid := t.sessionID
	t.mu.Unlock()

	if sid == "" {
		return nil
	}

	req, err := http.NewRequest(http.MethodDelete, t.endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Mcp-Session-Id", sid)
	t.applyHeaders(req)

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// SessionID returns the current session ID.
func (t *HTTPTransport) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// ── internal ────────────────────────────────────────

func (t *HTTPTransport) doPost(ctx context.Context, method string, params json.RawMessage) (*http.Response, error) {
	id := t.idSeq.Add(1)
	rpcReq := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	body, err := json.Marshal(rpcReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	t.applyHeaders(req)

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("MCP HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	return resp, nil
}

func (t *HTTPTransport) applyHeaders(req *http.Request) {
	t.mu.Lock()
	sid := t.sessionID
	t.mu.Unlock()

	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	if t.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+t.authToken)
	}
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
}

func (t *HTTPTransport) captureSessionID(resp *http.Response) {
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}
}

const maxMCPResponseSize = 50 * 1024 * 1024 // 50 MB

func (t *HTTPTransport) readJSONResponse(body io.Reader) (json.RawMessage, error) {
	body = io.LimitReader(body, maxMCPResponseSize)
	var rpcResp jsonRPCResponse
	if err := json.NewDecoder(body).Decode(&rpcResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

func (t *HTTPTransport) collectSSEResult(body io.Reader) (json.RawMessage, error) {
	var last json.RawMessage
	err := t.streamSSE(body, func(data json.RawMessage) {
		last = data
	})
	if err != nil {
		return nil, err
	}
	return last, nil
}

func (t *HTTPTransport) streamSSE(body io.Reader, handler func(json.RawMessage)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 1MB max line for large tool results
	var dataLines []string

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			// Blank line = end of event
			if len(dataLines) > 0 {
				payload := strings.Join(dataLines, "\n")
				var rpcResp jsonRPCResponse
				if err := json.Unmarshal([]byte(payload), &rpcResp); err == nil {
					if rpcResp.Error != nil {
						return fmt.Errorf("RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
					}
					if rpcResp.Result != nil {
						handler(rpcResp.Result)
					}
				}
				dataLines = nil
			}
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, line[6:])
		}
		// Ignore "event:", "id:", "retry:", comments
	}

	// Flush remaining buffered data (server closed without trailing blank line)
	if len(dataLines) > 0 {
		payload := strings.Join(dataLines, "\n")
		var rpcResp jsonRPCResponse
		if err := json.Unmarshal([]byte(payload), &rpcResp); err == nil {
			if rpcResp.Error != nil {
				return fmt.Errorf("RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
			}
			if rpcResp.Result != nil {
				handler(rpcResp.Result)
			}
		}
	}

	return scanner.Err()
}
