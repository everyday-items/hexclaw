package cron

// Coverage completion for the Starlark engine deep branches, the python
// validation chain, and the migration db helpers — error/edge paths the
// functional tests don't reach.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func execStar(t *testing.T, eng *StarlarkEngine, script string, timeoutSec int) *RunResult {
	t.Helper()
	res, err := eng.Execute(context.Background(), &JobSpec{Runtime: RuntimeStarlark, Script: script, TimeoutSec: timeoutSec})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

func TestStarlarkExecute_NilAndEmptySpec(t *testing.T) {
	eng := NewStarlarkEngine()
	if _, err := eng.Execute(context.Background(), nil); err == nil {
		t.Error("nil spec must error")
	}
	if _, err := eng.Execute(context.Background(), &JobSpec{Runtime: RuntimeStarlark, Script: "   "}); err == nil {
		t.Error("empty script must error")
	}
}

func TestStarlarkExecute_RuntimeErrorBacktrace(t *testing.T) {
	// A reference to an undefined name is an EvalError → Backtrace branch.
	res := execStar(t, NewStarlarkEngine(), `emit(undefined_symbol)`, 10)
	if res.Status != "error" || res.Error == "" {
		t.Fatalf("runtime error must surface, got status=%q err=%q", res.Status, res.Error)
	}
}

func TestStarlarkExecute_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // block until the client (deadline) cancels
	}))
	defer srv.Close()
	eng := NewStarlarkEngine()
	eng.client = &http.Client{} // reach the loopback test server; SSRF is tested elsewhere
	script := `emit(http_get("` + srv.URL + `"))`
	res := execStar(t, eng, script, 1) // 1s budget, server never replies
	if res.Status != "timeout" {
		t.Fatalf("a script blocked past its budget must be timeout, got %q (err=%q)", res.Status, res.Error)
	}
}

func TestStarlarkHTTP_PostBodyAndHeaders(t *testing.T) {
	var gotBody, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(201)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	eng := NewStarlarkEngine()
	eng.client = &http.Client{}
	script := `emit({"status": "success", "data": http_post("` + srv.URL + `", body = "hello", headers = {"Content-Type": "application/json"})["status"]})`
	res := execStar(t, eng, script, 10)
	if res.Status != "success" || res.Data != int64(201) {
		t.Fatalf("post status = %v (status=%q err=%q)", res.Data, res.Status, res.Error)
	}
	if gotBody != "hello" || gotCT != "application/json" {
		t.Errorf("server saw body=%q ct=%q", gotBody, gotCT)
	}
}

func TestStarlarkEmit_NonDictAndErrorField(t *testing.T) {
	// Non-dict emit → status=success, data=value.
	res := execStar(t, NewStarlarkEngine(), `emit("just a string")`, 10)
	if res.Status != "success" || res.Data != "just a string" {
		t.Errorf("non-dict emit: status=%q data=%v", res.Status, res.Data)
	}
	// dict with explicit status + error field (a dict without status is treated
	// as "no emit", so a real error result carries status="error").
	res = execStar(t, NewStarlarkEngine(), `emit({"status": "error", "error": "boom"})`, 10)
	if res.Status != "error" || res.Error != "boom" {
		t.Errorf("error-field emit: status=%q err=%q", res.Status, res.Error)
	}
}

func TestStarlarkConversions_FloatAndBuiltinValue(t *testing.T) {
	// Non-integer float → starlark.Float (goToStarlark float branch).
	res := execStar(t, NewStarlarkEngine(), `emit({"data": json_decode("{\"x\": 1.5}")})`, 10)
	m, ok := res.Data.(map[string]any)
	if !ok || m["x"] != 1.5 {
		t.Errorf("float round-trip: %#v", res.Data)
	}
	// Emit a builtin value → starlarkToGo default branch (v.String()).
	res = execStar(t, NewStarlarkEngine(), `emit({"data": now})`, 10)
	if s, _ := res.Data.(string); !strings.Contains(s, "now") {
		t.Errorf("builtin value default conversion: %#v", res.Data)
	}
}

func TestStarlarkBuiltins_MoreArgAndShapeErrors(t *testing.T) {
	cases := []struct{ name, script, errSub string }{
		{"json_encode missing arg", `emit({"data": json_encode()})`, "json_encode"},
		{"now extra arg", `emit({"data": now(1)})`, "now"},
		{"html_unescape missing arg", `emit({"data": html_unescape()})`, "html_unescape"},
		{"re_findall no-group branch", `emit({"status": "success", "data": re_findall("a", "banana")})`, ""}, // success path, no capture group
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := execStar(t, NewStarlarkEngine(), c.script, 10)
			if c.errSub == "" {
				if res.Status != "success" {
					t.Fatalf("expected success, got status=%q err=%q", res.Status, res.Error)
				}
				return
			}
			if res.Status != "error" || !strings.Contains(res.Error, c.errSub) {
				t.Errorf("expected error mentioning %q, got status=%q err=%q", c.errSub, res.Status, res.Error)
			}
		})
	}
}

