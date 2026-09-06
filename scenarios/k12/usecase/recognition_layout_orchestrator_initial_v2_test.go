package usecase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/png"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

var errRecognitionLayoutInitialV2ProbeComplete = errors.New(
	"recognition layout initial v2 probe complete",
)

type recognitionLayoutInitialV2Probe struct {
	records *k12storage.Store

	agentName     string
	jobID         string
	calls         int
	headerDigest  string
	headerEnabled bool
	parent        k12.ModelInvocation
	manifest      k12.ModelPhysicalInvocation
	runtime       k12.RecognitionLayoutPlanRuntimeV2
	jobFields     k12.GradingJobFields
	entryErr      error
}

func (p *recognitionLayoutInitialV2Probe) Recognize(
	ctx context.Context,
	_ []byte,
) ([]RecognizedQuestion, error) {
	p.calls++
	p.headerDigest, p.headerEnabled =
		k12.RecognitionLayoutPlanV2HeaderDigestFromContext(ctx)
	parents, err := p.records.ListModelInvocations(
		context.Background(),
		p.agentName,
		p.jobID,
	)
	if err != nil {
		p.entryErr = err
		return nil, errRecognitionLayoutInitialV2ProbeComplete
	}
	for _, parent := range parents {
		if parent.Stage == k12.GradingStageRecognizing {
			p.parent = parent
			break
		}
	}
	children, err := p.records.ListModelPhysicalInvocations(
		context.Background(),
		p.agentName,
		p.jobID,
	)
	if err != nil {
		p.entryErr = err
		return nil, errRecognitionLayoutInitialV2ProbeComplete
	}
	for _, child := range children {
		if child.ParentInvocationID == p.parent.InvocationID {
			p.manifest = child
			break
		}
	}
	p.runtime, p.entryErr = p.records.LoadRecognitionLayoutPlanRuntimeV2(
		context.Background(),
		p.agentName,
		p.parent.InvocationID,
	)
	record, err := p.records.Get(context.Background(), p.jobID)
	if err != nil {
		p.entryErr = errors.Join(p.entryErr, err)
		return nil, errRecognitionLayoutInitialV2ProbeComplete
	}
	p.jobFields, err = k12.ParseGradingJobFields(record.Fields)
	p.entryErr = errors.Join(p.entryErr, err)
	return nil, errRecognitionLayoutInitialV2ProbeComplete
}

