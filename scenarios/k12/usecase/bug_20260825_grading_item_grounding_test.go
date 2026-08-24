package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

const gradingItemGroundingSourceDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type gradingItemPinnedGrounding struct {
	mu            sync.Mutex
	active        string
	freezes       int
	freezeInputs  []GroundingSnapshot
	queries       []GroundingSnapshot
	legacyUse     int
	noHit         bool
	citationDrift bool
	multiChunk    bool
}

func (g *gradingItemPinnedGrounding) Ground(
	context.Context, string, string, string,
) (string, bool, error) {
	g.mu.Lock()
	g.legacyUse++
	g.mu.Unlock()
	return "", false, fmt.Errorf("legacy grounding must not be used")
}

func (g *gradingItemPinnedGrounding) FreezeGroundingSnapshot(
	_ context.Context,
	requested GroundingSnapshot,
) (GroundingSnapshot, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.freezes++
	g.freezeInputs = append(g.freezeInputs, requested)
	requested.VectorRevisionID = g.active
	return requested, nil
}

func (g *gradingItemPinnedGrounding) GroundSnapshot(
	ctx context.Context,
	snapshot GroundingSnapshot,
	knowledgePoint, grade string,
) (string, bool, error) {
	result, err := g.GroundSnapshotWithEvidence(ctx, snapshot, knowledgePoint, grade)
	return result.Text, result.Found, err
}

func (g *gradingItemPinnedGrounding) GroundSnapshotWithEvidence(
	_ context.Context,
	snapshot GroundingSnapshot,
	knowledgePoint, _ string,
) (GroundingSnapshotResult, error) {
	g.mu.Lock()
	g.queries = append(g.queries, snapshot)
	noHit := g.noHit
	citationDrift := g.citationDrift
	multiChunk := g.multiChunk
	g.mu.Unlock()
	if noHit {
		return GroundingSnapshotResult{}, nil
	}
	querySum := sha256.Sum256([]byte(knowledgePoint))
	citationSum := sha256.Sum256([]byte("教材中的两位数加法依据"))
	citationDigest := hex.EncodeToString(citationSum[:])
	if citationDrift {
		citationDigest = "invalid-citation"
	}
	receipts := []GroundingEvidenceReceipt{{
		TextbookBindingID:  snapshot.TextbookBindingID,
		TextbookManifestID: snapshot.TextbookManifestID,
		DocumentID:         snapshot.DocumentID,
		DocumentGeneration: snapshot.DocumentGeneration,
		VectorRevisionID:   snapshot.VectorRevisionID,
		QueryDigest:        "sha256:" + hex.EncodeToString(querySum[:]),
		ChunkID:            "segment-1",
		LogicalPage:        1,
		PDFPage:            1,
		SourceDigest:       gradingItemGroundingSourceDigest,
		CitationDigest:     citationDigest,
	}}
	if multiChunk {
		extraCitation := sha256.Sum256([]byte("教材中的补充依据"))
		receipts = append(receipts, GroundingEvidenceReceipt{
			TextbookBindingID:  snapshot.TextbookBindingID,
			TextbookManifestID: snapshot.TextbookManifestID,
			DocumentID:         snapshot.DocumentID,
			DocumentGeneration: snapshot.DocumentGeneration,
			VectorRevisionID:   snapshot.VectorRevisionID,
			QueryDigest:        "sha256:" + hex.EncodeToString(querySum[:]),
			ChunkID:            "segment-2",
			LogicalPage:        1,
			PDFPage:            1,
			SourceDigest:       gradingItemGroundingSourceDigest,
			CitationDigest:     hex.EncodeToString(extraCitation[:]),
		})
	}
	return GroundingSnapshotResult{
		Text:     "教材中的两位数加法依据",
		Found:    true,
		Receipts: receipts,
	}, nil
}

func (g *gradingItemPinnedGrounding) switchActive(revision string) {
	g.mu.Lock()
	g.active = revision
	g.mu.Unlock()
}

func (g *gradingItemPinnedGrounding) snapshot() (int, int, []GroundingSnapshot) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.freezes, g.legacyUse, append([]GroundingSnapshot(nil), g.queries...)
}

