package k12

import (
	"encoding/json"
	"strings"
	"testing"
)

// 领域层契约：统一 GradingJob 状态机（架构设计 §6.7 状态机图 + 硬规则 1/4/5/6/7）。
// 转移偏序直接对照 mermaid 图逐边核验；捷径边（queued→非 normalizing）由规则 3/6 授权。

func hasEdge(tr map[string][]string, from, to string) bool {
	for _, v := range tr[from] {
		if v == to {
			return true
		}
	}
	return false
}

// TestGradingJobSchemaLegalEdges §6.7 状态机图的每条合法边都必须声明。
func TestGradingJobSchemaLegalEdges(t *testing.T) {
	tr := GradingJobSchema().Transitions
	if !hasEdge(tr, GradingStageAwaitingConfirmation, GradingStageOutcomeUnknown) {
		t.Error("局部题调用结果未知时必须保留 outcome_unknown 停点")
	}
	legal := [][2]string{
		{GradingStageQueued, GradingStageNormalizing},
		{GradingStageQueued, GradingStageAssessing}, // 规则 6：修正重批·检查点已预置捷径
		{GradingStageNormalizing, GradingStageRecognizing},
		{GradingStageRecognizing, GradingStageAwaitingConfirmation},
		{GradingStageRecognizing, GradingStageLocating}, // 规则 1：并行启动锚点增强
		{GradingStageLocating, GradingStageAssessing},
		{GradingStageAwaitingConfirmation, GradingStageAssessing},
		{GradingStageAssessing, GradingStageRendering},
		{GradingStageRendering, GradingStageProjecting},
		{GradingStageProjecting, GradingStageCompleted},
		// 可取消态 → cancelled（规则 5/7）
		{GradingStageQueued, GradingStageCancelled},
		{GradingStageNormalizing, GradingStageCancelled},
		{GradingStageRecognizing, GradingStageCancelled},
		{GradingStageLocating, GradingStageCancelled},
		{GradingStageAwaitingConfirmation, GradingStageCancelled},
		{GradingStageAssessing, GradingStageCancelled},
		// 自动阶段 → failed_retryable（rendering 除外，规则 2）
		{GradingStageNormalizing, GradingStageFailedRetryable},
		{GradingStageRecognizing, GradingStageFailedRetryable},
		{GradingStageLocating, GradingStageFailedRetryable},
		{GradingStageAssessing, GradingStageFailedRetryable},
		{GradingStageProjecting, GradingStageFailedRetryable},
		{GradingStageFailedRetryable, GradingStageQueued},
		{GradingStageFailedRetryable, GradingStageFailedTerminal},
	}
	for _, e := range legal {
		if !hasEdge(tr, e[0], e[1]) {
			t.Errorf("§6.7 合法边缺失: %s→%s", e[0], e[1])
		}
	}
}

// TestGradingJobSchemaIllegalEdges 图上没有的边必须被拒绝（含规则 2/5 的取消与失败边界）。
func TestGradingJobSchemaIllegalEdges(t *testing.T) {
	tr := GradingJobSchema().Transitions
	illegal := [][2]string{
		{GradingStageNormalizing, GradingStageAssessing},                // 跳阶段
		{GradingStageRendering, GradingStageCancelled},                  // 规则 5：rendering 不可取消
		{GradingStageProjecting, GradingStageCancelled},                 // 规则 5：projecting 不可取消
		{GradingStageRendering, GradingStageFailedRetryable},            // 规则 2：rendering 失败降级续跑，不进失败态
		{GradingStageRendering, GradingStageFailedTerminal},             // 规则 2
		{GradingStageCompleted, GradingStageQueued},                     // 终态不可离开（规则 6：修正=新 Job）
		{GradingStageCompleted, GradingStageAssessing},                  //
		{GradingStageCancelled, GradingStageQueued},                     // 终态
		{GradingStageFailedTerminal, GradingStageQueued},                // 终态
		{GradingStageAssessing, GradingStageAwaitingConfirmation},       // 不可回退
		{GradingStageAwaitingConfirmation, GradingStageFailedRetryable}, // 人工等待态无自动失败（规则 7）
	}
	for _, e := range illegal {
		if hasEdge(tr, e[0], e[1]) {
			t.Errorf("§6.7 非法边不应声明: %s→%s", e[0], e[1])
		}
	}
	// 三个终态在 map 中显式收口为空集
	for _, terminal := range []string{GradingStageCompleted, GradingStageCancelled, GradingStageFailedTerminal} {
		if len(tr[terminal]) != 0 {
			t.Errorf("终态 %s 不得有出边: %v", terminal, tr[terminal])
		}
	}
}

