package apihttp_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/engineadapter"
)

// newFeedbackServer 用 fake 点评生成闭包（Skill Executor 缝）建服务。
func newFeedbackServer(t *testing.T, fn engineadapter.WorkFeedbackGenerateFunc) http.Handler {
	t.Helper()
	return newServerWithSolver(t, fakeSolveExec{}, assembly.WithWorkFeedbackGenerator(fn))
}

func createWritingWorkHTTP(t *testing.T, h http.Handler) string {
	t.Helper()
	rec, out := do(t, h, "POST", "/creative-works",
		`{"agent":"mingming","work_type":"writing","title":"《春天的校园》","task":"观察春景","content_markdown":"柳枝像绿色的丝带"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建作品: %d %v", rec.Code, out)
	}
	return out["record_id"].(string)
}

// TestGenerateWorkFeedbackHTTP_Writing POST /creative-works/{id}/generate-feedback：
// 写作点评生成 → feedback_ready + feedback_source=ai。
func TestGenerateWorkFeedbackHTTP_Writing(t *testing.T) {
	h := newFeedbackServer(t, func(_ context.Context, subject, prompt, grade string) (string, error) {
		return "「柳枝像绿色的丝带」比喻贴切；建议结尾补一个听觉细节。", nil
	})
	id := createWritingWorkHTTP(t, h)

	rec, out := do(t, h, "POST", "/creative-works/"+id+"/generate-feedback", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || out["status"] != "feedback_ready" {
		t.Fatalf("生成点评: %d %v", rec.Code, out)
	}
	vers := out["versions"].([]any)
	v0 := vers[0].(map[string]any)
	if v0["feedback"] == "" || v0["feedback_source"] != "ai" {
		t.Fatalf("点评应落库且来源为 ai: %v", v0)
	}
	// 追溯契约：方法论基座来源戳随点评透出（本环境无盘上 loader → 内嵌快照）。
	if v0["feedback_skill"] != "writing-feedback@1.0.0/embedded" {
		t.Fatalf("feedback_skill 应透出内嵌来源戳: %v", v0["feedback_skill"])
	}
	structured, _ := v0["structured_feedback"].(map[string]any)
	if _, exists := structured["allowed_actions"]; exists {
		t.Fatalf("退役动作不得残留在结构化点评 DTO: %v", structured["allowed_actions"])
	}
}

// TestGenerateWorkFeedbackHTTP_ScoreRejected INV-011：生成输出含打分 → 拒绝入库，
// 作品保持 draft。
func TestGenerateWorkFeedbackHTTP_ScoreRejected(t *testing.T) {
	h := newFeedbackServer(t, func(context.Context, string, string, string) (string, error) {
		return "写得不错，可以打 90 分。", nil
	})
	id := createWritingWorkHTTP(t, h)

	rec, _ := do(t, h, "POST", "/creative-works/"+id+"/generate-feedback", `{"agent":"mingming"}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("含打分输出应被拒, got %d", rec.Code)
	}
	_, got := do(t, h, "GET", "/creative-works/"+id+"?agent=mingming", "")
	if got["status"] != "draft" {
		t.Fatalf("拒绝后应保持 draft: %v", got)
	}
}

// TestGenerateWorkFeedbackHTTP_ExecutorFailure 生成失败 → 502 诚实报错，不留假点评。
func TestGenerateWorkFeedbackHTTP_ExecutorFailure(t *testing.T) {
	h := newFeedbackServer(t, func(context.Context, string, string, string) (string, error) {
		return "", fmt.Errorf("provider down")
	})
	id := createWritingWorkHTTP(t, h)

	rec, _ := do(t, h, "POST", "/creative-works/"+id+"/generate-feedback", `{"agent":"mingming"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("生成失败应 502, got %d", rec.Code)
	}
	_, got := do(t, h, "GET", "/creative-works/"+id+"?agent=mingming", "")
	if got["status"] != "draft" {
		t.Fatalf("失败后应保持 draft: %v", got)
	}
}

// TestGenerateWorkFeedbackHTTP_StatusGuard feedback_ready 状态再生成 → 409。
func TestGenerateWorkFeedbackHTTP_StatusGuard(t *testing.T) {
	h := newFeedbackServer(t, func(context.Context, string, string, string) (string, error) {
		return "好句在开头；建议结尾具体化。", nil
	})
	id := createWritingWorkHTTP(t, h)
	if rec, out := do(t, h, "POST", "/creative-works/"+id+"/generate-feedback", `{"agent":"mingming"}`); rec.Code != http.StatusOK {
		t.Fatalf("首次生成: %d %v", rec.Code, out)
	}
	rec, _ := do(t, h, "POST", "/creative-works/"+id+"/generate-feedback", `{"agent":"mingming"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("feedback_ready 再生成应 409, got %d", rec.Code)
	}
}

// TestGenerateWorkFeedbackHTTP_OwnerIsolation 归属隔离。
func TestGenerateWorkFeedbackHTTP_OwnerIsolation(t *testing.T) {
	h := newFeedbackServer(t, func(context.Context, string, string, string) (string, error) {
		return "好句在开头；建议结尾具体化。", nil
	})
	id := createWritingWorkHTTP(t, h)
	rec, _ := do(t, h, "POST", "/creative-works/"+id+"/generate-feedback", `{"agent":"other-child"}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("跨实例生成点评应被拒, got %d", rec.Code)
	}
}

// TestGenerateWorkFeedbackHTTP_Unconfigured 未接线生成闭包 → 诚实报错（非 2xx），
// 不产生任何点评。
func TestGenerateWorkFeedbackHTTP_Unconfigured(t *testing.T) {
	h := newServer(t) // 默认装配：无点评生成闭包
	id := createWritingWorkHTTP(t, h)
	rec, _ := do(t, h, "POST", "/creative-works/"+id+"/generate-feedback", `{"agent":"mingming"}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("未配置点评生成能力应报错, got %d", rec.Code)
	}
	_, got := do(t, h, "GET", "/creative-works/"+id+"?agent=mingming", "")
	if got["status"] != "draft" {
		t.Fatalf("未配置时应保持 draft: %v", got)
	}
}
