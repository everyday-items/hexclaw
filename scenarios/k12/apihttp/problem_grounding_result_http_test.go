package apihttp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type problemGroundingResultGradingStub struct {
	jobID      string
	projection usecase.ImageTaskHomeworkProjection
}

func (s *problemGroundingResultGradingStub) StartPhotoGradingJob(
	context.Context,
	usecase.StartPhotoGradingInput,
) (usecase.GradingJobView, bool, error) {
	return usecase.GradingJobView{
		Record: &records.AgentRecord{RecordID: s.jobID},
	}, true, nil
}

func (*problemGroundingResultGradingStub) ConfirmPhotoGradingJob(
	context.Context,
	string,
	usecase.ConfirmPhotoGradingInput,
) (usecase.GradingJobView, bool, error) {
	return usecase.GradingJobView{}, false, nil
}

func (*problemGroundingResultGradingStub) StartAsync(string) bool { return true }

func (s *problemGroundingResultGradingStub) ImageTaskHomeworkProjection(
	context.Context,
	string,
	string,
) (usecase.ImageTaskHomeworkProjection, error) {
	return s.projection, nil
}

// 公开 `/result` 与 task target projection 必须复读同一份逐题证据，
// 不能只返回整页汇总。
func TestK12ImageTaskHTTPResultProjectsProblemGroundingReceipts(t *testing.T) {
	fixture := newImageTaskHTTPFixture(t)
	fixture.classifier.result = usecase.ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"visible handwritten answers"},
		Confidence:     0.99,
	}
	var projection usecase.ImageTaskHomeworkProjection
	if err := json.Unmarshal([]byte(`{
		"problem_grounding_receipts":[{
			"problem_id":"problem-http","operation":"solve",
			"identity_digest":"sha256:problem-http",
			"textbook_binding_id":"binding-http","textbook_manifest_id":"manifest-http",
			"document_id":"document-http","document_generation":4,
			"vector_revision_id":"revision-http","query_digest":"sha256:query-http",
			"chunk_id":"chunk-http","logical_page":3,"pdf_page":5,
			"source_digest":"source-http","citation_digest":"citation-http"
		}]
	}`), &projection); err != nil {
		t.Fatal(err)
	}
	projection.Stage = k12.GradingStageCompleted
	fixture.coordinator.Grading = &problemGroundingResultGradingStub{
		jobID: "problem-grounding-http-job", projection: projection,
	}
	created, fresh, err := fixture.coordinator.Create(context.Background(), usecase.CreateImageTaskInput{
		AgentName:         "mingming",
		LearnerID:         "learner-problem-grounding-http",
		SourceKind:        k12.ImageTaskSourceDesktop,
		SourceRef:         "message-problem-grounding-http",
		SourceSessionID:   "session-problem-grounding-http",
		SourceAssetRefs:   []string{fixture.assetID},
		MessageIntent:     "请批改",
		AttemptGeneration: 1,
		RouteRequest: k12.ImageTaskRouteSnapshot{
			Provider: "hexclaw-gpt", Model: "gpt-5.6-sol", SelectionSource: "explicit",
		},
	})
	if err != nil || !fresh {
		t.Fatalf("create image task: fresh=%v err=%v", fresh, err)
	}
	view, err := fixture.coordinator.Run(
		context.Background(), "mingming", created.Dispatch.DispatchID,
	)
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := do(t, fixture.handler, http.MethodGet,
		"/image-tasks/"+view.Dispatch.DispatchID+"/result?agent=mingming", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("result status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		ProblemGroundingReceipts []struct {
			ProblemID      string `json:"problem_id"`
			Operation      string `json:"operation"`
			IdentityDigest string `json:"identity_digest"`
			ChunkID        string `json:"chunk_id"`
		} `json:"problem_grounding_receipts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.ProblemGroundingReceipts) != 1 ||
		response.ProblemGroundingReceipts[0].ProblemID != "problem-http" ||
		response.ProblemGroundingReceipts[0].Operation != "solve" ||
		response.ProblemGroundingReceipts[0].IdentityDigest != "sha256:problem-http" ||
		response.ProblemGroundingReceipts[0].ChunkID != "chunk-http" {
		t.Fatalf("public result omitted problem grounding receipt: %s", rec.Body.Bytes())
	}
}
