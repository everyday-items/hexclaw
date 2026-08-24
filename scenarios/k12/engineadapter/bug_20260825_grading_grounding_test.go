package engineadapter

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
)

func gradingGroundingProviderContext(t *testing.T) context.Context {
	t.Helper()
	snapshot := usecase.GroundingSnapshot{
		AgentName: "mingming", LearnerID: "mingming", Subject: "数学",
		TextbookBindingID: "binding-grounding", TextbookManifestID: "manifest-grounding",
		DocumentID: "document-grounding", DocumentGeneration: 1,
		SourceDigest: strings.Repeat("a", 64), Edition: "人教版", Volume: "下册",
		SegmentRefs: []string{"segment-grounding"},
		PageRefs: []k12.TextbookGroundingPageRef{{
			LogicalPage: 1, PDFPage: 1, SegmentRefs: []string{"segment-grounding"},
		}},
		VectorRevisionID: "revision-grounding",
	}
	result := usecase.GroundingSnapshotResult{
		Text: "两位数加法应先把相同数位对齐。", Found: true,
		Receipts: []usecase.GroundingEvidenceReceipt{{
			TextbookBindingID: "binding-grounding", TextbookManifestID: "manifest-grounding",
			DocumentID: "document-grounding", DocumentGeneration: 1,
			VectorRevisionID: "revision-grounding", QueryDigest: "sha256:" + strings.Repeat("b", 64),
			ChunkID: "segment-grounding", LogicalPage: 1, PDFPage: 1,
			SourceDigest: strings.Repeat("a", 64), CitationDigest: strings.Repeat("c", 64),
		}},
	}
	ctx, err := usecase.WithVerifiedGradingGrounding(context.Background(), snapshot, result)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

type gradingGroundingVerifiedExec struct {
	problem string
}

func (e *gradingGroundingVerifiedExec) Execute(context.Context, map[string]any) (*skill.Result, error) {
	return &skill.Result{Metadata: map[string]string{"grade_correct": "true"}}, nil
}

func (e *gradingGroundingVerifiedExec) GradeVerified(
	_ context.Context,
	problem, _, _ string,
) (*skill.Result, error) {
	e.problem = problem
	return &skill.Result{Metadata: map[string]string{"grade_correct": "true"}}, nil
}

// K12-GRADING-TYPED-GROUNDING-ITEMS-001：解题与批改的每条模型输入都消费同一份
// 已核验教材正文，但不会把 binding、revision 等内部证据身份泄露给模型提示词。
func TestSolveAdapterConsumesVerifiedItemGrounding(t *testing.T) {
	ctx := gradingGroundingProviderContext(t)
	exec := &fakeExec{
		solveResult: &skill.Result{
			Content: "95", Metadata: map[string]string{"solve_verdict": "agree"},
		},
		gradeResult: &skill.Result{Metadata: map[string]string{"grade_correct": "true"}},
	}
	adapter := NewSolveAdapter(exec)
	if _, err := adapter.SolveSubject(ctx, "数学", "57+38=", "五年级下", ""); err != nil {
		t.Fatal(err)
	}
	assertGradingGroundingProviderProblem(t, exec.lastArgs["problem"])
	if _, err := adapter.GradeSubject(ctx, "数学", "57+38=", "95", ""); err != nil {
		t.Fatal(err)
	}
	assertGradingGroundingProviderProblem(t, exec.lastArgs["problem"])

	verified := &gradingGroundingVerifiedExec{}
	if _, err := NewSolveAdapter(verified).GradeVerified(
		ctx, "数学", "57+38=", "95", "正确解法",
	); err != nil {
		t.Fatal(err)
	}
	assertGradingGroundingProviderProblem(t, verified.problem)
}

func assertGradingGroundingProviderProblem(t *testing.T, value any) {
	t.Helper()
	problem, ok := value.(string)
	if !ok || !strings.Contains(problem, "57+38=") ||
		!strings.Contains(problem, "两位数加法应先把相同数位对齐") ||
		!strings.Contains(problem, "Verified textbook evidence") ||
		!strings.Contains(problem, "Respond in Chinese") {
		t.Fatalf("provider input omitted verified textbook evidence: %#v", value)
	}
	for _, forbidden := range []string{"binding-grounding", "manifest-grounding", "revision-grounding"} {
		if strings.Contains(problem, forbidden) {
			t.Fatalf("provider input leaked grounding identity %q: %q", forbidden, problem)
		}
	}
}
