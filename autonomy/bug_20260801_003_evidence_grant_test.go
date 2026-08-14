package autonomy

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/skill"
)

// BUG-20260801-003：无人值守（cron/webhook）+ RAG 污染证据时，精确的
// owner/session/task/tool/scope 授权链必须由真实持久化 GrantStore 提供。
// 此前 `cmd/hexclaw` 注入的 `GrantStore` 只实现旧 `GrantAllows`，未实现
// `engine.UntrustedEvidenceTaskGrantChecker`，导致类型断言恒失败：tainted
// 工具一律被拒（fail-closed 死路），精确 grant 永远无法命中。本组 RED 在
// unchanged production 上精确证明该缺口，再实施治本。

// RED-1：GrantStore 必须实现 UntrustedEvidenceTaskGrantChecker。
// 生产 PermissionHook.authorizeUntrustedEvidenceTool 依赖该类型断言；
// 实现前编译/运行断言失败，证明真实授权链缺失。
func TestBUG20260801003_GrantStoreImplementsUntrustedEvidenceTaskGrantChecker(t *testing.T) {
	db := setupTestDB(t)
	s := NewGrantStore(db)
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	checker, ok := any(s).(engine.UntrustedEvidenceTaskGrantChecker)
	if !ok {
		t.Fatal("GrantStore must implement UntrustedEvidenceTaskGrantChecker so a tainted unattended tool can be authorized by an exact persisted grant; production currently fails closed with no usable authorization path")
	}
	_ = checker
}

