package apihttp_test

// POST /cron/fill-basket?agent=X 是旧定时脚本的兼容端点，只返回零结果且不得写练习集。

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

func TestCronFillBasket_LegacyEndpointNeverWritesPracticeSet(t *testing.T) {
	h, db := newServerWithDB(t)

	// 先准备一条已到期错题，确保 no-op 不是因为没有候选。
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
	if out["added"] != float64(0) || out["skipped"] != float64(0) {
		t.Fatalf("legacy 端点应保持零副作用结果, got %v", out)
	}

	rec, out = do(t, h, "GET", "/practice-sets?agent=mingming&status=draft", "")
	if rec.Code != 200 {
		t.Fatalf("practice-sets 状态 %d", rec.Code)
	}
	sets, _ := out["items"].([]any)
	if len(sets) != 0 {
		t.Fatalf("legacy 端点不得创建待打印篮: %v", out)
	}

	// 重放仍然是同一个零副作用结果。
	rec, out = do(t, h, "POST", "/cron/fill-basket?agent=mingming", "")
	if rec.Code != 200 || out["added"] != float64(0) || out["skipped"] != float64(0) {
		t.Fatalf("重触发仍应保持零副作用, got %d %v", rec.Code, out)
	}
}