// TestGradingStageCancellable 规则 5 + 规则 7：可取消态判定。
func TestGradingStageCancellable(t *testing.T) {
	for _, s := range []string{
		GradingStageQueued, GradingStageNormalizing, GradingStageRecognizing,
		GradingStageLocating, GradingStageAwaitingConfirmation, GradingStageAssessing,
	} {
		if !GradingStageCancellable(s) {
			t.Errorf("%s 应可取消", s)
		}
	}
	for _, s := range []string{
		GradingStageRendering, GradingStageProjecting, GradingStageCompleted,
		GradingStageCancelled, GradingStageFailedRetryable, GradingStageFailedTerminal,
	} {
		if GradingStageCancellable(s) {
			t.Errorf("%s 不应可取消", s)
		}
	}
}

// TestBuildGradingIdempotencyKey §4.10：幂等键含创建来源与 confirmed_version，修正后键变化。
func TestBuildGradingIdempotencyKey(t *testing.T) {
	k0 := BuildGradingIdempotencyKey("im", "msg-1", 0)
	k1 := BuildGradingIdempotencyKey("im", "msg-1", 1)
	if k0 == k1 {
		t.Fatalf("confirmed_version 变化必须产生新幂等键（规则 6）")
	}
	if !strings.Contains(k0, "im") || !strings.Contains(k0, "msg-1") {
		t.Errorf("幂等键应关联创建来源: %q", k0)
	}
	if BuildGradingIdempotencyKey("im", "msg-1", 0) != k0 {
		t.Errorf("同输入幂等键必须稳定")
	}
	if BuildGradingIdempotencyKey("desktop", "msg-1", 0) == k0 {
		t.Errorf("不同来源不得撞键")
	}
}

// TestGradingResumeStage 规则 3：从最近成功阶段恢复，禁止重跑已成功阶段；
// 规则 6：检查点预置到确认后 → queued 起跑直达 assessing。
func TestGradingResumeStage(t *testing.T) {
	cp := func(stages ...string) []GradingStageCheckpoint {
		out := make([]GradingStageCheckpoint, 0, len(stages))
		for _, s := range stages {
			out = append(out, GradingStageCheckpoint{Stage: s, ArtifactDigest: "d-" + s})
		}
		return out
	}
	cases := []struct {
		name string
		cps  []GradingStageCheckpoint
		want string
	}{
		{"无检查点从头跑", nil, GradingStageNormalizing},
		{"固化完成后识别", cp(GradingStageNormalizing), GradingStageRecognizing},
		{"识别完成后等确认", cp(GradingStageNormalizing, GradingStageRecognizing), GradingStageAwaitingConfirmation},
		{"确认已冻结直达批改", cp(GradingStageNormalizing, GradingStageRecognizing, GradingStageAwaitingConfirmation), GradingStageAssessing},
		{"锚点检查点不影响主链", cp(GradingStageNormalizing, GradingStageRecognizing, GradingStageLocating, GradingStageAwaitingConfirmation), GradingStageAssessing},
		{"批改完成后渲染", cp(GradingStageNormalizing, GradingStageRecognizing, GradingStageAwaitingConfirmation, GradingStageAssessing), GradingStageRendering},
		{"渲染完成后投影", cp(GradingStageNormalizing, GradingStageRecognizing, GradingStageAwaitingConfirmation, GradingStageAssessing, GradingStageRendering), GradingStageProjecting},
	}
	for _, c := range cases {
		if got := GradingResumeStage(c.cps); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

// TestGradingJobValidateFields §5.4 字段表：必填与枚举校验。
func TestGradingJobValidateFields(t *testing.T) {
	valid := GradingJobFields{
		SubmissionID:      "sub-1",
		SourceKind:        "im",
		IdempotencyKey:    BuildGradingIdempotencyKey("im", "msg-1", 0),
		ConfirmationState: GradingConfirmationPending,
		AnchorState:       GradingAnchorPending,
		ModelSnapshot:     GradingModelSnapshot{Provider: "openrouter", Model: "test-vlm"},
	}
	mustJSON := func(f GradingJobFields) string {
		raw, _ := json.Marshal(f)
		return string(raw)
	}
	if err := GradingJobSchema().ValidateFields(mustJSON(valid)); err != nil {
		t.Fatalf("合法字段不应报错: %v", err)
	}
	bad := []func(f *GradingJobFields){
		func(f *GradingJobFields) { f.SubmissionID = "" },
		func(f *GradingJobFields) { f.IdempotencyKey = "" },
		func(f *GradingJobFields) { f.ConfirmationState = "maybe" },
		func(f *GradingJobFields) { f.AnchorState = "ready" }, // §6.7 拆分裁决枚举为 pending/located/degraded
		func(f *GradingJobFields) { f.ModelSnapshot = GradingModelSnapshot{} },
		func(f *GradingJobFields) { f.ConfirmedVersion = -1 },
	}
	for i, mutate := range bad {
		f := valid
		mutate(&f)
		if err := GradingJobSchema().ValidateFields(mustJSON(f)); err == nil {
			t.Errorf("非法字段用例 #%d 应被拒绝", i)
		}
	}
}
