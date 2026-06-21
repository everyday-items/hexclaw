package instances

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/secret"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// newEncryptedManager 构造一个注入了 secret.Box 的 Manager，并返回底层 *sql.DB 以便直查 config_json。
func newEncryptedManager(t *testing.T) (*Manager, func()) {
	t.Helper()
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "instances.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 存储失败: %v", err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化 SQLite 存储失败: %v", err)
	}
	mgr := NewManager(store.DB())
	if err := mgr.Init(context.Background()); err != nil {
		t.Fatalf("初始化实例管理器失败: %v", err)
	}
	box, err := secret.LoadBox(t.TempDir())
	if err != nil {
		t.Fatalf("LoadBox 失败: %v", err)
	}
	mgr.SetSecretBox(box)
	return mgr, func() { store.Close() }
}

// rawConfig 直查库里 config_json 原始值（未经 Manager 解密）。
func rawConfig(t *testing.T, mgr *Manager, name string) string {
	t.Helper()
	var raw string
	if err := mgr.db.QueryRow(`SELECT config_json FROM platform_instances WHERE name = ?`, name).Scan(&raw); err != nil {
		t.Fatalf("直查 config_json 失败: %v", err)
	}
	return raw
}

// TestUpsertGetRoundTripEncrypted 验证 Upsert→Get 透明加解密：库里是密文，Get 出明文。
func TestUpsertGetRoundTripEncrypted(t *testing.T) {
	mgr, cleanup := newEncryptedManager(t)
	defer cleanup()
	ctx := context.Background()

	inst := &Instance{
		Provider: "slack",
		Name:     "slack-enc",
		Enabled:  true,
		Config:   []byte(`{"token":"xoxb-supersecret"}`),
	}
	if err := mgr.Upsert(ctx, inst); err != nil {
		t.Fatalf("Upsert 失败: %v", err)
	}

	// 库里必须是密文，且不含明文 token 片段。
	raw := rawConfig(t, mgr, "slack-enc")
	if !secret.IsEncrypted(raw) {
		t.Fatalf("库里 config_json 应为 enc:v1: 密文，得到 %q", raw)
	}
	if strings.Contains(raw, "xoxb-supersecret") {
		t.Fatal("库里不应出现明文凭据")
	}

	// Get 出来应是解密后的明文 JSON。
	got, err := mgr.Get(ctx, "slack-enc")
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if string(got.Config) != `{"token":"xoxb-supersecret"}` {
		t.Fatalf("解密后 config 不一致: %q", got.Config)
	}

	// List 同样应解密。
	list, err := mgr.List(ctx)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(list) != 1 || string(list[0].Config) != `{"token":"xoxb-supersecret"}` {
		t.Fatalf("List 解密后 config 不一致: %+v", list)
	}
}

