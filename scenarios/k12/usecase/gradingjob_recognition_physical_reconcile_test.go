package usecase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type dd036ReconcileProviderSpy struct {
	calls int
}

func (r *dd036ReconcileProviderSpy) Recognize(
	context.Context,
	[]byte,
) ([]RecognizedQuestion, error) {
	r.calls++
	return nil, errors.New("unexpected recognition provider call")
}

// REG-DD-036-PHYSICAL-RECONCILE-001: a Job-scoped recognition result receipt
// alone cannot prove provider success. Recovery must keep the parent and Job
// parked unless the DD-036 parent has an exact succeeded physical receipt set.
func TestDD036RecognitionReconcileRejectsIncompletePhysicalReceiptSet(t *testing.T) {
	tests := []struct {
		name       string
		childState k12.ModelInvocationStatus
	}{
		{name: "missing"},
		{name: "prepared", childState: k12.ModelInvocationPrepared},
		{name: "sent", childState: k12.ModelInvocationSent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			runDir := t.TempDir()
			recognizer := &dd036ReconcileProviderSpy{}
			deps := recoveryDeps(t, recognizer, nil, &photoAnnotatorFake{})
			policy := k12.ApprovedRecognizingRequestPolicy()
			snapshot := k12.GradingModelSnapshot{
				Provider:                 "hexclaw-gpt",
				Model:                    k12.RecognizingPolicyModel,
				Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
				Capability:               "vision",
				TimeoutMS:                120000,
				RecognizingRequestPolicy: policy,
			}

			first := newRecoverableOrchestrator(t, deps, runDir)
			job, created, err := first.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
				Photo:         orchestratorPhotoRequest(),
				SourceKind:    "desktop",
				SourceKey:     "dd036-reconcile-" + tt.name,
				ModelSnapshot: snapshot,
			})
			if err != nil || !created {
				t.Fatalf("start Job: created=%v err=%v", created, err)
			}
			jobID := job.Record.RecordID
			run := first.lookup(jobID)
			if run == nil {
				t.Fatal("missing grading runtime")
			}
			job, err = first.advanceOK(ctx, run, jobID, "")
			if err != nil {
				t.Fatalf("advance queued: %v", err)
			}
			job, err = first.advanceOK(ctx, run, jobID, "image:test")
			if err != nil || job.Record.Status != k12.GradingStageRecognizing {
				t.Fatalf("advance normalizing: stage=%s err=%v", job.Record.Status, err)
			}

			parent, err := first.beginModelInvocationWithPolicy(
				ctx,
				job,
				k12.GradingStageRecognizing,
				recognizingInvocationDigest(run.req.Image, job.Fields.ModelSnapshot, policy),
				policy,
			)
			if err != nil || parent.Status != k12.ModelInvocationSent {
				t.Fatalf("begin recognizing invocation: status=%s err=%v", parent.Status, err)
			}
			questions, err := NormalizeRecognizedProblems(
				job.Fields.SubmissionID,
				[]RecognizedQuestion{{
					Question:    "1+1=",
					Subject:     "数学",
					AnswerState: AnswerStateBlank,
				}},
			)
			if err != nil {
				t.Fatalf("normalize recognition result: %v", err)
			}
			run.questions = questions
			writeDD036RecognitionReceipt(
				t,
				first,
				jobID,
				parent.InvocationID,
				run,
				nil,
			)
			// The restarted runtime must recover only the immutable receipt; do
			// not make run.json a second success source.
			run.questions = nil

			if tt.childState != "" {
				call := k12.RecognitionPhysicalCall{
					Unit:  k12.RecognitionPhysicalUnitWholePage,
					Image: run.req.Image,
				}
				requestDigest, err := recognizingPhysicalInvocationDigest(parent, call)
				if err != nil {
					t.Fatalf("physical request digest: %v", err)
				}
				child, _, err := deps.Records.PrepareModelPhysicalInvocation(
					ctx,
					k12.ModelPhysicalInvocation{
						PhysicalInvocationID: stableRecognitionPhysicalInvocationID(
							parent.InvocationID,
							call.Unit,
						),
						ParentInvocationID:    parent.InvocationID,
						AgentName:             parent.AgentName,
						JobID:                 parent.JobID,
						Stage:                 parent.Stage,
						PhysicalUnit:          call.Unit,
						RequestDigest:         requestDigest,
						RouteSnapshot:         parent.RouteSnapshot,
						RequestPolicySnapshot: parent.RequestPolicySnapshot,
						Attempt:               1,
						CreatedAt:             deps.now(),
						UpdatedAt:             deps.now(),
					},
				)
				if err != nil {
					t.Fatalf("prepare physical child: %v", err)
				}
				if tt.childState == k12.ModelInvocationSent {
					child, claimed, err := deps.Records.ClaimModelPhysicalInvocationSent(
						ctx,
						parent.AgentName,
						child.PhysicalInvocationID,
					)
					if err != nil || !claimed || child.Status != k12.ModelInvocationSent {
						t.Fatalf(
							"claim physical child: claimed=%v status=%s err=%v",
							claimed,
							child.Status,
							err,
						)
					}
				}
			}

			parent, err = deps.Records.MarkModelInvocationOutcomeUnknown(
				ctx,
				parent.AgentName,
				parent.InvocationID,
				"provider_outcome_unknown",
			)
			if err != nil {
				t.Fatalf("park parent invocation: %v", err)
			}
			parked, err := first.markGradingOutcomeUnknown(
				ctx,
				run,
				jobID,
				"provider_outcome_unknown",
			)
			if err != nil || parked.Record.Status != k12.GradingStageOutcomeUnknown {
				t.Fatalf("park Job: stage=%s err=%v", parked.Record.Status, err)
			}

			restarted := newRecoverableOrchestrator(t, deps, runDir)
			recoveredRun, err := restarted.ensureRun(ctx, jobID)
			if err != nil {
				t.Fatalf("recover runtime: %v", err)
			}
			reconciled, _, reconcileErr := restarted.reconcileDurableGradingOutcome(
				ctx,
				recoveredRun,
				parked,
			)
			if reconcileErr != nil &&
				!errors.Is(reconcileErr, ErrModelInvocationRequiresReconciliation) {
				t.Fatalf("unexpected reconciliation error: %v", reconcileErr)
			}
			if reconciled {
				t.Errorf("incomplete physical receipt set was reconciled")
			}

			storedJob, err := deps.GetGradingJob(ctx, run.agentName, jobID)
			if err != nil {
				t.Fatalf("reload Job: %v", err)
			}
			if storedJob.Record.Status != k12.GradingStageOutcomeUnknown ||
				storedJob.Fields.FailedStage != k12.GradingStageRecognizing {
				t.Errorf(
					"Job escaped parking: stage=%s failed_stage=%s",
					storedJob.Record.Status,
					storedJob.Fields.FailedStage,
				)
			}
			storedParent, err := deps.Records.GetModelInvocation(
				ctx,
				parent.AgentName,
				parent.InvocationID,
			)
			if err != nil {
				t.Fatalf("reload parent invocation: %v", err)
			}
			if storedParent.Status != k12.ModelInvocationOutcomeUnknown {
				t.Errorf("parent escaped parking: status=%s", storedParent.Status)
			}
			children, err := deps.Records.ListModelPhysicalInvocations(
				ctx,
				parent.AgentName,
				jobID,
			)
			if err != nil {
				t.Fatalf("list physical children: %v", err)
			}
			if tt.childState == "" {
				if len(children) != 0 {
					t.Errorf("missing case created physical children: %+v", children)
				}
			} else if len(children) != 1 || children[0].Status != tt.childState {
				t.Errorf(
					"physical child drift: count=%d children=%+v want_status=%s",
					len(children),
					children,
					tt.childState,
				)
			}
			if recognizer.calls != 0 {
				t.Errorf("reconciliation resent provider request: calls=%d", recognizer.calls)
			}
		})
	}
}

