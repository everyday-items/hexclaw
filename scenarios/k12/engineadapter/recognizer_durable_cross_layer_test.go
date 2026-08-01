package engineadapter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/resourcegov"
	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

type dd036CrossLayerMode string

const (
	dd036CrossLayerWholeValid       dd036CrossLayerMode = "whole_valid"
	dd036CrossLayerWholeValidEmpty  dd036CrossLayerMode = "whole_valid_empty"
	dd036CrossLayerProtocolFallback dd036CrossLayerMode = "protocol_fallback"
	dd036CrossLayerWholeEmptyObject dd036CrossLayerMode = "whole_empty_object"
	dd036CrossLayerDanglingChild    dd036CrossLayerMode = "dangling_subproblem"
	dd036CrossLayerSegment1Protocol dd036CrossLayerMode = "segment_1_protocol_failure"
	dd036CrossLayerSegment3Failure  dd036CrossLayerMode = "segment_3_failure"
	dd036CrossLayerProviderFailure  dd036CrossLayerMode = "provider_failure"
	dd036CrossLayerTransportFailure dd036CrossLayerMode = "transport_failure"
	dd036CrossLayerTimeout          dd036CrossLayerMode = "timeout"
	dd036CrossLayerCancel           dd036CrossLayerMode = "cancel"
)

const dd036CrossLayerQuestionJSON = `[
	{
		"question":"1+1=",
		"subject":"数学",
		"answer_state":"blank",
		"student_answer":"",
		"recognition_confidence":0.99
	}
]`

type dd036CrossLayerVisionProbe struct {
	store            *k12storage.Store
	jobID            string
	snapshot         k12.GradingModelSnapshot
	expectedDeadline time.Time
	mode             dd036CrossLayerMode
	cancelRun        context.CancelFunc

	mu              sync.Mutex
	units           []k12.RecognitionPhysicalUnit
	deadlines       []time.Time
	invariantErrors []string
}

func (p *dd036CrossLayerVisionProbe) addInvariantError(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.invariantErrors = append(p.invariantErrors, fmt.Sprintf(format, args...))
}

