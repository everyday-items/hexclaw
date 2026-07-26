package apihttp_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

type httpPracticeGenerator struct{}

func (httpPracticeGenerator) GeneratePracticeVariant(
	context.Context,
	string,
	string,
	string,
) (usecase.SolveResult, error) {
	return usecase.SolveResult{
		Solution: "## 问题\n5÷0.5=?\n\n## 解答\n把除数化为整数。\n\n## 答案\n10",
	}, nil
}

type httpPracticeValidator struct{}

func (httpPracticeValidator) Solve(
	context.Context,
	string,
	string,
	string,
) (usecase.SolveResult, error) {
	return usecase.SolveResult{
		Solution: "## 解答\n独立验算\n\n## 答案\n10",
		Evidence: usecase.SolveEvidence{
			Verdict:      usecase.VerdictAgree,
			EvidenceType: usecase.EvidenceNumericExec,
		},
	}, nil
}

func TestSinglePracticeGenerationHTTP_AcceptsThenRecoversFromDurableState(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO agents(name) VALUES('mingming')`); err != nil {
		t.Fatal(err)
	}
	rt, err := assembly.Wire(db, fakeSolveExec{})
	if err != nil {
		t.Fatal(err)
	}
	rt.Deps.Solver = httpPracticeValidator{}
	rt.Deps.PracticeVariant = httpPracticeGenerator{}
	rt.Deps.PracticeGenerationRoute = func(
		context.Context,
		k12.GradingModelSnapshot,
	) (k12.GradingModelSnapshot, error) {
		return k12.GradingModelSnapshot{
			Provider: "provider-a", Model: "model-a",
			Route: "provider-a/model-a", Capability: "text",
		}, nil
	}
	coordinator := &usecase.SinglePracticeGenerationCoordinator{
		Deps: &rt.Deps, Records: rt.Records, BaseContext: context.Background(),
	}
	h := apihttp.NewHandler(apihttp.Runtime{
		Views: rt.Registry.Views, Records: rt.Records, Deps: rt.Deps,
		PracticeGeneration: coordinator,
	})
	source, err := k12.NewMistakeRecord("mingming", "session-1", k12.MistakeFields{
		Subject: "数学", Question: "4÷0.5=8", KnowledgePoint: "小数除法",
		CanonicalAnswer: "8", EntrySource: k12.MistakeEntryPhoto,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rt.Records.Put(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	rec, out := do(
		t, h, http.MethodPost,
		"/mistakes/"+source.RecordID+"/practice-generation",
		`{"agent":"mingming","idempotency_key":"http-practice-1",`+
			`"grade":"五年级下","textbook":"人教版","difficulty":"same"}`,
	)
	if rec.Code != http.StatusAccepted || out["state"] != "pending" {
		t.Fatalf("start status=%d out=%v", rec.Code, out)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = coordinator.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}
	rec, out = do(
		t, h, http.MethodGet,
		"/mistakes/"+source.RecordID+"/practice-generation?agent=mingming", "",
	)
	if rec.Code != http.StatusOK || out["state"] != "joined" ||
		out["practice_item_id"] == "" {
		t.Fatalf("get status=%d out=%v", rec.Code, out)
	}
}
