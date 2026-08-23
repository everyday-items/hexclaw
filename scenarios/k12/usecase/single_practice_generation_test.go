package usecase_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type singlePracticeGenerator struct {
	mu        sync.Mutex
	fail      bool
	ambiguous bool
	calls     int
	snapshot  k12.GradingModelSnapshot
}

type singlePracticeProviderResponseError struct{}

func (singlePracticeProviderResponseError) Error() string {
	return "provider unavailable"
}

func (singlePracticeProviderResponseError) ProviderResponseStatusCode() int {
	return 503
}

func (g *singlePracticeGenerator) GeneratePracticeVariant(
	ctx context.Context,
	_, _, _ string,
) (usecase.SolveResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	g.snapshot, _ = k12.GradingModelSnapshotFromContext(ctx)
	if g.fail {
		if g.ambiguous {
			return usecase.SolveResult{}, fmt.Errorf("connection reset after send")
		}
		return usecase.SolveResult{}, singlePracticeProviderResponseError{}
	}
	return usecase.SolveResult{
		Solution: "## 问题\n5÷0.5=?\n\n## 解答\n把除数化为整数。\n\n## 答案\n10",
	}, nil
}

type singlePracticeValidator struct{}

func (singlePracticeValidator) Solve(
	_ context.Context,
	_, _, _ string,
) (usecase.SolveResult, error) {
	return usecase.SolveResult{
		Solution: "## 解答\n独立验算\n\n## 答案\n10",
		Evidence: usecase.SolveEvidence{
			Verdict:      usecase.VerdictAgree,
			EvidenceType: usecase.EvidenceNumericExec,
		},
	}, nil
}

type countingSinglePracticeValidator struct {
	calls int
}

func (v *countingSinglePracticeValidator) Solve(
	_ context.Context,
	_, _, _ string,
) (usecase.SolveResult, error) {
	v.calls++
	return singlePracticeValidator{}.Solve(context.Background(), "", "", "")
}

func singlePracticeTestDigest(parts ...[]byte) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(part)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func singlePracticeTestPrompt() string {
	return "生成一道同等难度的小学变式练习。保持来源知识点与方法，不复述原题，不泄露答案；教材边界=人教版；年级=五年级下；来源题=4÷0.5=8；知识点=小数除法。严格输出 ## 问题 / ## 解答 / ## 答案 三段 Markdown。"
}

