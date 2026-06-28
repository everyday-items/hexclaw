package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// 评审 #5：supervisor 反馈环（迭代 orchestrate）的 RED-first 取证。
//
// 不变量：传 goal + max_rounds>1 时，每轮 fan-out 后由 supervisor 评判总目标是否达成——未达成则
// 派发针对性的后续子任务进入下一轮，直至 done / 无后续 / 触顶 / 预算尽。默认 max_rounds=1 时退化为
// 单轮（不调 supervisor），与既有行为一致。supervisor 输出不可解析时停（不死循环）。

// supExec 区分 fan-out 子（out:<agent>）与 supervisor（按队列返回决策 JSON）。
type supExec struct {
	mu           sync.Mutex
	calls        []string
	supDecisions []string // 每次 supervisor 调用按序返回的决策 JSON
	supIdx       int
}

func (s *supExec) fn(_ context.Context, spec SubAgentSpec) (SubAgentResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, spec.Agent)
	if spec.Agent == supervisorAgentName {
		d := `{"done":true}` // 队列耗尽默认收工
		if s.supIdx < len(s.supDecisions) {
			d = s.supDecisions[s.supIdx]
			s.supIdx++
		}
		return SubAgentResult{Output: d}, nil
	}
	return SubAgentResult{Output: "out:" + spec.Agent}, nil
}

func (s *supExec) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *supExec) called(agent string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.calls {
		if a == agent {
			return true
		}
	}
	return false
}

// 未达成 → supervisor 派后续子任务进入第二轮。
func TestOrchestrate_Supervisor_IteratesUntilDone(t *testing.T) {
	SetOrchestrateSynthesis(false) // 隔离合成
	defer func(o int) { maxSupervisorRounds = o }(maxSupervisorRounds)
	SetMaxSupervisorRounds(3)

	se := &supExec{supDecisions: []string{`{"done":false,"reason":"缺一块","next":[{"agent":"b","task":"补"}]}`}}
	o := NewOrchestrateSkill(se.fn, nil)
	res, err := o.Execute(context.Background(), map[string]any{
		"goal":       "完成总目标",
		"max_rounds": float64(3),
		"subtasks":   []any{map[string]any{"agent": "a0", "task": "t"}},
	})
	if err != nil {
		t.Fatalf("orchestrate 报错：%v", err)
	}
	if !se.called(supervisorAgentName) {
		t.Fatalf("应调 supervisor 评判（calls=%v）", se.calls)
	}
	if !se.called("b") {
		t.Fatalf("supervisor 判未达成应进入第二轮派 b（calls=%v）", se.calls)
	}
	if !strings.Contains(res.Content, "out:b") {
		t.Errorf("正文应含第二轮子产出，得：%s", res.Content)
	}
}

// supervisor 判 done → 停在第一轮，不进第二轮。
func TestOrchestrate_Supervisor_StopsWhenDone(t *testing.T) {
	SetOrchestrateSynthesis(false)
	defer func(o int) { maxSupervisorRounds = o }(maxSupervisorRounds)
	SetMaxSupervisorRounds(3)

	se := &supExec{supDecisions: []string{`{"done":true,"reason":"足够了"}`}}
	o := NewOrchestrateSkill(se.fn, nil)
	_, err := o.Execute(context.Background(), map[string]any{
		"goal":       "g",
		"max_rounds": float64(3),
		"subtasks":   []any{map[string]any{"agent": "a0", "task": "t"}},
	})
	if err != nil {
		t.Fatalf("orchestrate 报错：%v", err)
	}
	// a0 + supervisor(done) = 2，不应有第二轮。
	if se.count() != 2 {
		t.Fatalf("supervisor 判 done 应停在第一轮（calls=%v）", se.calls)
	}
}

// 默认 max_rounds=1 → 单轮，不调 supervisor（回归保护）。
func TestOrchestrate_Supervisor_DefaultSingleRound(t *testing.T) {
	SetOrchestrateSynthesis(false)
	se := &supExec{}
	o := NewOrchestrateSkill(se.fn, nil)
	_, err := o.Execute(context.Background(), map[string]any{
		"subtasks": []any{
			map[string]any{"agent": "a0", "task": "t"},
			map[string]any{"agent": "a1", "task": "t"},
		},
	})
	if err != nil {
		t.Fatalf("orchestrate 报错：%v", err)
	}
	if se.called(supervisorAgentName) {
		t.Fatalf("默认单轮不应调 supervisor（calls=%v）", se.calls)
	}
	if se.count() != 2 {
		t.Fatalf("默认应只跑 2 个子任务，得 %d", se.count())
	}
}

// supervisor 输出不可解析 → 当作收工停止（不死循环）。
func TestOrchestrate_Supervisor_UnparseableStops(t *testing.T) {
	SetOrchestrateSynthesis(false)
	defer func(o int) { maxSupervisorRounds = o }(maxSupervisorRounds)
	SetMaxSupervisorRounds(3)

	se := &supExec{supDecisions: []string{`这不是 JSON，是大白话`}}
	o := NewOrchestrateSkill(se.fn, nil)
	_, err := o.Execute(context.Background(), map[string]any{
		"goal":       "g",
		"max_rounds": float64(3),
		"subtasks":   []any{map[string]any{"agent": "a0", "task": "t"}},
	})
	if err != nil {
		t.Fatalf("orchestrate 报错：%v", err)
	}
	// a0 + supervisor(乱码→停) = 2，不得无限派轮。
	if se.count() != 2 {
		t.Fatalf("supervisor 不可解析应停（calls=%v）", se.calls)
	}
}
