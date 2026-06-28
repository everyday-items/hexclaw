package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// extractSubAgentReports 从工具结果里抠出哨兵块并反序列化（模拟前端解析），测试复用。
func extractSubAgentReports(t *testing.T, content string) []SubAgentReport {
	t.Helper()
	open := "```" + subAgentSentinelLang + "\n"
	i := strings.Index(content, open)
	if i < 0 {
		t.Fatalf("结果里没有 %s 哨兵块：\n%s", subAgentSentinelLang, content)
	}
	rest := content[i+len(open):]
	j := strings.Index(rest, "\n```")
	if j < 0 {
		t.Fatalf("哨兵块未闭合：\n%s", content)
	}
	var reports []SubAgentReport
	if err := json.Unmarshal([]byte(rest[:j]), &reports); err != nil {
		t.Fatalf("哨兵块 JSON 解析失败：%v\n%s", err, rest[:j])
	}
	return reports
}

func TestEncodeSubAgentReports_Empty(t *testing.T) {
	if got := encodeSubAgentReports(nil); got != "" {
		t.Errorf("空回执应返回空串，得 %q", got)
	}
}

func TestNewSubAgentReport_Status(t *testing.T) {
	ok := newSubAgentReport("coder", "done", nil, 1500*time.Millisecond)
	if ok.Status != subAgentStatusOK || ok.Duration != "1.5s" || ok.Output != "done" {
		t.Errorf("ok 回执异常：%+v", ok)
	}
	errRep := newSubAgentReport("coder", "partial", errors.New("boom"), 500*time.Millisecond)
	if errRep.Status != subAgentStatusError || errRep.Error != "boom" || errRep.Output != "" {
		t.Errorf("error 回执应清空 output 并带 error：%+v", errRep)
	}
	if errRep.Duration != "500ms" {
		t.Errorf("亚秒耗时应为 500ms，得 %q", errRep.Duration)
	}
	toRep := newSubAgentReport("coder", "", context.DeadlineExceeded, time.Second)
	if toRep.Status != subAgentStatusTimeout {
		t.Errorf("deadline 错误应判定 timeout，得 %q", toRep.Status)
	}
}

// ---- orchestrate / spawn 必须发出可解析的哨兵块 ----

func TestOrchestrate_EmitsParseableSentinel(t *testing.T) {
	rec := &recordingExec{}
	o := NewOrchestrateSkill(rec.fn, nil)
	res, err := o.Execute(context.Background(), map[string]any{"subtasks": []any{
		map[string]any{"agent": "researcher", "task": "t1"},
		map[string]any{"agent": "coder", "task": "t2"},
	}})
	if err != nil {
		t.Fatalf("orchestrate 报错：%v", err)
	}
	reports := extractSubAgentReports(t, res.Content)
	if len(reports) != 2 {
		t.Fatalf("应有 2 份子 Agent 回执，得 %d", len(reports))
	}
	byName := map[string]SubAgentReport{}
	for _, r := range reports {
		byName[r.Agent] = r
	}
	for _, name := range []string{"researcher", "coder"} {
		r, ok := byName[name]
		if !ok {
			t.Fatalf("缺 %s 回执", name)
		}
		if r.Status != subAgentStatusOK {
			t.Errorf("%s 状态应 ok，得 %q", name, r.Status)
		}
		if !strings.Contains(r.Output, "out:"+name) {
			t.Errorf("%s 回执应含其输出，得 %q", name, r.Output)
		}
	}
}

func TestSpawn_EmitsParseableSentinel(t *testing.T) {
	rec := &recordingExec{}
	s := NewSpawnSkill(rec.fn, nil)
	res, err := s.Execute(context.Background(), map[string]any{"agent_name": "coder", "task": "x"})
	if err != nil {
		t.Fatalf("spawn 报错：%v", err)
	}
	reports := extractSubAgentReports(t, res.Content)
	if len(reports) != 1 {
		t.Fatalf("spawn 应有 1 份回执，得 %d", len(reports))
	}
	if reports[0].Agent != "coder" || reports[0].Status != subAgentStatusOK {
		t.Errorf("spawn 回执异常：%+v", reports[0])
	}
	if !strings.Contains(reports[0].Output, "out:coder") {
		t.Errorf("spawn 回执应含输出，得 %q", reports[0].Output)
	}
}