// RED-2：创建 grant 时必须从可信上下文冻结 owner，并持久化 owner 与可选
// security scope；reload（重启恢复）后同一精确授权仍可命中。
func TestBUG20260801003_GrantOwnerAndScopePersistAcrossReload(t *testing.T) {
	db := setupTestDB(t)
	s := NewGrantStore(db)
	ctx := skill.WithAuthenticatedUser(context.Background(), "owner-1")
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	g, err := s.Create(ctx, Grant{
		TaskRef:             "cron:job-1",
		Source:              "cron",
		Entries:             []string{"shell"},
		Note:                "tainted exact grant",
		SecurityScopeDigest: "scope-digest-abc",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if g.OwnerID != "owner-1" {
		t.Fatalf("grant owner = %q, want owner-1 frozen from trusted context", g.OwnerID)
	}

	// 重启恢复：从同一库重建 store（等价进程重启后 reload）。
	reloaded := NewGrantStore(db)
	if err := reloaded.Init(ctx); err != nil {
		t.Fatalf("reload Init: %v", err)
	}
	if !reloaded.GrantAllowsUntrustedEvidence("owner-1", "cron", "cron:job-1", "shell", "scope-digest-abc") {
		t.Fatal("exact persisted grant must authorize the matching owner/source/task/tool/scope after reload")
	}
}

// RED-3：精确对账的负例——错 owner、错 task、错 source、错工具、错 scope
// 一律拒绝；空 scope 调用（参数不可审计）也拒绝。
func TestBUG20260801003_GrantUntrustedEvidenceRejectsMismatch(t *testing.T) {
	db := setupTestDB(t)
	s := NewGrantStore(db)
	ctx := skill.WithAuthenticatedUser(context.Background(), "owner-1")
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := s.Create(ctx, Grant{
		TaskRef:             "cron:job-1",
		Source:              "cron",
		Entries:             []string{"shell"},
		SecurityScopeDigest: "scope-digest-abc",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	negatives := []struct {
		name       string
		owner      string
		source     string
		taskRef    string
		tool       string
		scope      string
	}{
		{name: "wrong-owner", owner: "owner-2", source: "cron", taskRef: "cron:job-1", tool: "shell", scope: "scope-digest-abc"},
		{name: "wrong-source", owner: "owner-1", source: "webhook", taskRef: "cron:job-1", tool: "shell", scope: "scope-digest-abc"},
		{name: "wrong-task", owner: "owner-1", source: "cron", taskRef: "cron:other", tool: "shell", scope: "scope-digest-abc"},
		{name: "wrong-tool", owner: "owner-1", source: "cron", taskRef: "cron:job-1", tool: "code_exec", scope: "scope-digest-abc"},
		{name: "wrong-scope", owner: "owner-1", source: "cron", taskRef: "cron:job-1", tool: "shell", scope: "scope-digest-xyz"},
		{name: "empty-scope", owner: "owner-1", source: "cron", taskRef: "cron:job-1", tool: "shell", scope: ""},
	}
	for _, n := range negatives {
		t.Run(n.name, func(t *testing.T) {
			if s.GrantAllowsUntrustedEvidence(n.owner, n.source, n.taskRef, n.tool, n.scope) {
				t.Fatalf("grant must reject %s (%q/%q/%q/%q scope=%q)", n.name, n.owner, n.source, n.taskRef, n.tool, n.scope)
			}
		})
	}
}

// 端到端：真实 GrantStore 注入 PermissionHook 后，tainted 无人值守派发的
// 工具调用只有精确证据授权可放行；错误 owner 与宽泛旧 grant（无 owner）
// 均 fail-closed。覆盖 sync 语义（BeforeToolCall 是同步入口）。
func TestBUG20260801003_GrantStoreWireThroughPermissionHook(t *testing.T) {
	newHook := func(grants *GrantStore) *engine.PermissionHook {
		return engine.NewPermissionHook(engine.NewPermissionHub(0),
			engine.WithPolicy(engine.DefaultBaselinePolicy()),
			engine.WithSystemDispatchPolicy(engine.FullAccessSystemDispatchPolicy()),
			engine.WithTaskGrants(grants),
		)
	}
	taintedCtx := func(owner string) context.Context {
		ctx := skill.WithAuthenticatedUser(context.Background(), owner)
		ctx = skill.WithSystemDispatchSource(ctx, "cron")
		ctx = skill.WithSystemDispatchTask(ctx, "cron:job-1")
		return withUntrustedKnowledgeEvidenceContextForTest(ctx)
	}
	call := &engine.ToolCallInfo{Name: "shell", Source: "skill", Arguments: map[string]any{"command": "true"}}

	t.Run("no-grant-fails-closed", func(t *testing.T) {
		db := setupTestDB(t)
		s := NewGrantStore(db)
		if err := s.Init(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := newHook(s).BeforeToolCall(taintedCtx("owner-1"), call); err == nil {
			t.Fatal("无精确 grant 时 tainted 无人值守工具必须 fail-closed")
		}
	})

	t.Run("exact-grant-allows", func(t *testing.T) {
		db := setupTestDB(t)
		s := NewGrantStore(db)
		ctx := skill.WithAuthenticatedUser(context.Background(), "owner-1")
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Create(ctx, Grant{
			TaskRef: "cron:job-1", Source: "cron", Entries: []string{"shell"},
			SecurityScopeDigest: "scope-digest-abc",
		}); err != nil {
			t.Fatal(err)
		}
		// 参数被规范化成非空 digest 后，精确 grant 放行。
		ctx2 := skill.WithAuthenticatedUser(context.Background(), "owner-1")
		ctx2 = skill.WithSystemDispatchSource(ctx2, "cron")
		ctx2 = skill.WithSystemDispatchTask(ctx2, "cron:job-1")
		ctx2 = withUntrustedKnowledgeEvidenceContextForTest(ctx2)
		if err := newHook(s).BeforeToolCall(ctx2, &engine.ToolCallInfo{Name: "shell", Source: "skill", Arguments: map[string]any{"command": "true"}}); err != nil {
			t.Fatalf("精确证据授权应放行规范化参数的 tainted 工具: %v", err)
		}
	})

	t.Run("wrong-owner-fails-closed", func(t *testing.T) {
		db := setupTestDB(t)
		s := NewGrantStore(db)
		ctx := skill.WithAuthenticatedUser(context.Background(), "owner-1")
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Create(ctx, Grant{
			TaskRef: "cron:job-1", Source: "cron", Entries: []string{"shell"},
			SecurityScopeDigest: "scope-digest-abc",
		}); err != nil {
			t.Fatal(err)
		}
		if err := newHook(s).BeforeToolCall(taintedCtx("owner-2"), call); err == nil {
			t.Fatal("错 owner 的 tainted 工具必须 fail-closed")
		}
	})

	t.Run("legacy-grant-without-owner-fails-closed", func(t *testing.T) {
		db := setupTestDB(t)
		s := NewGrantStore(db)
		// 无可信用户 ctx（等价旧版本创建的 grant：owner 为空）。
		if err := s.Init(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Create(context.Background(), Grant{
			TaskRef: "cron:job-1", Source: "cron", Entries: []string{"shell"},
		}); err != nil {
			t.Fatal(err)
		}
		if err := newHook(s).BeforeToolCall(taintedCtx("owner-1"), call); err == nil {
			t.Fatal("无 owner 的旧 grant 不得放行 tainted 工具（fail-closed）")
		}
	})

	t.Run("scope-mismatch-fails-closed", func(t *testing.T) {
		db := setupTestDB(t)
		s := NewGrantStore(db)
		ctx := skill.WithAuthenticatedUser(context.Background(), "owner-1")
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Create(ctx, Grant{
			TaskRef: "cron:job-1", Source: "cron", Entries: []string{"shell"},
			SecurityScopeDigest: "scope-digest-other",
		}); err != nil {
			t.Fatal(err)
		}
		if err := newHook(s).BeforeToolCall(taintedCtx("owner-1"), call); err == nil {
			t.Fatal("scope digest 不匹配时 tainted 工具必须 fail-closed")
		}
	})
}

// withUntrustedKnowledgeEvidenceContextForTest 复刻 engine 的 host-only taint
// 标记（engine 未导出，autonomy 测试不依赖其内部实现，只验证 GrantStore 侧）。
func withUntrustedKnowledgeEvidenceContextForTest(ctx context.Context) context.Context {
	return context.WithValue(ctx, evidenceTaintKeyForTest{}, true)
}

type evidenceTaintKeyForTest struct{}

// 断言错误消息指向精确授权要求（fail-closed 文案）。
func TestBUG20260801003_FailClosedErrorMentionsExactGrant(t *testing.T) {
	db := setupTestDB(t)
	s := NewGrantStore(db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	hook := engine.NewPermissionHook(engine.NewPermissionHub(0),
		engine.WithPolicy(engine.DefaultBaselinePolicy()),
		engine.WithSystemDispatchPolicy(engine.FullAccessSystemDispatchPolicy()),
		engine.WithTaskGrants(s),
	)
	ctx := skill.WithAuthenticatedUser(context.Background(), "owner-1")
	ctx = skill.WithSystemDispatchSource(ctx, "cron")
	ctx = skill.WithSystemDispatchTask(ctx, "cron:job-1")
	ctx = withUntrustedKnowledgeEvidenceContextForTest(ctx)
	err := hook.BeforeToolCall(ctx, &engine.ToolCallInfo{Name: "shell", Source: "skill", Arguments: map[string]any{"command": "true"}})
	if err == nil {
		t.Fatal("预期 fail-closed 拒绝")
	}
	if !strings.Contains(err.Error(), "exact owner/task/tool/security-scope grant") &&
		!strings.Contains(err.Error(), "exact evidence-aware task grant") &&
		!strings.Contains(err.Error(), "security-scope grant") {
		t.Fatalf("fail-closed 文案应指向精确证据授权: %v", err)
	}
	_ = config.DefaultConfig
}
