package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type customPaperSolver struct {
	mu                   sync.Mutex
	generateCalls        int
	validateCalls        int
	failAt               int
	validateFailAt       int
	wrongGeneratedAnswer bool
	duplicate            bool
	prompts              []string
}

func (s *customPaperSolver) GenerateSimilar(_ context.Context, _ string, prompt, _ string) (usecase.SolveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generateCalls++
	s.prompts = append(s.prompts, prompt)
	if s.failAt > 0 && s.generateCalls == s.failAt {
		return usecase.SolveResult{}, fmt.Errorf("generator unavailable")
	}
	n := s.generateCalls
	if s.duplicate {
		n = 1
	}
	answer := n
	if s.wrongGeneratedAnswer {
		answer = 999
	}
	return usecase.SolveResult{Solution: fmt.Sprintf("## 问题\n变式题 %d\n\n## 解答\n过程 %d\n\n## 答案\n%d", n, n, answer)}, nil
}

func (s *customPaperSolver) Solve(_ context.Context, problem, _, _ string) (usecase.SolveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.validateCalls++
	if s.validateFailAt > 0 && s.validateCalls == s.validateFailAt {
		return usecase.SolveResult{}, fmt.Errorf("validator unavailable")
	}
	answer := strings.TrimSpace(strings.TrimPrefix(problem, "变式题"))
	return usecase.SolveResult{
		Solution: fmt.Sprintf("## 解答\n独立验算\n\n## 答案\n%s", answer),
		Evidence: usecase.SolveEvidence{Verdict: usecase.VerdictAgree, EvidenceType: usecase.EvidenceNumericExec},
	}, nil
}

func seedCustomPaperMistakes(t *testing.T, d usecase.Deps, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		rec, err := k12.NewMistakeRecord("xiaoming", "source", k12.MistakeFields{
			Subject: "数学", Question: fmt.Sprintf("来源题 %d", i), KnowledgePoint: "小数乘法",
			CanonicalAnswer: fmt.Sprintf("答案 %d", i), EntrySource: k12.MistakeEntryPhoto,
		})
		if err != nil {
			t.Fatal(err)
		}
		due := int64(500 + i)
		rec.DueAt = &due
		if _, err := d.Records.Put(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
	}
}

func customPaperReq(key string) usecase.CustomPaperRequest {
	return usecase.CustomPaperRequest{
		IdempotencyKey: key,
		Scope:          "week",
		Total:          "5",
		PerSource:      2,
		Difficulty:     "harder",
		Textbook:       "人教版",
		Grade:          "五年级下",
		SourceSession:  "session-custom",
	}
}

func TestGenerateCustomPaper_UsesAllParametersAndCommitsOnce(t *testing.T) {
	d := newDataDeps(t)
	seedCustomPaperMistakes(t, d, 3)
	solver := &customPaperSolver{}
	d.Solver = solver

	result, err := d.GenerateCustomPaper(context.Background(), "xiaoming", customPaperReq("paper-command-1"))
	if err != nil {
		t.Fatalf("自定义组卷: %v", err)
	}
	if result.GenerationJobID == "" || result.Status != k12.PracticeGenerationCommitted {
		t.Fatalf("命令应 committed 且有稳定 job id: %+v", result)
	}
	if result.Set.Record == nil || result.Set.Record.Status != k12.PracticeStatusDraft || len(result.Items) != 5 {
		t.Fatalf("应原子提交 5 道到同一 draft 篮: %+v", result)
	}
	if solver.generateCalls != 5 || solver.validateCalls != 5 {
		t.Fatalf("每题必须生成并独立验证一次: generate=%d validate=%d", solver.generateCalls, solver.validateCalls)
	}
	perSource := map[string]int{}
	for _, item := range result.Items {
		perSource[item.SourceProblemID]++
		if item.ActualDifficulty != "harder" || item.VerificationStatus != k12.PracticeItemVerified || item.VerificationEvidence == "" {
			t.Fatalf("每题须返回实际难度与验证结果: %+v", item)
		}
	}
	for source, count := range perSource {
		if count > 2 {
			t.Fatalf("来源 %s 超过 per_source=2: %d", source, count)
		}
	}
	for _, prompt := range solver.prompts {
		if !strings.Contains(prompt, "更难") || !strings.Contains(prompt, "人教版") || !strings.Contains(prompt, "五年级下") {
			t.Fatalf("difficulty/textbook/grade 未真实进入生成约束: %q", prompt)
		}
	}
}

