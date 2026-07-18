package apihttp_test

// POST /cron/fill-basket?agent=X 契约（RED 先行）——§3.13 每周复习自动装篮的 HTTP 入口：
// 调 FillBasketFromDue，响应 {added, skipped}；幂等（cron 重触发不重复装篮）。
// 同时钉死批改入库带 canonical_answer（§3.8 治本①）：判错入库的错题装篮后可 verified 出答案卷。

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

// newServerWithDB 同 newServer，但暴露 db 供测试改 due_at（把错题拨到期）。
func newServerWithDB(t *testing.T) (http.Handler, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO agents(name) VALUES('mingming')`)
	k, err := assembly.Wire(db, fakeSolveExec{})
	if err != nil {
		t.Fatal(err)
	}
	return apihttp.NewHandler(apihttp.Runtime{Views: k.Registry.Views, Records: k.Records, Deps: k.Deps}), db
}

func TestCronFillBasket_AddsDueMistakeIdempotently(t *testing.T) {
	h, db := newServerWithDB(t)

	// 批改判错 → 错题入库（fakeSolveExec 固定判错，解法 "解：11.4"）。
	body := `{"agent":"mingming","grade":"五年级上","source_session":"s1","problem":"3.8×3=?","student_answer":"10.4","knowledge_points":["小数乘法"]}`
	rec, out := do(t, h, "POST", "/grade", body)
	if rec.Code != 200 || out["record_created"] != true {
		t.Fatalf("grade 入库失败: %d %v", rec.Code, out)
	}
	// 拨到期：首次复习 due=now+1天 → 改成过去，模拟周五到期扫描。
	if _, err := db.Exec(`UPDATE k12_mistakes SET due_at = 1`); err != nil {
		t.Fatal(err)
	}

	// 缺 agent → 400。
	if rec, _ := do(t, h, "POST", "/cron/fill-basket", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("缺 agent 应 400, got %d", rec.Code)
	}

	rec, out = do(t, h, "POST", "/cron/fill-basket?agent=mingming", "")
	if rec.Code != 200 {
		t.Fatalf("fill-basket 状态 %d: %v", rec.Code, out)
	}
	if out["added"] != float64(1) || out["skipped"] != float64(0) {
		t.Fatalf("首轮应 {added:1, skipped:0}, got %v", out)
	}

	// 篮内该题带批改答案（canonical_answer 治本①）→ 数学达门 verified，可出答案卷。
	rec, out = do(t, h, "GET", "/practice-sets?agent=mingming&status=draft", "")
	if rec.Code != 200 {
		t.Fatalf("practice-sets 状态 %d", rec.Code)
	}
	sets, _ := out["items"].([]any)
	if len(sets) != 1 {
		t.Fatalf("应恰有 1 个待打印篮: %v", out)
	}
	items, _ := sets[0].(map[string]any)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("篮内应 1 题: %v", sets[0])
	}
	it := items[0].(map[string]any)
	if it["added_via"] != "weekly" {
		t.Errorf("added_via=%v want weekly", it["added_via"])
	}
	if it["verification_status"] != "verified" {
		t.Errorf("批改入库带答案的数学题应 verified, got %v", it["verification_status"])
	}
	if ans, _ := it["expected_answer_markdown"].(string); ans == "" {
		t.Errorf("expected_answer_markdown 应带批改结论答案（canonical_answer）, got %v", it)
	}

	// 幂等：cron 重触发不重复装。
	rec, out = do(t, h, "POST", "/cron/fill-basket?agent=mingming", "")
	if rec.Code != 200 || out["added"] != float64(0) || out["skipped"] != float64(1) {
		t.Fatalf("重触发应 {added:0, skipped:1}, got %d %v", rec.Code, out)
	}
}
