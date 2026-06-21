package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

func TestPolicy_Evaluate_DefaultActionWhenNoMatch(t *testing.T) {
	p := NewPermissionPolicy(ActionAllow)
	dec := p.Evaluate(&ToolCallInfo{Name: "anything", Source: "skill"})
	if dec.Action != ActionAllow {
		t.Errorf("expected default Allow; got %v", dec.Action)
	}
	if dec.MatchedRule != "<default>" {
		t.Errorf("expected <default>; got %q", dec.MatchedRule)
	}
}

func TestPolicy_Evaluate_DefaultDeny(t *testing.T) {
	p := NewPermissionPolicy(ActionDeny)
	dec := p.Evaluate(&ToolCallInfo{Name: "shell"})
	if dec.Action != ActionDeny {
		t.Errorf("default deny 应被使用；got %v", dec.Action)
	}
}

func TestPolicy_Evaluate_GlobToolPattern(t *testing.T) {
	p := NewPermissionPolicy(ActionAllow,
		PolicyRule{Name: "block-shell-family", ToolPattern: "shell*", Action: ActionDeny, Risk: "dangerous", Reason: "no shell"},
	)
	dec := p.Evaluate(&ToolCallInfo{Name: "shell_exec"})
	if dec.Action != ActionDeny {
		t.Errorf("shell* glob 应命中 shell_exec；got %v", dec.Action)
	}
	if dec.MatchedRule != "block-shell-family" {
		t.Errorf("MatchedRule wrong: %q", dec.MatchedRule)
	}
	if dec.Risk != "dangerous" {
		t.Errorf("Risk should propagate")
	}

	// 不命中时退回默认
	dec2 := p.Evaluate(&ToolCallInfo{Name: "fs_read"})
	if dec2.Action != ActionAllow {
		t.Errorf("fs_read 不应被 shell* 误命中")
	}
}

func TestPolicy_Evaluate_FirstMatchWins(t *testing.T) {
	p := NewPermissionPolicy(ActionAllow,
		PolicyRule{Name: "deny-fs-write", ToolPattern: "fs.write", Action: ActionDeny},
		PolicyRule{Name: "allow-fs-all", ToolPattern: "fs.*", Action: ActionAllow},
	)
	dec := p.Evaluate(&ToolCallInfo{Name: "fs.write"})
	if dec.Action != ActionDeny {
		t.Errorf("第一条命中规则应胜出；got %v", dec.Action)
	}
	dec2 := p.Evaluate(&ToolCallInfo{Name: "fs.read"})
	if dec2.Action != ActionAllow {
		t.Errorf("fs.read 应命中 allow-fs-all；got %v", dec2.Action)
	}
}

func TestPolicy_Evaluate_SourceFilter(t *testing.T) {
	p := NewPermissionPolicy(ActionAllow,
		PolicyRule{Name: "approve-mcp-only", ToolPattern: "*", SourceMatch: "mcp", Action: ActionRequireApproval, Risk: "sensitive"},
	)
	dec := p.Evaluate(&ToolCallInfo{Name: "search", Source: "mcp"})
	if dec.Action != ActionRequireApproval {
		t.Errorf("mcp source 应命中；got %v", dec.Action)
	}
	dec2 := p.Evaluate(&ToolCallInfo{Name: "search", Source: "skill"})
	if dec2.Action != ActionAllow {
		t.Errorf("skill source 不应命中 SourceMatch=mcp 规则；got %v", dec2.Action)
	}
}

func TestPolicy_Evaluate_ArgumentMatch(t *testing.T) {
	p := NewPermissionPolicy(ActionAllow,
		PolicyRule{
			Name:          "block-etc-write",
			ToolPattern:   "fs.write",
			ArgumentMatch: map[string]string{"path": "/etc/*"},
			Action:        ActionDeny,
			Reason:        "/etc 禁写",
		},
	)
	// 匹配
	dec := p.Evaluate(&ToolCallInfo{Name: "fs.write", Arguments: map[string]any{"path": "/etc/hosts"}})
	if dec.Action != ActionDeny {
		t.Errorf("应命中 /etc/* deny；got %v", dec.Action)
	}
	// arg pattern 不匹配
	dec2 := p.Evaluate(&ToolCallInfo{Name: "fs.write", Arguments: map[string]any{"path": "/tmp/a"}})
	if dec2.Action != ActionAllow {
		t.Errorf("/tmp 应通过；got %v", dec2.Action)
	}
	// arg key 缺失
	dec3 := p.Evaluate(&ToolCallInfo{Name: "fs.write", Arguments: nil})
	if dec3.Action != ActionAllow {
		t.Errorf("缺 path arg 应不命中规则；got %v", dec3.Action)
	}
}

