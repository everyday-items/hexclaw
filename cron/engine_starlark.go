package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/hexagon-codes/toolkit/net/ip"
	"github.com/hexagon-codes/toolkit/util/hash"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// StarlarkEngine runs a cron script as in-process Starlark — a pure-Go, sandboxed
// Python dialect. It needs no external interpreter, so it works identically on
// macOS/Linux/Windows with zero user setup.
//
// Security model: Starlark has no import/eval/exec/getattr/file/network of its
// own. Every capability a script has is a host builtin we inject here (http_get,
// http_post, json_decode, json_encode, re_findall, kb_ingest, emit). That is a
// structural sandbox — the opposite of an AST denylist that must chase every new
// escape.
type StarlarkEngine struct {
	client     *http.Client
	maxBody    int64
	stdoutTail int
	kbIngest   KBIngestFunc
}

// KBIngestFunc persists a document into the local knowledge base in-process and
// returns the new document ID. StarlarkEngine exposes it to scripts as the
// kb_ingest builtin so a collector can store its results without an HTTP POST to
// the app's own API — a round-trip that loopback SSRF blocking now forbids.
type KBIngestFunc func(ctx context.Context, title, content, source string) (string, error)

// starlarkMaxBody caps a single http_get/http_post response body so a runaway
// page cannot exhaust memory. 8 MiB comfortably covers normal HTML/JSON pages.
const starlarkMaxBody = 8 << 20

// starlarkMaxSteps bounds a single run's Starlark execution steps so an infinite
// loop (while True / huge range) is killed instead of spinning to the timeout.
// ~100M steps is far above any real fetch->parse->store job.
const starlarkMaxSteps = 100_000_000

// NewStarlarkEngine builds the default pure-Go script engine.
func NewStarlarkEngine() *StarlarkEngine {
	return &StarlarkEngine{
		client:     ssrfGuardedClient(30 * time.Second),
		maxBody:    starlarkMaxBody,
		stdoutTail: 64 * 1024,
	}
}

func (e *StarlarkEngine) Name() string   { return RuntimeStarlark }
func (e *StarlarkEngine) Available() bool { return true } // pure Go, always present

// SetKBIngest wires the in-process knowledge-base writer behind the kb_ingest
// builtin. With no writer set, kb_ingest returns an explicit error rather than
// silently dropping content. Mirrors the codebase's Set* injection style
// (SetAgentRunner / SetKnowledgeBase).
func (e *StarlarkEngine) SetKBIngest(fn KBIngestFunc) { e.kbIngest = fn }

// Validate statically checks Starlark syntax and rejects module loading. No AST
// denylist is needed: the dialect grants no escape primitives, so a parse plus a
// load() ban plus the output-contract check is the whole safety surface.
func (e *StarlarkEngine) Validate(script string) error {
	return validateStarlarkSource(script)
}

// validateStarlarkSource is the stateless Starlark validation shared by the
// engine's Validate method and the compiler. Being package-level, the compile
// path validates without constructing an engine (and its SSRF client) per call.
func validateStarlarkSource(script string) error {
	if strings.TrimSpace(script) == "" {
		return fmt.Errorf("script is empty")
	}
	opts := &syntax.FileOptions{}
	f, err := opts.Parse("job.star", script, 0)
	if err != nil {
		return fmt.Errorf("starlark syntax error: %w", err)
	}
	for _, stmt := range f.Stmts {
		if _, ok := stmt.(*syntax.LoadStmt); ok {
			return fmt.Errorf("load() is not allowed in cron scripts")
		}
	}
	if !strings.Contains(script, "emit(") {
		return fmt.Errorf("script must call emit({...}) to return its result")
	}
	return nil
}

