package cron

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Bug 2026-06-22（cron「百度热搜采集」失败）：http_get 后 json_decode 报
//
//	Error in json_decode: json_decode: invalid character '<' looking for beginning of value
//
// 根因：builtinHTTP 从不设置默认 User-Agent，e.client.Do 发出 Go 默认 UA
// "Go-http-client/1.1"。百度热搜等站点对非浏览器 UA 返回反爬 HTML（以 '<' 开头），
// 脚本 json_decode 解析 HTML 即失败。
//
// 本测试用一个"按 UA 分流"的 loopback 服务器精确复现该症状：非浏览器 UA → HTML；
// 浏览器 UA → JSON。修复前 http_get 拿到 HTML → json_decode 失败（RED）；
// 修复后 http_get 默认带浏览器 UA → 拿到 JSON → 脚本成功（GREEN）。
// 永久保留作回归锁，防止有人去掉默认 UA。
func TestBug20260622_StarlarkHTTPGet_SetsDefaultUserAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		if ua == "" || strings.Contains(ua, "Go-http-client") {
			// 反爬：非浏览器 UA → HTML（复现 json_decode 拿到 '<' 的症状）
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<!DOCTYPE html><html><body>anti-bot</body></html>"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	eng := NewStarlarkEngine()
	// 与 job.star 同构：http_get(url) → json_decode(resp["body"])。
	script := "def run():\n" +
		"    resp = http_get(\"" + srv.URL + "\")\n" +
		"    data = json_decode(resp[\"body\"])\n" +
		"    return {\"status\": \"success\", \"data\": data[\"ok\"]}\n" +
		"emit(run())"
	res, err := eng.Execute(context.Background(), &JobSpec{Runtime: RuntimeStarlark, Script: script, TimeoutSec: 10})
	if err != nil {
		t.Fatalf("Execute 基础设施错误: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("http_get 应带默认浏览器 User-Agent，避免反爬站返回 HTML 致 json_decode 失败；got status=%q err=%q", res.Status, res.Error)
	}
}