func (g *gradingItemPinnedGrounding) ownerConsumption(
	bindingID string,
) (freezes int, queries int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, snapshot := range g.freezeInputs {
		if snapshot.TextbookBindingID == bindingID {
			freezes++
		}
	}
	for _, snapshot := range g.queries {
		if snapshot.TextbookBindingID == bindingID {
			queries++
		}
	}
	return freezes, queries
}

type gradingItemGroundedPhysicalSolver struct {
	grounding         *gradingItemPinnedGrounding
	outOfScopeProblem string
	solution          string
	once              sync.Once
	mu                sync.Mutex
	calls             int
}

func (*gradingItemGroundedPhysicalSolver) UsesGradingPhysicalCalls() bool { return true }

func (s *gradingItemGroundedPhysicalSolver) Solve(
	ctx context.Context,
	problem, _, _ string,
) (SolveResult, error) {
	want := SolveResult{
		Solution: "正确解法",
		Evidence: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec},
	}
	if strings.TrimSpace(s.solution) != "" {
		want.Solution = strings.TrimSpace(s.solution)
	}
	if problem == s.outOfScopeProblem {
		want = SolveResult{
			OutOfScopeKP: "超出当前范围",
			Evidence:     SolveEvidence{Verdict: VerdictOutOfScope, EvidenceType: EvidenceNone},
		}
	}
	raw, err := json.Marshal(want)
	if err != nil {
		return SolveResult{}, err
	}
	sum := sha256.Sum256([]byte("solve\x00" + problem))
	result, err := ExecuteGradingPhysicalCall(ctx, GradingPhysicalCallSpec{
		Operation:     k12.GradingItemOperationSolveVerify,
		RequestDigest: hex.EncodeToString(sum[:]),
	}, func(context.Context) (string, error) {
		s.mu.Lock()
		s.calls++
		s.mu.Unlock()
		// 第一题真实调用已经开始后切换 mutable active pointer；后续阶段只能继续旧计划。
		s.once.Do(func() { s.grounding.switchActive("revision-b") })
		return string(raw), nil
	})
	if err != nil {
		return SolveResult{}, err
	}
	var got SolveResult
	if err := json.Unmarshal([]byte(result.Payload), &got); err != nil {
		return SolveResult{}, err
	}
	return got, nil
}

func (s *gradingItemGroundedPhysicalSolver) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type gradingItemGroundedPhysicalGrader struct {
	mu    sync.Mutex
	calls int
}

func (*gradingItemGroundedPhysicalGrader) UsesGradingPhysicalCalls() bool { return true }

func (g *gradingItemGroundedPhysicalGrader) Grade(
	ctx context.Context,
	problem, studentAnswer, solution string,
) (GradeOutcome, error) {
	want := GradeOutcome{Verdict: VerdictAgree}
	raw, err := json.Marshal(want)
	if err != nil {
		return GradeOutcome{}, err
	}
	sum := sha256.Sum256([]byte(strings.Join(
		[]string{"grade", problem, studentAnswer, solution}, "\x00",
	)))
	result, err := ExecuteGradingPhysicalCall(ctx, GradingPhysicalCallSpec{
		Operation:     k12.GradingItemOperationGrade,
		RequestDigest: hex.EncodeToString(sum[:]),
	}, func(context.Context) (string, error) {
		g.mu.Lock()
		g.calls++
		g.mu.Unlock()
		return string(raw), nil
	})
	if err != nil {
		return GradeOutcome{}, err
	}
	var got GradeOutcome
	if err := json.Unmarshal([]byte(result.Payload), &got); err != nil {
		return GradeOutcome{}, err
	}
	return got, nil
}

func (g *gradingItemGroundedPhysicalGrader) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

type gradingItemGroundingEnvelope struct {
	Schema    string `json:"schema"`
	Grounding struct {
		Snapshot       GroundingSnapshot          `json:"snapshot"`
		Receipts       []GroundingEvidenceReceipt `json:"receipts"`
		IdentityDigest string                     `json:"identity_digest"`
	} `json:"grounding"`
}

