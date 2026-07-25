package apihttp_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
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
	rec, out := doCurrent(
		t,
		h,
		http.MethodPost,
		"/creative-works",
		`{"agent":"mingming","work_type":"writing","content_markdown":"柳枝像绿色的丝带"}`,
		map[string]string{"Idempotency-Key": "create-writing-work"},
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建作品: %d %v", rec.Code, out)
	}
	workID, _ := out["work_id"].(string)
	if workID == "" {
		t.Fatalf("创建作品未返回 work_id: %v", out)
	}
	return workID
}

func generateWritingFeedbackHTTP(
	t *testing.T,
	h http.Handler,
	workID, commandKey, agent string,
) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return doCurrent(
		t,
		h,
		http.MethodPost,
		"/creative-works/"+workID+"/generate-feedback",
		fmt.Sprintf(`{"agent":%q}`, agent),
		map[string]string{"Idempotency-Key": commandKey},
	)
}

func currentFeedbackGeneration(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	latest, _ := body["latest_feedback"].(map[string]any)
	if latest != nil {
		return latest
	}
	initial, _ := body["initial_feedback"].(map[string]any)
	if initial == nil {
		t.Fatalf("缺少 current feedback generation: %v", body)
	}
	return initial
}

func TestGenerateWorkFeedbackHTTPWritingReturnsCurrentCanonicalDTO(t *testing.T) {
	h := newFeedbackServer(t, func(context.Context, string, string, string) (string, error) {
		return "「柳枝像绿色的丝带」比喻贴切；建议结尾补一个听觉细节。", nil
	})
	id := createWritingWorkHTTP(t, h)

	rec, out := generateWritingFeedbackHTTP(t, h, id, "feedback-1", "mingming")
	latest := currentFeedbackGeneration(t, out)
	feedback, _ := latest["feedback"].(map[string]any)
	if rec.Code != http.StatusOK ||
		latest["status"] != k12.WorkFeedbackSucceeded ||
		feedback["feedback_id"] == "" ||
		feedback["feedback_type"] != k12.WorkTypeWriting {
		t.Fatalf("生成点评 current DTO: status=%d body=%v", rec.Code, out)
	}
	for _, field := range []string{
		"evidence_refs",
		"visible_evidence",
		"affirmation",
		"parent_guidance",
		"next_step",
		"source_snapshot",
	} {
		if _, exists := feedback[field]; !exists {
			t.Fatalf("current feedback missing %s: %v", field, feedback)
		}
	}
	if _, exists := feedback["allowed_actions"]; exists {
		t.Fatalf("退役动作不得残留在结构化点评 DTO: %v", feedback)
	}
}

func TestGenerateWorkFeedbackHTTPRejectsScoreAndPersistsFailedGeneration(t *testing.T) {
	h := newFeedbackServer(t, func(context.Context, string, string, string) (string, error) {
		return "写得不错，可以打 90 分。", nil
	})
	id := createWritingWorkHTTP(t, h)

	rec, _ := generateWritingFeedbackHTTP(t, h, id, "feedback-score", "mingming")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("含打分输出应被拒, got %d", rec.Code)
	}
	_, got := doCurrent(
		t,
		h,
		http.MethodGet,
		"/creative-works/"+id+"?agent=mingming",
		"",
		nil,
	)
	latest := currentFeedbackGeneration(t, got)
	if latest["status"] != k12.WorkFeedbackFailed || latest["feedback"] != nil {
		t.Fatalf("拒绝后只能留下失败 generation: %v", got)
	}
}

func TestGenerateWorkFeedbackHTTPProviderFailureIsHonest(t *testing.T) {
	h := newFeedbackServer(t, func(context.Context, string, string, string) (string, error) {
		return "", fmt.Errorf("provider down")
	})
	id := createWritingWorkHTTP(t, h)

	rec, _ := generateWritingFeedbackHTTP(t, h, id, "feedback-provider", "mingming")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("生成失败应 502, got %d", rec.Code)
	}
	_, got := doCurrent(
		t,
		h,
		http.MethodGet,
		"/creative-works/"+id+"?agent=mingming",
		"",
		nil,
	)
	if currentFeedbackGeneration(t, got)["status"] != k12.WorkFeedbackFailed {
		t.Fatalf("失败后应保留失败 generation: %v", got)
	}
}

func TestGenerateWorkFeedbackHTTPCommandReplayAndRegeneration(t *testing.T) {
	h := newFeedbackServer(t, func(context.Context, string, string, string) (string, error) {
		return "好句在开头；建议结尾具体化。", nil
	})
	id := createWritingWorkHTTP(t, h)
	rec, first := generateWritingFeedbackHTTP(t, h, id, "initial-generation-resume", "mingming")
	if rec.Code != http.StatusOK {
		t.Fatalf("首次生成: %d %v", rec.Code, first)
	}
	firstID := currentFeedbackGeneration(t, first)["generation_id"]

	rec, regenerated := generateWritingFeedbackHTTP(t, h, id, "feedback-regenerate", "mingming")
	regeneratedID := currentFeedbackGeneration(t, regenerated)["generation_id"]
	if rec.Code != http.StatusOK || regeneratedID == firstID {
		t.Fatalf(
			"首次生成完成后的新命令必须追加 generation: status=%d first=%v regenerated=%v body=%v",
			rec.Code,
			firstID,
			regeneratedID,
			regenerated,
		)
	}

	rec, replay := generateWritingFeedbackHTTP(t, h, id, "feedback-regenerate", "mingming")
	replayID := currentFeedbackGeneration(t, replay)["generation_id"]
	if rec.Code != http.StatusOK ||
		replayID != regeneratedID {
		t.Fatalf(
			"相同命令必须重放同一 generation: status=%d first=%v replay=%v body=%v",
			rec.Code,
			regeneratedID,
			replayID,
			replay,
		)
	}

	rec, next := generateWritingFeedbackHTTP(t, h, id, "feedback-new", "mingming")
	if rec.Code != http.StatusOK ||
		currentFeedbackGeneration(t, next)["generation_id"] == regeneratedID {
		t.Fatalf("另一条新命令必须继续追加 generation: %d %v", rec.Code, next)
	}
}

func TestGenerateWorkFeedbackHTTPOwnerIsolation(t *testing.T) {
	h := newFeedbackServer(t, func(context.Context, string, string, string) (string, error) {
		return "好句在开头；建议结尾具体化。", nil
	})
	id := createWritingWorkHTTP(t, h)
	rec, _ := generateWritingFeedbackHTTP(t, h, id, "feedback-other", "other-child")
	if rec.Code == http.StatusOK {
		t.Fatalf("跨实例生成点评应被拒, got %d", rec.Code)
	}
}

func TestGenerateWorkFeedbackHTTPUnconfiguredPersistsNoFakeFeedback(t *testing.T) {
	h := newServer(t)
	id := createWritingWorkHTTP(t, h)
	rec, _ := generateWritingFeedbackHTTP(t, h, id, "feedback-unconfigured", "mingming")
	if rec.Code == http.StatusOK {
		t.Fatalf("未配置点评生成能力应报错, got %d", rec.Code)
	}
	_, got := doCurrent(
		t,
		h,
		http.MethodGet,
		"/creative-works/"+id+"?agent=mingming",
		"",
		nil,
	)
	latest := currentFeedbackGeneration(t, got)
	if latest["status"] != k12.WorkFeedbackFailed || latest["feedback"] != nil {
		t.Fatalf("未配置时不得产生假点评: %v", got)
	}
}
