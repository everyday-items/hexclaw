package cron

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 2026-06-22：cron Starlark 引擎已移除 SSRF/私网 dial 守卫（桌面端按宿主机语义放开网络，
// 修复 fake-ip 透明代理把公网域名映射进保留段被误杀的回归）。本测试锁住"放开"这一行为：
// http_get 能连到 loopback 测试服务器——这恰是旧守卫会拦死的目标——防止后续有人无意中把
// dial 守卫加回来。
func TestNetworkUnrestricted_StarlarkReachesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	eng := NewStarlarkEngine()
	// srv.URL 形如 http://127.0.0.1:PORT —— 旧 SSRF 守卫会以 "blocked address" 拒绝。
	script := "def run():\n    resp = http_get(\"" + srv.URL + "\")\n    return {\"status\": \"success\", \"data\": resp[\"status\"]}\nemit(run())"
	res, err := eng.Execute(context.Background(), &JobSpec{Runtime: RuntimeStarlark, Script: script, TimeoutSec: 10})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("loopback 应可达（SSRF 已移除），got status=%q err=%q", res.Status, res.Error)
	}
	if strings.Contains(res.Error, "blocked") {
		t.Fatalf("仍出现 SSRF 拦截语义，守卫疑似被加回: %q", res.Error)
	}
}