func seedGradingItemActiveTextbookBinding(t *testing.T, o *GradingOrchestrator) {
	seedGradingItemActiveTextbookBindingForOwner(
		t, o, "desktop-user", "binding-math",
	)
}

func seedGradingItemActiveTextbookBindingForOwner(
	t *testing.T,
	o *GradingOrchestrator,
	ownerID string,
	bindingID string,
) {
	t.Helper()
	db := o.deps.Records.DB()
	catalogDigest := strings.Repeat("b", 64)
	catalog := `{"subject":"math","textbook_edition":"人教版","textbook_version":"2022","title":"义务教育教科书·数学五年级上册","volume":"上册","page_min":1,"page_max":1,"units":[{"unit_id":"u1","title":"第一单元","page_from":1,"page_to":1,"lessons":[]}],"page_refs":[{"logical_page":1,"pdf_page":1,"segment_refs":["segment-1"]}]}`
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO kb_semantic_corpora
			(corpus_uid,owner_id,corpus_alias,kind,content_version,created_at,updated_at)
			VALUES('corpus-desktop',?,'default','general',1,1,1)`, []any{ownerID}},
		{`INSERT INTO kb_documents
			(id,title,content,source,deleted,corpus_uid,created_at,updated_at)
			VALUES('doc-math','义务教育教科书·数学五年级上册.pdf','教材正文',
			'upload:math.pdf',0,'corpus-desktop',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, nil},
		{`INSERT INTO kb_chunks
			(id,doc_id,content,chunk_index,created_at,page_start,page_end,
			 source_digest,source_offset_start,source_offset_end)
			VALUES('segment-1','doc-math','教材正文',0,CURRENT_TIMESTAMP,1,1,?,0,?)`,
			[]any{gradingItemGroundingSourceDigest, len("教材正文")}},
		{`INSERT INTO kb_semantic_document_generations
			(owner_id,corpus_uid,document_id,content_generation,created_at)
			VALUES(?,'corpus-desktop','doc-math',1,1)`, []any{ownerID}},
		{`INSERT INTO kb_semantic_document_bindings
			(document_id,owner_id,corpus_uid,content_generation,lifecycle_state,
			 text_state,version,created_at,updated_at)
			VALUES('doc-math',?,'corpus-desktop',1,'active','ready',1,1,1)`, []any{ownerID}},
		{`INSERT INTO k12_textbook_manifests
			(manifest_id,owner_id,document_id,document_generation,document_title,
			 subject,source_digest,state,retryable,failure_message,text_index_state,
			 vector_index_state,catalog_json,catalog_digest,created_at,updated_at)
			VALUES('manifest-math',?,'doc-math',1,
			'义务教育教科书·数学五年级上册.pdf','math',?,
			'ready_for_confirmation',0,'','ready','ready',?,?,1,1)`,
			[]any{ownerID, gradingItemGroundingSourceDigest, catalog, catalogDigest}},
		{`INSERT INTO k12_textbook_page_mappings
			(mapping_id,manifest_id,logical_page,pdf_page,evidence_page,
			 evidence_offset_start,evidence_offset_end,evidence_digest,method,
			 verification_state,document_id,document_generation,source_digest,
			 created_at,updated_at)
			VALUES('manifest-page-proof-1','manifest-math',1,1,1,0,1,?,
			'printed_anchor','verified','doc-math',1,?,1,1)`,
			[]any{strings.Repeat("c", 64), gradingItemGroundingSourceDigest}},
		{`INSERT INTO k12_textbook_manifest_segments
			(segment_id,manifest_id,logical_page,segment_ref,pdf_page,document_id,
			 document_generation,source_digest,created_at,updated_at)
			VALUES('manifest-segment-1','manifest-math',1,'segment-1',1,
			'doc-math',1,?,1,1)`, []any{gradingItemGroundingSourceDigest}},
		{`INSERT INTO k12_textbook_bindings
			(textbook_binding_id,owner_id,agent_name,subject,textbook_manifest_id,
			 document_id,document_generation,status,created_at,updated_at)
			VALUES(?,?,'mingming','math','manifest-math',
			'doc-math',1,'active',1,1)`, []any{bindingID, ownerID}},
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed active textbook binding: %v", err)
		}
	}
}