type dd036RecognitionReconcileFixture struct {
	ctx        context.Context
	deps       Deps
	runDir     string
	recognizer *dd036ReconcileProviderSpy
	first      *GradingOrchestrator
	run        *gradingRun
	jobID      string
	parent     k12.ModelInvocation
	parked     GradingJobView
	questions  []RecognizedQuestion
}

func newDD036RecognitionReconcileFixture(
	t *testing.T,
	sourceKey string,
	receiptQuestion string,
) *dd036RecognitionReconcileFixture {
	t.Helper()
	return newDD036RecognitionReconcileFixtureWithPhoto(
		t,
		sourceKey,
		receiptQuestion,
		orchestratorPhotoRequest(),
	)
}

func newDD036RecognitionReconcileFixtureWithPhoto(
	t *testing.T,
	sourceKey string,
	receiptQuestion string,
	photo PhotoGradeRequest,
) *dd036RecognitionReconcileFixture {
	t.Helper()

	ctx := context.Background()
	runDir := t.TempDir()
	recognizer := &dd036ReconcileProviderSpy{}
	deps := recoveryDeps(t, recognizer, nil, &photoAnnotatorFake{})
	policy := k12.ApprovedRecognizingRequestPolicy()
	snapshot := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    k12.RecognizingPolicyModel,
		Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
		Capability:               "vision",
		TimeoutMS:                120000,
		RecognizingRequestPolicy: policy,
	}
	first := newRecoverableOrchestrator(t, deps, runDir)
	job, created, err := first.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo:         photo,
		SourceKind:    "desktop",
		SourceKey:     sourceKey,
		ModelSnapshot: snapshot,
	})
	if err != nil || !created {
		t.Fatalf("start Job: created=%v err=%v", created, err)
	}
	jobID := job.Record.RecordID
	run := first.lookup(jobID)
	if run == nil {
		t.Fatal("missing grading runtime")
	}
	job, err = first.advanceOK(ctx, run, jobID, "")
	if err != nil {
		t.Fatalf("advance queued: %v", err)
	}
	job, err = first.advanceOK(ctx, run, jobID, "image:test")
	if err != nil || job.Record.Status != k12.GradingStageRecognizing {
		t.Fatalf("advance normalizing: stage=%s err=%v", job.Record.Status, err)
	}
	parent, err := first.beginModelInvocationWithPolicy(
		ctx,
		job,
		k12.GradingStageRecognizing,
		recognizingInvocationDigest(run.req.Image, job.Fields.ModelSnapshot, policy),
		policy,
	)
	if err != nil || parent.Status != k12.ModelInvocationSent {
		t.Fatalf("begin recognizing invocation: status=%s err=%v", parent.Status, err)
	}
	questions, err := NormalizeRecognizedProblems(
		job.Fields.SubmissionID,
		[]RecognizedQuestion{{
			Question:    receiptQuestion,
			Subject:     "数学",
			AnswerState: AnswerStateBlank,
		}},
	)
	if err != nil {
		t.Fatalf("normalize recognition result: %v", err)
	}
	run.questions = questions

	return &dd036RecognitionReconcileFixture{
		ctx:        ctx,
		deps:       deps,
		runDir:     runDir,
		recognizer: recognizer,
		first:      first,
		run:        run,
		jobID:      jobID,
		parent:     parent,
		questions:  cloneRecognizedQuestions(questions),
	}
}

