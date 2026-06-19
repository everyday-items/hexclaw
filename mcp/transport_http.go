package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/toolkit/net/sse"
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.endpoint, nil)
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
	if resp.StatusCode == http.StatusNotFound {
		t.mu.Lock()
		t.sessionID = ""
		t.mu.Unlock()
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("MCP HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	t.mu.Lock()
	t.sessionID = ""
	t.mu.Unlock()
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

const maxTotalSSEBytes = 100 * 1024 * 1024 // 100 MB total SSE stream limit

// streamSSE 解析 MCP over HTTP 的 text/event-stream 响应，逐个 SSE 事件取出其
// data 负载，按 JSON-RPC 响应语义分发 result 或将 error 作为 Go 错误向上传播。
//
// SSE 行级解析（总字节上限 + 严格 data 前缀两项安全不变量）下沉到
// toolkit 的 sse.Reader，通过 WithMaxTotalBytes / WithStrictDataPrefix 启用：
//   - WithMaxTotalBytes：限制累计读取字节数，防御不可信上游用超长流耗尽内存的
//     DoS 攻击；超限时 Reader 返回 sentinel 错误 sse.ErrMaxBytesExceeded，
//     本函数以 %w 包装该 sentinel 后返回，使调用方既能 errors.Is 判定身份、
//     又能从文案中读出本包约定的超限语义。
//   - WithStrictDataPrefix：仅识别规范的 "data: "（含单空格）前缀，与本包既有
//     的严格度保持一致（"data:" 无空格的行被忽略，不视为 data 字段）。
//
// JSON-RPC 层语义（error 传播、仅 result != nil 才回调、空/非法 data 静默忽略）
// 不属于通用 SSE 能力，保留在本函数内：Reader 只负责把每个事件的 data 还原出来。
func (t *HTTPTransport) streamSSE(body io.Reader, handler func(json.RawMessage)) error {
	reader := sse.NewReaderWithOptions(body,
		sse.WithMaxTotalBytes(maxTotalSSEBytes), // 100MB 总流量上限，防 DoS
		sse.WithStrictDataPrefix(),              // 仅接受规范 "data: " 前缀
	)

	for {
		event, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// 正常读到流尾。Reader 在 EOF 前已把末尾未以空行收尾的事件
				// 一并返回，无需额外 flush。
				return nil
			}
			if errors.Is(err, sse.ErrMaxBytesExceeded) {
				// 以 %w 包装 toolkit 的 sentinel，复用其超限身份：
				// 调用方可 errors.Is(err, sse.ErrMaxBytesExceeded) 精确判定，
				// 无需依赖字符串子串；文案保留 "exceeded maximum size" 语义，
				// 与本包既有错误契约（characterization 测试钉死）一致。
				return fmt.Errorf("SSE stream exceeded maximum size of %d bytes: %w", maxTotalSSEBytes, err)
			}
			return err
		}

		// event.Data 为该 SSE 事件全部 data 行以 "\n" 拼接后的负载。
		// 空负载（如仅有 event/id 字段的事件）交给 Unmarshal 时会失败，
		// 与历史"仅 len(dataLines) > 0 才处理"的效果一致，被静默忽略。
		var rpcResp jsonRPCResponse
		if err := json.Unmarshal([]byte(event.Data), &rpcResp); err == nil {
			if rpcResp.Error != nil {
				return fmt.Errorf("RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
			}
			if rpcResp.Result != nil {
				handler(rpcResp.Result)
			}
		}
	}
}
