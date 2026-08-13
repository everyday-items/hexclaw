package usecase

import (
	"bytes"
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type recognitionInitialCallRecoveryFixture struct {
	orchestrator *GradingOrchestrator
	run          *gradingRun
	job          GradingJobView
	parent       k12.ModelInvocation
	child        k12.ModelPhysicalInvocation
	image        []byte
}

func recognitionInitialCallRecoveryPlanName(planVersion int) string {
	if planVersion == k12.RecognitionPlanVersionV2 {
		return "v2"
	}
	return "v1"
}

func TestREGK12RecognitionDurabilityBudget20260808003RebuildsEveryInitialCallEntryFromPersistedFacts(
	t *testing.T,
) {
	for _, planVersion := range []int{
		k12.RecognitionPlanVersionV1,
		k12.RecognitionPlanVersionV2,
	} {
		planVersion := planVersion
		t.Run(recognitionInitialCallRecoveryPlanName(planVersion), func(t *testing.T) {
			fixture := newRecognitionInitialCallRecoveryFixture(t, planVersion)
			call, err := fixture.orchestrator.rebuildInitialRecognitionPhysicalCall(
				context.Background(),
				fixture.parent,
				fixture.child,
				fixture.image,
			)
			if err != nil {
				t.Fatalf("rebuild plan=%d initial call: %v", planVersion, err)
			}
			if !recognitionPhysicalChildMatchesCall(
				fixture.parent,
				fixture.child,
				call,
			) {
				t.Fatalf("plan=%d rebuilt call does not match child: %+v", planVersion, call)
			}
			switch planVersion {
			case k12.RecognitionPlanVersionV1:
				if call.PlanVersion != 0 || call.PlanDigest != "" ||
					len(call.TargetIDs) != 0 ||
					!bytes.Equal(call.Image, fixture.image) {
					t.Fatalf(
						"v1 reconstruction changed historical bytes or zero plan fields: %+v",
						call,
					)
				}
			case k12.RecognitionPlanVersionV2:
				canonicalPage, canonicalErr :=
					k12.CanonicalizeRecognitionPageV2(fixture.image)
				if canonicalErr != nil {
					t.Fatal(canonicalErr)
				}
				if call.PlanVersion != k12.RecognitionPlanVersionV2 ||
					call.PlanDigest != fixture.child.PlanDigest ||
					!bytes.Equal(call.Image, canonicalPage.PNG) ||
					bytes.Equal(call.Image, fixture.image) {
					t.Fatalf(
						"v2 reconstruction did not use header digest and canonical PNG: %+v",
						call,
					)
				}
			}

			resumable, err := fixture.orchestrator.preparedWholePageRecognitionCanResume(
				context.Background(),
				fixture.parent,
				fixture.image,
			)
			if err != nil || !resumable {
				t.Fatalf(
					"plan=%d prepared-resume entry resumable=%v err=%v",
					planVersion,
					resumable,
					err,
				)
			}
		})
	}
}

func TestREGK12RecognitionDurabilityBudget20260808003BeforeLocalEntryClosesExactPreparedManifest(
	t *testing.T,
) {
	for _, planVersion := range []int{
		k12.RecognitionPlanVersionV1,
		k12.RecognitionPlanVersionV2,
	} {
		planVersion := planVersion
		t.Run(recognitionInitialCallRecoveryPlanName(planVersion), func(t *testing.T) {
			fixture := newRecognitionInitialCallRecoveryFixture(t, planVersion)
			definiteNoSend, observedOtherWorker, err :=
				fixture.orchestrator.settleRecognitionFailureBeforeLocalPhysicalCall(
					context.Background(),
					fixture.parent,
					fixture.image,
				)
			if err != nil || !definiteNoSend || observedOtherWorker {
				t.Fatalf(
					"plan=%d before-local definite=%v observed=%v err=%v",
					planVersion,
					definiteNoSend,
					observedOtherWorker,
					err,
				)
			}
			child, getErr := fixture.orchestrator.deps.Records.GetModelPhysicalInvocation(
				context.Background(),
				fixture.child.AgentName,
				fixture.child.PhysicalInvocationID,
			)
			if getErr != nil || child.Status != k12.ModelInvocationFailed ||
				child.FailureKind != "provider_request_not_sent" {
				t.Fatalf("plan=%d child=%+v err=%v", planVersion, child, getErr)
			}
			children, listErr := fixture.orchestrator.deps.Records.
				ListModelPhysicalInvocations(
					context.Background(),
					fixture.parent.AgentName,
					fixture.parent.JobID,
				)
			if listErr != nil || len(children) != 1 {
				t.Fatalf(
					"plan=%d before-local created extra physical calls: count=%d err=%v",
					planVersion,
					len(children),
					listErr,
				)
			}
		})
	}
}

func newRecognitionInitialCallRecoveryFixture(
	t *testing.T,
	planVersion int,
) recognitionInitialCallRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	currentUnix := int64(1_800_200_000)
	imageBytes := append(
		append([]byte(nil), recognitionLayoutInitialV2PagePNG(t)...),
		[]byte("v1-must-keep-these-original-trailing-bytes")...,
	)
	budget := orchestratorTestBudget()
	if planVersion == k12.RecognitionPlanVersionV2 {
		budget = recognitionLayoutInitialV2Budget()
	}
	route := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    k12.RecognizingPolicyModel,
		Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
		Capability:               "vision",
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
	deps.Now = func() int64 { return currentUnix }
	deps.GradingBudgetSnapshot = budget
	orchestrator := trackGradingOrchestrator(t, NewGradingOrchestrator(
		deps,
		func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
			return route, nil
		},
	))
	job, created, err := orchestrator.StartPhotoGradingJob(
		ctx,
		StartPhotoGradingInput{
			Photo: PhotoGradeRequest{
				AgentName:     "mingming",
				Grade:         "五年级上",
				SourceSession: "initial-call-expiry",
				Image:         imageBytes,
			},
			SourceKind: "desktop",
			SourceKey:  "initial-call-expiry-" + string(rune('0'+planVersion)),
		},
	)
	if err != nil || !created {
		t.Fatalf("start recovery fixture: created=%v err=%v", created, err)
	}
	run := orchestrator.lookup(job.Record.RecordID)
	if run == nil {
		t.Fatal("recovery fixture lost in-memory run")
	}
	job, err = deps.AdvanceGradingStage(
		ctx,
		job.Record.AgentName,
		job.Record.RecordID,
		AdvanceGradingInput{Outcome: GradingOutcomeOK},
	)
	if err != nil {
		t.Fatalf("advance recovery fixture to normalizing: %v", err)
	}
	job, err = deps.AdvanceGradingStage(
		ctx,
		job.Record.AgentName,
		job.Record.RecordID,
		AdvanceGradingInput{
			Outcome:        GradingOutcomeOK,
			ArtifactDigest: "normalized:initial-call-expiry",
		},
	)
	if err != nil || job.Record.Status != k12.GradingStageRecognizing {
		t.Fatalf("advance recovery fixture to recognizing: job=%+v err=%v", job, err)
	}
	policy := k12.ApprovedRecognizingRequestPolicy()
	parent, err := orchestrator.beginRecognizingModelInvocationWithPolicy(
		ctx,
		job,
		imageBytes,
		recognizingInvocationDigest(imageBytes, job.Fields.ModelSnapshot, policy),
		policy,
	)
	if err != nil {
		t.Fatalf("publish initial physical child: %v", err)
	}
	children, err := deps.Records.ListModelPhysicalInvocations(
		ctx,
		parent.AgentName,
		parent.JobID,
	)
	if err != nil || len(children) != 1 {
		t.Fatalf("initial physical children=%d err=%v: %+v", len(children), err, children)
	}
	currentUnix = job.Fields.Deadline + 1
	return recognitionInitialCallRecoveryFixture{
		orchestrator: orchestrator,
		run:          run,
		job:          job,
		parent:       parent,
		child:        children[0],
		image:        imageBytes,
	}
}

