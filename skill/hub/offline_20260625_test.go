package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// 离线优先 catalog：断网 / GitHub 不可达时，市场必须仍非空（内嵌出厂快照兜底）。
// 用不存在的本地路径作 RepoURL → readURL 走本地文件读取并瞬时失败 → 零网络、必落 embed。
func TestHub_EnsureCatalog_EmbedSeed_Offline(t *testing.T) {
	h := New(HubConfig{Enabled: true, RepoURL: "/nonexistent-hub-offline-xyz"}, "")
	// 无 cacheDir → 跳过磁盘缓存层，直接验证「内嵌种子」兜底。

	h.EnsureCatalog()

	cat := h.GetCatalog()
	if cat == nil {
		t.Fatal("离线时 catalog 仍应由内嵌种子填充，不应为 nil")
	}
	if len(cat.Skills) == 0 {
		t.Fatal("内嵌种子应含条目，得空")
	}

	var hasSkill, hasMysql bool
	for _, s := range cat.Skills {
		if s.Name == "code-review-pro" && s.Type == "skill" {
			hasSkill = true
		}
		if s.Name == "mysql" && s.Type == "mcp" {
			hasMysql = true
			if s.Env["MYSQL_HOST"] != "localhost" {
				t.Errorf("内嵌 mysql 的 env 应保留 MYSQL_HOST: %+v", s.Env)
			}
		}
	}
	if !hasSkill {
		t.Error("内嵌种子应含已知 skill 条目 code-review-pro(Type=skill)")
	}
	if !hasMysql {
		t.Error("内嵌种子应含已知 mcp 条目 mysql(Type=mcp)")
	}
}

// 磁盘缓存层：一次成功拉取后落盘，下次（即便断网）从缓存秒起，且内容是上次成功的快照（区别于内嵌种子）。
func TestHub_DiskCache_RoundTrip(t *testing.T) {
	repoDir := t.TempDir()
	cacheDir := t.TempDir()

	// 本地「仓库」：哨兵名只存在于此，内嵌种子里没有 → 用以证明确实走了缓存层。
	// updated_at 取未来时间，确保缓存「不比内嵌种子旧」（真实 index.json 永远带 updated_at）。
	writeFile(t, filepath.Join(repoDir, "index.json"),
		`{"version":"t","updated_at":"2030-01-01T00:00:00Z","skills":[{"name":"cache-sentinel-skill","description":"d","category":"c"}]}`)
	writeFile(t, filepath.Join(repoDir, "mcp-registry.json"),
		`{"version":"t","servers":[{"name":"cache-sentinel-mcp","status":"pinned","command":"npx","args":["-y","x@1.2.3"],"env":{"K":""},"artifact":{"ecosystem":"npm","package":"x","version":"1.2.3","integrity":"sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==","source_registry":"https://registry.npmjs.org"}}]}`)

	// Hub A：从本地仓库成功拉取 → 应写磁盘缓存。
	a := New(HubConfig{Enabled: true, RepoURL: repoDir}, "")
	a.SetCacheDir(cacheDir)
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatalf("本地仓库 Refresh 应成功: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "hub-catalog.json")); err != nil {
		t.Fatalf("Refresh 成功后应落磁盘缓存: %v", err)
	}

	// Hub B：RepoURL 指向不存在路径（断网模拟）+ 同一缓存目录 → 应从缓存 seed 出哨兵。
	b := New(HubConfig{Enabled: true, RepoURL: "/nonexistent-hub-cache-xyz"}, "")
	b.SetCacheDir(cacheDir)
	b.EnsureCatalog()

	cat := b.GetCatalog()
	if cat == nil {
		t.Fatal("应从磁盘缓存 seed 出 catalog")
	}
	var fromCache bool
	for _, s := range cat.Skills {
		if s.Name == "cache-sentinel-skill" {
			fromCache = true
		}
	}
	if !fromCache {
		t.Error("断网时应优先从磁盘缓存（上次成功拉取）seed，而非内嵌种子")
	}
}

// skillMetaToMcp 投影应无损保留 Env / Command / Args（CLI / agentic 安装依赖这些）。
func TestSkillMetaToMcp_PreservesEnv(t *testing.T) {
	s := SkillMeta{
		Name:       "mysql",
		Command:    "npx",
		Args:       []string{"-y", "@benborla29/mcp-server-mysql"},
		Env:        map[string]string{"MYSQL_HOST": "localhost", "MYSQL_PASS": ""},
		ConfigHint: "需 MYSQL_HOST",
		Source:     "registry",
	}
	m := skillMetaToMcp(s)
	if m.Name != "mysql" || m.Command != "npx" || len(m.Args) != 2 {
		t.Errorf("基础字段未保留: %+v", m)
	}
	if m.Env["MYSQL_HOST"] != "localhost" {
		t.Errorf("Env 未保留: %+v", m.Env)
	}
	if _, ok := m.Env["MYSQL_PASS"]; !ok {
		t.Error("空值 env 占位也应保留")
	}
}