func writeDD036RecognitionReceipt(
	t *testing.T,
	orchestrator *GradingOrchestrator,
	jobID string,
	invocationID string,
	run *gradingRun,
	physical []gradingRecognitionPhysicalReceipt,
) {
	t.Helper()

	path := orchestrator.recognitionReceiptPath(jobID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create recognition receipt dir: %v", err)
	}
	receipt := gradingRecognitionReceiptFile{
		JobID:               jobID,
		InvocationID:        invocationID,
		AgentName:           run.agentName,
		CanonicalDigest:     CanonicalRecognizedQuestionsDigest(run.questions),
		Questions:           cloneRecognizedQuestions(run.questions),
		PhysicalInvocations: append([]gradingRecognitionPhysicalReceipt(nil), physical...),
		CreatedAt:           orchestrator.deps.now(),
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal recognition receipt: %v", err)
	}
	if err := atomicWriteFileNoReplace(path, raw); err != nil {
		t.Fatalf("write recognition receipt: %v", err)
	}
}

func (f *dd036RecognitionReconcileFixture) persistValidReceipt(t *testing.T) {
	t.Helper()

	f.run.questions = cloneRecognizedQuestions(f.questions)
	if err := f.first.persistRecognitionReceipt(
		f.jobID,
		f.parent.InvocationID,
		f.run,
	); err != nil {
		t.Fatalf("persist recognition result receipt: %v", err)
	}
	f.run.questions = nil
}

