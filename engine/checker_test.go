package engine

import (
	"context"
	"errors"
	"testing"
)

func TestCheckerAcceptanceMode(t *testing.T) {
	ctx := context.Background()
	c := NewChecker(NewAcceptanceEngine(), nil, 1)
	goal := Goal{Intent: "整理周报", Acceptance: []AcceptanceSpec{{Kind: AcceptContains, Value: "周报"}}}

	v := c.Assess(ctx, goal, "本周周报已生成")
	if !v.Done || v.Mode != "acceptance" || v.Confidence <= 0.5 {
		t.Fatalf("形式化验收通过应高置信 done: %+v", v)
	}
	v = c.Assess(ctx, goal, "没东西")
	if v.Done || v.Mode != "acceptance" {
		t.Fatalf("形式化验收不过应 not done: %+v", v)
	}
}

func TestCheckerVoteMajority(t *testing.T) {
	ctx := context.Background()
	// 3 次投票：DONE, DONE, NOT_DONE → 多数 DONE，置信 2/3
	seq := []string{"DONE\n看起来齐了", "DONE\nok", "NOT_DONE\n还差图"}
	i := 0
	judge := LLMJudgeFunc(func(_ context.Context, _ string) (string, error) {
		r := seq[i%len(seq)]
		i++
		return r, nil
	})
	c := NewChecker(nil, judge, 3)
	v := c.Assess(ctx, Goal{Intent: "写篇稿子"}, "一篇稿子")
	if !v.Done || v.Mode != "llm_vote" {
		t.Fatalf("2/3 DONE 应判 done: %+v", v)
	}
	if v.Confidence < 0.66 || v.Confidence > 0.67 {
		t.Fatalf("置信度应约 2/3: %v", v.Confidence)
	}
	if len(v.Votes) != 3 {
		t.Fatalf("应有 3 票: %+v", v.Votes)
	}
}

func TestCheckerVoteMinorityAndFailClosed(t *testing.T) {
	ctx := context.Background()
	// 1 DONE, 2 NOT_DONE → not done
	seq := []string{"DONE", "NOT_DONE", "NOT_DONE"}
	i := 0
	judge := LLMJudgeFunc(func(_ context.Context, _ string) (string, error) { r := seq[i%len(seq)]; i++; return r, nil })
	v := NewChecker(nil, judge, 3).Assess(ctx, Goal{Intent: "x"}, "y")
	if v.Done {
		t.Fatalf("少数 DONE 不应判 done: %+v", v)
	}
	// 裁判全报错 → fail-closed not done
	errJudge := LLMJudgeFunc(func(_ context.Context, _ string) (string, error) { return "", errors.New("boom") })
	v = NewChecker(nil, errJudge, 3).Assess(ctx, Goal{Intent: "x"}, "y")
	if v.Done {
		t.Fatalf("裁判出错应 fail-closed not done: %+v", v)
	}
}

func TestCheckerNoCapability(t *testing.T) {
	v := NewChecker(nil, nil, 1).Assess(context.Background(), Goal{Intent: "x"}, "y")
	if v.Done || v.Mode != "none" {
		t.Fatalf("无验收无裁判应 mode=none not done: %+v", v)
	}
}

func TestParseDoneVote(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"DONE\n齐了", true},
		{"NOT_DONE\n差点", false},
		{"完成了\n好", true},
		{"未完成\n还差", false},
		{"嗯我觉得还行吧", false}, // 含糊 → fail-closed
	}
	for _, c := range cases {
		if got, _ := parseDoneVote(c.in); got != c.want {
			t.Errorf("parseDoneVote(%q)=%v want %v", c.in, got, c.want)
		}
	}
}