// McpHub 门面离线可用：所有抓取走内嵌种子，Search/Get 不为空，Env 投影完整。
func TestMcpHub_Facade_OfflineEmbed(t *testing.T) {
	inner := New(HubConfig{Enabled: true, RepoURL: "/nonexistent-hub-facade-xyz"}, "")
	mh := &McpHub{inner: inner} // 注入断网 inner，不触 DefaultCacheDir / 真实 home

	if mh.Count() == 0 {
		t.Fatal("离线时门面应由内嵌种子返回 MCP 列表，得 0")
	}
	got, err := mh.Get("mysql")
	if err != nil {
		t.Fatalf("内嵌种子含 mysql，Get 不应失败: %v", err)
	}
	if got.Env["MYSQL_HOST"] != "localhost" {
		t.Errorf("门面应保留 mysql env: %+v", got.Env)
	}
	if len(mh.Search("mysql")) == 0 {
		t.Error("Search(mysql) 应命中内嵌种子里的 mysql")
	}
}

// 升级后离线：磁盘缓存（上次拉取的旧版内容）不应盖过更新的内嵌出厂快照。
// 旧 seed() 无条件优先缓存 → 升级到新二进制后离线，市场反而显示旧版条目 = 退化。
func TestHub_Seed_PrefersNewerEmbedOverStaleCache(t *testing.T) {
	cacheDir := t.TempDir()
	// 陈旧缓存：updated_at(2020) 远早于内嵌种子(2026-06-21)，含独有哨兵。
	stale := `{"version":"old","updated_at":"2020-01-01T00:00:00Z","skills":[{"name":"stale-cache-skill","type":"skill"}]}`
	writeFile(t, filepath.Join(cacheDir, "hub-catalog.json"), stale)

	h := New(HubConfig{Enabled: true, RepoURL: "/nonexistent-upgrade-xyz"}, "")
	h.SetCacheDir(cacheDir)
	h.EnsureCatalog()

	cat := h.GetCatalog()
	for _, s := range cat.Skills {
		if s.Name == "stale-cache-skill" {
			t.Fatal("陈旧缓存(2020)不应盖过更新的内嵌种子(2026-06-21)——升级后离线会显示旧版市场")
		}
	}
	var hasEmbed bool
	for _, s := range cat.Skills {
		if s.Name == "code-review-pro" {
			hasEmbed = true
		}
	}
	if !hasEmbed {
		t.Error("应服务内嵌种子(含 code-review-pro)")
	}
}

// 反向守卫：比内嵌更新的缓存（moving-branch 场景拉到的新内容）应继续优先，别被修复误伤。
func TestHub_Seed_FresherCacheWins(t *testing.T) {
	cacheDir := t.TempDir()
	fresh := `{"version":"new","updated_at":"2030-01-01T00:00:00Z","skills":[{"name":"fresh-cache-skill","type":"skill"}]}`
	writeFile(t, filepath.Join(cacheDir, "hub-catalog.json"), fresh)

	h := New(HubConfig{Enabled: true, RepoURL: "/nonexistent-fresh-xyz"}, "")
	h.SetCacheDir(cacheDir)
	h.EnsureCatalog()

	var ok bool
	for _, s := range h.GetCatalog().Skills {
		if s.Name == "fresh-cache-skill" {
			ok = true
		}
	}
	if !ok {
		t.Error("比内嵌更新的缓存(2030)应优先服务（保留 moving-branch 新鲜度）")
	}
}

// 🟡-1：离线（网络持续失败）时，失败的网络刷新必须退避节流——否则 lastSync 失败永不设置，
// TTL 闸永久失效，每个市场请求都 re-spawn 一个最长 30s 的失败刷新协程（浪费）。
func TestHub_OfflineRefresh_ThrottledOnFailure(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError) // 模拟拉取失败
	}))
	defer srv.Close()

	h := New(HubConfig{Enabled: true, RepoURL: srv.URL}, "")
	for range 5 {
		h.EnsureCatalog()
		// 等后台刷新协程跑完（refreshing 落回 false），让本次命中被计数，确定性。
		for j := 0; j < 500 && h.refreshing.Load(); j++ {
			time.Sleep(time.Millisecond)
		}
	}

	if got := atomic.LoadInt32(&hits); got > 1 {
		t.Fatalf("离线连续 5 次 EnsureCatalog 应只触发 1 次网络刷新（失败退避节流），实际 %d 次", got)
	}
}

// shouldRefresh 节流真值表（确定性，穷举 TTL/退避分支）。
func TestShouldRefresh_TruthTable(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	zero := time.Time{}
	cases := []struct {
		name        string
		lastSync    time.Time
		lastAttempt time.Time
		want        bool
	}{
		{"首次(都为零)", zero, zero, true},
		{"数据够新(成功5min前)", now.Add(-5 * time.Minute), now.Add(-5 * time.Minute), false},
		{"数据过期但刚试过(退避中)", now.Add(-11 * time.Minute), now.Add(-5 * time.Second), false},
		{"数据过期且退避到期", now.Add(-11 * time.Minute), now.Add(-2 * time.Minute), true},
		{"离线-从未成功但刚试过(退避)", zero, now.Add(-5 * time.Second), false}, // ← 🟡-1 修复点
		{"离线-从未成功且退避到期(重试)", zero, now.Add(-2 * time.Minute), true},
	}
	for _, c := range cases {
		if got := shouldRefresh(now, c.lastSync, c.lastAttempt); got != c.want {
			t.Errorf("%s: shouldRefresh=%v, want %v", c.name, got, c.want)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写测试文件失败 %s: %v", path, err)
	}
}
