package cron

// P1 functional verification: StarlarkEngine runs a full collector end to end
// (http_get -> re_findall -> json_decode -> now -> http_post -> emit) with zero
// external runtime, and its Validate gate rejects malformed/unsafe scripts.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStarlarkEngine_CollectorEndToEnd(t *testing.T) {
	sdata := `{"data":{"cards":[{"content":[{"word":"标题A"},{"word":"标题B"},{"word":"标题A"},{"word":"标题C"}]}]}}`
	page := "<html><head></head><body><!--s-data:" + sdata + "--></body></html>"

	var ingested string
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(page))
	})
	mux.HandleFunc("/ingest", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ingested = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"doc-1"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	script := `
def run():
    resp = http_get("` + srv.URL + `/page", headers = {"User-Agent": "test"})
    if resp["status"] != 200:
        return {"status": "error", "error": "bad fetch"}
    blocks = re_findall("(?s)<!--s-data:(.*?)-->", resp["body"])
    if len(blocks) == 0:
        return {"status": "error", "error": "no s-data"}
    data = json_decode(blocks[0])
    titles = []
    seen = {}
    for card in data.get("data", {}).get("cards", []):
        for item in card.get("content", []):
            w = item.get("word", "").strip()
            if w and w not in seen:
                seen[w] = True
                titles.append(w)
    payload = json_encode({"date": now()["date"], "titles": titles})
    post = http_post("` + srv.URL + `/ingest", body = payload, headers = {"Content-Type": "application/json"})
    if post["status"] != 200:
        return {"status": "error", "error": "ingest failed"}
    return {"status": "success", "data": {"count": len(titles)}}

emit(run())
`
	eng := NewStarlarkEngine()
	// httptest binds to 127.0.0.1, which the SSRF guard now blocks (F-3). This
	// test exercises the fetch/parse/post pipeline, not the guard — loopback
	// blocking is verified in bug_20260615_engine_security_test.go — so swap in a
	// plain client to reach the local test server (same-package white-box).
	eng.client = &http.Client{}
	res, err := eng.Execute(context.Background(), &JobSpec{Runtime: RuntimeStarlark, Script: script, TimeoutSec: 10})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("status = %q, error = %q, stdout = %q", res.Status, res.Error, res.Stdout)
	}
	m, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("data not a map: %T", res.Data)
	}
	if m["count"] != int64(3) { // 标题A deduped: A,B,C
		t.Errorf("count = %v, want 3 (deduped)", m["count"])
	}
	if !strings.Contains(ingested, "标题A") || !strings.Contains(ingested, "标题C") {
		t.Errorf("ingest payload missing titles: %s", ingested)
	}
}

func TestStarlarkEngine_Validate(t *testing.T) {
	eng := NewStarlarkEngine()
	cases := []struct {
		name    string
		script  string
		wantErr bool
	}{
		{"ok", `emit({"status": "success"})`, false},
		{"missing emit", `x = 1 + 1`, true},
		{"syntax error", `def f(:`, true},
		{"load banned", `load("foo.star", "bar")` + "\nemit({})", true},
		{"empty", `   `, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := eng.Validate(c.script)
			if (err != nil) != c.wantErr {
				t.Errorf("Validate(%q) err = %v, wantErr = %v", c.name, err, c.wantErr)
			}
		})
	}
}

func TestStarlarkEngine_EmitErrorStatus(t *testing.T) {
	eng := NewStarlarkEngine()
	res, err := eng.Execute(context.Background(), &JobSpec{
		Runtime: RuntimeStarlark,
		Script:  `emit({"status": "error", "error": "boom"})`,
		TimeoutSec: 5,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != "error" || res.Error != "boom" {
		t.Errorf("status/error = %q/%q, want error/boom", res.Status, res.Error)
	}
}

func TestStarlarkEngine_NoEmitIsError(t *testing.T) {
	eng := NewStarlarkEngine()
	res, err := eng.Execute(context.Background(), &JobSpec{
		Runtime:    RuntimeStarlark,
		Script:     `x = 1`,
		TimeoutSec: 5,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != "error" {
		t.Errorf("a script that never calls emit must be an error, got %q", res.Status)
	}
}
