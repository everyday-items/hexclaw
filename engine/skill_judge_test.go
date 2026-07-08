package engine

import (
	"context"
	"errors"
	"testing"

	hexagon "github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
)

// fakeProvider 实现 hexagon.Provider 仅用于测试 judge 逻辑。
type fakeProvider struct {
	resp string
	err  error
}

func (p *fakeProvider) Name() string { return "fake" }
func (p *fakeProvider) Complete(_ context.Context, _ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	if p.err != nil {
		return nil, p.err
	}
	return &hexagon.CompletionResponse{Content: p.resp}, nil
}
func (p *fakeProvider) Stream(_ context.Context, _ hexagon.CompletionRequest) (*hexagon.Stream, error) {
	return nil, nil
}
func (p *fakeProvider) Models() []hexagon.ModelInfo                  { return nil }
func (p *fakeProvider) CountTokens(_ []hexagon.Message) (int, error) { return 0, nil }

func TestNewLLMSkillJudge_NilProviderReturnsTen(t *testing.T) {
	j := NewLLMSkillJudge(nil, "x", LLMSkillJudgeOptions{SampleRate: 1})
	score, _ := j(skill.Execution{SkillName: "math"})
	if score != 10 {
		t.Errorf("nil provider 应返回 10；got=%d", score)
	}
}

func TestNewLLMSkillJudge_LowScore(t *testing.T) {
	p := &fakeProvider{resp: "3\n答非所问"}
	j := NewLLMSkillJudge(p, "m", LLMSkillJudgeOptions{SampleRate: 1})
	score, reason := j(skill.Execution{SkillName: "x"})
	if score != 3 || reason != "答非所问" {
		t.Errorf("解析错；score=%d reason=%q", score, reason)
	}
}

func TestNewLLMSkillJudge_HighScore(t *testing.T) {
	p := &fakeProvider{resp: "9\n完美回答"}
	j := NewLLMSkillJudge(p, "m", LLMSkillJudgeOptions{SampleRate: 1})
	score, _ := j(skill.Execution{SkillName: "x"})
	if score != 9 {
		t.Errorf("got=%d want=9", score)
	}
}

func TestNewLLMSkillJudge_ParseFailFallbackTen(t *testing.T) {
	p := &fakeProvider{resp: "garbage no score here"}
	j := NewLLMSkillJudge(p, "m", LLMSkillJudgeOptions{SampleRate: 1})
	score, _ := j(skill.Execution{SkillName: "x"})
	if score != 10 {
		t.Errorf("解析失败应返回 10；got=%d", score)
	}
}

func TestNewLLMSkillJudge_LLMErrorFailOpen(t *testing.T) {
	p := &fakeProvider{err: errors.New("boom")}
	j := NewLLMSkillJudge(p, "m", LLMSkillJudgeOptions{SampleRate: 1})
	score, _ := j(skill.Execution{SkillName: "x"})
	if score != 10 {
		t.Errorf("LLM 错误应返回 10（fail-open）；got=%d", score)
	}
}

func TestNewLLMSkillJudge_SamplingZeroRateAllSkip(t *testing.T) {
	// SampleRate 0 → 走 default 0.1，但因为 rand 不可预测，这里改用 nil-provider 路径已覆盖
	// 此处验证显式 SampleRate=1 全采样可达
	p := &fakeProvider{resp: "5"}
	j := NewLLMSkillJudge(p, "m", LLMSkillJudgeOptions{SampleRate: 1})
	score, _ := j(skill.Execution{SkillName: "x"})
	if score != 5 {
		t.Errorf("SampleRate=1 应走 LLM；got=%d", score)
	}
}

func TestParseJudgeOutput(t *testing.T) {
	cases := []struct {
		in       string
		want     int
		wantHasR bool
	}{
		{"7\n答案部分正确", 7, true},
		{"7", 7, false},
		{"评分：8\n准确实用", 8, true}, // fallback 全文搜
		{"", 10, false},         // 空 → 10
		{"abc", 10, false},      // 无数字 → 10
	}
	for _, c := range cases {
		score, reason := parseJudgeOutput(c.in)
		if score != c.want {
			t.Errorf("parseJudgeOutput(%q) score=%d want=%d", c.in, score, c.want)
		}
		hasR := reason != ""
		if hasR != c.wantHasR {
			t.Errorf("parseJudgeOutput(%q) reason=%q hasReason=%v want=%v", c.in, reason, hasR, c.wantHasR)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("不超长不截断")
	}
	if got := truncate("0123456789", 5); got != "01234…" {
		t.Errorf("应截断+省略号；got=%s", got)
	}
}