func (f *dd036RecognitionReconcileFixture) persistRawReceipt(
	t *testing.T,
	physical []gradingRecognitionPhysicalReceipt,
) {
	t.Helper()

	f.run.questions = cloneRecognizedQuestions(f.questions)
	writeDD036RecognitionReceipt(
		t,
		f.first,
		f.jobID,
		f.parent.InvocationID,
		f.run,
		physical,
	)
	f.run.questions = nil
}

func (f *dd036RecognitionReconcileFixture) addSucceededPhysicalChild(
	t *testing.T,
	unit k12.RecognitionPhysicalUnit,
	image []byte,
	physicalIDOverride string,
	requestDigestOverride string,
	resultDigestOverride string,
	resultPayload string,
) k12.ModelPhysicalInvocation {
	t.Helper()

	call := k12.RecognitionPhysicalCall{Unit: unit, Image: image}
	requestDigest, err := recognizingPhysicalInvocationDigest(f.parent, call)
	if err != nil {
		t.Fatalf("physical request digest for %s: %v", unit, err)
	}
	if requestDigestOverride != "" {
		requestDigest = requestDigestOverride
	}
	physicalID := stableRecognitionPhysicalInvocationID(
		f.parent.InvocationID,
		unit,
	)
	if physicalIDOverride != "" {
		physicalID = physicalIDOverride
	}
	child, created, err := f.deps.Records.PrepareModelPhysicalInvocation(
		f.ctx,
		k12.ModelPhysicalInvocation{
			PhysicalInvocationID:  physicalID,
			ParentInvocationID:    f.parent.InvocationID,
			AgentName:             f.parent.AgentName,
			JobID:                 f.parent.JobID,
			Stage:                 f.parent.Stage,
			PhysicalUnit:          unit,
			RequestDigest:         requestDigest,
			RouteSnapshot:         f.parent.RouteSnapshot,
			RequestPolicySnapshot: f.parent.RequestPolicySnapshot,
			Attempt:               1,
			CreatedAt:             f.deps.now(),
			UpdatedAt:             f.deps.now(),
		},
	)
	if err != nil || !created {
		t.Fatalf(
			"prepare physical child unit=%s created=%v child=%+v err=%v",
			unit,
			created,
			child,
			err,
		)
	}
	child, claimed, err := f.deps.Records.ClaimModelPhysicalInvocationSent(
		f.ctx,
		f.parent.AgentName,
		child.PhysicalInvocationID,
	)
	if err != nil || !claimed || child.Status != k12.ModelInvocationSent {
		t.Fatalf(
			"claim physical child unit=%s claimed=%v child=%+v err=%v",
			unit,
			claimed,
			child,
			err,
		)
	}
	child, err = f.deps.Records.
		MarkModelPhysicalInvocationSucceededWithContent(
			f.ctx,
			f.parent.AgentName,
			child.PhysicalInvocationID,
			resultPayload,
			"",
		)
	if err != nil || child.Status != k12.ModelInvocationSucceeded {
		t.Fatalf("succeed physical child unit=%s child=%+v err=%v", unit, child, err)
	}
	if resultDigestOverride != "" {
		if _, err := f.deps.Records.DB().ExecContext(
			f.ctx,
			`UPDATE k12_model_physical_invocations
			    SET result_digest=?
			  WHERE physical_invocation_id=?`,
			resultDigestOverride,
			child.PhysicalInvocationID,
		); err != nil {
			t.Fatalf(
				"override physical result digest unit=%s: %v",
				unit,
				err,
			)
		}
		child, err = f.deps.Records.GetModelPhysicalInvocation(
			f.ctx,
			f.parent.AgentName,
			child.PhysicalInvocationID,
		)
		if err != nil {
			t.Fatalf("reload physical result override unit=%s: %v", unit, err)
		}
	}
	if unit == k12.RecognitionPhysicalUnitWholePage &&
		resultPayload == "not-json" {
		if err := f.deps.Records.AuthorizeRecognitionFallback(
			f.ctx,
			f.parent.AgentName,
			f.parent.InvocationID,
			child.PhysicalInvocationID,
			resultPayload,
		); err != nil {
			t.Fatalf("authorize fallback fixture: %v", err)
		}
	}
	return child
}