func TestStarlarkPrintCallback(t *testing.T) {
	// A script calling print() exercises the thread.Print stdout sink.
	res := execStar(t, NewStarlarkEngine(), "print(\"hello stdout\")\nemit({\"status\": \"success\"})", 10)
	if res.Status != "success" || !strings.Contains(res.Stdout, "hello stdout") {
		t.Errorf("print sink: status=%q stdout=%q", res.Status, res.Stdout)
	}
}

func TestStarlarkArgErrors_EmitHTTPDecodeReSub(t *testing.T) {
	for _, c := range []struct{ name, script, sub string }{
		{"emit no arg", `emit()`, "emit"},
		{"http_get no url", `emit(http_get())`, "http_get"},
		{"http_post no url", `emit(http_post())`, "http_post"},
		{"json_decode no arg", `emit({"status": "success", "data": json_decode()})`, "json_decode"},
		{"re_sub no arg", `emit({"status": "success", "data": re_sub()})`, "re_sub"},
		{"http_get bad url", `emit(http_get("http:// bad"))`, "http_get"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := execStar(t, NewStarlarkEngine(), c.script, 10)
			if res.Status != "error" || !strings.Contains(res.Error, c.sub) {
				t.Errorf("expected error mentioning %q, got status=%q err=%q", c.sub, res.Status, res.Error)
			}
		})
	}
}

func TestStarlarkKBIngest_WriterErrorPropagates(t *testing.T) {
	eng := NewStarlarkEngine()
	eng.SetKBIngest(func(context.Context, string, string, string) (string, error) {
		return "", context.DeadlineExceeded
	})
	res := execStar(t, eng, `emit({"status": "success", "data": kb_ingest(title = "t", content = "c", source = "s")})`, 10)
	if res.Status != "error" || !strings.Contains(res.Error, "kb_ingest") {
		t.Errorf("writer error must propagate, got status=%q err=%q", res.Status, res.Error)
	}
}

func TestStarlarkSSRF_AllowsPublic(t *testing.T) {
	// A non-private TEST-NET address passes the dialer guard (Control returns nil)
	// — it then fails to connect within the budget, but the allow-branch is taken.
	res := execStar(t, NewStarlarkEngine(), `emit(http_get("http://192.0.2.1:9/"))`, 2)
	if res.Status == "success" {
		t.Errorf("TEST-NET dial cannot succeed; expected error/timeout, got success")
	}
	if strings.Contains(res.Error, "blocked address") {
		t.Errorf("public address must pass the SSRF guard, got: %q", res.Error)
	}
}

func TestGoToStarlark_DefaultBranch(t *testing.T) {
	// A value json.Unmarshal can never produce (a channel) hits the default arm.
	if v := goToStarlark(make(chan int)); v.Type() != "NoneType" {
		t.Errorf("unknown Go type must map to None, got %s", v.Type())
	}
}

// --- python validation chain (engine_python.Validate + validators.validateSpec) ---

func TestPythonEngine_ValidateRejectsBadSyntax(t *testing.T) {
	if err := NewScriptExecutor().Validate("def (:"); err == nil {
		t.Error("python syntax error must be rejected by Validate")
	}
}

func TestValidateSpec_AllGuards(t *testing.T) {
	if err := validateSpec(nil); err == nil {
		t.Error("nil spec must error")
	}
	if err := validateSpec(&JobSpec{Runtime: "starlark", Script: "x"}); err == nil {
		t.Error("non-python3 runtime must error in v1 validateSpec")
	}
	if err := validateSpec(&JobSpec{Runtime: "python3", Script: "   "}); err == nil {
		t.Error("empty script must error")
	}
	if err := validateSpec(&JobSpec{Runtime: "python3", Script: "def (:"}); err == nil {
		t.Error("syntax error must propagate")
	}
	// valid syntax, no forbidden import, but missing output contract.
	if err := validateSpec(&JobSpec{Runtime: "python3", Script: "x = 1\n"}); err == nil {
		t.Error("missing print(json.dumps()) contract must error")
	}
}

func TestValidateNoForbiddenImports_FlagsForbidden(t *testing.T) {
	if err := validateNoForbiddenImports("import socket\n"); err == nil {
		t.Error("forbidden module import must be flagged")
	}
}

// --- migration db helpers ---

func TestIsDuplicateColumnErr_Nil(t *testing.T) {
	if isDuplicateColumnErr(nil) {
		t.Error("nil error is not a duplicate-column error")
	}
}

func TestAddColumnExpectDuplicate_NonDuplicateErrorIsTolerated(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	// ALTER on a non-existent table is a non-duplicate error: the helper must not
	// panic and must return (it logs a warn). This exercises the warn branch.
	addColumnExpectDuplicate(context.Background(), db, "nope", "ALTER TABLE nope ADD COLUMN x TEXT")
}
