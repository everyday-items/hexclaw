package webhook

// GO-4（BUG-20260703）：未启用端点不接受任何数据面写入。
// 原实现在 Enabled 判断前就写 event_count/last_event_at——无 Secret 的端点
// 不验签，任意对端可对未启用端点无限刷写 DB（统计噪声 + 写放大）。
// 契约：未启用 → 验签照跑（错签 401 不设防降级）→ 直接 423，统计零写入。

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBug20260703_DisabledEndpointWritesNoStats(t *testing.T) {
	mgr := newDisabledTestManager(t)
	ctx := context.Background()
	// 已验签（BUG-20260718 fail-closed 后空 Secret 直接 401）+ 默认未启用：
	// 验证已授权对端打未启用端点也只 423、零统计写入。
	secret := "nostats-s3cret"
	if err := mgr.Register(ctx, &Webhook{Name: "nostats", Type: TypeGeneric, Secret: secret, Prompt: "p", UserID: "u"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	payload := []byte(`{"n":1}`)
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("POST", "/api/v1/webhooks/nostats", bytes.NewReader(payload))
		req.Header.Set("X-Webhook-Signature", genericSigHeader(secret, payload))
		if rec := serveWebhook(mgr, req); rec.Code != http.StatusLocked {
			t.Fatalf("未启用端点应 423，得到 %d: %s", rec.Code, rec.Body.String())
		}
	}

	list, err := mgr.List(ctx, "u")
	if err != nil || len(list) != 1 {
		t.Fatalf("List: %v %+v", err, list)
	}
	if list[0].EventCount != 0 {
		t.Errorf("[GO-4] 未启用端点被刷写统计：event_count=%d（应为 0）", list[0].EventCount)
	}
	if !list[0].LastEventAt.IsZero() {
		t.Errorf("[GO-4] 未启用端点被刷写统计：last_event_at=%v（应为零值）", list[0].LastEventAt)
	}
}

// 启用端点统计照常累计（回归守护：Enabled 闸前移不得误伤正常统计）。
func TestBug20260703_EnabledEndpointStillCountsEvents(t *testing.T) {
	mgr := newDisabledTestManager(t)
	ctx := context.Background()
	secret := "counting-s3cret"
	if err := mgr.Register(ctx, &Webhook{Name: "counting", Type: TypeGeneric, Secret: secret, Prompt: "p", UserID: "u", Enabled: true}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	payload := []byte(`{"n":1}`)
	req := httptest.NewRequest("POST", "/api/v1/webhooks/counting", bytes.NewReader(payload))
	req.Header.Set("X-Webhook-Signature", genericSigHeader(secret, payload))
	if rec := serveWebhook(mgr, req); rec.Code != http.StatusOK {
		t.Fatalf("启用端点应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}

	list, err := mgr.List(ctx, "u")
	if err != nil || len(list) != 1 {
		t.Fatalf("List: %v %+v", err, list)
	}
	if list[0].EventCount != 1 {
		t.Errorf("启用端点应计数 1 次，实际 %d", list[0].EventCount)
	}
	if list[0].LastEventAt.IsZero() {
		t.Error("启用端点应更新 last_event_at")
	}
}