func (f *dd036RecognitionReconcileFixture) park(t *testing.T) {
	t.Helper()

	parent, err := f.deps.Records.MarkModelInvocationOutcomeUnknown(
		f.ctx,
		f.parent.AgentName,
		f.parent.InvocationID,
		"provider_outcome_unknown",
	)
	if err != nil {
		t.Fatalf("park parent invocation: %v", err)
	}
	f.parent = parent
	f.parked, err = f.first.markGradingOutcomeUnknown(
		f.ctx,
		f.run,
		f.jobID,
		"provider_outcome_unknown",
	)
	if err != nil || f.parked.Record.Status != k12.GradingStageOutcomeUnknown {
		t.Fatalf("park Job: stage=%s err=%v", f.parked.Record.Status, err)
	}
}

func (f *dd036RecognitionReconcileFixture) reconcile(
	t *testing.T,
) (bool, GradingJobView, error) {
	t.Helper()

	restarted := newRecoverableOrchestrator(t, f.deps, f.runDir)
	recoveredRun, err := restarted.ensureRun(f.ctx, f.jobID)
	if err != nil {
		t.Fatalf("recover runtime: %v", err)
	}
	return restarted.reconcileDurableGradingOutcome(
		f.ctx,
		recoveredRun,
		f.parked,
	)
}

func (f *dd036RecognitionReconcileFixture) assertStillParked(
	t *testing.T,
	reconciled bool,
	reconcileErr error,
) {
	t.Helper()

	if reconcileErr != nil &&
		!errors.Is(reconcileErr, ErrModelInvocationRequiresReconciliation) {
		t.Fatalf("unexpected reconciliation error: %v", reconcileErr)
	}
	if reconciled {
		t.Errorf("invalid physical evidence was reconciled")
	}
	storedJob, err := f.deps.GetGradingJob(
		f.ctx,
		f.parent.AgentName,
		f.jobID,
	)
	if err != nil {
		t.Fatalf("reload Job: %v", err)
	}
	if storedJob.Record.Status != k12.GradingStageOutcomeUnknown ||
		storedJob.Fields.FailedStage != k12.GradingStageRecognizing {
		t.Errorf(
			"Job escaped parking: stage=%s failed_stage=%s",
			storedJob.Record.Status,
			storedJob.Fields.FailedStage,
		)
	}
	storedParent, err := f.deps.Records.GetModelInvocation(
		f.ctx,
		f.parent.AgentName,
		f.parent.InvocationID,
	)
	if err != nil {
		t.Fatalf("reload parent invocation: %v", err)
	}
	if storedParent.Status != k12.ModelInvocationOutcomeUnknown {
		t.Errorf("parent escaped parking: status=%s", storedParent.Status)
	}
	if f.recognizer.calls != 0 {
		t.Errorf("reconciliation resent provider request: calls=%d", f.recognizer.calls)
	}
}

