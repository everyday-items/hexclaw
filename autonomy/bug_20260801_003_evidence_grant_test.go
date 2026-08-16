package autonomy

import (
	"context"
	"testing"

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