func (p *dd036CrossLayerVisionProbe) vision(
	ctx context.Context,
	_ []byte,
	prompt string,
) (string, error) {
	unit := dd036CrossLayerPhysicalUnit(prompt)
	deadline, hasDeadline := ctx.Deadline()

	p.mu.Lock()
	p.units = append(p.units, unit)
	callNumber := len(p.units)
	if hasDeadline {
		p.deadlines = append(p.deadlines, deadline)
	}
	p.mu.Unlock()

	if !hasDeadline {
		p.addInvariantError("sender call %d unit=%s has no absolute deadline", callNumber, unit)
	} else if !deadline.Equal(p.expectedDeadline) {
		p.addInvariantError(
			"sender call %d unit=%s deadline=%s want=%s",
			callNumber,
			unit,
			deadline.Format(time.RFC3339Nano),
			p.expectedDeadline.Format(time.RFC3339Nano),
		)
	}
	snapshot, hasSnapshot := k12.GradingModelSnapshotFromContext(ctx)
	if !hasSnapshot ||
		k12.NormalizeGradingModelSnapshot(snapshot) !=
			k12.NormalizeGradingModelSnapshot(p.snapshot) {
		p.addInvariantError(
			"sender call %d unit=%s route=%+v has_snapshot=%v want=%+v",
			callNumber,
			unit,
			snapshot,
			hasSnapshot,
			p.snapshot,
		)
	}
	policy, hasPolicy := k12.GradingModelRequestPolicyFromContext(ctx)
	if !hasPolicy || policy != k12.ApprovedRecognizingRequestPolicy() {
		p.addInvariantError(
			"sender call %d unit=%s policy=%+v has_policy=%v",
			callNumber,
			unit,
			policy,
			hasPolicy,
		)
	}

	children, err := p.store.ListModelPhysicalInvocations(
		context.Background(),
		"mingming",
		p.jobID,
	)
	if err != nil {
		p.addInvariantError(
			"sender call %d unit=%s cannot read durable children: %v",
			callNumber,
			unit,
			err,
		)
	} else {
		sentMatches := 0
		for _, child := range children {
			if child.PhysicalUnit == unit &&
				child.Status == k12.ModelInvocationSent {
				sentMatches++
			}
		}
		if len(children) != callNumber || sentMatches != 1 {
			p.addInvariantError(
				"sender call %d unit=%s durable-before-send rows=%d sent_matches=%d children=%+v",
				callNumber,
				unit,
				len(children),
				sentMatches,
				children,
			)
		}
	}

	switch p.mode {
	case dd036CrossLayerWholeValid:
		return dd036CrossLayerQuestionJSON, nil
	case dd036CrossLayerWholeValidEmpty:
		return `[]`, nil
	case dd036CrossLayerProtocolFallback:
		if unit == k12.RecognitionPhysicalUnitWholePage {
			return "not-json", nil
		}
		if unit == k12.RecognitionPhysicalUnitSegment1 {
			return dd036CrossLayerQuestionJSON, nil
		}
		return `[]`, nil
	case dd036CrossLayerWholeEmptyObject:
		if unit == k12.RecognitionPhysicalUnitWholePage {
			return `[{}]`, nil
		}
		if unit == k12.RecognitionPhysicalUnitSegment1 {
			return dd036CrossLayerQuestionJSON, nil
		}
		return `[]`, nil
	case dd036CrossLayerDanglingChild:
		if unit == k12.RecognitionPhysicalUnitWholePage {
			return `[{
				"problem_id":"dangling-child",
				"problem_kind":"subproblem",
				"parent_problem_id":"missing-parent",
				"subproblem_no":"1",
				"question":"1+1=",
				"subject":"数学",
				"answer_state":"blank",
				"student_answer":"",
				"recognition_confidence":0.99
			}]`, nil
		}
		if unit == k12.RecognitionPhysicalUnitSegment1 {
			return dd036CrossLayerQuestionJSON, nil
		}
		return `[]`, nil
	case dd036CrossLayerSegment1Protocol:
		switch unit {
		case k12.RecognitionPhysicalUnitWholePage,
			k12.RecognitionPhysicalUnitSegment1:
			return "not-json", nil
		default:
			return `[]`, nil
		}
	case dd036CrossLayerSegment3Failure:
		switch unit {
		case k12.RecognitionPhysicalUnitWholePage:
			return "not-json", nil
		case k12.RecognitionPhysicalUnitSegment3:
			return "", dd036CrossLayerProviderError()
		default:
			return `[]`, nil
		}
	case dd036CrossLayerProviderFailure:
		return "", dd036CrossLayerProviderError()
	case dd036CrossLayerTransportFailure:
		return "", io.ErrUnexpectedEOF
	case dd036CrossLayerTimeout:
		<-ctx.Done()
		return "", ctx.Err()
	case dd036CrossLayerCancel:
		if p.cancelRun == nil {
			return "", fmt.Errorf("cancel callback is not configured")
		}
		p.cancelRun()
		<-ctx.Done()
		return "", ctx.Err()
	default:
		return "", fmt.Errorf("unsupported cross-layer mode %q", p.mode)
	}
}

func dd036CrossLayerProviderError() error {
	return &llm.ProviderError{
		Provider:   "hexclaw-gpt",
		StatusCode: http.StatusBadGateway,
		Status:     "502 Bad Gateway",
	}
}

func dd036CrossLayerPhysicalUnit(prompt string) k12.RecognitionPhysicalUnit {
	if strings.Contains(prompt, "整页印刷题清单") {
		return k12.RecognitionPhysicalUnitPrintedInventory
	}
	for index := 1; index <= len(denseWorksheetRanges); index++ {
		if strings.Contains(
			prompt,
			fmt.Sprintf("纵向分片 %d/%d", index, len(denseWorksheetRanges)),
		) {
			unit, _ := k12.RecognitionPhysicalSegmentUnit(index)
			return unit
		}
	}
	return k12.RecognitionPhysicalUnitWholePage
}

type dd036CrossLayerHarness struct {
	store        *k12storage.Store
	orchestrator *usecase.GradingOrchestrator
	probe        *dd036CrossLayerVisionProbe
	frozenNow    int64
}

