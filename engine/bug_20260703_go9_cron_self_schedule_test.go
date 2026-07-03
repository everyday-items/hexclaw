package engine

// GO-9（BUG-20260703）：automation 类别（cron_task）对 cron 派发源自动放行
// = 自排程回路——cron job 里的 agent 可静默再建 cron job，job 造 job 无任何
// 总量兜底（AddJob 无配额、原 cronRecursiveToolDenylist 已是 no-op 死代码）。
//
// 契约：function_first / balanced 下 cron 源调 cron_task 一律转审批（fail-closed）；
// workflow 编排源保留 automation（用户显式编排、非自我复制回路）；full_access
// 是操作者经配置文件显式选择的最高档，保持 "*" 不变。

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestBug20260703_CronSourceDoesNotAutoAllowCronTask(t *testing.T) {
	for _, profile := range []string{SystemDispatchProfileFunctionFirst, SystemDispatchProfileBalanced} {
		p := NewSystemDispatchPolicyFromConfig(config.AutonomyConfig{Profile: profile})
		if p.Allows(cronDispatchSource, "cron_task") {
			t.Errorf("[GO-9] profile=%s：cron 源自动放行 cron_task，自排程回路未收口", profile)
		}
		if !p.Allows(workflowDispatchSource, "cron_task") {
			t.Errorf("profile=%s：workflow 编排源应保留 automation 放行", profile)
		}
	}
}

// 操作者显式 override 仍可对 cron 源放行 automation（清晰的开关，而非硬编码禁令）。
func TestBug20260703_ExplicitOverrideCanReAllowCronAutomation(t *testing.T) {
	entries := []string{"read", "automation"}
	p := NewSystemDispatchPolicyFromConfig(config.AutonomyConfig{
		Profile:        SystemDispatchProfileFunctionFirst,
		SystemDispatch: config.SystemDispatchAutonomyConfig{Cron: &entries},
	})
	if !p.Allows(cronDispatchSource, "cron_task") {
		t.Fatal("显式 override 放行 automation 后 cron 源应允许 cron_task")
	}
}
