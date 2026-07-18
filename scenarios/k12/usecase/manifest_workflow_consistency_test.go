package usecase

import (
	"testing"

	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// TestManifestScheduledWorkflowsMatchCronSpecs §6.2 一致性校验：Manifest 是唯一事实源，
// 默认定时任务描述符（DefaultCronSpecs）的 kind 集合必须与 Manifest.ScheduledWorkflows 一致，
// 防两处清单漂移（新增/撤下任务描述符必须同步 Manifest 声明）。
func TestManifestScheduledWorkflowsMatchCronSpecs(t *testing.T) {
	declared := map[string]bool{}
	for _, k := range k12.Manifest(k12.NewCurriculumStub()).ScheduledWorkflows {
		declared[k] = true
	}
	specs := DefaultCronSpecs("http://127.0.0.1:1", "agent-x", nil)
	seen := map[string]bool{}
	for _, s := range specs {
		seen[string(s.Kind)] = true
		if !declared[string(s.Kind)] {
			t.Errorf("cron 描述符 %q 未在 Manifest.ScheduledWorkflows 声明（Manifest 为唯一事实源）", s.Kind)
		}
	}
	for k := range declared {
		if !seen[k] {
			t.Errorf("Manifest 声明的工作流 %q 没有对应 cron 描述符", k)
		}
	}
}
