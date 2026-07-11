package usecase

import (
	"strings"
	"testing"
)

func TestDefaultCronSpecs_Shape(t *testing.T) {
	specs := DefaultCronSpecs("http://127.0.0.1:8787/", "小明的辅导老师", []string{"dingtalk"})
	if len(specs) != 6 {
		t.Fatalf("应有 6 个默认任务, got %d", len(specs))
	}
	wantSchedules := map[string]string{
		"错题卷（每周五）":     "0 19 * * 5",
		"复习提醒（每天）":     "0 20 * * *",
		"学情报告（每月）":     "0 9 1 * *",
		"学年归档建议（6/25）": "0 9 25 6 *",
		"学期确认（3/1）":    "0 9 1 3 *",
		"学期确认（9/1）":    "0 9 1 9 *",
	}
	for _, s := range specs {
		var want string
		matched := false
		for name, schedule := range wantSchedules {
			if strings.HasPrefix(s.Name, name+"·") {
				want, matched = schedule, true
				break
			}
		}
		if !matched || want != s.Schedule {
			t.Errorf("任务 %q schedule=%q, 期望匹配默认任务名及其 agent 作用域", s.Name, s.Schedule)
		}
		if len(s.Deliver) != 1 || s.Deliver[0] != "dingtalk" {
			t.Errorf("任务 %q deliver 应为 [dingtalk], got %v", s.Name, s.Deliver)
		}
		// 脚本应抓对应端点并 emit。
		if !strings.Contains(s.Script, "http_get(") || !strings.Contains(s.Script, "emit(") {
			t.Errorf("任务 %q 脚本应含 http_get + emit\n%s", s.Name, s.Script)
		}
		// agent 名走 URL query 编码（中文转义）。
		if !strings.Contains(s.Script, "agent=%E5%B0%8F") {
			t.Errorf("任务 %q 脚本应含 URL 编码的 agent 名\n%s", s.Name, s.Script)
		}
		// 空 body 静默跳过的分支必须在脚本里。
		if !strings.Contains(s.Script, "if not body:") {
			t.Errorf("任务 %q 脚本应含空 body 跳过分支", s.Name)
		}
	}
}

func TestDefaultCronSpecs_EndpointsDistinct(t *testing.T) {
	specs := DefaultCronSpecs("http://h", "a", nil)
	paths := map[string]bool{}
	for _, s := range specs {
		for _, p := range []string{"mistake-sheet", "daily-reminder", "monthly-report", "semester-check"} {
			if strings.Contains(s.Script, "/api/k12/cron/"+p+"?") {
				paths[p] = true
			}
		}
	}
	// 四类端点都被某个任务覆盖（学期春秋两任务共用 semester-check）。
	for _, p := range []string{"mistake-sheet", "daily-reminder", "monthly-report", "semester-check"} {
		if !paths[p] {
			t.Errorf("端点 %q 未被任何默认任务覆盖", p)
		}
	}
}

func TestDefaultCronSpecs_HasStableAgentKindKey(t *testing.T) {
	specs := DefaultCronSpecs("http://h", "child-a", nil)
	want := []string{
		"child-a/weekly-sheet",
		"child-a/daily-reminder",
		"child-a/monthly-report",
		"child-a/year-archive",
		"child-a/semester-spring",
		"child-a/semester-fall",
	}
	if len(specs) != len(want) {
		t.Fatalf("spec count=%d want=%d", len(specs), len(want))
	}
	for i := range specs {
		if specs[i].Key != want[i] {
			t.Errorf("spec[%d].Key=%q want=%q", i, specs[i].Key, want[i])
		}
	}
}
