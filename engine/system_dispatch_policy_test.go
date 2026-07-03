package engine

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestSystemDispatchPolicy_FunctionFirstMatrix(t *testing.T) {
	p := DefaultSystemDispatchPolicy()

	allowed := []struct {
		source string
		tool   string
	}{
		// BUG-20260702：exec 拆宿主/沙箱——外部源只放沙箱执行 code_exec，
		// 宿主直执行 shell/code 转需审批；内部编排源 workflow/spawn 仍放宿主 shell/code。
		{webhookDispatchSource, "code_exec"},
		{webhookDispatchSource, "file_edit"},
		{webhookDispatchSource, "send_message"},
		{webhookDispatchSource, "media_generate"},
		{spawnDispatchSource, "code_exec"},
		{spawnDispatchSource, "shell"},
		{spawnDispatchSource, "code"},
		{workflowDispatchSource, "shell"},
		{workflowDispatchSource, "code"},
		// GO-9：cron 源不再自动放行 cron_task（自排程回路）；workflow 编排源保留。
		{workflowDispatchSource, "cron_task"},
		{workflowDispatchSource, "app_heal"},
	}
	for _, tt := range allowed {
		if !p.Allows(tt.source, tt.tool) {
			t.Errorf("function_first should allow source=%s tool=%s", tt.source, tt.tool)
		}
	}

	denied := []struct {
		source string
		tool   string
	}{
		{webhookDispatchSource, "shell"},
		{webhookDispatchSource, "code"},
		{cronDispatchSource, "shell"},
		{heartbeatDispatchSource, "shell"},
		{webhookDispatchSource, "manage_skill"},
		{webhookDispatchSource, "publish_wechat"},
		{webhookDispatchSource, "cron_task"},
		{cronDispatchSource, "cron_task"}, // GO-9：cron 内自建 cron 转审批
		{spawnDispatchSource, "send_message"},
		{cronDispatchSource, "app_heal"},
		{solveDispatchSource, "code_exec"},
	}
	for _, tt := range denied {
		if p.Allows(tt.source, tt.tool) {
			t.Errorf("function_first should deny source=%s tool=%s", tt.source, tt.tool)
		}
	}
}

func TestSystemDispatchPolicy_FullAccessIsExplicitButSolveStillGrantScoped(t *testing.T) {
	p := FullAccessSystemDispatchPolicy()
	for _, tool := range []string{"manage_skill", "publish_wechat", "cron_task", "shell"} {
		if !p.Allows(webhookDispatchSource, tool) {
			t.Errorf("full_access should allow webhook tool=%s", tool)
		}
	}
	if p.Allows(solveDispatchSource, "code_exec") {
		t.Fatal("full_access must not make forgeable source=solve a code_exec grant")
	}
}

func TestSystemDispatchPolicy_SourceOverrideReplacesProfileDefault(t *testing.T) {
	p := NewSystemDispatchPolicyFromConfig(config.AutonomyConfig{
		Profile: SystemDispatchProfileFunctionFirst,
		SystemDispatch: config.SystemDispatchAutonomyConfig{
			Webhook: &[]string{"capability", "publish_*"},
		},
	})

	if !p.Allows(webhookDispatchSource, "manage_skill") {
		t.Fatal("webhook override should allow capability category")
	}
	if !p.Allows(webhookDispatchSource, "publish_wechat") {
		t.Fatal("webhook override should allow publish_* glob")
	}
	if p.Allows(webhookDispatchSource, "shell") {
		t.Fatal("source override should replace, not merge with, profile defaults")
	}
}

func TestSystemDispatchPolicy_EmptyOverrideDisablesSourceAutoApproval(t *testing.T) {
	p := NewSystemDispatchPolicyFromConfig(config.AutonomyConfig{
		Profile: SystemDispatchProfileFunctionFirst,
		SystemDispatch: config.SystemDispatchAutonomyConfig{
			Webhook: &[]string{},
		},
	})
	if p.Allows(webhookDispatchSource, "shell") {
		t.Fatal("empty webhook override should auto-approve nothing")
	}
}