// 第二个 chunk 与首个 chunk 共用同一已核验页，用于锁定单次
// pinned query 返回多个合法命中时的公开 exact-set。
func seedGradingItemSecondTextbookSegment(t *testing.T, o *GradingOrchestrator) {
	t.Helper()
	catalog := `{"subject":"math","textbook_edition":"人教版","textbook_version":"2022","title":"义务教育教科书·数学五年级上册","volume":"上册","page_min":1,"page_max":1,"units":[{"unit_id":"u1","title":"第一单元","page_from":1,"page_to":1,"lessons":[]}],"page_refs":[{"logical_page":1,"pdf_page":1,"segment_refs":["segment-1","segment-2"]}]}`
	statements := []struct {
		query string
		args  []any
	}{
		{`UPDATE k12_textbook_manifests SET catalog_json=? WHERE manifest_id='manifest-math'`, []any{catalog}},
		{`INSERT INTO kb_chunks
			(id,doc_id,content,chunk_index,created_at,page_start,page_end,
			 source_digest,source_offset_start,source_offset_end)
			VALUES('segment-2','doc-math','教材正文',1,CURRENT_TIMESTAMP,1,1,?,0,?)`,
			[]any{gradingItemGroundingSourceDigest, len("教材正文")}},
		{`INSERT INTO k12_textbook_manifest_segments
			(segment_id,manifest_id,logical_page,segment_ref,pdf_page,document_id,
			 document_generation,source_digest,created_at,updated_at)
			VALUES('manifest-segment-2','manifest-math',1,'segment-2',1,
			'doc-math',1,?,1,1)`, []any{gradingItemGroundingSourceDigest}},
	}
	for _, statement := range statements {
		if _, err := o.deps.Records.DB().Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed second textbook segment: %v", err)
		}
	}
}