func dd036ReconcilePhysicalImage(
	f *dd036RecognitionReconcileFixture,
	unit k12.RecognitionPhysicalUnit,
) []byte {
	if unit == k12.RecognitionPhysicalUnitWholePage {
		return f.run.req.Image
	}
	inputs, ok := k12.DenseWorksheetFallbackPhysicalInputs(f.run.req.Image)
	if !ok {
		return nil
	}
	for _, input := range inputs {
		if input.Unit == unit {
			return input.Image
		}
	}
	return nil
}

func dd036DenseReconcileImage(t *testing.T) []byte {
	t.Helper()

	source := image.NewRGBA(image.Rect(0, 0, 800, 1200))
	for y := 0; y < 1200; y++ {
		for x := 0; x < 800; x++ {
			source.SetRGBA(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatalf("encode dense reconciliation fixture: %v", err)
	}
	return encoded.Bytes()
}

// REG-DD-036-PHYSICAL-RECONCILE-002: the two approved terminal sets are
// positive reconciliation evidence. This specifically exercises DD-036
// outcome_unknown recovery rather than the policy-zero legacy bypass.
func TestDD036RecognitionReconcileAcceptsExactSucceededPhysicalSet(t *testing.T) {
	fullFallback := []k12.RecognitionPhysicalUnit{
		k12.RecognitionPhysicalUnitWholePage,
		k12.RecognitionPhysicalUnitSegment1,
		k12.RecognitionPhysicalUnitSegment2,
		k12.RecognitionPhysicalUnitSegment3,
		k12.RecognitionPhysicalUnitSegment4,
		k12.RecognitionPhysicalUnitSegment5,
		k12.RecognitionPhysicalUnitPrintedInventory,
	}
	tests := []struct {
		name  string
		units []k12.RecognitionPhysicalUnit
	}{
		{
			name:  "whole_page",
			units: []k12.RecognitionPhysicalUnit{k12.RecognitionPhysicalUnitWholePage},
		},
		{name: "full_fallback", units: fullFallback},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			photo := orchestratorPhotoRequest()
			if tt.name == "full_fallback" {
				photo.Image = dd036DenseReconcileImage(t)
			}
			fixture := newDD036RecognitionReconcileFixtureWithPhoto(
				t,
				"dd036-reconcile-success-"+tt.name,
				"1+1=",
				photo,
			)
			for index, unit := range tt.units {
				payload := `[]`
				if unit == k12.RecognitionPhysicalUnitWholePage {
					if len(tt.units) == 1 {
						payload = `[{"question":"1+1=","subject":"数学","answer_state":"blank"}]`
					} else {
						payload = `not-json`
					}
				} else if index == 1 {
					payload = `[{"question":"1+1=","subject":"数学","answer_state":"blank"}]`
				}
				fixture.addSucceededPhysicalChild(
					t,
					unit,
					dd036ReconcilePhysicalImage(fixture, unit),
					"",
					"",
					"",
					payload,
				)
			}
			fixture.persistValidReceipt(t)
			fixture.park(t)

			reconciled, next, reconcileErr := fixture.reconcile(t)
			if reconcileErr != nil || !reconciled {
				t.Fatalf(
					"exact succeeded set did not reconcile: reconciled=%v next=%+v err=%v",
					reconciled,
					next,
					reconcileErr,
				)
			}
			if next.Record == nil || next.Record.Status != k12.GradingStageQueued {
				t.Errorf("reconciled Job stage=%v, want queued", next.Record)
			}
			storedParent, err := fixture.deps.Records.GetModelInvocation(
				fixture.ctx,
				fixture.parent.AgentName,
				fixture.parent.InvocationID,
			)
			if err != nil {
				t.Fatalf("reload reconciled parent: %v", err)
			}
			if storedParent.Status != k12.ModelInvocationReconciled ||
				storedParent.FailureKind != "reconciled_succeeded" {
				t.Errorf("parent was not reconciled from exact physical set: %+v", storedParent)
			}
			children, err := fixture.deps.Records.ListModelPhysicalInvocations(
				fixture.ctx,
				fixture.parent.AgentName,
				fixture.jobID,
			)
			if err != nil {
				t.Fatalf("list reconciled physical children: %v", err)
			}
			if len(children) != len(tt.units) {
				t.Errorf("physical child count=%d want=%d", len(children), len(tt.units))
			}
			for index, child := range children {
				if index >= len(tt.units) ||
					child.PhysicalUnit != tt.units[index] ||
					child.Status != k12.ModelInvocationSucceeded {
					t.Errorf(
						"physical exact set drift at %d: child=%+v want_unit=%v",
						index,
						child,
						tt.units,
					)
				}
			}
			if fixture.recognizer.calls != 0 {
				t.Errorf("positive reconciliation resent provider: calls=%d", fixture.recognizer.calls)
			}
		})
	}
}

