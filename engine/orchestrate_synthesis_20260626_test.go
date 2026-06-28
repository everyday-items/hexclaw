package engine

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fanoutSubtasks 造 n 个最简子任务的 orchestrate 参数（agent=a0..a{n-1}，task=t）。
func fanoutSubtasks(n int) map[string]any {
	subs := make([]any, n)
	for i := 0; i < n; i++ {
		subs[i] = map[string]any{"agent": "a" + strconv.Itoa(i), "task": "t"}
	}
	return map[string]any{"subtasks": subs}
}

// 评审 #4：orchestrate reduce 合成（synthesizer + 冲突检测）的 RED-first 取证。
//
// 不变量：≥2 个成功子产出时，orchestrate 不再裸拼接，而是再派一个「synthesizer」归并为
// 一份连贯结论（冲突检测在 prompt 内）；归并失败/单结果/未开启时回退裸拼接（永不更差）。

// synthExec 区分 fan-out 子（返回 out:<agent>）与 synthesizer（返回归并串/错误）。
type synthExec struct {
	mu       sync.Mutex
	calls    []string
	synthOut string
	synthErr error
}

func (s *synthExec) fn(_ context.Context, spec SubAgentSpec) (SubAgentResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, spec.Agent)
	s.mu.Unlock()
	if spec.Agent == synthesizerAgentName {
		if s.synthErr != nil {
			return SubAgentResult{}, s.synthErr
		}
		return SubAgentResult{Output: s.synthOut}, nil
	}
	return SubAgentResult{Output: "out:" + spec.Agent}, nil
}

func (s *synthExec) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *synthExec) called(agent string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.calls {
		if a == agent {
			return true
		}
	}
	return false
}

// ≥2 成功子 → 派 synthesizer 归并，正文用归并结果（而非裸拼接）。
func TestOrchestrate_Synthesis_MergesResults(t *testing.T) {
	defer SetOrchestrateSynthesis(false)
	SetOrchestrateSynthesis(true)
	defer func(old int) { maxOrchestrateConcurrency = old }(maxOrchestrateConcurrency)
	SetMaxOrchestrateConcurrency(2)

	se := &synthExec{synthOut: "SYNTH_MERGED_RESULT"}
	o := NewOrchestrateSkill(se.fn, nil)
	res, err := o.Execute(context.Background(), fanoutSubtasks(2))
	if err != nil {
		t.Fatalf("orchestrate 报错：%v", err)
	}
	if !se.called(synthesizerAgentName) {
		t.Fatalf("≥2 子产出应派 synthesizer 归并，未派（calls=%v）", se.calls)
	}
	if se.count() != 3 {
		t.Fatalf("应为 2 子 + 1 归并 = 3 次执行，实得 %d", se.count())
	}
	if !strings.Contains(res.Content, "SYNTH_MERGED_RESULT") {
		t.Errorf("正文应承载归并结果，得：%s", res.Content)
	}
}

// 未开启（默认/本地）→ 裸拼接，不派 synthesizer。
func TestOrchestrate_Synthesis_DisabledConcatenates(t *testing.T) {
	SetOrchestrateSynthesis(false)
	se := &synthExec{synthOut: "SHOULD_NOT_APPEAR"}
	o := NewOrchestrateSkill(se.fn, nil)
	res, err := o.Execute(context.Background(), fanoutSubtasks(2))
	if err != nil {
		t.Fatalf("orchestrate 报错：%v", err)
	}
	if se.called(synthesizerAgentName) {
		t.Fatalf("未开启时不应派 synthesizer（calls=%v）", se.calls)
	}
	if !strings.Contains(res.Content, "out:a0") || strings.Contains(res.Content, "SHOULD_NOT_APPEAR") {
		t.Errorf("未开启应裸拼接子产出，得：%s", res.Content)
	}
}

// 归并失败 → 回退裸拼接（永不更差）。
func TestOrchestrate_Synthesis_FallsBackOnError(t *testing.T) {
	defer SetOrchestrateSynthesis(false)
	SetOrchestrateSynthesis(true)
	se := &synthExec{synthErr: errors.New("synth boom")} // 非瞬时错误，不重试
	o := NewOrchestrateSkill(se.fn, nil)
	res, err := o.Execute(context.Background(), fanoutSubtasks(2))
	if err != nil {
		t.Fatalf("orchestrate 报错：%v", err)
	}
	if se.count() != 3 {
		t.Fatalf("应尝试归并（2 子 + 1 synth），实得 %d", se.count())
	}
	if !strings.Contains(res.Content, "out:a0") || !strings.Contains(res.Content, "out:a1") {
		t.Errorf("归并失败应回退裸拼接保留子产出，得：%s", res.Content)
	}
}

// 单结果无需归并 → 不派 synthesizer。
func TestOrchestrate_Synthesis_SkipsSingleResult(t *testing.T) {
	defer SetOrchestrateSynthesis(false)
	SetOrchestrateSynthesis(true)
	se := &synthExec{synthOut: "X"}
	o := NewOrchestrateSkill(se.fn, nil)
	res, err := o.Execute(context.Background(), fanoutSubtasks(1))
	if err != nil {
		t.Fatalf("orchestrate 报错：%v", err)
	}
	if se.called(synthesizerAgentName) {
		t.Fatalf("单结果不应归并（calls=%v）", se.calls)
	}
	if !strings.Contains(res.Content, "out:a0") {
		t.Errorf("单结果应直接透出子产出，得：%s", res.Content)
	}
}