func newDD036CrossLayerStore(
	t *testing.T,
) (*k12storage.Store, scenario.ConstraintProvider) {
	t.Helper()

	db, err := sql.Open(
		"sqlite",
		filepath.Join(t.TempDir(), "dd036-cross-layer.db"),
	)
	if err != nil {
		t.Fatalf("open file SQLite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close file SQLite: %v", err)
		}
	})
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatalf("migrate file SQLite: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO agents(name, metadata) VALUES(?, ?)`,
		"mingming",
		`{"k12.grade_term":"五年级上"}`,
	); err != nil {
		t.Fatalf("insert test agent: %v", err)
	}

	registry := scenario.NewRegistry()
	constraint := k12.NewCurriculumStub()
	if err := registry.Assemble(k12.Pack(constraint)); err != nil {
		t.Fatalf("assemble K12 registry: %v", err)
	}
	return k12storage.NewStore(db, registry.Records), constraint
}

func newDD036CrossLayerHarness(
	t *testing.T,
	store *k12storage.Store,
	constraint scenario.ConstraintProvider,
	mode dd036CrossLayerMode,
	options ...RecognizerOption,
) *dd036CrossLayerHarness {
	t.Helper()

	snapshot := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    k12.RecognizingPolicyModel,
		Route:                    "hexclaw-gpt/gpt-5.6-sol",
		Capability:               "vision",
		TimeoutMS:                120_000,
		RecognizingRequestPolicy: k12.ApprovedRecognizingRequestPolicy(),
	}
	probe := &dd036CrossLayerVisionProbe{
		store:    store,
		snapshot: snapshot,
		mode:     mode,
	}
	frozenNow := time.Now().Unix()
	deps := usecase.Deps{
		Recognizer: NewRecognizerAdapter(probe.vision, options...),
		Records:    store,
		Constraint: constraint,
		Now:        func() int64 { return frozenNow },
	}
	orchestrator := usecase.NewGradingOrchestrator(
		deps,
		func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
			return snapshot, nil
		},
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := orchestrator.Shutdown(ctx); err != nil {
			t.Errorf("shutdown grading orchestrator: %v", err)
		}
	})
	return &dd036CrossLayerHarness{
		store:        store,
		orchestrator: orchestrator,
		probe:        probe,
		frozenNow:    frozenNow,
	}
}

func (h *dd036CrossLayerHarness) start(
	t *testing.T,
	mode dd036CrossLayerMode,
) usecase.GradingJobView {
	t.Helper()
	job, created, err := h.orchestrator.StartPhotoGradingJob(
		context.Background(),
		usecase.StartPhotoGradingInput{
			Photo: usecase.PhotoGradeRequest{
				AgentName:     "mingming",
				Grade:         "五年级上",
				SourceSession: "dd036-cross-layer",
				Image:         denseWorksheetTestImage(t, 1000, 1800),
			},
			SourceKind: "desktop",
			SourceKey:  "dd036-cross-layer-" + string(mode),
		},
	)
	if err != nil || !created {
		t.Fatalf("start grading job created=%v err=%v", created, err)
	}
	h.probe.jobID = job.Record.RecordID
	return job
}

func dd036CrossLayerUnitStrings(
	units []k12.RecognitionPhysicalUnit,
) []string {
	out := make([]string, len(units))
	for index := range units {
		out[index] = string(units[index])
	}
	return out
}

func dd036CrossLayerStatusStrings(
	statuses []k12.ModelInvocationStatus,
) []string {
	out := make([]string, len(statuses))
	for index := range statuses {
		out[index] = string(statuses[index])
	}
	return out
}