// REG-DD-036-PHYSICAL-RECONCILE-003: succeeded is not sufficient when the
// deterministic child identity or image-bound request digest is inconsistent.
func TestDD036RecognitionReconcileRejectsSucceededPhysicalIdentityDrift(t *testing.T) {
	tests := []struct {
		name                  string
		physicalIDOverride    string
		requestDigestOverride string
		resultDigestOverride  string
	}{
		{
			name:               "wrong_stable_physical_id",
			physicalIDOverride: "modelphysical-dd036-wrong-stable-id",
		},
		{
			name:                  "wrong_request_digest",
			requestDigestOverride: "sha256:dd036-wrong-physical-request",
		},
		{
			name:                 "wrong_result_digest",
			resultDigestOverride: "sha256:dd036-wrong-physical-result",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newDD036RecognitionReconcileFixture(
				t,
				"dd036-reconcile-identity-drift-"+tt.name,
				"1+1=",
			)
			child := fixture.addSucceededPhysicalChild(
				t,
				k12.RecognitionPhysicalUnitWholePage,
				fixture.run.req.Image,
				tt.physicalIDOverride,
				tt.requestDigestOverride,
				tt.resultDigestOverride,
				`[{"question":"1+1=","subject":"数学","answer_state":"blank"}]`,
			)
			fixture.persistRawReceipt(
				t,
				recognitionPhysicalReceiptSet(
					[]k12.ModelPhysicalInvocation{child},
				),
			)
			fixture.park(t)

			reconciled, _, reconcileErr := fixture.reconcile(t)
			fixture.assertStillParked(t, reconciled, reconcileErr)
		})
	}
}

// REG-DD-036-PHYSICAL-RECONCILE-004: a self-consistent local questions file
// cannot be combined with an unrelated succeeded physical response. The
// receipt must be cryptographically bound to the exact child result set.
func TestDD036RecognitionReconcileRejectsReceiptChildSetDrift(t *testing.T) {
	fixture := newDD036RecognitionReconcileFixture(
		t,
		"dd036-reconcile-receipt-child-set-drift",
		"9+9=",
	)
	fixture.addSucceededPhysicalChild(
		t,
		k12.RecognitionPhysicalUnitWholePage,
		fixture.run.req.Image,
		"",
		"",
		"",
		`[{"question":"1+1=","subject":"数学","answer_state":"blank"}]`,
	)
	fixture.persistRawReceipt(t, nil)
	fixture.park(t)

	reconciled, _, reconcileErr := fixture.reconcile(t)
	fixture.assertStillParked(t, reconciled, reconcileErr)
}
