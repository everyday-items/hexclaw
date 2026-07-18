package usecase

import (
	"strings"
	"testing"
)

// 默认任务集对齐架构设计-v0.5.0 §3.13 定时任务与工作流（2026-07-18 裁决）：
// 每周复习（错题卷）/ 回传提醒 / 学期确认（3.1、9.1）为 cron 描述符；每日复习提醒保留；
// 月度学情报告与学年归档建议**不再默认注册 cron**（内容端点保留，家长可手动查看）；
// 学情刷新是事件触发，不进 cron。
func TestDefaultCronSpecs_Shape(t *testing.T) {
	specs := DefaultCronSpecs("http://127.0.0.1:8787/", "小明的辅导老师", []string{"dingtalk"})
	if len(specs) != 4 {
		t.Fatalf("§3.13 对齐后应有 4 个默认任务（每周复习1+回传提醒1+学期确认2；学情刷新事件触发不进 cron；daily-reminder 为超集已撤）, got %d", len(specs))
	}
	wantSchedules := map[string]string{
		"错题卷（每周五）":  "0 19 * * 5",
		"回传提醒（每天）":  "0 20 * * *",
		"学期确认（3/1）": "0 9 1 3 *",
		"学期确认（9/1）": "0 9 1 9 *",
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
		// Kind 是描述符的一部分（单一事实源），且与幂等键后缀一致。
		if s.Kind == "" || !strings.HasSuffix(s.Key, "/"+string(s.Kind)) {
			t.Errorf("任务 %q Kind=%q 应非空且与 Key=%q 后缀一致", s.Name, s.Kind, s.Key)
		}
	}
}

func TestDefaultCronSpecs_EndpointsDistinct(t *testing.T) {
	specs := DefaultCronSpecs("http://h", "a", nil)
	paths := map[string]bool{}
	for _, s := range specs {
		for _, p := range []string{"mistake-sheet", "return-reminder", "semester-check"} {
			if strings.Contains(s.Script, "/api/k12/cron/"+p+"?") {
				paths[p] = true
			}
		}
	}
	// 四类端点都被某个任务覆盖（学期春秋两任务共用 semester-check）。
	for _, p := range []string{"mistake-sheet", "return-reminder", "semester-check"} {
		if !paths[p] {
			t.Errorf("端点 %q 未被任何默认任务覆盖", p)
		}
	}
}

func TestDefaultCronSpecs_HasStableAgentKindKey(t *testing.T) {
	specs := DefaultCronSpecs("http://h", "child-a", nil)
	want := []string{
		"child-a/weekly-sheet",
		"child-a/return-reminder",
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

// 每周复习两步脚本（§3.13 每周复习 · §3.8 装篮入口2）：先 POST fill-basket 自动装篮，
// 再 GET mistake-sheet 投递错题卷；装篮失败**不阻断**错题卷投递（记 error 字段继续出卷）。
func TestDefaultCronSpecs_WeeklyTwoStepFillThenSheet(t *testing.T) {
	var weekly *CronSpec
	specs := DefaultCronSpecs("http://127.0.0.1:8787", "小明的辅导老师", nil)
	for i := range specs {
		if specs[i].Kind == KindWeeklySheet {
			weekly = &specs[i]
		}
	}
	if weekly == nil {
		t.Fatal("默认任务集应含 weekly-sheet")
	}
	s := weekly.Script
	iFill := strings.Index(s, "http_post('http://127.0.0.1:8787/api/k12/cron/fill-basket?agent=")
	iSheet := strings.Index(s, "http_get('http://127.0.0.1:8787/api/k12/cron/mistake-sheet?agent=")
	if iFill < 0 {
		t.Fatalf("weekly 脚本应先 http_post fill-basket（自动装篮）:\n%s", s)
	}
	if iSheet < 0 {
		t.Fatalf("weekly 脚本应 http_get mistake-sheet（错题卷投递）:\n%s", s)
	}
	if iFill > iSheet {
		t.Errorf("装篮须在出卷之前（先装篮再投递）: fill@%d sheet@%d", iFill, iSheet)
	}
	// 装篮失败不阻断投递：fill 非 2xx 只记 error 字段，不 return error 中断。
	if !strings.Contains(s, "fill_err") {
		t.Errorf("weekly 脚本应把装篮失败记入 error 字段（不阻断出卷）:\n%s", s)
	}
	if !strings.Contains(s, "if not body:") {
		t.Errorf("weekly 脚本应保留空 body 静默跳过分支:\n%s", s)
	}
	// 脚本注释引用权威条款。
	if !strings.Contains(s, "§3.13") || !strings.Contains(s, "§3.8") {
		t.Errorf("weekly 脚本注释应引用 §3.13 / §3.8 入口2:\n%s", s)
	}
}

// 冻结守卫：§3.13 之外的 monthly-report / year-archive 描述符不得回流默认任务集
// （内容端点保留，但不再随建档注册 cron）。
func TestDefaultCronSpecs_FrozenExcludesReportAndYearArchive(t *testing.T) {
	for _, s := range DefaultCronSpecs("http://h", "a", nil) {
		for _, banned := range []string{"monthly-report", "year-archive"} {
			if strings.Contains(s.Key, banned) || string(s.Kind) == banned {
				t.Errorf("§3.13 冻结：默认任务集不得含 %q, got key=%q", banned, s.Key)
			}
		}
	}
}
