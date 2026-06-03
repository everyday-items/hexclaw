package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

// TestContract_APIError_StandardFields 守住 APIError 序列化契约
// （前端类型 `errors.ts::ApiError` 依赖此结构）。
func TestContract_APIError_StandardFields(t *testing.T) {
	w := httptest.NewRecorder()
	writeAPIError(w, http.StatusBadRequest, CodeCronInvalidSched, "无效调度表达式")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}

	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("非 JSON: %v\n%s", err, w.Body.String())
	}

	// 必须有 code / message / error（兼容） — 锁定 wire format
	for _, key := range []string{"code", "message", "error"} {
		if _, ok := out[key]; !ok {
			t.Errorf("APIError 缺字段 %q\n%s", key, w.Body.String())
		}
	}
	if out["code"] != CodeCronInvalidSched {
		t.Errorf("code mismatch: %v", out["code"])
	}
	if out["message"] != out["error"] {
		t.Errorf("error (legacy) 应 == message")
	}
}

// TestContract_AddCronJob_BadRequest 端到端验 API contract（400 + APIError 结构）。
func TestContract_AddCronJob_BadRequest(t *testing.T) {
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	// 不注入 scheduler，预期返 503
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cron/jobs", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAddCronJob(w, req)
	if w.Code != http.StatusBadRequest {
		// 校验顺序：先 body 校验（name/schedule/prompt 必填） → 后查 scheduler
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var e APIError
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode APIError: %v", err)
	}
	if e.Code != CodeBadRequest {
		t.Errorf("code = %q, want %q", e.Code, CodeBadRequest)
	}
	if e.Message == "" || e.LegacyEr == "" {
		t.Errorf("Message/LegacyEr 不应为空: %+v", e)
	}
}