// Execute runs the script under the spec's timeout and captures the value passed
// to emit() as the structured RunResult.
func (e *StarlarkEngine) Execute(ctx context.Context, spec *JobSpec) (*RunResult, error) {
	if spec == nil || strings.TrimSpace(spec.Script) == "" {
		return nil, fmt.Errorf("spec or script is empty")
	}
	start := time.Now()
	timeout := time.Duration(effectiveTimeoutSec(spec.TimeoutSec)) * time.Second
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout strings.Builder
	var emitted starlark.Value

	thread := &starlark.Thread{
		Name: "cron-starlark",
		Print: func(_ *starlark.Thread, msg string) {
			stdout.WriteString(msg)
			stdout.WriteByte('\n')
		},
	}
	thread.SetMaxExecutionSteps(starlarkMaxSteps) // kill infinite loops, not just on timeout
	// Cooperative cancellation: starlark checks the cancel flag between steps.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-runCtx.Done():
			thread.Cancel("deadline")
		case <-stop:
		}
	}()

	emit := starlark.NewBuiltin("emit", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var v starlark.Value
		if err := starlark.UnpackArgs(b.Name(), args, kwargs, "result", &v); err != nil {
			return nil, err
		}
		emitted = v
		return starlark.None, nil
	})

	predeclared := starlark.StringDict{
		"http_get":    starlark.NewBuiltin("http_get", e.builtinHTTP(runCtx, http.MethodGet)),
		"http_post":   starlark.NewBuiltin("http_post", e.builtinHTTP(runCtx, http.MethodPost)),
		"json_decode": starlark.NewBuiltin("json_decode", builtinJSONDecode),
		"json_encode": starlark.NewBuiltin("json_encode", builtinJSONEncode),
		"re_findall":    starlark.NewBuiltin("re_findall", builtinReFindall),
		"re_sub":        starlark.NewBuiltin("re_sub", builtinReSub),
		"html_unescape": starlark.NewBuiltin("html_unescape", builtinHTMLUnescape),
		"url_encode":    starlark.NewBuiltin("url_encode", builtinURLEncode),
		"sha256":        starlark.NewBuiltin("sha256", builtinSHA256),
		"now":           starlark.NewBuiltin("now", builtinNow),
		"kb_ingest":     starlark.NewBuiltin("kb_ingest", e.builtinKBIngest(runCtx)),
		"emit":          emit,
	}

	opts := &syntax.FileOptions{}
	_, runErr := starlark.ExecFileOptions(opts, thread, "job.star", spec.Script, predeclared)

	result := &RunResult{
		Stdout:     tailString(stdout.String(), e.stdoutTail),
		DurationMs: time.Since(start).Milliseconds(),
	}
	if runCtx.Err() == context.DeadlineExceeded {
		result.Status = "timeout"
		result.Error = fmt.Sprintf("script exceeded %ds", int(timeout.Seconds()))
		return result, nil
	}
	if runErr != nil {
		result.Status = "error"
		if evalErr, ok := runErr.(*starlark.EvalError); ok {
			result.Error = evalErr.Backtrace()
		} else {
			result.Error = runErr.Error()
		}
		return result, nil
	}
	applyEmitted(emitted, result)
	if result.Status == "" {
		result.Status = "error"
		result.Error = "script finished without calling emit({...})"
	}
	return result, nil
}

// builtinHTTP returns an http_get/http_post builtin bound to the run context.
// Signature: http_get(url, headers={}) / http_post(url, body="", headers={})
// -> {"status": int, "body": str}.
func (e *StarlarkEngine) builtinHTTP(ctx context.Context, method string) func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
	return func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var url, body string
		var headers starlark.Value
		if method == http.MethodPost {
			if err := starlark.UnpackArgs(b.Name(), args, kwargs, "url", &url, "body?", &body, "headers?", &headers); err != nil {
				return nil, err
			}
		} else {
			if err := starlark.UnpackArgs(b.Name(), args, kwargs, "url", &url, "headers?", &headers); err != nil {
				return nil, err
			}
		}
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return nil, fmt.Errorf("%s: only http/https URLs allowed, got %q", b.Name(), url)
		}
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rdr)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", b.Name(), err)
		}
		if d, ok := headers.(*starlark.Dict); ok {
			for _, item := range d.Items() {
				k, _ := starlark.AsString(item[0])
				v, _ := starlark.AsString(item[1])
				req.Header.Set(k, v)
			}
		}
		resp, err := e.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", b.Name(), err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, e.maxBody))
		out := starlark.NewDict(2)
		_ = out.SetKey(starlark.String("status"), starlark.MakeInt(resp.StatusCode))
		_ = out.SetKey(starlark.String("body"), starlark.String(string(data)))
		return out, nil
	}
}