// TestLegacyPlaintextReadsThenEncryptsOnUpsert 验证历史明文行可正常读取，
// 且经一次 Upsert 后转为密文。
func TestLegacyPlaintextReadsThenEncryptsOnUpsert(t *testing.T) {
	mgr, cleanup := newEncryptedManager(t)
	defer cleanup()
	ctx := context.Background()

	// 直接以明文写入一行（模拟历史数据）。
	_, err := mgr.db.ExecContext(ctx,
		`INSERT INTO platform_instances (id, provider, name, enabled, mode, status, config_json, last_error, created_at, updated_at)
		 VALUES ('pi-legacy', 'discord', 'discord-legacy', 1, 'runtime', 'stopped', '{"token":"plain-legacy"}', '', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatalf("插入历史明文行失败: %v", err)
	}

	// 读取应原样得到明文 JSON（IsEncrypted=false 分支）。
	got, err := mgr.Get(ctx, "discord-legacy")
	if err != nil {
		t.Fatalf("Get 历史明文失败: %v", err)
	}
	if string(got.Config) != `{"token":"plain-legacy"}` {
		t.Fatalf("历史明文读取不一致: %q", got.Config)
	}

	// 再 Upsert 一次（带回 Get 出的明文 config），库里应转为密文。
	if err := mgr.Upsert(ctx, got); err != nil {
		t.Fatalf("Upsert 历史行失败: %v", err)
	}
	if raw := rawConfig(t, mgr, "discord-legacy"); !secret.IsEncrypted(raw) {
		t.Fatalf("Upsert 后应转为密文，得到 %q", raw)
	}
}

// TestEncryptExistingAtRest 验证回填把明文行就地加密，且明文不变可读。
func TestEncryptExistingAtRest(t *testing.T) {
	mgr, cleanup := newEncryptedManager(t)
	defer cleanup()
	ctx := context.Background()

	_, err := mgr.db.ExecContext(ctx,
		`INSERT INTO platform_instances (id, provider, name, enabled, mode, status, config_json, last_error, created_at, updated_at)
		 VALUES ('pi-seed', 'telegram', 'tg-seed', 1, 'runtime', 'stopped', '{"token":"seed-token"}', '', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatalf("插入种子明文行失败: %v", err)
	}

	n, err := mgr.EncryptExistingAtRest(ctx)
	if err != nil {
		t.Fatalf("EncryptExistingAtRest 失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应加密 1 行，实际 %d", n)
	}
	if raw := rawConfig(t, mgr, "tg-seed"); !secret.IsEncrypted(raw) {
		t.Fatalf("回填后应为密文，得到 %q", raw)
	}
	// 解密后明文不变。
	got, err := mgr.Get(ctx, "tg-seed")
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if string(got.Config) != `{"token":"seed-token"}` {
		t.Fatalf("回填后解密不一致: %q", got.Config)
	}
	// 幂等：再跑一次应加密 0 行。
	n2, err := mgr.EncryptExistingAtRest(ctx)
	if err != nil {
		t.Fatalf("二次 EncryptExistingAtRest 失败: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("已加密行不应被重复加密，实际加密 %d 行", n2)
	}
}

// TestEncryptExistingAtRestNilBox 验证无 box 时回填为 no-op，不报错。
func TestEncryptExistingAtRestNilBox(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()
	if err := mgr.Upsert(ctx, &Instance{Provider: "slack", Name: "no-box", Config: []byte(`{"token":"x"}`)}); err != nil {
		t.Fatalf("Upsert 失败: %v", err)
	}
	n, err := mgr.EncryptExistingAtRest(ctx)
	if err != nil {
		t.Fatalf("无 box 回填应不报错: %v", err)
	}
	if n != 0 {
		t.Fatalf("无 box 应加密 0 行，实际 %d", n)
	}
}

// TestSendResolvesByInstanceID 验证 Send 能以实例 ID（pi-xxx）解析目标。
func TestSendResolvesByInstanceID(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()
	ctx := context.Background()

	inst := &Instance{Provider: "slack", Name: "slack-send", Enabled: true, Config: []byte(`{"token":"x"}`)}
	if err := mgr.Upsert(ctx, inst); err != nil {
		t.Fatalf("Upsert 失败: %v", err)
	}
	// 模拟运行中：注册 stub adapter + metadata（携带 ID）。
	mgr.mu.Lock()
	mgr.running[inst.Name] = &stubAdapter{}
	mgr.metadata[inst.Name] = inst
	mgr.mu.Unlock()

	// 以 ID 发送应命中。
	if err := mgr.Send(ctx, inst.ID, "chat-1", &adapter.Reply{Content: "hi"}); err != nil {
		t.Fatalf("以实例 ID 发送应成功，得到 %v", err)
	}
	// 以 name 发送仍兼容。
	if err := mgr.Send(ctx, inst.Name, "chat-1", &adapter.Reply{Content: "hi"}); err != nil {
		t.Fatalf("以实例名发送应成功，得到 %v", err)
	}
	// 以 provider 回退仍兼容。
	if err := mgr.Send(ctx, inst.Provider, "chat-1", &adapter.Reply{Content: "hi"}); err != nil {
		t.Fatalf("以 provider 发送应成功，得到 %v", err)
	}
	// 未知目标应失败。
	if err := mgr.Send(ctx, "pi-nonexistent", "chat-1", &adapter.Reply{Content: "hi"}); err == nil {
		t.Fatal("未知目标应返回错误")
	}
}
