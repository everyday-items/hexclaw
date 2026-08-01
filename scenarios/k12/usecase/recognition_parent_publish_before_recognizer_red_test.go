package usecase

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

type dd036RecognizerEntryLedgerProbe struct {
	records       *k12storage.Store
	agentName     string
	jobID         string
	entryParent   k12.ModelInvocation
	entryChildren []k12.ModelPhysicalInvocation
	entryErr      error
}

func (r *dd036RecognizerEntryLedgerProbe) Recognize(
	ctx context.Context,
	image []byte,
) ([]RecognizedQuestion, error) {
	parents, err := r.records.ListModelInvocations(
		context.Background(),
		r.agentName,
		r.jobID,
	)
	if err != nil {
		r.entryErr = fmt.Errorf("list parents at recognizer entry: %w", err)
		return nil, r.entryErr
	}
	for _, parent := range parents {
		if parent.Stage == k12.GradingStageRecognizing {
			r.entryParent = parent
			break
		}
	}
	r.entryChildren, err = r.records.ListModelPhysicalInvocations(
		context.Background(),
		r.agentName,
		r.jobID,
	)
	if err != nil {
		r.entryErr = fmt.Errorf("list children at recognizer entry: %w", err)
		return nil, r.entryErr
	}

	physical, err := k12.ExecuteRecognitionPhysicalCall(
		ctx,
		k12.RecognitionPhysicalCall{
			Unit:  k12.RecognitionPhysicalUnitWholePage,
			Image: image,
		},
		func(context.Context) (string, error) {
			return `[{"question":"1+1=","answer_state":"blank"}]`, nil
		},
	)
	if err != nil {
		return nil, err
	}
	if physical.InvocationID == "" || physical.Payload == "" {
		return nil, ErrModelInvocationRequiresReconciliation
	}
	return []RecognizedQuestion{{
		Question:    "1+1=",
		AnswerState: AnswerStateBlank,
	}}, nil
}

// REG-K12-RECOGNIZING-POLICY-003: publishing the recognizing parent as sent
// must atomically publish its deterministic whole_page child as prepared.
// Recognizer code is not allowed to observe the parent authorization without
// the exact durable child that scopes the only permitted physical request.
func TestREGK12RecognizingPolicy003PublishesPreparedWholePageBeforeRecognizerEntry(
	t *testing.T,
) {
	snapshot := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    k12.RecognizingPolicyModel,
		Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
		Capability:               "vision",
		TimeoutMS:                120_000,
		RecognizingRequestPolicy: k12.ApprovedRecognizingRequestPolicy(),
	}
	deps, _ := newPipeline(
		t,
		fakeSolver{
			solution: "2",
			ev: SolveEvidence{
				Verdict:      VerdictAgree,
				EvidenceType: EvidenceNumericExec,
			},
		},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}},
		nil,
	)
	deps.Now = func() int64 { return time.Now().Unix() }
	probe := &dd036RecognizerEntryLedgerProbe{records: deps.Records}
	deps.Recognizer = probe
	orchestrator := trackGradingOrchestrator(t, NewGradingOrchestrator(
		deps,
		func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
			return snapshot, nil
		},
	))
	request := orchestratorPhotoRequest()
	job, created, err := orchestrator.StartPhotoGradingJob(
		context.Background(),
		StartPhotoGradingInput{
			Photo:      request,
			SourceKind: "desktop",
			SourceKey:  "dd036-publish-child-before-recognizer-entry",
		},
	)
	if err != nil || !created {
		t.Fatalf("start grading job created=%v err=%v", created, err)
	}
	probe.agentName = job.Record.AgentName
	probe.jobID = job.Record.RecordID

	if _, err := orchestrator.RunGradingJob(
		context.Background(),
		job.Record.RecordID,
	); err != nil {
		t.Fatalf("run grading job: %v", err)
	}
	if probe.entryErr != nil {
		t.Fatal(probe.entryErr)
	}
	if probe.entryParent.InvocationID == "" ||
		probe.entryParent.Status != k12.ModelInvocationSent {
		t.Fatalf(
			"recognizer entry parent=%+v, want recognizing parent already sent",
			probe.entryParent,
		)
	}
	if len(probe.entryChildren) != 1 {
		t.Fatalf(
			"recognizer entry saw parent status=%s but durable physical children=%d, want exact whole_page prepared child published before Recognizer entry: parent=%+v children=%+v",
			probe.entryParent.Status,
			len(probe.entryChildren),
			probe.entryParent,
			probe.entryChildren,
		)
	}

	child := probe.entryChildren[0]
	expectedCall := k12.RecognitionPhysicalCall{
		Unit:  k12.RecognitionPhysicalUnitWholePage,
		Image: request.Image,
	}
	expectedDigest, err := recognizingPhysicalInvocationDigest(
		probe.entryParent,
		expectedCall,
	)
	if err != nil {
		t.Fatalf("compute expected whole_page child digest: %v", err)
	}
	expectedID := stableRecognitionPhysicalInvocationID(
		probe.entryParent.InvocationID,
		k12.RecognitionPhysicalUnitWholePage,
	)
	if child.PhysicalInvocationID != expectedID ||
		child.ParentInvocationID != probe.entryParent.InvocationID ||
		child.AgentName != probe.entryParent.AgentName ||
		child.JobID != probe.entryParent.JobID ||
		child.Stage != k12.GradingStageRecognizing ||
		child.PhysicalUnit != k12.RecognitionPhysicalUnitWholePage ||
		child.RequestDigest != expectedDigest ||
		child.RouteSnapshot != probe.entryParent.RouteSnapshot ||
		child.RequestPolicySnapshot != probe.entryParent.RequestPolicySnapshot ||
		child.Attempt != 1 ||
		child.Status != k12.ModelInvocationPrepared {
		t.Fatalf(
			"recognizer entry physical child is not the exact prepared whole_page authorization: parent=%+v child=%+v expected_id=%s expected_digest=%s",
			probe.entryParent,
			child,
			expectedID,
			expectedDigest,
		)
	}
}
