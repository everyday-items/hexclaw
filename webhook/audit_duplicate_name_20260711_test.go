package webhook

// hex-test 审计 · 契约#8：重名 webhook 注册返回 500 + SQLite 原文外泄。
// 症状：webhook.go Register 把 `UNIQUE constraint failed` 原样包裹返回,handler
// 无条件 500 + err.Error() 塞进响应体 → 用户看到 SQLite 内部约束串,且语义应为 409 冲突。
// RED：重名 Register 返回不可分类且含 SQLite 原文 → FAIL；GREEN：返回 ErrWebhookExists sentinel。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestManager_RegisterDuplicateName_ReturnsClassifiableConflict(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	mgr := NewManager(db)
	if err := mgr.Init(ctx); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	wh := &Webhook{Name: "dup", Type: TypeGeneric, Prompt: "p1", UserID: "u1"}
	if err := mgr.Register(ctx, wh); err != nil {
		t.Fatalf("首次注册应成功: %v", err)
	}

	err := mgr.Register(ctx, &Webhook{Name: "dup", Type: TypeGeneric, Prompt: "p2", UserID: "u1"})
	if !errors.Is(err, ErrWebhookExists) {
		t.Fatalf("重名注册应返回可分类的 ErrWebhookExists(供 handler 转 409), 实际 %v", err)
	}
	if e := err.Error(); strings.Contains(e, "UNIQUE") || strings.Contains(e, "constraint") {
		t.Fatalf("错误信息不得外泄 SQLite 内部约束串: %q", e)
	}
}
