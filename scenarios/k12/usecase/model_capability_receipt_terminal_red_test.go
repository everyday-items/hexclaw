package usecase

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// K12-FROZEN-MODEL-ROUTE-001：能力回执不匹配在任何模型请求前可确定地失败，
// 不能被误记为网络结果未知或进入自动重试。
func TestFrozenModelCapabilityReceiptErrorIsTerminal(t *testing.T) {
	if sentProviderOutcomeUnknown(k12.ErrModelCapabilityUnverified, nil) {
		t.Fatal("capability receipt error must not become provider outcome unknown")
	}
	if gradingErrRetryable(k12.ErrModelCapabilityUnverified) {
		t.Fatal("capability receipt error must be terminal, not retryable")
	}
}