// REG-DD-036: this is the actual cross-layer E1 seam. A real
// GradingOrchestrator installs its durable physical-call executor into the
// context consumed by the production RecognizerAdapter, and every VisionFunc
// send is observed only after its file-SQLite child has crossed prepared→sent.
func TestDD036GradingOrchestratorRecognizerAdapterDurablePhysicalCalls(t *testing.T) {
	store, constraint := newDD036CrossLayerStore(t)
	allFallbackUnits := []k12.RecognitionPhysicalUnit{
		k12.RecognitionPhysicalUnitWholePage,
		k12.RecognitionPhysicalUnitSegment1,
		k12.RecognitionPhysicalUnitSegment2,
		k12.RecognitionPhysicalUnitSegment3,
		k12.RecognitionPhysicalUnitSegment4,
		k12.RecognitionPhysicalUnitSegment5,
		k12.RecognitionPhysicalUnitPrintedInventory,
	}
	testCases := []struct {
		name              string
		mode              dd036CrossLayerMode
		wantUnits         []k12.RecognitionPhysicalUnit
		wantChildStatuses []k12.ModelInvocationStatus
		wantFailureKinds  []string
		wantParentStatus  k12.ModelInvocationStatus
		wantJobStatus     string
		wantJobFailed     bool
		wantRunError      bool
		useCallerTimeout  bool
		useCallerCancel   bool
		wantZeroSegments  bool
	}{
		{
			name:              "valid_whole_page_is_one_child",
			mode:              dd036CrossLayerWholeValid,
			wantUnits:         []k12.RecognitionPhysicalUnit{k12.RecognitionPhysicalUnitWholePage},
			wantChildStatuses: []k12.ModelInvocationStatus{k12.ModelInvocationSucceeded},
			wantFailureKinds:  []string{""},
			wantParentStatus:  k12.ModelInvocationSucceeded,
			wantJobStatus:     k12.GradingStageAwaitingConfirmation,
		},
		{
			name:              "valid_empty_array_remains_one_child_without_fallback",
			mode:              dd036CrossLayerWholeValidEmpty,
			wantUnits:         []k12.RecognitionPhysicalUnit{k12.RecognitionPhysicalUnitWholePage},
			wantChildStatuses: []k12.ModelInvocationStatus{k12.ModelInvocationSucceeded},
			wantFailureKinds:  []string{""},
			wantParentStatus:  k12.ModelInvocationSucceeded,
			wantJobStatus:     k12.GradingStageFailedTerminal,
			wantRunError:      true,
		},
		{
			name:      "whole_protocol_failure_completes_all_seven_children",
			mode:      dd036CrossLayerProtocolFallback,
			wantUnits: allFallbackUnits,
			wantChildStatuses: []k12.ModelInvocationStatus{
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
			},
			wantFailureKinds: []string{"", "", "", "", "", "", ""},
			wantParentStatus: k12.ModelInvocationSucceeded,
			wantJobStatus:    k12.GradingStageAwaitingConfirmation,
		},
		{
			name:      "whole_dangling_subproblem_triggers_all_seven_children",
			mode:      dd036CrossLayerDanglingChild,
			wantUnits: allFallbackUnits,
			wantChildStatuses: []k12.ModelInvocationStatus{
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
			},
			wantFailureKinds: []string{"", "", "", "", "", "", ""},
			wantParentStatus: k12.ModelInvocationSucceeded,
			wantJobStatus:    k12.GradingStageAwaitingConfirmation,
		},
		{
			name:      "whole_semantically_empty_object_triggers_all_seven_children",
			mode:      dd036CrossLayerWholeEmptyObject,
			wantUnits: allFallbackUnits,
			wantChildStatuses: []k12.ModelInvocationStatus{
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
			},
			wantFailureKinds: []string{"", "", "", "", "", "", ""},
			wantParentStatus: k12.ModelInvocationSucceeded,
			wantJobStatus:    k12.GradingStageAwaitingConfirmation,
		},
		{
			name:      "segment_one_protocol_failure_is_definite_after_two_successful_children",
			mode:      dd036CrossLayerSegment1Protocol,
			wantUnits: allFallbackUnits[:2],
			wantChildStatuses: []k12.ModelInvocationStatus{
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
			},
			wantFailureKinds: []string{"", ""},
			wantParentStatus: k12.ModelInvocationFailed,
			wantJobFailed:    true,
			wantRunError:     true,
		},
		{
			name:      "segment_three_provider_failure_stops_after_four_children",
			mode:      dd036CrossLayerSegment3Failure,
			wantUnits: allFallbackUnits[:4],
			wantChildStatuses: []k12.ModelInvocationStatus{
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationSucceeded,
				k12.ModelInvocationFailed,
			},
			wantFailureKinds: []string{"", "", "", "provider_response_http_502"},
			wantParentStatus: k12.ModelInvocationFailed,
			wantJobStatus:    k12.GradingStageFailedRetryable,
			wantRunError:     true,
		},
		{
			name:              "whole_provider_response_has_no_segment_retry",
			mode:              dd036CrossLayerProviderFailure,
			wantUnits:         []k12.RecognitionPhysicalUnit{k12.RecognitionPhysicalUnitWholePage},
			wantChildStatuses: []k12.ModelInvocationStatus{k12.ModelInvocationFailed},
			wantFailureKinds:  []string{"provider_response_http_502"},
			wantParentStatus:  k12.ModelInvocationFailed,
			wantJobStatus:     k12.GradingStageFailedRetryable,
			wantRunError:      true,
			wantZeroSegments:  true,
		},
		{
			name:              "whole_transport_ambiguity_has_no_segment_retry",
			mode:              dd036CrossLayerTransportFailure,
			wantUnits:         []k12.RecognitionPhysicalUnit{k12.RecognitionPhysicalUnitWholePage},
			wantChildStatuses: []k12.ModelInvocationStatus{k12.ModelInvocationOutcomeUnknown},
			wantFailureKinds:  []string{"provider_outcome_unknown"},
			wantParentStatus:  k12.ModelInvocationOutcomeUnknown,
			wantJobStatus:     k12.GradingStageOutcomeUnknown,
			wantRunError:      true,
			wantZeroSegments:  true,
		},
		{
			name:              "whole_timeout_has_no_segment_retry",
			mode:              dd036CrossLayerTimeout,
			wantUnits:         []k12.RecognitionPhysicalUnit{k12.RecognitionPhysicalUnitWholePage},
			wantChildStatuses: []k12.ModelInvocationStatus{k12.ModelInvocationOutcomeUnknown},
			wantFailureKinds:  []string{"provider_outcome_unknown"},
			wantParentStatus:  k12.ModelInvocationOutcomeUnknown,
			wantJobStatus:     k12.GradingStageOutcomeUnknown,
			wantRunError:      true,
			useCallerTimeout:  true,
			wantZeroSegments:  true,
		},
		{
			name:              "whole_cancel_has_no_segment_retry",
			mode:              dd036CrossLayerCancel,
			wantUnits:         []k12.RecognitionPhysicalUnit{k12.RecognitionPhysicalUnitWholePage},
			wantChildStatuses: []k12.ModelInvocationStatus{k12.ModelInvocationOutcomeUnknown},
			wantFailureKinds:  []string{"provider_outcome_unknown"},
			wantParentStatus:  k12.ModelInvocationOutcomeUnknown,
			wantJobStatus:     k12.GradingStageOutcomeUnknown,
			wantRunError:      true,
			useCallerCancel:   true,
			wantZeroSegments:  true,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			harness := newDD036CrossLayerHarness(
				t,
				store,
				constraint,
				testCase.mode,
			)
			job := harness.start(t, testCase.mode)

			runCtx := context.Background()
			cancelRun := func() {}
			expectedDeadline := time.Unix(
				harness.frozenNow+k12.GradingStageBudgetSeconds(k12.GradingStageRecognizing),
				0,
			)
			if testCase.useCallerTimeout {
				runCtx, cancelRun = context.WithTimeout(context.Background(), 5*time.Second)
				expectedDeadline, _ = runCtx.Deadline()
			} else if testCase.useCallerCancel {
				runCtx, cancelRun = context.WithCancel(context.Background())
				harness.probe.cancelRun = cancelRun
			}
			defer cancelRun()
			harness.probe.expectedDeadline = expectedDeadline

			view, runErr := harness.orchestrator.RunGradingJob(
				runCtx,
				job.Record.RecordID,
			)
			if (runErr != nil) != testCase.wantRunError {
				t.Fatalf(
					"RunGradingJob error=%v want_error=%v view=%+v",
					runErr,
					testCase.wantRunError,
					view,
				)
			}
			if view.Record == nil {
				t.Fatalf(
					"job record missing want_status=%s run_err=%v",
					testCase.wantJobStatus,
					runErr,
				)
			}
			if testCase.wantJobFailed {
				if view.Record.Status != k12.GradingStageFailedRetryable &&
					view.Record.Status != k12.GradingStageFailedTerminal {
					t.Fatalf(
						"job status=%s want a definite failed state (never outcome_unknown) run_err=%v",
						view.Record.Status,
						runErr,
					)
				}
			} else if view.Record.Status != testCase.wantJobStatus {
				t.Fatalf(
					"job status=%s want=%s run_err=%v",
					view.Record.Status,
					testCase.wantJobStatus,
					runErr,
				)
			}

			harness.probe.mu.Lock()
			sentUnits := append(
				[]k12.RecognitionPhysicalUnit(nil),
				harness.probe.units...,
			)
			deadlines := append([]time.Time(nil), harness.probe.deadlines...)
			invariantErrors := append(
				[]string(nil),
				harness.probe.invariantErrors...,
			)
			harness.probe.mu.Unlock()
			if len(invariantErrors) != 0 {
				t.Fatalf("sender context/durable-before-send invariants failed: %s", strings.Join(invariantErrors, "; "))
			}
			if fmt.Sprint(sentUnits) != fmt.Sprint(testCase.wantUnits) {
				t.Fatalf(
					"VisionFunc units=%v want=%v",
					sentUnits,
					testCase.wantUnits,
				)
			}
			if len(deadlines) != len(testCase.wantUnits) {
				t.Fatalf(
					"sender deadline count=%d want=%d deadlines=%v",
					len(deadlines),
					len(testCase.wantUnits),
					deadlines,
				)
			}
			for index, deadline := range deadlines {
				if !deadline.Equal(expectedDeadline) {
					t.Fatalf(
						"sender deadline[%d]=%s want same absolute deadline=%s",
						index,
						deadline.Format(time.RFC3339Nano),
						expectedDeadline.Format(time.RFC3339Nano),
					)
				}
			}

			parents, err := harness.store.ListModelInvocations(
				context.Background(),
				"mingming",
				job.Record.RecordID,
			)
			if err != nil {
				t.Fatalf("list parent invocations: %v", err)
			}
			var parent k12.ModelInvocation
			for _, invocation := range parents {
				if invocation.Stage == k12.GradingStageRecognizing {
					parent = invocation
					break
				}
			}
			if parent.InvocationID == "" ||
				parent.Status != testCase.wantParentStatus {
				t.Fatalf(
					"recognizing parent=%+v want_status=%s all=%+v",
					parent,
					testCase.wantParentStatus,
					parents,
				)
			}

			children, err := harness.store.ListModelPhysicalInvocations(
				context.Background(),
				"mingming",
				job.Record.RecordID,
			)
			if err != nil {
				t.Fatalf("list durable physical children: %v", err)
			}
			if len(children) != len(testCase.wantUnits) {
				t.Fatalf(
					"physical child count=%d want=%d children=%+v",
					len(children),
					len(testCase.wantUnits),
					children,
				)
			}
			gotUnits := make([]k12.RecognitionPhysicalUnit, len(children))
			gotStatuses := make([]k12.ModelInvocationStatus, len(children))
			requestDigests := make(map[string]struct{}, len(children))
			segmentCount := 0
			for index, child := range children {
				gotUnits[index] = child.PhysicalUnit
				gotStatuses[index] = child.Status
				if strings.HasPrefix(string(child.PhysicalUnit), "segment_") {
					segmentCount++
				}
				if child.ParentInvocationID != parent.InvocationID ||
					child.AgentName != "mingming" ||
					child.JobID != job.Record.RecordID ||
					child.Stage != k12.GradingStageRecognizing ||
					child.Attempt != 1 ||
					child.RouteSnapshot != parent.RouteSnapshot ||
					child.RequestPolicySnapshot != parent.RequestPolicySnapshot ||
					child.RequestDigest == "" ||
					child.RequestDigest == parent.RequestDigest {
					t.Fatalf(
						"physical child[%d] identity/attempt drift parent=%+v child=%+v",
						index,
						parent,
						child,
					)
				}
				if child.FailureKind != testCase.wantFailureKinds[index] {
					t.Fatalf(
						"physical child[%d] failure_kind=%q want=%q child=%+v",
						index,
						child.FailureKind,
						testCase.wantFailureKinds[index],
						child,
					)
				}
				if child.Status == k12.ModelInvocationSucceeded {
					if child.ResultDigest == "" {
						t.Fatalf("successful physical child[%d] has no result digest: %+v", index, child)
					}
				} else if child.ResultDigest != "" {
					t.Fatalf("non-success physical child[%d] leaked result digest: %+v", index, child)
				}
				if _, duplicate := requestDigests[child.RequestDigest]; duplicate {
					t.Fatalf("physical child request digest is not unit-unique: %+v", children)
				}
				requestDigests[child.RequestDigest] = struct{}{}
			}
			if fmt.Sprint(gotUnits) != fmt.Sprint(testCase.wantUnits) {
				t.Fatalf(
					"durable child units=%v want=%v",
					dd036CrossLayerUnitStrings(gotUnits),
					dd036CrossLayerUnitStrings(testCase.wantUnits),
				)
			}
			if fmt.Sprint(gotStatuses) != fmt.Sprint(testCase.wantChildStatuses) {
				t.Fatalf(
					"durable child statuses=%v want=%v children=%+v",
					dd036CrossLayerStatusStrings(gotStatuses),
					dd036CrossLayerStatusStrings(testCase.wantChildStatuses),
					children,
				)
			}
			if testCase.wantZeroSegments && segmentCount != 0 {
				t.Fatalf(
					"whole-call provider/transport/timeout/cancel failure started %d segments: %+v",
					segmentCount,
					children,
				)
			}
		})
	}
}