// V2 Job 在进入 Recognizer 前发布父项、头部和清单授权；非 Sol 模型不携带专属参数。
func TestREGK12RecognitionDurabilityBudget20260808001OrchestratesInitialV2HeaderAndManifest(
	t *testing.T,
) {
	const nowUnix int64 = 1_800_000_000
	policy := k12.ModelRequestPolicySnapshot{}
	route := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    "gpt-5.6-luna",
		Route:                    "hexclaw-gpt/gpt-5.6-luna",
		Capability:               "vision",
		RecognizingRequestPolicy: policy,
	}
	budget := recognitionLayoutInitialV2Budget()
	page := recognitionLayoutInitialV2PagePNG(t)
	canonicalPage, err := k12.CanonicalizeRecognitionPageV2(page)
	if err != nil {
		t.Fatalf("canonicalize fixture: %v", err)
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
	deps.Now = func() int64 { return nowUnix }
	deps.GradingBudgetSnapshot = budget
	probe := &recognitionLayoutInitialV2Probe{records: deps.Records}
	deps.Recognizer = probe
	orchestrator := trackGradingOrchestrator(t, NewGradingOrchestrator(
		deps,
		func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
			return route, nil
		},
	))
	job, created, err := orchestrator.StartPhotoGradingJob(
		context.Background(),
		StartPhotoGradingInput{
			Photo: PhotoGradeRequest{
				AgentName:     "mingming",
				Grade:         "五年级上",
				SourceSession: "layout-v2-initial-session",
				Image:         page,
			},
			SourceKind: "desktop",
			SourceKey:  "layout-v2-initial",
		},
	)
	if err != nil || !created {
		t.Fatalf("start V2 grading job: created=%v err=%v", created, err)
	}
	probe.agentName = job.Record.AgentName
	probe.jobID = job.Record.RecordID

	_, runErr := orchestrator.RunGradingJob(
		context.Background(),
		job.Record.RecordID,
	)
	if !errors.Is(runErr, errRecognitionLayoutInitialV2ProbeComplete) {
		t.Fatalf("run error=%v, want focused probe stop", runErr)
	}
	if probe.entryErr != nil {
		t.Fatalf("recognizer-entry evidence: %v", probe.entryErr)
	}
	if probe.calls != 1 || !probe.headerEnabled || probe.headerDigest == "" {
		t.Fatalf(
			"recognizer entry calls=%d v2=%v header=%q, want one V2 call",
			probe.calls,
			probe.headerEnabled,
			probe.headerDigest,
		)
	}
	if probe.parent.InvocationID == "" ||
		probe.parent.Status != k12.ModelInvocationSent {
		t.Fatalf("recognizer entry parent=%+v, want sent", probe.parent)
	}
	if !probe.parent.RequestPolicySnapshot.IsZero() {
		t.Fatalf("non-Sol recognition inherited request policy: %+v", probe.parent.RequestPolicySnapshot)
	}
	if probe.runtime.HeaderDigest != probe.headerDigest ||
		probe.runtime.Status != "prepared_manifest" ||
		probe.runtime.AuthorizedPlan != nil {
		t.Fatalf("recognizer entry runtime=%+v context=%q", probe.runtime, probe.headerDigest)
	}
	header := probe.runtime.Header
	if header.PlanID != recognitionLayoutInitialV2PlanID(probe.parent.InvocationID) ||
		header.ParentInvocationID != probe.parent.InvocationID ||
		header.AgentName != probe.parent.AgentName ||
		header.JobID != probe.parent.JobID ||
		header.PageDigest != canonicalPage.Digest ||
		header.ParentRequestDigest != probe.parent.RequestDigest ||
		header.RouteSnapshot != probe.parent.RouteSnapshot ||
		header.RequestPolicySnapshot != probe.parent.RequestPolicySnapshot ||
		header.PhysicalCallCapMillis != budget.PhysicalCallCapMillis ||
		header.BudgetBuckets != budget.RecognizingBuckets ||
		header.AdapterWorkerHardCap != budget.WorkerHardCap ||
		header.EffectiveConcurrency != budget.EffectiveConcurrency {
		t.Fatalf("recognizer entry header drifted: %+v", header)
	}
	if probe.jobFields.Deadline != nowUnix+901 ||
		header.StageStartedAtUnixMillis != nowUnix*1000 {
		t.Fatalf(
			"recognizing window deadline=%d stage_start_ms=%d, want %d/%d",
			probe.jobFields.Deadline,
			header.StageStartedAtUnixMillis,
			nowUnix+901,
			nowUnix*1000,
		)
	}
	expectedCall := k12.RecognitionPhysicalCall{
		PlanVersion: k12.RecognitionPlanVersionV2,
		PlanDigest:  probe.headerDigest,
		Unit:        k12.RecognitionPhysicalUnitWholePage,
		Image:       canonicalPage.PNG,
	}
	expectedManifestID, err := stableRecognitionPhysicalInvocationIDForCall(
		probe.parent.InvocationID,
		expectedCall,
	)
	if err != nil {
		t.Fatal(err)
	}
	expectedManifestDigest, err := recognizingPhysicalInvocationDigest(
		probe.parent,
		expectedCall,
	)
	if err != nil {
		t.Fatal(err)
	}
	if probe.manifest.PhysicalInvocationID != expectedManifestID ||
		probe.manifest.ParentInvocationID != probe.parent.InvocationID ||
		probe.manifest.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
		probe.manifest.PlanDigest != probe.headerDigest ||
		probe.manifest.CandidateExactSetDigest != "" ||
		probe.manifest.PhysicalUnit != k12.RecognitionPhysicalUnitWholePage ||
		probe.manifest.RequestDigest != expectedManifestDigest ||
		probe.manifest.Status != k12.ModelInvocationPrepared {
		t.Fatalf(
			"recognizer entry manifest=%+v want id=%q digest=%q",
			probe.manifest,
			expectedManifestID,
			expectedManifestDigest,
		)
	}
}

