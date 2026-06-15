package mcp

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// Characterization of streamSSE's hardening that the generic SSE-spec parsers
// deliberately do not provide: a hard total-byte ceiling and the strict
// "data: " (space-required) prefix. These pin the security-relevant behavior so
// any future move to a shared reader is a conscious, reviewed decision.

// repeatLineReader streams a single fixed line lazily, capped at maxBytes, so
// the multi-megabyte ceiling can be exercised without materializing the whole
// stream. A hard ceiling guarantees termination if the size guard ever regresses.
type repeatLineReader struct {
	line     []byte
	pos      int
	emitted  int64
	maxBytes int64
}

func (r *repeatLineReader) Read(p []byte) (int, error) {
	if r.emitted >= r.maxBytes {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && r.emitted < r.maxBytes {
		if r.pos == len(r.line) {
			r.pos = 0
		}
		c := copy(p[n:], r.line[r.pos:])
		n += c
		r.pos += c
		r.emitted += int64(c)
	}
	return n, nil
}

func TestStreamSSE_TotalByteCeilingReturnsError(t *testing.T) {
	transport := NewHTTPTransport("http://localhost")

	// Lines of (1MiB-1) filler + newline contribute exactly 1MiB each toward the
	// total counter; the 100MiB ceiling trips on the 101st line. Non-"data: "
	// lines are ignored, so no payload accumulates in memory.
	const lineLen = 1<<20 - 1
	line := append([]byte(strings.Repeat("a", lineLen)), '\n')
	body := &repeatLineReader{line: line, maxBytes: 102 * (1 << 20)}

	var dispatched int
	err := transport.streamSSE(body, func(json.RawMessage) { dispatched++ })

	if err == nil {
		t.Fatal("a stream past the 100MiB ceiling must return an error")
	}
	if !strings.Contains(err.Error(), "exceeded maximum size") {
		t.Fatalf("expected an over-size error, got: %v", err)
	}
	if dispatched != 0 {
		t.Fatalf("oversize filler must not dispatch events, got %d", dispatched)
	}
}

func TestStreamSSE_DataPrefixRequiresLeadingSpace(t *testing.T) {
	transport := NewHTTPTransport("http://localhost")

	// "data:" without the following space is not the "data: " prefix the parser
	// recognizes, so the line is ignored and no JSON-RPC result is dispatched.
	sseBody := "data:{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n\n"

	var received []json.RawMessage
	err := transport.streamSSE(strings.NewReader(sseBody), func(d json.RawMessage) {
		received = append(received, d)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(received) != 0 {
		t.Fatalf("a data line without a leading space must be ignored, got %d events", len(received))
	}
}
