package main

// BUG-20260718（测试验收清单 §15 / PLATAPI-006 / PLATROUTE-005 / CRUD-002/010 /
// E2E-DELETE-001）：删除 Agent「只清 K12 Cron；未完整清 records/assets/...binding，
// Asset store 明确无删除能力」。
//
// 核实结论（以当前代码为准）：
//   - k12_* records 与 agent_rules（IM 绑定）经外键 ON DELETE CASCADE + 连接级
//     foreign_keys=ON，在 Agent 行删除时随之抹除——本测试断言此级联真实生效；
//   - 磁盘作品资产（assetstore）不在 DB 内、无级联，且 DetachAgentResources 此前
//     只摘 cron，不碰资产——这是真正的残留 bug（RED）。修复后 DetachAgentResources
//     必须在摘 cron 的同时抹除该 agent 资产，并在注销 saga 回滚时原样恢复。

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/cron"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	"encoding/base64"

	_ "modernc.org/sqlite"
)

func residualTinyPNG(t *testing.T) []byte {
	t.Helper()
	const b64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBug20260718_DeleteAgentPurgesAssetsRecordsBindings(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("HEXCLAW_ASSET_ROOT", root)

	// data.db DSN 打开 foreign_keys（与生产 storage/sqlite 一致），级联才生效。
	db, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(1)&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate.Run(ctx, db, migrate.All); err != nil {
		t.Fatal(err)
	}

	const agent = "kid-agent"
	// Agent 行 + 一条 K12 错题 + 一条 IM 绑定规则 + 一张作品资产。
	if _, err := db.ExecContext(ctx, `INSERT INTO agents(name) VALUES(?)`, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO k12_mistakes(record_id, agent_name, status, dedupe_key, created_at, updated_at)
		VALUES('m1', ?, 'open', 'dk1', 0, 0)`, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agent_rules(platform, instance_id, user_id, chat_id, agent_name)
		VALUES('dingtalk', 'inst', 'u1', 'c1', ?)`, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := assetstore.Save(agent, residualTinyPNG(t)); err != nil {
		t.Fatal(err)
	}

	sched := cron.NewScheduler(db, nil, nil)
	if err := sched.Init(ctx); err != nil {
		t.Fatal(err)
	}
	cleaner := k12CronRegistrar{sched: sched}

	// —— 摘除阶段：DetachAgentResources 必须抹除资产（RED：修复前只摘 cron）——
	rollback, err := cleaner.DetachAgentResources(ctx, agentrouter.AgentConfig{
		Name:     agent,
		Metadata: map[string]string{"scenario": "k12-tutor"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, agent)); !os.IsNotExist(err) {
		t.Fatalf("删除 Agent 后作品资产目录仍残留本机（assetstore 未被清理）: stat err=%v", err)
	}

	// —— 注销 saga 回滚：持久化删除失败时资产必须原样恢复 ——
	if rollback == nil {
		t.Fatal("k12 agent 清理必须返回补偿回调")
	}
	if err := rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, agent)); len(entries) != 1 {
		t.Fatalf("回滚补偿必须恢复资产, got %d 个文件", len(entries))
	}

	// —— 提交阶段：Agent 行删除，records/binding 经外键级联抹除 ——
	if _, err := db.ExecContext(ctx, `DELETE FROM agents WHERE name = ?`, agent); err != nil {
		t.Fatal(err)
	}
	var mistakes, rules int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_mistakes WHERE agent_name = ?`, agent).Scan(&mistakes)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_rules WHERE agent_name = ?`, agent).Scan(&rules)
	if mistakes != 0 {
		t.Fatalf("删除 Agent 后 k12_mistakes 应级联清空, 残留 %d", mistakes)
	}
	if rules != 0 {
		t.Fatalf("删除 Agent 后 IM 绑定(agent_rules) 应级联清空, 残留 %d", rules)
	}
}