func seedSinglePracticeMistake(t *testing.T, d usecase.Deps, idempotencySuffix string) string {
	t.Helper()
	rec, err := k12.NewMistakeRecord("xiaoming", "source-session", k12.MistakeFields{
		Subject: "数学", Question: "4÷0.5=8", KnowledgePoint: "小数除法",
		CanonicalAnswer: "8", EntrySource: k12.MistakeEntryPhoto,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Records.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return rec.RecordID + idempotencySuffix
}

func singlePracticeRequest(key string) usecase.SinglePracticeGenerationRequest {
	return usecase.SinglePracticeGenerationRequest{
		IdempotencyKey: key,
		Grade:          "五年级下",
		Textbook:       "人教版",
		Difficulty:     "same",
		Provider:       "provider-a",
		Model:          "model-a",
		SourceSession:  "source-session",
	}
}

func TestSinglePracticeGeneration_OneClickPersistsThenCommitsOnFrozenRoute(t *testing.T) {
	d := newDataDeps(t)
	generator := &singlePracticeGenerator{}
	d.Solver = singlePracticeValidator{}
	d.PracticeVariant = generator
	routeCalls := 0
	d.PracticeGenerationRoute = func(
		_ context.Context,
		requested k12.GradingModelSnapshot,
	) (k12.GradingModelSnapshot, error) {
		routeCalls++
		if requested.Provider != "provider-a" || requested.Model != "model-a" {
			t.Fatalf("explicit route lost: %+v", requested)
		}
		return k12.GradingModelSnapshot{
			Provider: "provider-a", Model: "model-a",
			Route: "provider-a/model-a", Capability: "text",
		}, nil
	}
	sourceID := seedSinglePracticeMistake(t, d, "")
	req := singlePracticeRequest("single:" + sourceID + ":1")

	pending, err := d.StartSinglePracticeGeneration(context.Background(), "xiaoming", sourceID, req)
	if err != nil {
		t.Fatal(err)
	}
	if pending.State != usecase.SinglePracticePending ||
		pending.GenerationJobID == "" || pending.PracticeItemID == "" {
		t.Fatalf("pending projection=%+v", pending)
	}
	// The same accepted command must replay before consulting a mutable default
	// route again.
	replayed, err := d.StartSinglePracticeGeneration(context.Background(), "xiaoming", sourceID, req)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.GenerationJobID != pending.GenerationJobID || routeCalls != 1 {
		t.Fatalf("replay changed identity or re-resolved route: replay=%+v routeCalls=%d", replayed, routeCalls)
	}
	joined, err := d.ProcessSinglePracticeGeneration(
		context.Background(), "xiaoming", pending.GenerationJobID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if joined.State != usecase.SinglePracticeJoined || joined.Item == nil ||
		!k12.PracticeItemPublishable(*joined.Item) {
		t.Fatalf("joined projection=%+v", joined)
	}
	if generator.snapshot.Provider != "provider-a" ||
		generator.snapshot.Model != "model-a" ||
		generator.snapshot.Route != "provider-a/model-a" {
		t.Fatalf("worker ignored frozen route: %+v", generator.snapshot)
	}
	invocations, err := d.Records.ListPracticeGenerationInvocations(
		context.Background(), "xiaoming", pending.GenerationJobID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(invocations) != 2 ||
		invocations[0].Status != k12.ModelInvocationSucceeded ||
		invocations[1].Status != k12.ModelInvocationSucceeded {
		t.Fatalf("generate+validate must each have one succeeded invocation: %+v", invocations)
	}
}

func TestSinglePracticeGeneration_FailureRetryReusesFrozenJobAndPlaceholder(t *testing.T) {
	d := newDataDeps(t)
	generator := &singlePracticeGenerator{fail: true}
	d.Solver = singlePracticeValidator{}
	d.PracticeVariant = generator
	d.PracticeGenerationRoute = func(
		_ context.Context,
		_ k12.GradingModelSnapshot,
	) (k12.GradingModelSnapshot, error) {
		return k12.GradingModelSnapshot{
			Provider: "provider-a", Model: "model-a", Route: "provider-a/model-a",
			Capability: "text",
		}, nil
	}
	sourceID := seedSinglePracticeMistake(t, d, "")
	pending, err := d.StartSinglePracticeGeneration(
		context.Background(), "xiaoming", sourceID,
		singlePracticeRequest("single:"+sourceID+":failure"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ProcessSinglePracticeGeneration(
		context.Background(), "xiaoming", pending.GenerationJobID,
	); err == nil {
		t.Fatal("provider failure unexpectedly succeeded")
	}
	failed, err := d.GetSinglePracticeGeneration(
		context.Background(), "xiaoming", sourceID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != usecase.SinglePracticeFailed {
		t.Fatalf("failed projection=%+v", failed)
	}
	generator.mu.Lock()
	generator.fail = false
	generator.mu.Unlock()
	requeued, err := d.RetrySinglePracticeGeneration(
		context.Background(), "xiaoming", sourceID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if requeued.GenerationJobID != pending.GenerationJobID ||
		requeued.PracticeItemID != pending.PracticeItemID ||
		requeued.State != usecase.SinglePracticePending {
		t.Fatalf("retry changed durable identity: pending=%+v retry=%+v", pending, requeued)
	}
	joined, err := d.ProcessSinglePracticeGeneration(
		context.Background(), "xiaoming", pending.GenerationJobID,
	)
	if err != nil || joined.State != usecase.SinglePracticeJoined {
		t.Fatalf("retry process: joined=%+v err=%v", joined, err)
	}
}

func TestSinglePracticeGeneration_RecoversDurableGenerationOutputWithoutResend(t *testing.T) {
	d := newDataDeps(t)
	generator := &singlePracticeGenerator{}
	d.Solver = singlePracticeValidator{}
	d.PracticeVariant = generator
	d.PracticeGenerationRoute = func(
		_ context.Context,
		_ k12.GradingModelSnapshot,
	) (k12.GradingModelSnapshot, error) {
		return k12.GradingModelSnapshot{
			Provider: "provider-a", Model: "model-a",
			Route: "provider-a/model-a", Capability: "text",
		}, nil
	}
	sourceID := seedSinglePracticeMistake(t, d, "")
	pending, err := d.StartSinglePracticeGeneration(
		context.Background(), "xiaoming", sourceID,
		singlePracticeRequest("single:"+sourceID+":checkpoint"),
	)
	if err != nil {
		t.Fatal(err)
	}
	job, err := d.Records.AdvanceSinglePracticeGeneration(
		context.Background(), "xiaoming", pending.GenerationJobID,
		k12.PracticeGenerationGenerating, 1, k12.PracticeItem{}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	var route k12.GradingModelSnapshot
	if err = json.Unmarshal([]byte(job.RouteSnapshot), &route); err != nil {
		t.Fatal(err)
	}
	invocation, _, err := d.Records.PreparePracticeGenerationInvocation(
		context.Background(),
		k12.ModelInvocation{
			InvocationID: "modelinv-practice-checkpoint",
			AgentName:    "xiaoming",
			JobID:        job.GenerationJobID,
			Stage:        "practice_generate",
			RequestDigest: singlePracticeTestDigest(
				[]byte("practice_generate"), []byte(singlePracticeTestPrompt()),
			),
			RouteSnapshot: route,
			Attempt:       1,
			CreatedAt:     1000,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	invocation, err = d.Records.MarkPracticeGenerationInvocationSent(
		context.Background(), "xiaoming", invocation.InvocationID, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	generated := usecase.SolveResult{
		Solution: "## 问题\n5÷0.5=?\n\n## 解答\n把除数化为整数。\n\n## 答案\n10",
	}
	raw, err := json.Marshal(generated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Records.SaveSinglePracticeGenerationOutput(
		context.Background(), "xiaoming", job.GenerationJobID, 1, string(raw),
	); err != nil {
		t.Fatal(err)
	}

	joined, err := d.ProcessSinglePracticeGeneration(
		context.Background(), "xiaoming", job.GenerationJobID,
	)
	if err != nil || joined.State != usecase.SinglePracticeJoined {
		t.Fatalf("resume durable output: view=%+v err=%v", joined, err)
	}
	if generator.calls != 0 {
		t.Fatalf("durable provider result was resent %d times", generator.calls)
	}
	gotInvocation, err := d.Records.GetPracticeGenerationInvocation(
		context.Background(), "xiaoming", invocation.InvocationID,
	)
	if err != nil || gotInvocation.Status != k12.ModelInvocationSucceeded {
		t.Fatalf("sent invocation not converged from durable output: %+v err=%v", gotInvocation, err)
	}
}

func TestSinglePracticeGeneration_RecoversDurableValidationOutputWithoutResend(t *testing.T) {
	d := newDataDeps(t)
	generator := &singlePracticeGenerator{}
	validator := &countingSinglePracticeValidator{}
	d.Solver = validator
	d.PracticeVariant = generator
	d.PracticeGenerationRoute = func(
		_ context.Context,
		_ k12.GradingModelSnapshot,
	) (k12.GradingModelSnapshot, error) {
		return k12.GradingModelSnapshot{
			Provider: "provider-a", Model: "model-a",
			Route: "provider-a/model-a", Capability: "text",
		}, nil
	}
	sourceID := seedSinglePracticeMistake(t, d, "")
	pending, err := d.StartSinglePracticeGeneration(
		context.Background(), "xiaoming", sourceID,
		singlePracticeRequest("single:"+sourceID+":validation-checkpoint"),
	)
	if err != nil {
		t.Fatal(err)
	}
	job, err := d.Records.AdvanceSinglePracticeGeneration(
		context.Background(), "xiaoming", pending.GenerationJobID,
		k12.PracticeGenerationGenerating, 1, k12.PracticeItem{}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	var route k12.GradingModelSnapshot
	if err = json.Unmarshal([]byte(job.RouteSnapshot), &route); err != nil {
		t.Fatal(err)
	}
	generated := usecase.SolveResult{
		Solution: "## 问题\n5÷0.5=?\n\n## 解答\n把除数化为整数。\n\n## 答案\n10",
	}
	generatedRaw, _ := json.Marshal(generated)
	generateInvocation, _, err := d.Records.PreparePracticeGenerationInvocation(
		context.Background(),
		k12.ModelInvocation{
			InvocationID: "modelinv-practice-generate-durable",
			AgentName:    "xiaoming",
			JobID:        job.GenerationJobID,
			Stage:        k12.PracticeGenerationStageGenerate,
			RequestDigest: singlePracticeTestDigest(
				[]byte(k12.PracticeGenerationStageGenerate),
				[]byte(singlePracticeTestPrompt()),
			),
			RouteSnapshot: route,
			Attempt:       1,
			CreatedAt:     1000,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Records.MarkPracticeGenerationInvocationSent(
		context.Background(), "xiaoming", generateInvocation.InvocationID, "",
	); err != nil {
		t.Fatal(err)
	}
	job, err = d.Records.SaveSinglePracticeGenerationOutput(
		context.Background(), "xiaoming", job.GenerationJobID, 1, string(generatedRaw),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Records.MarkPracticeGenerationInvocationSucceeded(
		context.Background(), "xiaoming", generateInvocation.InvocationID,
		singlePracticeTestDigest(generatedRaw), "",
	); err != nil {
		t.Fatal(err)
	}
	job, err = d.Records.AdvanceSinglePracticeGeneration(
		context.Background(), "xiaoming", job.GenerationJobID,
		k12.PracticeGenerationValidating, 1, k12.PracticeItem{}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	validationRequest, _ := json.Marshal(struct {
		Subject  string `json:"subject"`
		Question string `json:"question"`
		Grade    string `json:"grade"`
	}{"数学", "5÷0.5=?", "五年级下"})
	validationInvocation, _, err := d.Records.PreparePracticeGenerationInvocation(
		context.Background(),
		k12.ModelInvocation{
			InvocationID: "modelinv-practice-validate-durable",
			AgentName:    "xiaoming",
			JobID:        job.GenerationJobID,
			Stage:        k12.PracticeGenerationStageValidate,
			RequestDigest: singlePracticeTestDigest(
				[]byte(k12.PracticeGenerationStageValidate), validationRequest,
			),
			RouteSnapshot: route,
			Attempt:       1,
			CreatedAt:     1000,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Records.MarkPracticeGenerationInvocationSent(
		context.Background(), "xiaoming", validationInvocation.InvocationID, "",
	); err != nil {
		t.Fatal(err)
	}
	validated, _ := validator.Solve(context.Background(), "", "", "")
	validator.calls = 0
	validatedRaw, _ := json.Marshal(validated)
	if _, err = d.Records.SaveSinglePracticeValidationOutput(
		context.Background(), "xiaoming", job.GenerationJobID, 1, string(validatedRaw),
	); err != nil {
		t.Fatal(err)
	}

	joined, err := d.ProcessSinglePracticeGeneration(
		context.Background(), "xiaoming", job.GenerationJobID,
	)
	if err != nil || joined.State != usecase.SinglePracticeJoined {
		t.Fatalf("resume validation output: view=%+v err=%v", joined, err)
	}
	if generator.calls != 0 || validator.calls != 0 {
		t.Fatalf("durable outputs resent: generator=%d validator=%d",
			generator.calls, validator.calls)
	}
	validationInvocation, err = d.Records.GetPracticeGenerationInvocation(
		context.Background(), "xiaoming", validationInvocation.InvocationID,
	)
	if err != nil || validationInvocation.Status != k12.ModelInvocationSucceeded {
		t.Fatalf("validation invocation not converged: %+v err=%v",
			validationInvocation, err)
	}
}

func TestSinglePracticeGeneration_SentWithoutDurableOutputRequiresReconciliation(t *testing.T) {
	d := newDataDeps(t)
	generator := &singlePracticeGenerator{}
	d.Solver = singlePracticeValidator{}
	d.PracticeVariant = generator
	d.PracticeGenerationRoute = func(
		_ context.Context,
		_ k12.GradingModelSnapshot,
	) (k12.GradingModelSnapshot, error) {
		return k12.GradingModelSnapshot{
			Provider: "provider-a", Model: "model-a",
			Route: "provider-a/model-a", Capability: "text",
		}, nil
	}
	sourceID := seedSinglePracticeMistake(t, d, "")
	pending, err := d.StartSinglePracticeGeneration(
		context.Background(), "xiaoming", sourceID,
		singlePracticeRequest("single:"+sourceID+":sent-no-output"),
	)
	if err != nil {
		t.Fatal(err)
	}
	job, err := d.Records.AdvanceSinglePracticeGeneration(
		context.Background(), "xiaoming", pending.GenerationJobID,
		k12.PracticeGenerationGenerating, 1, k12.PracticeItem{}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	var route k12.GradingModelSnapshot
	if err = json.Unmarshal([]byte(job.RouteSnapshot), &route); err != nil {
		t.Fatal(err)
	}
	invocation, _, err := d.Records.PreparePracticeGenerationInvocation(
		context.Background(),
		k12.ModelInvocation{
			InvocationID: "modelinv-practice-unknown",
			AgentName:    "xiaoming",
			JobID:        job.GenerationJobID,
			Stage:        "practice_generate",
			RequestDigest: singlePracticeTestDigest(
				[]byte("practice_generate"), []byte(singlePracticeTestPrompt()),
			),
			RouteSnapshot: route,
			Attempt:       1,
			CreatedAt:     1000,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Records.MarkPracticeGenerationInvocationSent(
		context.Background(), "xiaoming", invocation.InvocationID, "",
	); err != nil {
		t.Fatal(err)
	}
	if _, err = d.ProcessSinglePracticeGeneration(
		context.Background(), "xiaoming", job.GenerationJobID,
	); !errors.Is(err, usecase.ErrModelInvocationRequiresReconciliation) {
		t.Fatalf("sent without output err=%v want reconciliation", err)
	}
	if generator.calls != 0 {
		t.Fatalf("ambiguous sent invocation was resent %d times", generator.calls)
	}
}

func TestSinglePracticeGeneration_OutcomeUnknownCannotBlindRetry(t *testing.T) {
	d := newDataDeps(t)
	generator := &singlePracticeGenerator{fail: true, ambiguous: true}
	d.Solver = singlePracticeValidator{}
	d.PracticeVariant = generator
	d.PracticeGenerationRoute = func(
		_ context.Context,
		_ k12.GradingModelSnapshot,
	) (k12.GradingModelSnapshot, error) {
		return k12.GradingModelSnapshot{
			Provider: "provider-a", Model: "model-a",
			Route: "provider-a/model-a", Capability: "text",
		}, nil
	}
	sourceID := seedSinglePracticeMistake(t, d, "")
	pending, err := d.StartSinglePracticeGeneration(
		context.Background(), "xiaoming", sourceID,
		singlePracticeRequest("single:"+sourceID+":unknown"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.ProcessSinglePracticeGeneration(
		context.Background(), "xiaoming", pending.GenerationJobID,
	); !errors.Is(err, usecase.ErrModelInvocationRequiresReconciliation) {
		t.Fatalf("ambiguous provider result err=%v want reconciliation", err)
	}
	generator.mu.Lock()
	generator.fail = false
	generator.mu.Unlock()
	if _, err = d.RetrySinglePracticeGeneration(
		context.Background(), "xiaoming", sourceID,
	); !errors.Is(err, usecase.ErrModelInvocationRequiresReconciliation) {
		t.Fatalf("ordinary retry after outcome_unknown err=%v want reconciliation", err)
	}
	if generator.calls != 1 {
		t.Fatalf("outcome_unknown was resent: calls=%d", generator.calls)
	}
}

func TestSinglePracticeGenerationCoordinator_RecoversQueuedJobAfterRestart(t *testing.T) {
	d := newDataDeps(t)
	generator := &singlePracticeGenerator{}
	d.Solver = singlePracticeValidator{}
	d.PracticeVariant = generator
	d.PracticeGenerationRoute = func(
		_ context.Context,
		_ k12.GradingModelSnapshot,
	) (k12.GradingModelSnapshot, error) {
		return k12.GradingModelSnapshot{
			Provider: "provider-a", Model: "model-a", Route: "provider-a/model-a",
			Capability: "text",
		}, nil
	}
	sourceID := seedSinglePracticeMistake(t, d, "")
	pending, err := d.StartSinglePracticeGeneration(
		context.Background(), "xiaoming", sourceID,
		singlePracticeRequest("single:"+sourceID+":recover"),
	)
	if err != nil {
		t.Fatal(err)
	}
	restarted := &usecase.SinglePracticeGenerationCoordinator{
		Deps: &d, Records: d.Records, BaseContext: context.Background(),
	}
	recovered, err := restarted.Recover(context.Background())
	if err != nil || recovered != 1 {
		t.Fatalf("recover queued generation: recovered=%d err=%v", recovered, err)
	}
	if restarted.StartAsync("xiaoming", pending.GenerationJobID) {
		t.Fatal("active generation scheduled twice")
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := restarted.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}
	joined, err := d.GetSinglePracticeGeneration(context.Background(), "xiaoming", sourceID)
	if err != nil || joined.State != usecase.SinglePracticeJoined || generator.calls != 1 {
		t.Fatalf("recovered job did not converge once: view=%+v calls=%d err=%v",
			joined, generator.calls, err)
	}
	if recovered, err = restarted.Recover(context.Background()); err != nil || recovered != 0 {
		t.Fatalf("terminal job recovered again: recovered=%d err=%v", recovered, err)
	}
}

func TestSinglePracticeGeneration_RemoveCommittedItemRetiresJobAndReturnsReAdd(t *testing.T) {
	d := newDataDeps(t)
	generator := &singlePracticeGenerator{}
	d.Solver = singlePracticeValidator{}
	d.PracticeVariant = generator
	d.PracticeGenerationRoute = func(
		_ context.Context,
		_ k12.GradingModelSnapshot,
	) (k12.GradingModelSnapshot, error) {
		return k12.GradingModelSnapshot{
			Provider: "provider-a", Model: "model-a",
			Route: "provider-a/model-a", Capability: "text",
		}, nil
	}
	sourceID := seedSinglePracticeMistake(t, d, "")
	pending, err := d.StartSinglePracticeGeneration(
		context.Background(), "xiaoming", sourceID,
		singlePracticeRequest("single:"+sourceID+":remove-committed"),
	)
	if err != nil {
		t.Fatal(err)
	}
	joined, err := d.ProcessSinglePracticeGeneration(
		context.Background(), "xiaoming", pending.GenerationJobID,
	)
	if err != nil || joined.State != usecase.SinglePracticeJoined ||
		joined.PracticeSetID == "" || joined.PracticeItemID == "" {
		t.Fatalf("commit before remove: view=%+v err=%v", joined, err)
	}

	if err = d.RemoveFromBasket(
		context.Background(), "xiaoming",
		joined.PracticeSetID, joined.PracticeItemID,
	); err != nil {
		t.Fatal(err)
	}
	job, err := d.Records.GetPracticeGenerationJobByID(
		context.Background(), "xiaoming", pending.GenerationJobID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if job.RetiredAt == 0 || job.RetiredReason != "removed" {
		t.Fatalf("removed item did not retire generation: %+v", job)
	}
	projected, err := d.GetSinglePracticeGeneration(
		context.Background(), "xiaoming", sourceID,
	)
	if err != nil || projected.State != usecase.SinglePracticeReAdd {
		t.Fatalf("removed generation projection=%+v err=%v", projected, err)
	}
	basket, err := d.GetPracticeSet(
		context.Background(), "xiaoming", joined.PracticeSetID,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range basket.Fields.Items {
		if item.ItemID == joined.PracticeItemID {
			t.Fatalf("removed practice item still present: %+v", item)
		}
	}
}

func TestSinglePracticeGeneration_RemovePendingItemPreventsProviderCall(t *testing.T) {
	d := newDataDeps(t)
	generator := &singlePracticeGenerator{}
	validator := &countingSinglePracticeValidator{}
	d.Solver = validator
	d.PracticeVariant = generator
	d.PracticeGenerationRoute = func(
		_ context.Context,
		_ k12.GradingModelSnapshot,
	) (k12.GradingModelSnapshot, error) {
		return k12.GradingModelSnapshot{
			Provider: "provider-a", Model: "model-a",
			Route: "provider-a/model-a", Capability: "text",
		}, nil
	}
	sourceID := seedSinglePracticeMistake(t, d, "")
	pending, err := d.StartSinglePracticeGeneration(
		context.Background(), "xiaoming", sourceID,
		singlePracticeRequest("single:"+sourceID+":remove-pending"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if pending.PracticeSetID == "" || pending.PracticeItemID == "" {
		t.Fatalf("pending identity missing: %+v", pending)
	}
	if err = d.RemoveFromBasket(
		context.Background(), "xiaoming",
		pending.PracticeSetID, pending.PracticeItemID,
	); err != nil {
		t.Fatal(err)
	}
	projected, err := d.ProcessSinglePracticeGeneration(
		context.Background(), "xiaoming", pending.GenerationJobID,
	)
	if err != nil || projected.State != usecase.SinglePracticeReAdd {
		t.Fatalf("retired worker projection=%+v err=%v", projected, err)
	}
	if generator.calls != 0 || validator.calls != 0 {
		t.Fatalf("retired generation called model: generator=%d validator=%d",
			generator.calls, validator.calls)
	}
}
