package render

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestR11_RU13_FetchToDataURL_BlocksLoopback 验证 fetchToDataURL 在拉取前做 SSRF 校验。
//
// 真断言（非弱断言）——关键在于目标服务器真实可达且返回 200 图片：
//   - 启一个 httptest server（监听 127.0.0.1，属私网/loopback）返回合法 PNG；
//   - RED（无 SSRF 校验）下 fetchToDataURL 会成功连接并返回 data URL、err==nil ——
//     即"仅凭 err!=nil 判断"会被这个真实可达的私网服务器骗过，所以必须断言"被拒且
//     拒绝原因是 SSRF 校验"，而非任何网络错误；
//   - GREEN（接入 ssrf.ValidateURL）下必须在发起请求前就以 SSRF 原因拒绝。
func TestR11_RU13_FetchToDataURL_BlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		// 最小 PNG 魔数字节即可，fetchToDataURL 不校验图片内容
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n"))
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	_, err := fetchToDataURL(context.Background(), srv.URL, client, 1<<20)

	if err == nil {
		t.Fatalf("BUG RU-13: fetchToDataURL 成功拉取了 loopback 私网地址 %s（应被 SSRF 拦截）", srv.URL)
	}
	if !strings.Contains(err.Error(), "SSRF") {
		t.Fatalf("期望 SSRF 拒绝（含 \"SSRF\"），实际错误: %v —— 若这是网络错误说明未接入前置校验", err)
	}
}

// TestR11_RU13_FetchToDataURL_BlocksMetadataEndpoint 验证云元数据端点被前置拒绝。
//
// 真断言：169.254.169.254 是 AWS/Azure/GCP 元数据端点（凭据泄漏首要目标）。
//   - RED 下无校验会真的去 dial 该地址，最终以网络超时/连接错误失败（不含 "SSRF"）→ FAIL；
//   - GREEN 下 ssrf.ValidateURL 在发请求前即以 SSRF 原因拒绝（含 "SSRF"）→ PASS。
//
// 用短超时确保 RED 不会长时间挂起。
func TestR11_RU13_FetchToDataURL_BlocksMetadataEndpoint(t *testing.T) {
	client := &http.Client{Timeout: 1 * time.Second}
	const metadataURL = "http://169.254.169.254/latest/meta-data/"
	_, err := fetchToDataURL(context.Background(), metadataURL, client, 1<<20)

	if err == nil {
		t.Fatalf("BUG RU-13: fetchToDataURL 未拒绝元数据端点 %s", metadataURL)
	}
	if !strings.Contains(err.Error(), "SSRF") {
		t.Fatalf("期望 SSRF 拒绝（含 \"SSRF\"），实际错误: %v —— 说明是网络失败而非前置 SSRF 校验", err)
	}
}