// K12-GRADING-TYPED-GROUNDING-ITEMS-001：教材计划必须在第一道 Provider 调用前冻结，
// 并由两题 solve/grade 与最终摘要共同消费，active pointer 漂移不能生成第二份计划。
func TestK12GradingTypedGroundingItemsFreezesOnceAndPersistsOneEvidenceIdentity(t *testing.T) {
	grounding := &gradingItemPinnedGrounding{active: "revision-a"}
	solver := &gradingItemGroundedPhysicalSolver{grounding: grounding}
	grader := &gradingItemGroundedPhysicalGrader{}
	o := newParallelAnchorOrchestrator(t, &countingRecognizer{questions: []RecognizedQuestion{
		{
			Question: "57+38=", Subject: "数学", StudentAnswer: "95",
			AnswerState: AnswerStatePresent, KnowledgePoints: []string{"两位数加法"},
		},
		{
			Question: "26×3=", Subject: "数学", StudentAnswer: "78",
			AnswerState: AnswerStatePresent, KnowledgePoints: []string{"两位数乘一位数"},
		},
	}}, nil, WithGradingRunDir(t.TempDir()))
	o.deps.Solver = solver
	o.deps.Grader = grader
	o.deps.VerifiedGrader = nil
	o.deps.Grounding = grounding
	o.deps.TextbookOwnerID = "desktop-user"
	profile := o.deps.Profiles.(*memProfiles).m["mingming"]
	profile.TextbookEdition = "人教版"
	o.deps.Profiles.(*memProfiles).m["mingming"] = profile
	seedGradingItemActiveTextbookBinding(t, o)

	jobID := runItemResumeJobToAssessing(t, o, "typed-grounding-items")
	completed, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err != nil {
		t.Fatalf("complete grounded grading: %v", err)
	}
	if completed.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("grounded grading stage=%s want completed", completed.Record.Status)
	}
	if solver.callCount() != 2 || grader.callCount() != 2 {
		t.Fatalf("provider solve/grade calls=%d/%d want 2/2", solver.callCount(), grader.callCount())
	}
	freezes, legacyUse, queries := grounding.snapshot()
	if freezes != 1 {
		t.Fatalf("grounding freezes=%d want 1", freezes)
	}
	if legacyUse != 0 {
		t.Fatalf("legacy grounding calls=%d want 0", legacyUse)
	}
	if len(queries) < 2 {
		t.Fatalf("pinned grounding queries=%d want at least two item queries", len(queries))
	}
	for index, snapshot := range queries {
		if snapshot.VectorRevisionID != "revision-a" ||
			snapshot.TextbookBindingID != "binding-math" ||
			snapshot.DocumentID != "doc-math" || snapshot.DocumentGeneration != 1 {
			t.Fatalf("query %d drifted from frozen evidence: %+v", index, snapshot)
		}
	}

	invocations, err := o.deps.Records.ListGradingItemInvocations(
		context.Background(), "mingming", jobID,
	)
	if err != nil {
		t.Fatal(err)
	}
	groundedOperations := 0
	problemEvidenceIdentities := make(map[string]string)
	for _, invocation := range invocations {
		if invocation.Operation != k12.GradingItemOperationSolveVerify &&
			invocation.Operation != k12.GradingItemOperationGrade {
			continue
		}
		groundedOperations++
		var envelope gradingItemGroundingEnvelope
		if err := json.Unmarshal([]byte(invocation.ResultJSON), &envelope); err != nil {
			t.Fatalf("decode grounded invocation %s: %v", invocation.InvocationID, err)
		}
		if envelope.Schema != "k12_grading_grounded_physical_v1" ||
			envelope.Grounding.Snapshot.VectorRevisionID != "revision-a" ||
			len(envelope.Grounding.Receipts) == 0 ||
			envelope.Grounding.Receipts[0].CitationDigest == "" ||
			envelope.Grounding.IdentityDigest == "" {
			t.Fatalf("invocation %s omitted frozen grounding identity: %+v", invocation.InvocationID, envelope)
		}
		if existing := problemEvidenceIdentities[invocation.ProblemID]; existing != "" && existing != envelope.Grounding.IdentityDigest {
			t.Fatalf(
				"problem %s solve/grade grounding identity drifted: %s != %s",
				invocation.ProblemID, existing, envelope.Grounding.IdentityDigest,
			)
		}
		problemEvidenceIdentities[invocation.ProblemID] = envelope.Grounding.IdentityDigest
	}
	if groundedOperations != 4 {
		t.Fatalf("grounded solve/grade invocations=%d want 4", groundedOperations)
	}

	artifact, err := o.deps.Records.GetGradingFinalArtifactByJob(
		context.Background(), "mingming", jobID,
	)
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := o.deps.Records.GetModelInvocation(
		context.Background(), "mingming", artifact.SummaryInvocationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	var tips TutoringTips
	if err := json.Unmarshal([]byte(invocation.ResultJSON), &tips); err != nil {
		t.Fatal(err)
	}
	if len(tips.GroundingEvidenceReceipts) == 0 {
		t.Fatal("final summary omitted grounding receipts")
	}
	for _, receipt := range tips.GroundingEvidenceReceipts {
		if receipt.VectorRevisionID != "revision-a" || receipt.DocumentID != "doc-math" ||
			receipt.SourceDigest != gradingItemGroundingSourceDigest {
			t.Fatalf("final summary drifted from item grounding: %+v", receipt)
		}
	}
}

