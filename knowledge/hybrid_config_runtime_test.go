package knowledge

import (
	"context"
	"sync"
	"testing"
)

// 运行时检索参数面板（PUT /knowledge/config）需要 Manager 支持在线热替换混合检索配置。
// GetHybridConfig 读当前生效配置；SetHybridConfig 原子替换；并发检索与热替换不得数据竞争
// （go test -race 守卫）。
func TestManager_SetGetHybridConfig(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewSQLiteStore(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(store, store, &mockEmbedder{dim: 8}, WithSplitter(testSplitter()))

	// 初始 = DefaultHybridConfig。
	if got := mgr.GetHybridConfig(); got.MinScore != DefaultHybridConfig().MinScore {
		t.Fatalf("初始 MinScore 应为默认 %v，得 %v", DefaultHybridConfig().MinScore, got.MinScore)
	}

	c := DefaultHybridConfig()
	c.RerankEnabled = false
	c.ExpandEnabled = false
	c.MinScore = 0.42
	c.CandidateK = 17
	mgr.SetHybridConfig(c)

	got := mgr.GetHybridConfig()
	if got.RerankEnabled || got.ExpandEnabled || got.MinScore != 0.42 || got.CandidateK != 17 {
		t.Fatalf("SetHybridConfig 未生效，得 %+v", got)
	}

	// 返回值是快照：改返回的副本不影响内部状态。
	got.MinScore = 0.99
	if mgr.GetHybridConfig().MinScore != 0.42 {
		t.Fatal("GetHybridConfig 应返回快照副本，不可被外部改动穿透")
	}
}

// 并发检索 + 在线热替换：-race 下不得报竞争。
func TestManager_SetHybridConfig_RaceWithSearch(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	store := NewSQLiteStore(db)
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(store, store, &mockEmbedder{dim: 8}, WithSplitter(testSplitter()))
	if _, err := mgr.AddDocument(ctx, "并发文档", "知识库支持运行时调参。\n\n检索参数面板热生效。", "test"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	queryErrors := make(chan error, 1)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				if _, err := mgr.Query(ctx, "运行时调参", 3); err != nil {
					select {
					case queryErrors <- err:
					default:
					}
					return
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				c := DefaultHybridConfig()
				c.MinScore = float64(n%2) * 0.5
				c.RerankEnabled = j%2 == 0
				c.CandidateK = 20 + n
				mgr.SetHybridConfig(c)
			}
		}(i)
	}
	wg.Wait()
	select {
	case err := <-queryErrors:
		t.Fatalf("并发检索不应隐藏查询错误: %v", err)
	default:
	}
}
