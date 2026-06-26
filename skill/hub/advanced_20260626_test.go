package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// 方法1 不变量/fuzz：远程 registry/index JSON 是不可信输入面——任意字节都不得 panic，
// 且成功解析的不变量必须成立（index 每个 skill 都被打 type）。
func FuzzParseMcpServers(f *testing.F) {
	f.Add([]byte(`{"servers":[{"name":"x","command":"npx","env":{"K":""}}]}`))
	f.Add([]byte(`[{"name":"y"}]`))
	f.Add([]byte(`{}`))
	f.Add([]byte(``))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"servers":null}`))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = parseMcpServers(data) // 不变量：永不 panic
	})
}

func FuzzParseIndexCatalog(f *testing.F) {
	f.Add([]byte(`{"skills":[{"name":"x"}]}`))
	f.Add([]byte(`{"updated_at":"2026-06-21T00:00:00Z","skills":[]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(``))
	f.Fuzz(func(t *testing.T, data []byte) {
		cat, err := parseIndexCatalog(data)
		if err != nil {
			return
		}
		for _, s := range cat.Skills {
			if s.Type == "" { // 不变量：解析成功的 skill 必带 type（默认 skill）
				t.Fatalf("解析成功但 Type 为空: %+v", s)
			}
		}
	})
}

// 方法2 混沌/故障注入（应用层·上游挂起）：上游高延迟/挂死时 EnsureCatalog 仍即时返回（非阻塞），
// 由 embed seed 出非空 catalog——证明「永不阻塞在网络上」的核心不变量在故障下成立。
func TestChaos_SlowUpstreamNonBlocking(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-block // 模拟上游挂起（超时/高延迟）
	}))
	defer srv.Close()

	h := New(HubConfig{Enabled: true, RepoURL: srv.URL}, "")
	start := time.Now()
	h.EnsureCatalog()
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		close(block)
		t.Fatalf("上游挂起时 EnsureCatalog 应即时返回(非阻塞)，实际 %v", elapsed)
	}
	if h.GetCatalog() == nil {
		close(block)
		t.Fatal("上游挂起时仍应由 embed seed 出非空 catalog")
	}

	close(block) // 解除挂起，让后台协程退出
	for j := 0; j < 1000 && h.refreshing.Load(); j++ {
		time.Sleep(time.Millisecond)
	}
}

// 方法3 goroutine 泄漏：EnsureCatalog 的后台刷新协程必须有界、退出干净（CAS≤1 + ctx cancel）。
func TestNoGoroutineLeak_EnsureCatalog(t *testing.T) {
	opt := goleak.IgnoreCurrent() // 基线忽略既有协程，只抓本测试新泄漏
	h := New(HubConfig{Enabled: true, RepoURL: "/nonexistent-leak-xyz"}, "")
	for range 3 {
		h.EnsureCatalog()
	}
	for j := 0; j < 1000 && h.refreshing.Load(); j++ {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond) // 宽限，让 defer/cancel 跑完
	goleak.VerifyNone(t, opt)
}

// 方法9 兼容性矩阵：旧版缓存(无 env/source 字段)向后兼容 + registry 对象/裸数组双格式兼容。
func TestCompat_CacheAndRegistrySchema(t *testing.T) {
	// 旧 schema 缓存（无 env/source）应能解析，新字段默认零值（additive 向后兼容）。
	old := []byte(`{"version":"old","updated_at":"2030-01-01T00:00:00Z","skills":[{"name":"s","type":"skill"}]}`)
	var cat Catalog
	if err := json.Unmarshal(old, &cat); err != nil {
		t.Fatalf("旧 schema 缓存应向后兼容: %v", err)
	}
	if len(cat.Skills) != 1 || cat.Skills[0].Env != nil || cat.Skills[0].Source != "" {
		t.Errorf("旧缓存新字段应为零值: %+v", cat.Skills)
	}
	// registry 对象格式 + 极老裸数组格式都要兼容。
	if s, err := parseMcpServers([]byte(`{"servers":[{"name":"a"}]}`)); err != nil || len(s) != 1 {
		t.Errorf("对象格式 registry 应兼容: %v %+v", err, s)
	}
	if s, err := parseMcpServers([]byte(`[{"name":"b"}]`)); err != nil || len(s) != 1 {
		t.Errorf("裸数组 registry 应兼容: %v %+v", err, s)
	}
}

// 方法10 基准：离线 seed/EnsureCatalog 热路径必须微秒级（证明非阻塞，替代旧的最长 30s 同步 Refresh）。
func BenchmarkEnsureCatalog_Offline(b *testing.B) {
	h := New(HubConfig{Enabled: true, RepoURL: "/nonexistent-bench"}, "")
	h.EnsureCatalog() // 预热 seed
	b.ResetTimer()
	for range b.N {
		h.EnsureCatalog()
	}
}