// REG-DD-036: cancellation while the actual adapter is still queued for its
// CPU split permit proves that no provider request escaped. The already-sent
// stage parent must therefore converge to a definite failure, never to
// outcome_unknown, and no physical child may exist.
func TestDD036RecognizerGovernorCancellationBeforePhysicalSendIsDefinite(t *testing.T) {
	store, constraint := newDD036CrossLayerStore(t)
	governor, err := resourcegov.New(resourcegov.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	first, err := governor.Acquire(
		context.Background(),
		resourcegov.ResourceCPUHeavy,
		resourcegov.PriorityBackground,
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := governor.Acquire(
		context.Background(),
		resourcegov.ResourceCPUHeavy,
		resourcegov.PriorityBackground,
	)
	if err != nil {
		first.Release()
		t.Fatal(err)
	}
	t.Cleanup(first.Release)
	t.Cleanup(second.Release)

	harness := newDD036CrossLayerHarness(
		t,
		store,
		constraint,
		dd036CrossLayerWholeValid,
		WithRecognizerResourceGovernor(governor),
	)
	job := harness.start(t, dd036CrossLayerMode("governor_cancel"))
	harness.probe.expectedDeadline = time.Unix(
		harness.frozenNow+
			k12.GradingStageBudgetSeconds(k12.GradingStageRecognizing),
		0,
	)

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	type runResult struct {
		view usecase.GradingJobView
		err  error
	}
	done := make(chan runResult, 1)
	go func() {
		view, runErr := harness.orchestrator.RunGradingJob(
			runCtx,
			job.Record.RecordID,
		)
		done <- runResult{view: view, err: runErr}
	}()

	queueDeadline := time.Now().Add(5 * time.Second)
	for {
		queued := governor.Snapshot().
			Resources[resourcegov.ResourceCPUHeavy].
			QueuedInteractive
		if queued == 1 {
			break
		}
		if time.Now().After(queueDeadline) {
			t.Fatalf(
				"recognizer did not queue at CPU split boundary; queued=%d",
				queued,
			)
		}
		time.Sleep(time.Millisecond)
	}
	cancelRun()

	var result runResult
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunGradingJob did not return after governor wait cancellation")
	}
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("RunGradingJob error=%v want context cancellation", result.err)
	}
	if result.view.Record == nil ||
		(result.view.Record.Status != k12.GradingStageFailedRetryable &&
			result.view.Record.Status != k12.GradingStageFailedTerminal) {
		t.Fatalf(
			"zero-send governor cancellation job=%+v err=%v; want definite failed, never outcome_unknown",
			result.view,
			result.err,
		)
	}

	harness.probe.mu.Lock()
	visionUnits := append(
		[]k12.RecognitionPhysicalUnit(nil),
		harness.probe.units...,
	)
	harness.probe.mu.Unlock()
	if len(visionUnits) != 0 {
		t.Fatalf("zero-send governor cancellation reached VisionFunc: %v", visionUnits)
	}
	children, err := store.ListModelPhysicalInvocations(
		context.Background(),
		"mingming",
		job.Record.RecordID,
	)
	if err != nil {
		t.Fatalf("list physical children: %v", err)
	}
	if len(children) != 1 ||
		children[0].PhysicalUnit != k12.RecognitionPhysicalUnitWholePage ||
		children[0].Status != k12.ModelInvocationFailed ||
		children[0].FailureKind != "provider_request_not_sent" {
		t.Fatalf(
			"zero-send governor cancellation children=%+v; want exact whole_page failed/provider_request_not_sent",
			children,
		)
	}
	parents, err := store.ListModelInvocations(
		context.Background(),
		"mingming",
		job.Record.RecordID,
	)
	if err != nil {
		t.Fatalf("list parent invocations: %v", err)
	}
	var recognizing k12.ModelInvocation
	for _, invocation := range parents {
		if invocation.Stage == k12.GradingStageRecognizing {
			recognizing = invocation
			break
		}
	}
	if recognizing.InvocationID == "" ||
		recognizing.Status != k12.ModelInvocationFailed ||
		recognizing.Attempt != 1 {
		t.Fatalf(
			"zero-send governor parent=%+v; want failed attempt=1 parents=%+v",
			recognizing,
			parents,
		)
	}

	first.Release()
	second.Release()
	metric := governor.Snapshot().Resources[resourcegov.ResourceCPUHeavy]
	if metric.InUse != 0 || metric.QueuedInteractive != 0 {
		t.Fatalf("governor permit/queue leaked after cancellation: %+v", metric)
	}
}