// REG-K12-RECOGNITION-DURABILITY-BUDGET-20260808-003：已过期但仍处于 prepared
// 的 V2 清单可以证明未发生 Provider 发送。确定性恢复入口必须匹配不可变 V2 标识，
// 并结算物理子项及其父 Job，而不是永久停驻。
func TestREGK12RecognitionDurabilityBudget20260808003ExpiredPreparedManifestConverges(
	t *testing.T,
) {
	for _, planVersion := range []int{
		k12.RecognitionPlanVersionV1,
		k12.RecognitionPlanVersionV2,
	} {
		t.Run(recognitionInitialCallRecoveryPlanName(planVersion), func(t *testing.T) {
			fixture := newRecognitionInitialCallRecoveryFixture(t, planVersion)
			handled, settled, err :=
				fixture.orchestrator.settleConclusiveRecognitionRecovery(
					context.Background(),
					fixture.run,
					fixture.job,
					fixture.parent,
				)
			if !handled || err == nil {
				t.Fatalf(
					"plan=%d expired recovery handled=%v job=%+v err=%v",
					planVersion,
					handled,
					settled.Record,
					err,
				)
			}
			if settled.Record.Status != k12.GradingStageFailedRetryable {
				t.Fatalf(
					"plan=%d recovered job status=%s, want failed_retryable",
					planVersion,
					settled.Record.Status,
				)
			}
			parent, getErr := fixture.orchestrator.deps.Records.GetModelInvocation(
				context.Background(),
				fixture.parent.AgentName,
				fixture.parent.InvocationID,
			)
			if getErr != nil || parent.Status != k12.ModelInvocationFailed {
				t.Fatalf("plan=%d parent=%+v err=%v", planVersion, parent, getErr)
			}
			child, getErr := fixture.orchestrator.deps.Records.GetModelPhysicalInvocation(
				context.Background(),
				fixture.child.AgentName,
				fixture.child.PhysicalInvocationID,
			)
			if getErr != nil ||
				child.Status != k12.ModelInvocationFailed ||
				child.FailureKind != "provider_request_not_sent" {
				t.Fatalf("plan=%d child=%+v err=%v", planVersion, child, getErr)
			}
		})
	}
}
