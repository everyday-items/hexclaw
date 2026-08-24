package apihttp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func imageTaskGroundingReceiptFixture() usecase.GroundingEvidenceReceipt {
	return usecase.GroundingEvidenceReceipt{
		TextbookBindingID:  "binding-projection",
		TextbookManifestID: "manifest-projection",
		DocumentID:         "document-projection",
		DocumentGeneration: 3,
		VectorRevisionID:   "revision-projection",
		QueryDigest:        "sha256:" + strings.Repeat("a", 64),
		ChunkID:            "chunk-projection",
		LogicalPage:        8,
		PDFPage:            11,
		SourceDigest:       strings.Repeat("b", 64),
		CitationDigest:     strings.Repeat("c", 64),
	}
}

// K12-GRADING-GROUNDING-CITATION-REAL-001：同一个 ImageTask 的公开
// target_projection 必须投影 page-summary 中恢复的脱敏回执，不能另行检索补造。
func TestBUG20260824PublicImageTaskProjectsGroundingReceipts(t *testing.T) {
	want := []usecase.GroundingEvidenceReceipt{imageTaskGroundingReceiptFixture()}
	view := usecase.ImageTaskView{
		Dispatch: k12.ImageTaskDispatch{
			DispatchID: "dispatch-grounding-projection",
			TaskIntent: k12.ImageTaskIntentCompletedHomework,
			Status:     k12.ImageTaskStatusRouted,
		},
		Homework: &k12.HomeworkSubmission{},
		HomeworkProjection: &usecase.ImageTaskHomeworkProjection{
			Stage:                     k12.GradingStageCompleted,
			GroundingEvidenceReceipts: want,
		},
	}

	got := publicImageTask(view)
	projection, ok := got.TargetProjection.(imageTaskHomeworkProjectionDTO)
	if !ok {
		t.Fatalf("target projection type=%T", got.TargetProjection)
	}
	if !reflect.DeepEqual(projection.GroundingEvidenceReceipts, want) {
		t.Fatalf("target grounding receipts=%+v want %+v",
			projection.GroundingEvidenceReceipts, want)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"grounding_evidence_receipts":[{`) {
		t.Fatalf("public target projection omitted receipts: %s", raw)
	}
}

func TestBUG20260824PublicImageTaskNoHitProjectsEmptyGroundingReceipts(t *testing.T) {
	got := publicImageTask(usecase.ImageTaskView{
		Dispatch: k12.ImageTaskDispatch{
			DispatchID: "dispatch-grounding-no-hit",
			TaskIntent: k12.ImageTaskIntentCompletedHomework,
			Status:     k12.ImageTaskStatusRouted,
		},
		Homework:           &k12.HomeworkSubmission{},
		HomeworkProjection: &usecase.ImageTaskHomeworkProjection{Stage: k12.GradingStageCompleted},
	})
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"grounding_evidence_receipts":[]`) {
		t.Fatalf("no-hit target projection must carry []: %s", raw)
	}
	if !strings.Contains(string(raw), `"problem_grounding_receipts":[]`) {
		t.Fatalf("no-hit problem grounding projection must carry []: %s", raw)
	}
}

// 公开 target projection 必须保留逐题 operation 与脱敏教材 lineage，
// 不能只投影整页汇总回执。
func TestBUG20260825PublicImageTaskProjectsProblemGroundingReceipts(t *testing.T) {
	var homeworkProjection usecase.ImageTaskHomeworkProjection
	if err := json.Unmarshal([]byte(`{
		"problem_grounding_receipts":[{
			"problem_id":"problem-public","operation":"solve",
			"identity_digest":"sha256:problem-public",
			"textbook_binding_id":"binding-public","textbook_manifest_id":"manifest-public",
			"document_id":"document-public","document_generation":2,
			"vector_revision_id":"revision-public","query_digest":"sha256:query-public",
			"chunk_id":"chunk-public","logical_page":7,"pdf_page":9,
			"source_digest":"source-public","citation_digest":"citation-public"
		}]
	}`), &homeworkProjection); err != nil {
		t.Fatal(err)
	}
	homeworkProjection.Stage = k12.GradingStageCompleted
	got := publicImageTask(usecase.ImageTaskView{
		Dispatch: k12.ImageTaskDispatch{
			DispatchID: "dispatch-problem-grounding",
			TaskIntent: k12.ImageTaskIntentCompletedHomework,
			Status:     k12.ImageTaskStatusRouted,
		},
		Homework:           &k12.HomeworkSubmission{},
		HomeworkProjection: &homeworkProjection,
	})
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var public struct {
		TargetProjection struct {
			ProblemGroundingReceipts []struct {
				ProblemID      string `json:"problem_id"`
				Operation      string `json:"operation"`
				IdentityDigest string `json:"identity_digest"`
				ChunkID        string `json:"chunk_id"`
			} `json:"problem_grounding_receipts"`
		} `json:"target_projection"`
	}
	if err := json.Unmarshal(raw, &public); err != nil {
		t.Fatal(err)
	}
	receipts := public.TargetProjection.ProblemGroundingReceipts
	if len(receipts) != 1 || receipts[0].ProblemID != "problem-public" ||
		receipts[0].Operation != "solve" ||
		receipts[0].IdentityDigest != "sha256:problem-public" ||
		receipts[0].ChunkID != "chunk-public" {
		t.Fatalf("public target projection omitted problem grounding receipt: %s", raw)
	}
	for _, forbidden := range []string{"job_id", "invocation_id", "result_json", "prompt", "教材正文"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("public target projection leaked %q: %s", forbidden, raw)
		}
	}
}

// 公开 ImageTask 最终产物只保留客户端需要的状态与 Markdown，
// 不得暴露内部 Job 或 summary invocation 身份。
func TestBUG20260825PublicImageTaskRedactsFinalArtifactInternalIDs(t *testing.T) {
	artifact := &k12.GradingFinalArtifact{
		ArtifactID:          "artifact-public-redaction",
		AgentName:           "mingming",
		JobID:               "internal-grading-job",
		CanonicalMarkdown:   "# 已整理的批改讲解",
		SummaryInvocationID: "internal-summary-invocation",
	}
	got := publicImageTask(usecase.ImageTaskView{
		Dispatch: k12.ImageTaskDispatch{
			DispatchID: "dispatch-final-artifact-redaction",
			TaskIntent: k12.ImageTaskIntentCompletedHomework,
			Status:     k12.ImageTaskStatusRouted,
		},
		Homework: &k12.HomeworkSubmission{},
		HomeworkProjection: &usecase.ImageTaskHomeworkProjection{
			Stage: k12.GradingStageCompleted, FinalArtifact: artifact,
		},
	})
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var public struct {
		TargetProjection struct {
			FinalArtifact *k12.GradingFinalArtifact `json:"final_artifact"`
		} `json:"target_projection"`
	}
	if err := json.Unmarshal(raw, &public); err != nil {
		t.Fatal(err)
	}
	if public.TargetProjection.FinalArtifact == nil {
		t.Fatalf("public final artifact is missing: %s", raw)
	}
	if public.TargetProjection.FinalArtifact.JobID != "" ||
		public.TargetProjection.FinalArtifact.SummaryInvocationID != "" {
		t.Fatalf("public final artifact leaked internal IDs: %s", raw)
	}
	if public.TargetProjection.FinalArtifact.ArtifactID != artifact.ArtifactID ||
		public.TargetProjection.FinalArtifact.CanonicalMarkdown != artifact.CanonicalMarkdown {
		t.Fatalf("public final artifact lost client fields: %s", raw)
	}
	if artifact.JobID != "internal-grading-job" ||
		artifact.SummaryInvocationID != "internal-summary-invocation" {
		t.Fatalf("public projection mutated durable artifact: %+v", artifact)
	}
}