func TestREGK12RecognitionDurabilityBudget20260808001ReusesPreparedAndSucceededV2Manifest(
	t *testing.T,
) {
	ctx := context.Background()
	currentUnix := int64(1_800_100_000)
	policy := k12.ApprovedRecognizingRequestPolicy()
	route := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    k12.RecognizingPolicyModel,
		Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
		Capability:               "vision",
		RecognizingRequestPolicy: policy,
	}
	budget := recognitionLayoutInitialV2Budget()
	page := recognitionLayoutInitialV2PagePNG(t)
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
				SourceSession: "layout-v2-restart-session",
				Image:         page,
			},
			SourceKind: "desktop",
			SourceKey:  "layout-v2-restart",
		},
	)
	if err != nil || !created {
		t.Fatalf("start restart fixture: created=%v err=%v", created, err)
	}
	job, err = deps.AdvanceGradingStage(
		ctx,
		job.Record.AgentName,
		job.Record.RecordID,
		AdvanceGradingInput{Outcome: GradingOutcomeOK},
	)
	if err != nil || job.Record.Status != k12.GradingStageNormalizing {
		t.Fatalf("advance to normalizing: job=%+v err=%v", job, err)
	}
	job, err = deps.AdvanceGradingStage(
		ctx,
		job.Record.AgentName,
		job.Record.RecordID,
		AdvanceGradingInput{
			Outcome:        GradingOutcomeOK,
			ArtifactDigest: "normalized:v2-restart",
		},
	)
	if err != nil || job.Record.Status != k12.GradingStageRecognizing {
		t.Fatalf("advance to recognizing: job=%+v err=%v", job, err)
	}
	requestDigest := recognizingInvocationDigest(
		page,
		job.Fields.ModelSnapshot,
		policy,
	)
	parent, err := orchestrator.beginRecognizingModelInvocationWithPolicy(
		ctx,
		job,
		page,
		requestDigest,
		policy,
	)
	if err != nil {
		t.Fatalf("publish initial V2 manifest: %v", err)
	}
	initialRuntime, err := deps.Records.LoadRecognitionLayoutPlanRuntimeV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil {
		t.Fatalf("load initial runtime: %v", err)
	}
	initialManifest, err := deps.Records.GetModelPhysicalInvocation(
		ctx,
		parent.AgentName,
		initialRuntime.ManifestPhysicalInvocationID,
	)
	if err != nil || initialManifest.Status != k12.ModelInvocationPrepared {
		t.Fatalf("initial manifest=%+v err=%v", initialManifest, err)
	}

	currentUnix += 300
	preparedReplay, err := orchestrator.beginRecognizingModelInvocationWithPolicy(
		ctx,
		job,
		page,
		requestDigest,
		policy,
	)
	if err != nil || preparedReplay.InvocationID != parent.InvocationID {
		t.Fatalf("prepared restart parent=%+v err=%v", preparedReplay, err)
	}
	preparedRuntime, err := deps.Records.LoadRecognitionLayoutPlanRuntimeV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil || preparedRuntime.Header != initialRuntime.Header ||
		preparedRuntime.HeaderDigest != initialRuntime.HeaderDigest {
		t.Fatalf(
			"prepared restart recomputed header: before=%+v after=%+v err=%v",
			initialRuntime,
			preparedRuntime,
			err,
		)
	}

	sentManifest, claimed, err := deps.Records.ClaimModelPhysicalInvocationSent(
		ctx,
		parent.AgentName,
		initialManifest.PhysicalInvocationID,
	)
	if err != nil || !claimed {
		t.Fatalf("claim manifest sent: claimed=%v manifest=%+v err=%v", claimed, sentManifest, err)
	}
	if _, restartErr := orchestrator.beginRecognizingModelInvocationWithPolicy(
		ctx,
		job,
		page,
		requestDigest,
		policy,
	); !errors.Is(restartErr, ErrRecognitionPhysicalCallObservedInFlight) {
		t.Fatalf("sent manifest restart error=%v, want no-resend observer", restartErr)
	}
	children, err := deps.Records.ListModelPhysicalInvocations(
		ctx,
		parent.AgentName,
		parent.JobID,
	)
	if err != nil || len(children) != 1 {
		t.Fatalf("sent restart created children=%d err=%v: %+v", len(children), err, children)
	}

	succeededManifest, err := deps.Records.
		MarkModelPhysicalInvocationSucceededWithContent(
			ctx,
			parent.AgentName,
			initialManifest.PhysicalInvocationID,
			`{"targets":[{"manifest_ref":"manifest_0001"}]}`,
			"",
		)
	if err != nil || succeededManifest.Status != k12.ModelInvocationSucceeded {
		t.Fatalf("succeed manifest: manifest=%+v err=%v", succeededManifest, err)
	}
	succeededReplay, err := orchestrator.beginRecognizingModelInvocationWithPolicy(
		ctx,
		job,
		page,
		requestDigest,
		policy,
	)
	if err != nil || succeededReplay.InvocationID != parent.InvocationID {
		t.Fatalf(
			"succeeded manifest must enter adapter private replay: parent=%+v err=%v",
			succeededReplay,
			err,
		)
	}
	afterRuntime, err := deps.Records.LoadRecognitionLayoutPlanRuntimeV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil || afterRuntime.Header != initialRuntime.Header ||
		afterRuntime.HeaderDigest != initialRuntime.HeaderDigest {
		t.Fatalf(
			"succeeded restart changed header: before=%+v after=%+v err=%v",
			initialRuntime,
			afterRuntime,
			err,
		)
	}
	probe := &recognitionLayoutInitialV2Probe{
		records:   deps.Records,
		agentName: parent.AgentName,
		jobID:     parent.JobID,
	}
	orchestrator.deps.Recognizer = probe
	orchestrator.mu.Lock()
	run := orchestrator.runs[parent.JobID]
	orchestrator.mu.Unlock()
	if run == nil {
		t.Fatal("restart fixture lost its grading run")
	}
	if _, runErr := orchestrator.runRecognize(
		ctx,
		run,
		parent.JobID,
	); !errors.Is(runErr, errRecognitionLayoutInitialV2ProbeComplete) {
		t.Fatalf("succeeded manifest did not enter recognizer: %v", runErr)
	}
	if probe.entryErr != nil || probe.calls != 1 || !probe.headerEnabled ||
		probe.headerDigest != initialRuntime.HeaderDigest ||
		probe.runtime.Status != "manifest_succeeded" {
		t.Fatalf(
			"succeeded manifest adapter entry probe=%+v",
			probe,
		)
	}
}