// builtinKBIngest returns the kb_ingest(title, content, source) builtin bound to
// the run context. It writes straight into the knowledge base in-process and
// returns {"id": "<doc-id>"}. With no writer injected it errors explicitly, so a
// script can never report success while persisting nothing.
func (e *StarlarkEngine) builtinKBIngest(ctx context.Context) func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
	return func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var title, content, source string
		if err := starlark.UnpackArgs(b.Name(), args, kwargs, "title", &title, "content", &content, "source?", &source); err != nil {
			return nil, err
		}
		if e.kbIngest == nil {
			return nil, fmt.Errorf("kb_ingest: knowledge base is not enabled")
		}
		if strings.TrimSpace(content) == "" {
			return nil, fmt.Errorf("kb_ingest: content must not be empty")
		}
		id, err := e.kbIngest(ctx, title, content, source)
		if err != nil {
			return nil, fmt.Errorf("kb_ingest: %w", err)
		}
		out := starlark.NewDict(1)
		_ = out.SetKey(starlark.String("id"), starlark.String(id))
		return out, nil
	}
}

// builtinJSONDecode: json_decode(s) -> value.
func builtinJSONDecode(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var s string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "s", &s); err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, fmt.Errorf("json_decode: %w", err)
	}
	return goToStarlark(v), nil
}

// builtinJSONEncode: json_encode(value) -> str.
func builtinJSONEncode(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var v starlark.Value
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "value", &v); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(starlarkToGo(v))
	if err != nil {
		return nil, fmt.Errorf("json_encode: %w", err)
	}
	return starlark.String(string(encoded)), nil
}

// builtinReFindall: re_findall(pattern, s) -> list[str]. With one capture group,
// returns group 1 of each match (python re.findall single-group behavior);
// otherwise returns each whole match.
func builtinReFindall(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var pattern, s string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "pattern", &pattern, "s", &s); err != nil {
		return nil, err
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("re_findall: bad pattern: %w", err)
	}
	var out []starlark.Value
	if re.NumSubexp() >= 1 {
		for _, m := range re.FindAllStringSubmatch(s, -1) {
			out = append(out, starlark.String(m[1]))
		}
	} else {
		for _, m := range re.FindAllString(s, -1) {
			out = append(out, starlark.String(m))
		}
	}
	return starlark.NewList(out), nil
}

// builtinReSub: re_sub(pattern, repl, s) -> str. Regex replace (text cleanup).
func builtinReSub(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var pattern, repl, s string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "pattern", &pattern, "repl", &repl, "s", &s); err != nil {
		return nil, err
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("re_sub: bad pattern: %w", err)
	}
	return starlark.String(re.ReplaceAllString(s, repl)), nil
}

// builtinHTMLUnescape: html_unescape(s) -> str. Decodes &amp; &#x... entities.
func builtinHTMLUnescape(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var s string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "s", &s); err != nil {
		return nil, err
	}
	return starlark.String(html.UnescapeString(s)), nil
}

// builtinURLEncode: url_encode(s) -> str. Percent-encodes a query component.
func builtinURLEncode(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var s string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "s", &s); err != nil {
		return nil, err
	}
	return starlark.String(url.QueryEscape(s)), nil
}

// builtinSHA256: sha256(s) -> hex str. For change-detection / dedup (monitors).
func builtinSHA256(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var s string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "s", &s); err != nil {
		return nil, err
	}
	// 复用 toolkit/util/hash.SHA256：内部即 sha256.Sum256 + hex 编码，输出与原实现逐字节一致。
	return starlark.String(hash.SHA256(s)), nil
}

// builtinNow: now() -> dict of the current local time. Collectors routinely need
// the current date for titles/timestamps/per-day dedup (Starlark core has no
// datetime). Fields: year/month/day/hour/minute/second (int), date "YYYY-MM-DD",
// datetime "YYYY-MM-DD HH:MM:SS", unix (int).
func builtinNow(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackArgs(b.Name(), args, kwargs); err != nil {
		return nil, err
	}
	t := time.Now()
	d := starlark.NewDict(9)
	_ = d.SetKey(starlark.String("year"), starlark.MakeInt(t.Year()))
	_ = d.SetKey(starlark.String("month"), starlark.MakeInt(int(t.Month())))
	_ = d.SetKey(starlark.String("day"), starlark.MakeInt(t.Day()))
	_ = d.SetKey(starlark.String("hour"), starlark.MakeInt(t.Hour()))
	_ = d.SetKey(starlark.String("minute"), starlark.MakeInt(t.Minute()))
	_ = d.SetKey(starlark.String("second"), starlark.MakeInt(t.Second()))
	_ = d.SetKey(starlark.String("date"), starlark.String(t.Format("2006-01-02")))
	_ = d.SetKey(starlark.String("datetime"), starlark.String(t.Format("2006-01-02 15:04:05")))
	_ = d.SetKey(starlark.String("unix"), starlark.MakeInt64(t.Unix()))
	return d, nil
}

