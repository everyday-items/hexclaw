package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

// BUG-20260625 (R3 review-fullstack H-1 · AP-051-R/AP-001 producer-only 半修):
// webhook 绑定 cron job 时 prompt 可空（JobID 非空 → 事件触发指定 job，不跑 prompt，见 RegisterWebhookRequest.Prompt 注释），
// 但 handleRegisterWebhook 无条件要求 prompt 非空 → 绑 job 的 webhook 留空 prompt 被 400 拒，端到端断裂。
// 真实 httptest（非 mock）取证：job_id 非空 + prompt 空，必须越过校验（nil mgr → 503"未启用"，而非 400"prompt 不能为空"）。
func TestHandleRegisterWebhook_JobBoundEmptyPrompt_20260625(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil) // webhookMgr nil

	// job_id 非空、prompt 空 —— 业务上合法
	body := `{"name":"job-hook","type":"generic","job_id":"cron-123"}`
	req := httptest.NewRequest("POST", "/api/v1/webhooks", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleRegisterWebhook(w, req)

	if w.Code == http.StatusBadRequest && strings.Contains(w.Body.String(), "prompt") {
		t.Fatalf("绑 job 的 webhook 留空 prompt 被校验拒（端到端断裂）: %d %s", w.Code, w.Body.String())
	}
	// 越过校验后落到 nil manager → 503"未启用"，证明 prompt 校验已对 job-bound 豁免
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("期望越过 prompt 校验后到 503(manager nil)，实际 %d %s", w.Code, w.Body.String())
	}
}

// 反例守卫：无 job_id 时 prompt 仍必填（job-bound 豁免不得误伤普通 webhook）。
func TestHandleRegisterWebhook_NoJobEmptyPrompt_Still400_20260625(t *testing.T) {
	cfg := config.DefaultConfig()
	eng := &mockEngine{reply: &adapter.Reply{Content: "ok"}}
	srv := NewServer(cfg, eng, nil, nil)

	body := `{"name":"plain-hook","type":"generic"}` // 无 job_id 无 prompt
	req := httptest.NewRequest("POST", "/api/v1/webhooks", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleRegisterWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("无 job_id 且空 prompt 应仍 400，实际 %d %s", w.Code, w.Body.String())
	}
}