func TestGenerateCustomPaper_IdempotentReplayAndDigestConflict(t *testing.T) {
	d := newDataDeps(t)
	seedCustomPaperMistakes(t, d, 3)
	solver := &customPaperSolver{}
	d.Solver = solver
	req := customPaperReq("paper-command-idem")
	first, err := d.GenerateCustomPaper(context.Background(), "xiaoming", req)
	if err != nil {
		t.Fatal(err)
	}
	calls := solver.generateCalls
	second, err := d.GenerateCustomPaper(context.Background(), "xiaoming", req)
	if err != nil {
		t.Fatalf("幂等重放: %v", err)
	}
	if second.GenerationJobID != first.GenerationJobID || second.Set.Record.RecordID != first.Set.Record.RecordID || solver.generateCalls != calls {
		t.Fatalf("幂等重放不应再次生成/装篮: first=%+v second=%+v calls=%d→%d", first, second, calls, solver.generateCalls)
	}
	spaced := req
	spaced.IdempotencyKey = "  " + req.IdempotencyKey + "  "
	third, err := d.GenerateCustomPaper(context.Background(), "  xiaoming  ", spaced)
	if err != nil || third.GenerationJobID != first.GenerationJobID || solver.generateCalls != calls {
		t.Fatalf("owner/key 的无意义空白不得绕过同一幂等命令: third=%+v calls=%d→%d err=%v", third, calls, solver.generateCalls, err)
	}
	req.Difficulty = "easier"
	if _, err := d.GenerateCustomPaper(context.Background(), "xiaoming", req); !errors.Is(err, usecase.ErrInvalidInput) {
		t.Fatalf("同 key 不同请求摘要必须冲突: %v", err)
	}
}

