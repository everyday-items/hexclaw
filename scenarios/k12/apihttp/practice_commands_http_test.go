package apihttp_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/curriculum"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

type practiceCommandSolver struct{ n int }

func (s *practiceCommandSolver) GenerateSimilar(context.Context, string, string, string) (usecase.SolveResult, error) {
	s.n++
	return usecase.SolveResult{Solution: fmt.Sprintf("## 问题\n变式 %d\n## 解答\n过程\n## 答案\n%d", s.n, s.n)}, nil
}

func (*practiceCommandSolver) Solve(_ context.Context, problem, _, _ string) (usecase.SolveResult, error) {
	return usecase.SolveResult{Solution: problem, Evidence: usecase.SolveEvidence{
		Verdict: usecase.VerdictAgree, EvidenceType: usecase.EvidenceNumericExec,
	}}, nil
}

func newPracticeCommandServer(t *testing.T) (http.Handler, usecase.Deps) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('mingming')`); err != nil {
		t.Fatal(err)
	}
	cur := curriculum.New()
	reg := scenario.NewRegistry()
	if err := reg.Assemble(k12.Pack(cur)); err != nil {
		t.Fatal(err)
	}
	d := usecase.Deps{
		Records: k12storage.NewStore(db, reg.Records), Constraint: cur,
		Solver: &practiceCommandSolver{}, Now: func() int64 { return 1000 },
	}
	return apihttp.NewHandler(apihttp.Runtime{Views: reg.Views, Records: d.Records, Deps: d}), d
}

func seedPracticeCommandSources(t *testing.T, d usecase.Deps, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		rec, err := k12.NewMistakeRecord("mingming", "s", k12.MistakeFields{
			Subject: "数学", Question: fmt.Sprintf("来源 %d", i), KnowledgePoint: "整数计算",
			EntrySource: k12.MistakeEntryPhoto,
		})
		if err != nil {
			t.Fatal(err)
		}
		due := int64(900 + i)
		rec.DueAt = &due
		if _, err := d.Records.Put(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
	}
}

func saveHTTPReturnAsset(t *testing.T, agent string) string {
	t.Helper()
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	raw, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	assetID, err := assetstore.Save(agent, raw)
	if err != nil {
		t.Fatal(err)
	}
	return assetID
}

func TestCustomPaperHTTP_FormalCommandAndIdempotentReplay(t *testing.T) {
	h, d := newPracticeCommandServer(t)
	seedPracticeCommandSources(t, d, 3)
	body := `{"agent":"mingming","idempotency_key":"http-paper-1","scope":"week","total":5,"per_source":2,"difficulty":"harder","textbook":"人教版","grade":"五年级下"}`
	rec, out := do(t, h, "POST", "/practice-sets/custom-paper", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("正式组卷命令状态=%d body=%v", rec.Code, out)
	}
	items, _ := out["items"].([]any)
	set, _ := out["set"].(map[string]any)
	if out["status"] != k12.PracticeGenerationCommitted || len(items) != 5 || set["status"] != k12.PracticeStatusDraft {
		t.Fatalf("组卷响应必须给完整 committed 结果: %v", out)
	}
	firstID := out["generation_job_id"]
	rec, replay := do(t, h, "POST", "/practice-sets/custom-paper", body)
	if rec.Code != http.StatusOK || replay["generation_job_id"] != firstID || len(replay["items"].([]any)) != 5 {
		t.Fatalf("HTTP 幂等重放漂移: %v", replay)
	}
}

func TestSubmitHTTP_RequiresAndReturnsAppendOnlyAssetMapping(t *testing.T) {
	h, d := newPracticeCommandServer(t)
	id, _, err := d.CreatePracticeSet(context.Background(), "mingming", "s", k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceManual, Title: "回传卷",
		Items: []k12.PracticeItem{{ItemID: "q1", Subject: "数学", QuestionMarkdown: "1+1=?",
			ExpectedAnswerMarkdown: "2", VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.FinalizeBasket(context.Background(), "mingming", id, "print", ""); err != nil {
		t.Fatal(err)
	}
	rec, _ := do(t, h, "POST", "/practice-sets/"+id+"/submit", `{"agent":"mingming"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺照片/return_id/item_ids 必须 400, got %d", rec.Code)
	}
	assetID := saveHTTPReturnAsset(t, "mingming")
	body := fmt.Sprintf(`{"agent":"mingming","return_id":"return-http-1","asset_id":%q,"item_ids":["q1"]}`, assetID)
	rec, out := do(t, h, "POST", "/practice-sets/"+id+"/submit", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("回传状态=%d body=%v", rec.Code, out)
	}
	assets, _ := out["return_assets"].([]any)
	items, _ := out["items"].([]any)
	item := items[0].(map[string]any)
	if len(assets) != 1 || len(item["return_ids"].([]any)) != 1 || item["return_ids"].([]any)[0] != "return-http-1" {
		t.Fatalf("DTO 应可从题反查照片批次: %v", out)
	}
	rec, replay := do(t, h, "POST", "/practice-sets/"+id+"/submit", body)
	if rec.Code != http.StatusOK || len(replay["return_assets"].([]any)) != 1 {
		t.Fatalf("回传幂等重放不得追加: %v", replay)
	}
}