// REG-DD-036: once the recognizing route and approved request policy are in
// scope, a missing physical-call executor is an invalid production context.
// The adapter must fail before VisionFunc; silently falling back to a direct
// send would let a real POST escape without its immutable child receipt.
func TestDD036ApprovedRecognitionContextRequiresPhysicalExecutorBeforeVision(t *testing.T) {
	snapshot := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    k12.RecognizingPolicyModel,
		Route:                    "hexclaw-gpt/gpt-5.6-sol",
		Capability:               "vision",
		TimeoutMS:                120_000,
		RecognizingRequestPolicy: k12.ApprovedRecognizingRequestPolicy(),
	}
	t.Run("approved_scope_fails_before_send_without_executor", func(t *testing.T) {
		visionCalls := 0
		recognizer := NewRecognizerAdapter(
			func(context.Context, []byte, string) (string, error) {
				visionCalls++
				return dd036CrossLayerQuestionJSON, nil
			},
		)
		ctx := k12.WithGradingModelSnapshot(context.Background(), snapshot)
		ctx = k12.WithGradingModelRequestPolicy(
			ctx,
			k12.ApprovedRecognizingRequestPolicy(),
		)

		_, err := recognizer.Recognize(ctx, []byte("approved-scope-image"))
		if visionCalls != 0 {
			t.Fatalf(
				"approved recognizing scope without executor reached VisionFunc %d times",
				visionCalls,
			)
		}
		if !errors.Is(err, usecase.ErrRecognitionPhysicalCallBeforeSend) {
			t.Fatalf(
				"missing executor error=%v; want typed pre-send physical-context failure",
				err,
			)
		}
	})

	t.Run("legacy_scope_without_approved_policy_keeps_direct_adapter_contract", func(t *testing.T) {
		visionCalls := 0
		recognizer := NewRecognizerAdapter(
			func(context.Context, []byte, string) (string, error) {
				visionCalls++
				return dd036CrossLayerQuestionJSON, nil
			},
		)
		questions, err := recognizer.Recognize(
			context.Background(),
			[]byte("legacy-direct-image"),
		)
		if err != nil {
			t.Fatalf("legacy direct adapter recognition: %v", err)
		}
		if visionCalls != 1 || len(questions) != 1 {
			t.Fatalf(
				"legacy direct adapter calls=%d questions=%d want 1/1",
				visionCalls,
				len(questions),
			)
		}
	})
}