// K12-GRADING-GROUNDING-OWNER-SCOPE-001：ImageTask 的不可变 owner
// 是逐题教材 grounding 的唯一权限边界，不得被进程级 composition owner 替换。
func TestK12GradingImageTaskGroundingUsesFrozenOwnerForItemsAndFinalTips(t *testing.T) {
	const (
		compositionOwner = "owner-a"
		imageTaskOwner   = "owner-b"
		ownerABinding    = "binding-owner-a"
		ownerBBinding    = "binding-owner-b"
	)
	ctx := context.Background()
	grounding := &gradingItemPinnedGrounding{active: "revision-owner-b"}
	solver := &gradingItemGroundedPhysicalSolver{grounding: grounding}
	grader := &gradingItemGroundedPhysicalGrader{}
	o := newParallelAnchorOrchestrator(t, &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "57+38=", Subject: "数学", StudentAnswer: "95",
		AnswerState: AnswerStatePresent, KnowledgePoints: []string{"两位数加法"},
	}}}, nil, WithGradingRunDir(t.TempDir()))
	o.deps.Solver = solver
	o.deps.Grader = grader
	o.deps.VerifiedGrader = nil
	o.deps.Grounding = grounding
	o.deps.TextbookOwnerID = compositionOwner
	o.deps.GradingBudgetSnapshot = orchestratorTestBudget()
	profile := o.deps.Profiles.(*memProfiles).m["mingming"]
	profile.TextbookEdition = "人教版"
	o.deps.Profiles.(*memProfiles).m["mingming"] = profile
	seedGradingItemActiveTextbookBindingForOwner(
		t, o, imageTaskOwner, ownerBBinding,
	)

	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"visible answers"},
		Confidence:     1,
	}}
	coordinator := &ImageTaskCoordinator{
		Records: o.deps.Records, Classifier: classifier,
		ResolveRoute: imageTaskRouteForTest,
		ReadAsset: func(agentName, assetRef string) ([]byte, error) {
			if agentName != "mingming" || assetRef != imageTaskAssetForTest {
				t.Fatalf("unexpected ImageTask source %q %q", agentName, assetRef)
			}
			return []byte("owner-b-homework-image"), nil
		},
		Now: o.deps.now,
		NewID: func(kind string) string {
			switch kind {
			case "dispatch":
				return "dispatch-owner-b"
			case "classification":
				return "classification-owner-b"
			default:
				return "unused-owner-b"
			}
		},
	}
	input := testCreateImageTaskInput()
	input.OwnerScope = imageTaskOwner
	input.SourceRef = "message-owner-b"
	prepared, created, err := coordinator.Create(ctx, input)
	if err != nil || !created {
		t.Fatalf("create owner-b ImageTask: created=%v err=%v", created, err)
	}
	frozenOwner, err := o.deps.Records.GetImageTaskOwnerScope(
		ctx, prepared.Dispatch.AgentName, prepared.Dispatch.DispatchID,
	)
	if err != nil || frozenOwner != imageTaskOwner {
		t.Fatalf("ImageTask frozen owner=%q err=%v", frozenOwner, err)
	}

	view, created, err := o.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo:                     orchestratorPhotoRequest(),
		SourceKind:                "image_task",
		SourceKey:                 prepared.Dispatch.DispatchID,
		BudgetSnapshot:            orchestratorTestBudget(),
		ParentAutomaticAttemptID:  prepared.Dispatch.DispatchID + ":1",
		ParentAutomaticDeadlineAt: o.deps.now() + 300,
	})
	if err != nil || !created {
		t.Fatalf("start owner-b grading job: created=%v err=%v", created, err)
	}
	jobID := view.Record.RecordID
	view, err = o.RunGradingJob(ctx, jobID)
	if err != nil || view.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf("run owner-b grading to confirmation: stage=%s err=%v", view.Record.Status, err)
	}
	waitGradingView(t, o, jobID, func(candidate GradingJobView) bool {
		return candidate.Fields.AnchorState == k12.GradingAnchorLocated ||
			candidate.Fields.AnchorState == k12.GradingAnchorDegraded
	})
	confirmed, err := o.ConfirmAndRun(ctx, jobID, nil)
	if err != nil || confirmed.Record.Status != k12.GradingStageAssessing {
		t.Fatalf("confirm owner-b grading: stage=%s err=%v", confirmed.Record.Status, err)
	}
	completed, err := o.RunGradingJob(ctx, jobID)
	if err != nil || completed.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("finalize owner-b grading: stage=%s err=%v", completed.Record.Status, err)
	}
	if completed.Fields.SourceKind != "image_task" ||
		gradingSourceKeyFromIdempotencyKey(completed.Fields) != prepared.Dispatch.DispatchID {
		t.Fatalf("GradingJob did not freeze the ImageTask dispatch: %+v", completed.Fields)
	}

	freezes, legacyUse, queries := grounding.snapshot()
	bFreezes, bQueries := grounding.ownerConsumption(ownerBBinding)
	aFreezes, aQueries := grounding.ownerConsumption(ownerABinding)
	if freezes != 1 || bFreezes != freezes || len(queries) < 2 || bQueries != len(queries) {
		t.Fatalf(
			"owner-b grounding consumption freezes=%d/%d queries=%d/%d",
			bFreezes, freezes, bQueries, len(queries),
		)
	}
	if aFreezes != 0 || aQueries != 0 || legacyUse != 0 {
		t.Fatalf(
			"owner-a/legacy grounding was consumed: freezes=%d queries=%d legacy=%d",
			aFreezes, aQueries, legacyUse,
		)
	}

	artifact, err := o.deps.Records.GetGradingFinalArtifactByJob(ctx, "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := o.deps.Records.GetModelInvocation(
		ctx, "mingming", artifact.SummaryInvocationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	var tips TutoringTips
	if err := json.Unmarshal([]byte(invocation.ResultJSON), &tips); err != nil {
		t.Fatal(err)
	}
	if len(tips.GroundingEvidenceReceipts) == 0 {
		t.Fatal("final TutoringTips omitted owner-b grounding receipts")
	}
	for _, receipt := range tips.GroundingEvidenceReceipts {
		if receipt.TextbookBindingID != ownerBBinding {
			t.Fatalf("final TutoringTips consumed a non-owner-b binding: %+v", receipt)
		}
	}
}