func recognitionLayoutInitialV2PlanID(parentInvocationID string) string {
	sum := sha256.Sum256([]byte(
		"k12-recognition-layout-plan-v2\x00" + parentInvocationID,
	))
	return "layoutplan-" + hex.EncodeToString(sum[:16])
}

func recognitionLayoutInitialV2Budget() k12.GradingBudgetSnapshot {
	return k12.GradingBudgetSnapshot{
		PolicyVersion:          20260808,
		RecognitionPlanVersion: k12.RecognitionPlanVersionV2,
		StageSeconds: k12.GradingStageBudgets{
			Queued:      60,
			Normalizing: 60,
			Recognizing: 901,
			Locating:    60,
			Rendering:   60,
			Projecting:  60,
		},
		AssessingBuckets: []k12.GradingAssessingBudgetBucket{
			{MaxProblems: 1, Seconds: 90},
			{MaxProblems: 8, Seconds: 180},
			{MaxProblems: 16, Seconds: 300},
			{MaxProblems: 32, Seconds: 540},
		},
		ItemConcurrency: 2,
		RecognizingBuckets: k12.RecognitionLayoutBudgetBucketsV2{
			UpTo1ProblemMillis:   120000,
			UpTo8ProblemsMillis:  300000,
			UpTo16ProblemsMillis: 600000,
			UpTo32ProblemsMillis: 900001,
		},
		PhysicalCallCapMillis: 120000,
		WorkerHardCap:         2,
		EffectiveConcurrency:  1,
	}
}

func recognitionLayoutInitialV2PagePNG(t *testing.T) []byte {
	t.Helper()
	// 此共享夹具代表已经选择密集 V2 的 Job；其几何尺寸须保持在已冻结的
	// 密集页面分类边界内。
	page := image.NewGray(image.Rect(0, 0, 800, 1200))
	for y := range 12 {
		for x := range 8 {
			page.Pix[y*page.Stride+x] = uint8(x*19 + y*7)
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, page); err != nil {
		t.Fatalf("encode V2 page fixture: %v", err)
	}
	return encoded.Bytes()
}
