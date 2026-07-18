package usecase_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/curriculum"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

// newFileBackedDeps 用**文件库 + 生产 pragma**（WAL + busy_timeout 5000 + MaxOpenConns 4）构造 Deps，
// 忠实复现生产 SQLite 并发写环境（§6.15）——:memory: 多连接是独立库，无法测真实并发。
func newFileBackedDeps(t *testing.T) usecase.Deps {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "concurrency.db")
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('xiaoming')`); err != nil {
		t.Fatal(err)
	}
	cur := curriculum.New()
	reg := scenario.NewRegistry()
	if err := reg.Assemble(k12.Pack(cur)); err != nil {
		t.Fatal(err)
	}
	return usecase.Deps{Records: k12storage.NewStore(db, reg.Records), Constraint: cur, Now: func() int64 { return 1000 }}
}

// TestPracticeSetConcurrentWrites 并发创建练习集：N goroutine 并行写真实 SQLite，
// 验证无数据损坏、无 SQLITE_BUSY 致命失败、计数一致（§6.15 单机并发可靠性 + 方法3/10）。
func TestPracticeSetConcurrentWrites(t *testing.T) {
	d := newFileBackedDeps(t)
	ctx := context.Background()
	const N = 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	errs := 0
	start := time.Now()
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f := k12.PracticeSetFields{
				SourceKind: k12.PracticeSourceCustom,
				Title:      fmt.Sprintf("并发卷-%d", i),
				Items:      []k12.PracticeItem{{ItemID: "q", QuestionMarkdown: fmt.Sprintf("题%d", i), VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "验算"}},
			}
			if _, _, err := d.CreatePracticeSet(ctx, "xiaoming", "s", f); err != nil {
				mu.Lock()
				errs++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	if errs > 0 {
		t.Fatalf("并发写出现 %d 个失败（SQLITE_BUSY 或竞态）", errs)
	}
	sets, err := d.ListPracticeSets(ctx, "xiaoming", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != N {
		t.Fatalf("并发写后应有 %d 份练习集（无丢失无重复），got %d", N, len(sets))
	}
	t.Logf("并发写基线：%d 份/%.0fms（%.0f 写/秒）", N, float64(elapsed.Milliseconds()), float64(N)/elapsed.Seconds())
}
