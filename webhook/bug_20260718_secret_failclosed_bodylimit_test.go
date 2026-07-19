package webhook

// BUG-20260718（测试验收清单 §15 / WH-K12-006/013）：通用 Webhook 两处明确安全 bug。
//   a) 空 Secret 端点「跳过验签」（fail-open）——任意未授权对端可直接触发 Agent 派发；
//      正确行为是 fail-closed：空 Secret 无法验签，入站请求一律拒绝（401），不静默跳过。
//   b) 超限 body 被 io.LimitReader 静默截断——截断后的半个 payload 仍进入解析/派发；
//      正确行为是返回 413（Payload Too Large），不静默截断。
// timestamp/nonce/replay ledger 与图片下载 SSRF 防护为 Tier-2 安全建设，本轮申报缺口不硬做。

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBug20260718_EmptySecretFailsClosed(t *testing.T) {
	mgr := newDisabledTestManager(t)
	ctx := context.Background()
	// 空 Secret 端点（启用态，直指派发风险）。
	if err := mgr.Register(ctx, &Webhook{Name: "nosecret", Type: TypeGeneric, Prompt: "p", UserID: "u", Enabled: true}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var dispatched atomic.Int32
	mgr.SetHandler(func(context.Context, *Event, string) error {
		dispatched.Add(1)
		return nil
	})

	req := httptest.NewRequest("POST", "/api/v1/webhooks/nosecret", bytes.NewReader([]byte(`{"hello":"world"}`)))
	rec := serveWebhook(mgr, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("空 Secret 端点必须 fail-closed 拒绝(401)，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if dispatched.Load() != 0 {
		t.Fatal("空 Secret 端点绝不得派发 Agent（未验签裸奔）")
	}
}

func TestBug20260718_OversizeBodyReturns413NotTruncated(t *testing.T) {
	mgr := newDisabledTestManager(t)
	ctx := context.Background()
	secret := "s3cret"
	if err := mgr.Register(ctx, &Webhook{Name: "big", Type: TypeGeneric, Secret: secret, Prompt: "p", UserID: "u", Enabled: true}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var dispatched atomic.Int32
	mgr.SetHandler(func(context.Context, *Event, string) error {
		dispatched.Add(1)
		return nil
	})

	// 构造超过 maxPayloadSize 的 body。
	oversize := []byte(`{"pad":"` + strings.Repeat("A", maxPayloadSize+1024) + `"}`)
	req := httptest.NewRequest("POST", "/api/v1/webhooks/big", bytes.NewReader(oversize))
	rec := serveWebhook(mgr, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限 body 必须 413 而非静默截断，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if dispatched.Load() != 0 {
		t.Fatal("超限 body 不得进入派发")
	}
}