func TestPolicy_Evaluate_NonStringArgConverted(t *testing.T) {
	// arg 是 int / bool 时 toString 转字符串后再 glob
	p := NewPermissionPolicy(ActionAllow,
		PolicyRule{
			Name:          "block-truthy",
			ToolPattern:   "*",
			ArgumentMatch: map[string]string{"force": "true"},
			Action:        ActionDeny,
		},
	)
	dec := p.Evaluate(&ToolCallInfo{Name: "x", Arguments: map[string]any{"force": true}})
	if dec.Action != ActionDeny {
		t.Errorf("bool true 应转为 \"true\" 命中；got %v", dec.Action)
	}
}

func TestPermissionHook_PolicyDeny_BlocksWithoutHub(t *testing.T) {
	policy := NewPermissionPolicy(ActionAllow,
		PolicyRule{Name: "no-shell", ToolPattern: "shell", Action: ActionDeny, Reason: "shell forbidden"},
	)
	hub := NewPermissionHub(0)
	hook := NewPermissionHook(hub, WithPolicy(policy))
	ctx := withV2Flag(context.Background(), false) // 显式 OFF lifecycle.v2
	ctx = withPolicyFlag(ctx, true)

	err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "shell", Source: "skill"})
	if err == nil {
		t.Fatal("policy deny 应返回错误")
	}
	if !strings.Contains(err.Error(), "no-shell") {
		t.Errorf("error 应携带规则名；got %v", err)
	}
}

func TestPermissionHook_PolicyAllow_BypassesHub(t *testing.T) {
	policy := NewPermissionPolicy(ActionAllow)
	hub := NewPermissionHub(0)
	// 故意不设 sender —— 老路径若把 shell 当 dangerous 会拒绝；policy ON 时 default ActionAllow 应放行
	hook := NewPermissionHook(hub, WithPolicy(policy))
	ctx := withPolicyFlag(context.Background(), true)
	if err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "shell"}); err != nil {
		t.Errorf("policy allow 应放行；got %v", err)
	}
}

func TestPermissionHook_NoPolicy_FallsBackToClassifyRisk(t *testing.T) {
	// §11.10 后：policy 配置即生效（不再 flag-gated）。classifyRisk 黑名单只在
	// 未注入 policy（policy==nil）时兜底 —— 此时 shell 为 dangerous + 无 session ⇒ 拒。
	hub := NewPermissionHub(0)
	hook := NewPermissionHook(hub) // 不注入 policy
	err := hook.BeforeToolCall(context.Background(), &ToolCallInfo{Name: "shell"})
	if err == nil {
		t.Fatal("no policy ⇒ classifyRisk 兜底应拒绝 dangerous shell")
	}
}

func TestPermissionHook_PolicyApproval_RequiresHub(t *testing.T) {
	policy := NewPermissionPolicy(ActionAllow,
		PolicyRule{Name: "approve-fs", ToolPattern: "fs.write", Action: ActionRequireApproval, Risk: "sensitive"},
	)
	hub := NewPermissionHub(0)
	hook := NewPermissionHook(hub, WithPolicy(policy))
	ctx := withPolicyFlag(context.Background(), true)

	// 无 sender + 无 session ⇒ requestApproval 因无 session 直接拒
	err := hook.BeforeToolCall(ctx, &ToolCallInfo{Name: "fs.write"})
	if err == nil {
		t.Fatal("approval 路径应拒绝（无 session context）")
	}
}

// withPolicyFlag 在 ctx 中显式打开/关闭 tool.policy.engine flag
func withPolicyFlag(ctx context.Context, on bool) context.Context {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{
		FlagToolPolicyEngine: on,
	})
	return featureflag.WithContext(ctx, flags)
}
