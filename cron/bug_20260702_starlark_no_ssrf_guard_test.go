package cron

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// bug_20260702（C）：cron Starlark 出网注释与行为一致性。
//
// 三处旧注释（engine_starlark.go / cron.go / llm_compiler.go）宣称 http_post 到
// 127.0.0.1/localhost「已被沙箱 SSRF 拦截」，但引擎的 builtinHTTP 从无 SSRF/loopback
// 守卫——注释在撒谎。产品既定决策是「桌面按宿主机语义放开网络」（见
// TestNetworkUnrestricted_StarlarkReachesLoopback，锁 http_get），本测试补齐 http_post
// 面：http_post 到 loopback 同样**不被拒**，与已修正的注释一致。
func TestBug20260702_StarlarkHTTPPostLoopbackNotBlocked(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"doc-1"}`))
	}))
	defer srv.Close()

	eng := NewStarlarkEngine()
	// srv.URL 形如 http://127.0.0.1:PORT —— 若存在 SSRF 守卫，这里会被 "blocked" 拒绝。
	script := "def run():\n" +
		"    resp = http_post(\"" + srv.URL + "\", body = \"payload\", headers = {\"Content-Type\": \"application/json\"})\n" +
		"    return {\"status\": \"success\", \"data\": resp[\"status\"]}\n" +
		"emit(run())"
	res, err := eng.Execute(context.Background(), &JobSpec{Runtime: RuntimeStarlark, Script: script, TimeoutSec: 10})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("loopback http_post 应可达（Starlark 无 SSRF 门），got status=%q err=%q", res.Status, res.Error)
	}
	if strings.Contains(res.Error, "blocked") {
		t.Fatalf("出现 SSRF 拦截语义，守卫疑似被加回: %q", res.Error)
	}
	if gotBody != "payload" {
		t.Fatalf("loopback server 未收到 POST body，got %q", gotBody)
	}
}
