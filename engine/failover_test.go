package engine

import (
	"context"
	"errors"
	"testing"
)

// TestFailoverClassify_Fix_v0_3_12_H2 回归测试：
// 修复前所有上游错误统一走 isTransientError 布尔判定 → 全部退避重试。
// 但 402（余额）/ 401（凭证）/ 404（模型不存在）等重试无意义，浪费 Token + 延长用户等待。
// 修复后按 FailoverReason 分类，每类对应不同策略。
func TestFailoverClassify_Fix_v0_3_12_H2(t *testing.T) {
	t.Run("before_fix_behavior_all_errors_retry_blindly", func(t *testing.T) {
		// 修复前 isTransientError 只区分 transient 与否：
		// 402 / 401 / 404 都可能被误判为 transient 并重试，浪费资源
		t.Logf("修复前：isTransientError 只返回布尔，无法区分 402/401/404 等不可重试错误")
	})

	cases := []struct {
		name       string
		err        error
		httpStatus int
		body       string
		want       FailoverReason
	}{
		{"http_429_rate_limit", nil, 429, "", FailRateLimit},
		{"http_402_quota", nil, 402, "insufficient balance", FailQuotaExceeded},
		{"http_400_context_length", nil, 400, "context_length_exceeded", FailContextTooLong},
		{"http_400_other", nil, 400, "invalid json", FailUnknown},
		{"http_401_invalid_key", nil, 401, "", FailInvalidKey},
		{"http_403_forbidden", nil, 403, "", FailInvalidKey},
		{"http_404_model_not_found", nil, 404, "model not found", FailModelNotFound},
		{"http_503_provider_down", nil, 503, "", FailProviderDown},
		{"http_504_provider_down", nil, 504, "", FailProviderDown},
		{"err_context_deadline", context.DeadlineExceeded, 0, "", FailProviderDown},
		{"err_context_canceled", context.Canceled, 0, "", FailProviderDown},
		{"err_msg_rate_limit", errors.New("too many requests"), 0, "", FailRateLimit},
		{"err_msg_insufficient_balance_zh", errors.New("账号余额不足"), 0, "", FailQuotaExceeded},
		{"err_msg_context_window", errors.New("maximum context window exceeded"), 0, "", FailContextTooLong},
		{"err_msg_unauthorized", errors.New("Invalid API key provided"), 0, "", FailInvalidKey},
		{"err_msg_model_not_found", errors.New("model 'xxx' does not exist"), 0, "", FailModelNotFound},
		{"err_msg_timeout", errors.New("request timeout"), 0, "", FailProviderDown},
		{"err_msg_connection", errors.New("connection refused"), 0, "", FailProviderDown},
		{"nil_err_no_status", nil, 0, "", FailNone},
		{"unknown_error", errors.New("unexpected glitch"), 0, "", FailUnknown},
	}
	for _, tc := range cases {
		t.Run("after_fix_classify_"+tc.name, func(t *testing.T) {
			got := ClassifyError(tc.err, tc.httpStatus, tc.body)
			if got != tc.want {
				t.Errorf("ClassifyError(%v, %d, %q) = %s, want %s",
					tc.err, tc.httpStatus, tc.body, got, tc.want)
			}
		})
	}
}

// TestFailoverAction_Strategy_Fix_v0_3_12_H2 验证每种 FailoverReason → 推荐 Action 的策略路由
func TestFailoverAction_Strategy_Fix_v0_3_12_H2(t *testing.T) {
	t.Run("rate_limit_retries_with_backoff", func(t *testing.T) {
		a := HandleFailover(FailRateLimit)
		if !a.Retry || a.BackoffSeconds == 0 {
			t.Errorf("期望 RateLimit 退避重试，实际 Retry=%v Backoff=%d", a.Retry, a.BackoffSeconds)
		}
	})

	t.Run("quota_switches_credential", func(t *testing.T) {
		a := HandleFailover(FailQuotaExceeded)
		if !a.SwitchCredential {
			t.Error("期望 QuotaExceeded 切换凭证")
		}
	})

	t.Run("context_too_long_compresses", func(t *testing.T) {
		a := HandleFailover(FailContextTooLong)
		if !a.CompressContext {
			t.Error("期望 ContextTooLong 压缩 context")
		}
	})

	t.Run("provider_down_switches_provider", func(t *testing.T) {
		a := HandleFailover(FailProviderDown)
		if !a.SwitchProvider {
			t.Error("期望 ProviderDown 切换 Provider")
		}
	})

	t.Run("invalid_key_stops_retry", func(t *testing.T) {
		a := HandleFailover(FailInvalidKey)
		if a.Retry {
			t.Error("期望 InvalidKey 不重试（重试无意义）")
		}
		if a.UserFacing == "" {
			t.Error("期望给用户可见提示")
		}
	})

	t.Run("model_not_found_stops_retry", func(t *testing.T) {
		a := HandleFailover(FailModelNotFound)
		if a.Retry {
			t.Error("期望 ModelNotFound 不重试")
		}
	})

	t.Run("unknown_retries_once", func(t *testing.T) {
		a := HandleFailover(FailUnknown)
		if !a.Retry || a.MaxRetries != 1 {
			t.Errorf("期望 Unknown 重试 1 次，实际 Retry=%v MaxRetries=%d", a.Retry, a.MaxRetries)
		}
	})
}

// TestFailoverReason_String_Fix_v0_3_12_H2 便于日志/测试断言
func TestFailoverReason_String_Fix_v0_3_12_H2(t *testing.T) {
	pairs := map[FailoverReason]string{
		FailNone:           "none",
		FailRateLimit:      "rate_limit",
		FailQuotaExceeded:  "quota_exceeded",
		FailContextTooLong: "context_too_long",
		FailProviderDown:   "provider_down",
		FailInvalidKey:     "invalid_key",
		FailModelNotFound:  "model_not_found",
		FailUnknown:        "unknown",
	}
	for r, want := range pairs {
		if got := r.String(); got != want {
			t.Errorf("%v.String() = %q, want %q", int(r), got, want)
		}
	}
}
