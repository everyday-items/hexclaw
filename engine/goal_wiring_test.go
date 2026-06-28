package engine

import (
	"context"
	"strings"
	"testing"
)

func TestMakerFromExec(t *testing.T) {
	ctx := context.Background()
	var gotTask string
	exec := TaskExecFunc(func(_ context.Context, task string) (string, error) {
		gotTask = task
		return "产出 for: " + task, nil
	})
	m := MakerFromExec(exec)

	r, err := m.Make(ctx, "整理资料", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotTask, "整理资料") || strings.Contains(gotTask, "上一轮验收反馈") {
		t.Fatalf("首轮 task 应只含意图: %q", gotTask)
	}
	if !strings.Contains(r.Output, "整理资料") {
		t.Fatalf("产出应回带任务: %q", r.Output)
	}

	_, _ = m.Make(ctx, "整理资料", "缺一张趋势图", 2)
	if !strings.Contains(gotTask, "缺一张趋势图") || !strings.Contains(gotTask, "上一轮验收反馈") {
		t.Fatalf("第二轮 task 应含反馈: %q", gotTask)
	}
}

// 编译期断言：装配用到的适配器满足接口。
var (
	_ LLMJudge       = LLMJudgeFunc(nil)
	_ Maker          = MakerFunc(nil)
	_ WriteIsolator  = NoopIsolator{}
	_ EscalationSink = EscalationFunc(nil)
)