// applyEmitted maps the dict passed to emit() onto the RunResult contract.
func applyEmitted(v starlark.Value, r *RunResult) {
	if v == nil {
		return
	}
	d, ok := v.(*starlark.Dict)
	if !ok {
		r.Status = "success"
		r.Data = starlarkToGo(v)
		return
	}
	if sv, found, _ := d.Get(starlark.String("status")); found {
		if s, ok := starlark.AsString(sv); ok {
			r.Status = normalizeScriptStatus(s)
		}
	}
	if dv, found, _ := d.Get(starlark.String("data")); found {
		r.Data = starlarkToGo(dv)
	}
	if ev, found, _ := d.Get(starlark.String("error")); found {
		if s, ok := starlark.AsString(ev); ok && r.Error == "" {
			r.Error = s
		}
	}
}

// goToStarlark converts a decoded-JSON Go value into a Starlark value.
func goToStarlark(v any) starlark.Value {
	switch t := v.(type) {
	case nil:
		return starlark.None
	case bool:
		return starlark.Bool(t)
	case string:
		return starlark.String(t)
	case float64:
		if t == math.Trunc(t) && t >= math.MinInt64 && t <= math.MaxInt64 {
			return starlark.MakeInt64(int64(t))
		}
		return starlark.Float(t)
	case []any:
		elems := make([]starlark.Value, len(t))
		for i, e := range t {
			elems[i] = goToStarlark(e)
		}
		return starlark.NewList(elems)
	case map[string]any:
		d := starlark.NewDict(len(t))
		for k, val := range t {
			_ = d.SetKey(starlark.String(k), goToStarlark(val))
		}
		return d
	default:
		return starlark.None
	}
}

// starlarkToGo converts a Starlark value back into a plain Go value (for
// json_encode and RunResult.Data).
func starlarkToGo(v starlark.Value) any {
	switch t := v.(type) {
	case nil, starlark.NoneType:
		return nil
	case starlark.Bool:
		return bool(t)
	case starlark.String:
		return string(t)
	case starlark.Int:
		i, _ := t.Int64()
		return i
	case starlark.Float:
		return float64(t)
	case *starlark.List:
		out := make([]any, 0, t.Len())
		iter := t.Iterate()
		defer iter.Done()
		var x starlark.Value
		for iter.Next(&x) {
			out = append(out, starlarkToGo(x))
		}
		return out
	case *starlark.Dict:
		out := make(map[string]any, t.Len())
		for _, item := range t.Items() {
			k, _ := starlark.AsString(item[0])
			out[k] = starlarkToGo(item[1])
		}
		return out
	default:
		return v.String()
	}
}

// ssrfGuardedClient builds an HTTP client that refuses to connect to loopback,
// private, link-local (incl. cloud metadata 169.254.169.254), or unspecified
// addresses. Loopback is blocked too: a script's only localhost use was posting
// to the app's own knowledge-base API, which now goes through the in-process
// kb_ingest builtin instead. The check runs in the dialer Control hook on the
// resolved IP about to be dialed, so it also defeats DNS-rebinding.
func ssrfGuardedClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	dialer.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip != nil && isBlockedIP(ip) {
			return fmt.Errorf("blocked address %s (private/link-local not allowed)", ip)
		}
		return nil
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DialContext: dialer.DialContext},
	}
}

// isBlockedIP reports whether an IP is in a range a sandboxed script must not
// reach: loopback, private, link-local, or unspecified. Only public addresses
// are allowed. Loopback is blocked so a script cannot reach the app's own
// localhost services (KB ingest goes through the in-process kb_ingest builtin);
// private/link-local covers internal services and cloud metadata。
//
// 判定逻辑下沉到 toolkit/net/ip.IsPrivateOrReservedIP，二者拦截集合完全一致：
// IsLoopback || IsPrivate || IsLinkLocalUnicast || IsLinkLocalMulticast ||
// IsUnspecified（含云元数据端点 169.254.169.254），SSRF 防御语义不变。
func isBlockedIP(addr net.IP) bool {
	return ip.IsPrivateOrReservedIP(addr)
}