func runGradingItemGroundingFailClosed(
	t *testing.T,
	grounding *gradingItemPinnedGrounding,
	mutateScope func(*GradingOrchestrator),
) (*gradingItemGroundedPhysicalSolver, *gradingItemGroundedPhysicalGrader, error) {
	t.Helper()
	solver := &gradingItemGroundedPhysicalSolver{grounding: grounding}
	grader := &gradingItemGroundedPhysicalGrader{}
	o := newParallelAnchorOrchestrator(t, &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "57+38=", Subject: "数学", StudentAnswer: "95",
		AnswerState: AnswerStatePresent, KnowledgePoints: []string{"两位数加法"},
	}}}, nil, WithGradingRunDir(t.TempDir()))
	o.deps.Solver = solver
	o.deps.Grader = grader
	o.deps.VerifiedGrader = nil
	o.deps.Grounding = grounding
	o.deps.TextbookOwnerID = "desktop-user"
	seedGradingItemActiveTextbookBinding(t, o)
	if mutateScope != nil {
		mutateScope(o)
	}
	jobID := runItemResumeJobToAssessing(t, o, "typed-grounding-fail-closed")
	view, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err == nil || view.Record.Status == k12.GradingStageCompleted {
		t.Fatalf("invalid grounding must fail closed: stage=%s err=%v", view.Record.Status, err)
	}
	return solver, grader, err
}

// K12-GRADING-TYPED-GROUNDING-ITEMS-001：有 active binding 时，no-hit、引用漂移、
// scope 证据不完整都必须在任何 solve/grade Provider 请求前失败。
func TestK12GradingTypedGroundingItemsRejectsInvalidEvidenceBeforeProvider(t *testing.T) {
	tests := []struct {
		name        string
		grounding   *gradingItemPinnedGrounding
		mutateScope func(*GradingOrchestrator)
	}{
		{
			name:      "no hit",
			grounding: &gradingItemPinnedGrounding{active: "revision-a", noHit: true},
		},
		{
			name:      "citation drift",
			grounding: &gradingItemPinnedGrounding{active: "revision-a", citationDrift: true},
		},
		{
			name:      "incomplete scope",
			grounding: &gradingItemPinnedGrounding{active: "revision-a"},
			mutateScope: func(o *GradingOrchestrator) {
				if _, err := o.deps.Records.DB().Exec(
					`DELETE FROM k12_textbook_manifest_segments WHERE manifest_id='manifest-math'`,
				); err != nil {
					t.Fatalf("invalidate textbook scope: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			solver, grader, _ := runGradingItemGroundingFailClosed(
				t, test.grounding, test.mutateScope,
			)
			if solver.callCount() != 0 || grader.callCount() != 0 {
				t.Fatalf(
					"invalid grounding reached Provider: solve=%d grade=%d",
					solver.callCount(), grader.callCount(),
				)
			}
		})
	}
}