func TestGenerateCustomPaper_FailureLeavesZeroHalfBasketAndRetryDoesNotDuplicate(t *testing.T) {
	d := newDataDeps(t)
	seedCustomPaperMistakes(t, d, 3)
	solver := &customPaperSolver{failAt: 2}
	d.Solver = solver
	req := customPaperReq("paper-command-retry")
	if _, err := d.GenerateCustomPaper(context.Background(), "xiaoming", req); !errors.Is(err, usecase.ErrSolveFailed) {
		t.Fatalf("生成失败应透传 ErrSolveFailed: %v", err)
	}
	sets, err := d.ListPracticeSets(context.Background(), "xiaoming", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 0 {
		t.Fatalf("失败不得留下空篮或半篮: %+v", sets)
	}
	job, err := d.Records.GetPracticeGenerationJob(context.Background(), "xiaoming", req.IdempotencyKey)
	if err != nil || job.Status != k12.PracticeGenerationFailed || job.ResultSetID != "" {
		t.Fatalf("失败收据应可审计且无结果篮: job=%+v err=%v", job, err)
	}

	solver.failAt = 0
	result, err := d.GenerateCustomPaper(context.Background(), "xiaoming", req)
	if err != nil {
		t.Fatalf("同幂等请求失败后重试: %v", err)
	}
	if len(result.Items) != 5 {
		t.Fatalf("重试必须完整提交，不得混入首轮半成品: %d", len(result.Items))
	}
	replay, err := d.GenerateCustomPaper(context.Background(), "xiaoming", req)
	if err != nil || replay.Set.Record.RecordID != result.Set.Record.RecordID || len(replay.Items) != 5 {
		t.Fatalf("成功后重放不得重复: %+v err=%v", replay, err)
	}
}

func TestGenerateCustomPaper_UsesIndependentlyValidatedAnswer(t *testing.T) {
	d := newDataDeps(t)
	seedCustomPaperMistakes(t, d, 3)
	solver := &customPaperSolver{wrongGeneratedAnswer: true}
	d.Solver = solver

	result, err := d.GenerateCustomPaper(context.Background(), "xiaoming", customPaperReq("paper-command-answer"))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range result.Items {
		if item.ExpectedAnswerMarkdown == "999" || item.VerificationStatus != k12.PracticeItemVerified {
			t.Fatalf("强证据独立验算必须替换生成器自报答案后才可 verified: %+v", item)
		}
	}
}

func TestGenerateCustomPaper_ValidationFailureDoesNotMutateExistingBasket(t *testing.T) {
	d := newDataDeps(t)
	seedCustomPaperMistakes(t, d, 3)
	_, _, err := d.CreatePracticeSet(context.Background(), "xiaoming", "existing", k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceManual,
		Title:      "既有待打印集合",
		Items: []k12.PracticeItem{{
			ItemID: "existing-item", Subject: "数学", AddedVia: k12.PracticeAddedViaManual,
			QuestionMarkdown: "已有题", ExpectedAnswerMarkdown: "已有答案",
			VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := d.ListPracticeSets(context.Background(), "xiaoming", k12.PracticeStatusDraft)
	if err != nil || len(before) != 1 {
		t.Fatalf("读取既有集合: len=%d err=%v", len(before), err)
	}
	solver := &customPaperSolver{validateFailAt: 2}
	d.Solver = solver
	req := customPaperReq("paper-command-validation-fail")
	if _, err := d.GenerateCustomPaper(context.Background(), "xiaoming", req); !errors.Is(err, usecase.ErrSolveFailed) {
		t.Fatalf("验证失败应透传 ErrSolveFailed: %v", err)
	}
	after, err := d.ListPracticeSets(context.Background(), "xiaoming", k12.PracticeStatusDraft)
	if err != nil || len(after) != 1 {
		t.Fatalf("失败后读取集合: len=%d err=%v", len(after), err)
	}
	if after[0].Record.Version != before[0].Record.Version || len(after[0].Fields.Items) != 1 || after[0].Fields.Items[0].ItemID != "existing-item" {
		t.Fatalf("第 N 题验证失败不得改动既有集合: before=%+v after=%+v", before[0], after[0])
	}
}

func TestGenerateCustomPaper_AllDeduplicatedCommitsReceiptWithoutTouchingBasket(t *testing.T) {
	d := newDataDeps(t)
	seedCustomPaperMistakes(t, d, 3)
	id, _, err := d.CreatePracticeSet(context.Background(), "xiaoming", "existing", k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceManual,
		Title:      "既有待打印集合",
		Items: []k12.PracticeItem{{
			ItemID: "existing-duplicate", Subject: "数学", AddedVia: k12.PracticeAddedViaManual,
			QuestionMarkdown: "变式题 1", ExpectedAnswerMarkdown: "1",
			VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := d.GetPracticeSet(context.Background(), "xiaoming", id)
	if err != nil {
		t.Fatal(err)
	}
	solver := &customPaperSolver{duplicate: true}
	d.Solver = solver
	result, err := d.GenerateCustomPaper(context.Background(), "xiaoming", customPaperReq("paper-command-all-deduped"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Added != 0 || result.Deduplicated != solver.generateCalls || result.Deduplicated == 0 || len(result.Items) != 0 {
		t.Fatalf("全部重复应提交零新增收据: %+v", result)
	}
	after, err := d.GetPracticeSet(context.Background(), "xiaoming", id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Record.Version != before.Record.Version || len(after.Fields.Items) != 1 {
		t.Fatalf("去重零新增不得推进待打印集合版本: before=%d after=%d items=%d", before.Record.Version, after.Record.Version, len(after.Fields.Items))
	}
	replay, err := d.GenerateCustomPaper(context.Background(), "xiaoming", customPaperReq("paper-command-all-deduped"))
	if err != nil || replay.Deduplicated != result.Deduplicated || solver.generateCalls != result.Deduplicated {
		t.Fatalf("幂等重放必须返回同一去重收据且不再调用模型: first=%+v replay=%+v calls=%d err=%v", result, replay, solver.generateCalls, err)
	}
}

func TestGenerateCustomPaper_ConcurrentSameCommandCommitsOneBasket(t *testing.T) {
	d := newDataDeps(t)
	seedCustomPaperMistakes(t, d, 3)
	solver := &customPaperSolver{}
	d.Solver = solver
	req := customPaperReq("paper-command-concurrent")

	start := make(chan struct{})
	results := make([]usecase.CustomPaperResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = d.GenerateCustomPaper(context.Background(), "xiaoming", req)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发组卷 #%d: %v", i, err)
		}
	}
	if results[0].GenerationJobID != results[1].GenerationJobID ||
		results[0].Set.Record.RecordID != results[1].Set.Record.RecordID {
		t.Fatalf("同一并发命令必须收敛到一张收据/一个集合: %+v / %+v", results[0], results[1])
	}
	sets, err := d.ListPracticeSets(context.Background(), "xiaoming", k12.PracticeStatusDraft)
	if err != nil || len(sets) != 1 || len(sets[0].Fields.Items) != 5 {
		t.Fatalf("并发重放只可原子提交一份 5 题结果: sets=%+v err=%v", sets, err)
	}
}
