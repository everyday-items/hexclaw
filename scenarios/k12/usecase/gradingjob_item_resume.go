package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

var (
	ErrGradingItemInvocationFailed     = errors.New("grading item invocation failed")
	ErrGradingAssessmentExactSet       = errors.New("grading assessment exact set is incomplete")
	ErrGradingSourceRecognitionPending = errors.New(
		"grading source recognition result is pending",
	)
)

// DefinitiveProviderResponse is the adapter-boundary error contract for an
// external call that is known to have reached a provider and received an HTTP
// response. Transport failures deliberately do not implement this interface:
// after MarkSent their execution outcome is ambiguous and must not be resent.
type DefinitiveProviderResponse interface {
	error
	ProviderResponseStatusCode() int
}

// runAssessItems is the ADR-K12-021/024 assessing cutover. Frozen policies use
// their measured concurrency; typed legacy jobs retain the historical
// concurrency of two without manufacturing a frozen budget. The stage-level
// ModelInvocation remains the aggregate accounting/reconciliation anchor; every
// external solve/grade operation additionally gets its own durable ledger entry,
// while each final item and its local projection are committed atomically. Only
// the historical Assessment/Mistake/Outbox projection path is excluded from this
// cutover -- the aggregate invocation is deliberately complementary, not a
// competing write.
func (o *GradingOrchestrator) runAssessItems(
	ctx context.Context,
	run *gradingRun,
	job GradingJobView,
) (GradingJobView, error) {
	pendingSourceRecognition, err := o.deps.Records.HasPendingCurrentProblemSourceRecognition(
		ctx,
		job.Record.AgentName,
		job.Record.RecordID,
	)
	if err != nil {
		return job, err
	}
	if pendingSourceRecognition {
		// The process-local runtime still carries the pre-command OCR text until
		// the source worker commits V73 and refreshes it under the same Job lock.
		// Park at assessing before even preparing the aggregate invocation: a
		// failed receipt CAS after provider sends is too late and causes duplicate
		// physical calls on the new immutable revision.
		return job, ErrGradingSourceRecognitionPending
	}
	if err := job.Fields.BudgetSnapshot.Validate(); err != nil {
		v, advanceErr := o.deps.AdvanceGradingStage(ctx, run.agentName, job.Record.RecordID, AdvanceGradingInput{
			Outcome: GradingOutcomeFailed, FailureKind: "invalid_frozen_budget", Retryable: false,
		})
		if advanceErr != nil {
			return v, advanceErr
		}
		return v, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}

	stageInvocation, err := o.beginFrozenAssessInvocation(ctx, run, job)
	if err != nil {
		if stageInvocation.InvocationID == "" {
			return o.failModelInvocationBeforeSend(ctx, run, job.Record.RecordID, err)
		}
		if errors.Is(err, ErrModelInvocationRequiresReconciliation) {
			v, advanceErr := o.markGradingOutcomeUnknown(context.WithoutCancel(ctx), run,
				job.Record.RecordID, "invocation_reconciliation_required")
			if advanceErr != nil {
				return v, advanceErr
			}
			return v, err
		}
		return o.failStage(ctx, run, job.Record.RecordID, "assessment_invocation_prepare_failed", err)
	}
	if stageInvocation.Status == k12.ModelInvocationFailed {
		failureKind := strings.TrimSpace(stageInvocation.FailureKind)
		if failureKind == "" {
			return o.markFrozenAssessLedgerUnknown(ctx, run, job.Record.RecordID,
				fmt.Errorf("%w: invocation=%s terminal failure has no failure kind",
					ErrModelInvocationRequiresReconciliation, stageInvocation.InvocationID))
		}
		v, advanceErr := o.deps.AdvanceGradingStage(ctx, run.agentName, job.Record.RecordID, AdvanceGradingInput{
			Outcome: GradingOutcomeFailed, FailureKind: failureKind,
			Retryable: failureKind != "assess_item_invalid",
		})
		if advanceErr != nil {
			return v, advanceErr
		}
		return v, fmt.Errorf("%w: recovered terminal aggregate invocation=%s failure=%s",
			ErrGradingItemInvocationFailed, stageInvocation.InvocationID, failureKind)
	}
	if run.result != nil && (stageInvocation.Status == k12.ModelInvocationSent ||
		stageInvocation.Status == k12.ModelInvocationSucceeded) {
		receipts, listErr := o.deps.Records.ListGradingAssessmentItems(ctx, run.agentName, job.Record.RecordID)
		if listErr == nil {
			listErr = validateGradingAssessmentExactSet(*run.result, receipts)
		}
		if listErr == nil {
			if stageInvocation.Status == k12.ModelInvocationSent {
				if persistErr := o.persistRun(job.Record.RecordID, run); persistErr != nil {
					if ledgerErr := o.markFrozenAssessInvocationFailed(context.WithoutCancel(ctx),
						stageInvocation, "result_not_durable"); ledgerErr != nil {
						return o.markFrozenAssessLedgerUnknown(ctx, run, job.Record.RecordID,
							errors.Join(persistErr, ledgerErr))
					}
					return o.failStage(ctx, run, job.Record.RecordID, "result_not_durable", persistErr)
				}
			}
			if ledgerErr := o.markFrozenAssessInvocationSucceeded(context.WithoutCancel(ctx), stageInvocation,
				modelInvocationResultDigest(*run.result)); ledgerErr != nil {
				return o.markFrozenAssessLedgerUnknown(ctx, run, job.Record.RecordID, ledgerErr)
			}
			return o.advanceOK(ctx, run, job.Record.RecordID,
				fmt.Sprintf("items:%d mode:%s", len(run.result.Items), run.result.Mode))
		}
		return o.stopFrozenAssessReceiptReconciliation(ctx, run, job.Record.RecordID,
			stageInvocation, listErr)
	}
	if stageInvocation.Status == k12.ModelInvocationSucceeded {
		receipts, listErr := o.deps.Records.ListGradingAssessmentItems(ctx, run.agentName, job.Record.RecordID)
		if listErr == nil {
			listErr = validateFrozenAssessReceiptSet(run, receipts)
		}
		if listErr != nil {
			return o.stopFrozenAssessReceiptReconciliation(ctx, run, job.Record.RecordID,
				stageInvocation, listErr)
		}
	}

	assessDeps := o.deps
	assessDeps.Recognizer = presetRecognizer{questions: run.questions}
	switch {
	case run.anchored != nil:
		assessDeps.AnswerAnchorer = presetAnchorer{questions: run.anchored}
	case run.anchorFailed:
		assessDeps.AnswerAnchorer = presetAnchorer{err: errors.New("锚点定位在 locating 阶段已失败（检查点回放）")}
	default:
		assessDeps.AnswerAnchorer = nil
	}
	var recorder *recordingAnnotator
	if run.textOnly {
		assessDeps.PhotoAnnotator = nil
	} else if o.deps.PhotoAnnotator != nil {
		recorder = &recordingAnnotator{inner: o.deps.PhotoAnnotator}
		assessDeps.PhotoAnnotator = recorder
	}

	providerCtx, cancelProvider := o.durableGradingStageContext(ctx, job.Fields.Deadline)
	unregisterProvider := o.registerGradingModelCall(job.Record.RecordID, cancelProvider)
	if current, err := o.deps.GetGradingJob(context.WithoutCancel(ctx), run.agentName, job.Record.RecordID); err != nil {
		cancelProvider()
		unregisterProvider()
		return GradingJobView{}, err
	} else if current.Record.Status == k12.GradingStageCancelled {
		cancelProvider()
		unregisterProvider()
		if err := o.markFrozenAssessInvocationFailed(context.WithoutCancel(ctx), stageInvocation,
			"cancelled_before_item_calls"); err != nil {
			return current, err
		}
		return current, nil
	}

	itemConcurrency := job.Fields.BudgetSnapshot.ItemConcurrency
	if itemConcurrency == 0 {
		itemConcurrency = 2
	}
	result, assessErr := assessDeps.gradeFrozenHomeworkPhotoWithAssessor(
		providerCtx,
		run.req,
		itemConcurrency,
		func(itemCtx context.Context, req PhotoGradeRequest, mode PhotoMode, q RecognizedQuestion) (PhotoGradeItem, error) {
			return o.assessDurablePhotoItem(itemCtx, assessDeps, job, req, mode, q)
		},
	)
	cancelProvider()
	unregisterProvider()

	if assessErr != nil {
		if errors.Is(assessErr, ErrGradingPhysicalCallOutcomeUnknown) ||
			errors.Is(assessErr, ErrModelInvocationRequiresReconciliation) {
			if ledgerErr := o.markFrozenAssessInvocationOutcomeUnknown(context.WithoutCancel(ctx),
				stageInvocation, "item_invocation_outcome_unknown"); ledgerErr != nil {
				return o.markFrozenAssessLedgerUnknown(ctx, run, job.Record.RecordID,
					errors.Join(assessErr, ledgerErr))
			}
			if current, err := o.deps.GetGradingJob(context.WithoutCancel(ctx), run.agentName, job.Record.RecordID); err == nil && current.Record.Status == k12.GradingStageCancelled {
				return current, nil
			}
			v, err := o.markGradingOutcomeUnknown(context.WithoutCancel(ctx), run, job.Record.RecordID, "item_invocation_outcome_unknown")
			if err != nil {
				return v, err
			}
			return v, assessErr
		}
		failureKind := "assess_item_failed"
		if !gradingErrRetryable(assessErr) {
			failureKind = "assess_item_invalid"
		}
		if ledgerErr := o.markFrozenAssessInvocationFailed(context.WithoutCancel(ctx), stageInvocation,
			failureKind); ledgerErr != nil {
			return o.markFrozenAssessLedgerUnknown(ctx, run, job.Record.RecordID,
				errors.Join(assessErr, ledgerErr))
		}
		v, err := o.deps.AdvanceGradingStage(ctx, run.agentName, job.Record.RecordID, AdvanceGradingInput{
			Outcome: GradingOutcomeFailed, FailureKind: failureKind, Retryable: gradingErrRetryable(assessErr),
		})
		if err != nil {
			return v, err
		}
		return v, assessErr
	}

	receipts, err := o.deps.Records.ListGradingAssessmentItems(ctx, run.agentName, job.Record.RecordID)
	if err == nil {
		err = validateGradingAssessmentExactSet(result, receipts)
	}
	if err != nil {
		if ledgerErr := o.markFrozenAssessInvocationFailed(context.WithoutCancel(ctx), stageInvocation,
			"assessment_exact_set_incomplete"); ledgerErr != nil {
			return o.markFrozenAssessLedgerUnknown(ctx, run, job.Record.RecordID, errors.Join(err, ledgerErr))
		}
		return o.failStage(ctx, run, job.Record.RecordID, "assessment_exact_set_incomplete", err)
	}

	run.result = &result
	run.renderFailure = ""
	if recorder != nil {
		run.renderFailure = recorder.failure
	}
	if err := o.persistRun(job.Record.RecordID, run); err != nil {
		// All model outputs and local projections are already durable. A retry only
		// rebuilds the page artifact from receipts and is therefore safe.
		if ledgerErr := o.markFrozenAssessInvocationFailed(context.WithoutCancel(ctx), stageInvocation,
			"result_not_durable"); ledgerErr != nil {
			return o.markFrozenAssessLedgerUnknown(ctx, run, job.Record.RecordID, errors.Join(err, ledgerErr))
		}
		return o.failStage(ctx, run, job.Record.RecordID, "result_not_durable", err)
	}
	if err := o.markFrozenAssessInvocationSucceeded(context.WithoutCancel(ctx), stageInvocation,
		modelInvocationResultDigest(result)); err != nil {
		return o.markFrozenAssessLedgerUnknown(ctx, run, job.Record.RecordID, err)
	}
	return o.advanceOK(ctx, run, job.Record.RecordID,
		fmt.Sprintf("items:%d mode:%s", len(result.Items), result.Mode))
}

func (o *GradingOrchestrator) beginFrozenAssessInvocation(
	ctx context.Context,
	run *gradingRun,
	job GradingJobView,
) (k12.ModelInvocation, error) {
	requestRaw, err := json.Marshal(struct {
		Request   PhotoGradeRequest    `json:"request"`
		Questions []RecognizedQuestion `json:"questions"`
		Anchored  []RecognizedQuestion `json:"anchored,omitempty"`
	}{run.req, run.questions, run.anchored})
	if err != nil {
		return k12.ModelInvocation{}, err
	}
	invocation, _, err := o.deps.Records.PrepareModelInvocation(ctx, k12.ModelInvocation{
		InvocationID: "modelinv-" + idgen.ShortID(), AgentName: job.Record.AgentName,
		JobID: job.Record.RecordID, Stage: k12.GradingStageAssessing,
		RequestDigest: modelInvocationDigest([]byte(k12.GradingStageAssessing), requestRaw),
		RouteSnapshot: job.Fields.ModelSnapshot, Attempt: job.Fields.AttemptCount + 1,
		CreatedAt: o.deps.now(), UpdatedAt: o.deps.now(),
	})
	if err != nil {
		return k12.ModelInvocation{}, err
	}
	switch invocation.Status {
	case k12.ModelInvocationPrepared:
		return o.deps.Records.MarkModelInvocationSent(ctx, invocation.AgentName, invocation.InvocationID, "")
	case k12.ModelInvocationSent, k12.ModelInvocationSucceeded, k12.ModelInvocationFailed:
		// The aggregate status never authorizes a provider resend. Per-item
		// receipts/invocations below are the sole execution authority on resume.
		return invocation, nil
	case k12.ModelInvocationOutcomeUnknown, k12.ModelInvocationReconciled:
		return invocation, fmt.Errorf("%w: invocation=%s status=%s",
			ErrModelInvocationRequiresReconciliation, invocation.InvocationID, invocation.Status)
	default:
		return invocation, fmt.Errorf("%w: invocation=%s unexpected status=%s",
			ErrModelInvocationRequiresReconciliation, invocation.InvocationID, invocation.Status)
	}
}

func (o *GradingOrchestrator) markFrozenAssessInvocationSucceeded(
	ctx context.Context,
	invocation k12.ModelInvocation,
	resultDigest string,
) error {
	if invocation.Status == k12.ModelInvocationSucceeded {
		if invocation.ResultDigest != resultDigest {
			return fmt.Errorf("%w: invocation=%s aggregate result digest mismatch",
				ErrModelInvocationRequiresReconciliation, invocation.InvocationID)
		}
		return nil
	}
	if invocation.Status != k12.ModelInvocationSent {
		return fmt.Errorf("%w: invocation=%s status=%s cannot succeed",
			ErrModelInvocationRequiresReconciliation, invocation.InvocationID, invocation.Status)
	}
	stored, err := o.deps.Records.MarkModelInvocationSucceeded(ctx, invocation.AgentName,
		invocation.InvocationID, resultDigest, "")
	if err != nil {
		return err
	}
	if stored.ResultDigest != resultDigest {
		return fmt.Errorf("%w: invocation=%s aggregate result digest mismatch",
			ErrModelInvocationRequiresReconciliation, invocation.InvocationID)
	}
	return nil
}

func (o *GradingOrchestrator) markFrozenAssessInvocationFailed(
	ctx context.Context,
	invocation k12.ModelInvocation,
	failureKind string,
) error {
	stored, err := o.deps.Records.MarkModelInvocationFailed(ctx, invocation.AgentName,
		invocation.InvocationID, failureKind)
	if err != nil {
		return err
	}
	if stored.FailureKind != failureKind {
		return fmt.Errorf("%w: invocation=%s failure kind=%s, want %s",
			ErrModelInvocationRequiresReconciliation, invocation.InvocationID, stored.FailureKind, failureKind)
	}
	return nil
}

func (o *GradingOrchestrator) markFrozenAssessInvocationOutcomeUnknown(
	ctx context.Context,
	invocation k12.ModelInvocation,
	failureKind string,
) error {
	stored, err := o.deps.Records.MarkModelInvocationOutcomeUnknown(ctx, invocation.AgentName,
		invocation.InvocationID, failureKind)
	if err != nil {
		return err
	}
	if stored.FailureKind != failureKind {
		return fmt.Errorf("%w: invocation=%s failure kind=%s, want %s",
			ErrModelInvocationRequiresReconciliation, invocation.InvocationID, stored.FailureKind, failureKind)
	}
	return nil
}

func (o *GradingOrchestrator) markFrozenAssessLedgerUnknown(
	ctx context.Context,
	run *gradingRun,
	jobID string,
	cause error,
) (GradingJobView, error) {
	v, err := o.markGradingOutcomeUnknown(context.WithoutCancel(ctx), run, jobID,
		"invocation_ledger_write_failed")
	if err != nil {
		return v, err
	}
	return v, cause
}

func (o *GradingOrchestrator) stopFrozenAssessReceiptReconciliation(
	ctx context.Context,
	run *gradingRun,
	jobID string,
	invocation k12.ModelInvocation,
	cause error,
) (GradingJobView, error) {
	reconcileErr := fmt.Errorf("%w: invocation=%s aggregate receipt mismatch: %v",
		ErrModelInvocationRequiresReconciliation, invocation.InvocationID, cause)
	if invocation.Status == k12.ModelInvocationSent {
		if err := o.markFrozenAssessInvocationOutcomeUnknown(context.WithoutCancel(ctx), invocation,
			"assessment_receipt_reconciliation_required"); err != nil {
			reconcileErr = errors.Join(reconcileErr, err)
		}
	}
	v, err := o.markGradingOutcomeUnknown(context.WithoutCancel(ctx), run, jobID,
		"assessment_receipt_reconciliation_required")
	if err != nil {
		return v, err
	}
	return v, reconcileErr
}

func (o *GradingOrchestrator) assessDurablePhotoItem(
	ctx context.Context,
	deps Deps,
	job GradingJobView,
	req PhotoGradeRequest,
	mode PhotoMode,
	q RecognizedQuestion,
) (PhotoGradeItem, error) {
	item := PhotoGradeItem{Recognized: q}
	if strings.TrimSpace(q.ProblemID) == "" || strings.TrimSpace(q.AttemptID) == "" ||
		q.ConfirmedVersion < 1 || strings.TrimSpace(q.InputDigest) == "" {
		return item, fmt.Errorf("%w: durable assessment problem/attempt identity is incomplete", ErrInvalidInput)
	}
	skipped, err := currentProblemSkipMatchesInput(ctx, deps, job, q)
	if err != nil {
		return item, err
	}
	if skipped {
		return item, nil
	}
	if receipt, err := deps.Records.GetGradingAssessmentItem(ctx, req.AgentName, job.Record.RecordID, q.ProblemID); err == nil {
		return replayGradingAssessmentItem(q, receipt)
	} else if !errors.Is(err, records.ErrNotFound) {
		return item, err
	}

	gradeReq := GradeRequest{
		AgentName: req.AgentName, Subject: firstNonEmpty(q.Subject, req.Subject), Grade: req.Grade,
		SourceSession: req.SourceSession, Problem: q.Question, StudentAnswer: q.StudentAnswer,
		KnowledgePoints: photoGradeKnowledgePoints(q),
	}
	if mode == PhotoModeGrade {
		switch q.AnswerState {
		case AnswerStateBlank:
			item.Status = PhotoUnanswered
			return commitGradingAssessmentItem(ctx, deps, job, q, item, "", "", "",
				k12storage.GradingAssessmentEffects{})
		case AnswerStateUnclear:
			item.Status = PhotoAnswerUnclear
			item.Warning = "检测到学生笔迹，但未能可靠读出；请家长补录后再批改"
			return commitGradingAssessmentItem(ctx, deps, job, q, item, "", "", "",
				k12storage.GradingAssessmentEffects{})
		}
	}

	if err := validateGradeInput(gradeReq.Grade); err != nil {
		return item, err
	}
	subject, err := normalizeSubject(gradeReq.Subject)
	if err != nil {
		return item, err
	}
	gradeReq.Subject = subject
	if outOfScope, kp, unmapped := deps.outOfScope(ctx, gradeReq); outOfScope {
		solved := SolveHomeworkResult{
			OutOfScope: true, OutOfScopeKP: kp, CurriculumUnmapped: unmapped,
			Evidence: SolveEvidence{Verdict: VerdictOutOfScope, EvidenceType: EvidenceNone},
		}
		item.Status = PhotoOutOfScope
		if mode == PhotoModeSolve {
			item.Solve = solved
		} else {
			item.Grade = GradeResult{
				OutOfScope: true, OutOfScopeKP: kp, CurriculumUnmapped: unmapped, Evidence: solved.Evidence,
			}
		}
		return commitGradingAssessmentItem(ctx, deps, job, q, item, "", "", "",
			k12storage.GradingAssessmentEffects{})
	}

	if mode == PhotoModeSolve {
		solved, solveInvocationID, err := executeDurableSolveOperation(
			ctx, o, deps, job, q, gradeReq,
		)
		item.Solve = solved
		if err != nil {
			return item, err
		}
		skipped, err := currentProblemSkipMatchesInput(ctx, deps, job, q)
		if err != nil {
			return item, err
		}
		if skipped {
			return item, nil
		}
		durableCtx := context.WithoutCancel(ctx)
		if solved.OutOfScope {
			item.Status = PhotoOutOfScope
			return commitGradingAssessmentItem(durableCtx, deps, job, q, item,
				solveInvocationID, "", "", k12storage.GradingAssessmentEffects{})
		}
		guideRequest := parentTeachingGuideRequest(gradeReq, solved, GradeOutcome{})
		rawGuide, parentGuideInvocationID, err := executeGradingItemOperation(ctx, o, job, q,
			k12.GradingItemOperationParentGuide,
			struct {
				InputDigest string                     `json:"input_digest"`
				Request     ParentTeachingGuideRequest `json:"request"`
			}{q.InputDigest, guideRequest},
			func(callCtx context.Context) (ParentTeachingGuide, error) {
				return deps.generateParentTeachingGuide(callCtx, guideRequest)
			})
		if err != nil {
			return item, err
		}
		guide, err := finalizeParentTeachingGuide(rawGuide, solved.Solution)
		if err != nil {
			return item, err
		}
		item.ParentGuide = &guide
		item.Status = PhotoBlankSolved
		if !photoEvidenceTrusted(solved.Evidence) {
			item.Warning = "答案未通过程序级验算，请家长核对"
		}
		return commitGradingAssessmentItem(durableCtx, deps, job, q, item,
			solveInvocationID, "", parentGuideInvocationID, k12storage.GradingAssessmentEffects{})
	}

	solved, solveInvocationID, err := executeDurableSolveOperation(
		ctx, o, deps, job, q, gradeReq,
	)
	item.Solve = solved
	if err != nil {
		return item, err
	}
	// The solve response and invocation success are already durable. A deadline
	// racing with that commit must not discard the result during local receipt
	// reconciliation.
	durableCtx := context.WithoutCancel(ctx)
	skipped, err = currentProblemSkipMatchesInput(durableCtx, deps, job, q)
	if err != nil {
		return item, err
	}
	if skipped {
		return item, nil
	}
	// Once the provider has returned a complete response and the invocation
	// ledger is durably succeeded, finish the local receipt transaction even if
	// the stage deadline fired at the same instant. Retrying instead would turn a
	// known result into an artificial ambiguous state.
	if solved.OutOfScope {
		item.Status = PhotoOutOfScope
		if mode == PhotoModeGrade {
			item.Solve = SolveHomeworkResult{}
			item.Grade = GradeResult{
				OutOfScope: solved.OutOfScope, OutOfScopeKP: solved.OutOfScopeKP,
				CurriculumUnmapped: solved.CurriculumUnmapped, Evidence: solved.Evidence,
			}
		}
		return commitGradingAssessmentItem(durableCtx, deps, job, q, item,
			solveInvocationID, "", "", k12storage.GradingAssessmentEffects{})
	}
	graded, gradeInvocationID, err := executeDurableGradeOperation(
		ctx, o, deps, job, q, gradeReq, solved,
	)
	item.Grade = graded
	item.Solve = SolveHomeworkResult{}
	if err != nil {
		return item, err
	}
	skipped, err = currentProblemSkipMatchesInput(durableCtx, deps, job, q)
	if err != nil {
		return item, err
	}
	if skipped {
		return item, nil
	}
	switch {
	case graded.OutOfScope:
		item.Status = PhotoOutOfScope
	case !photoEvidenceTrusted(graded.Evidence):
		item.Status, item.Warning = PhotoUntrusted, "验算证据不足，暂不在图片上判对错"
	case graded.Outcome.Verdict == VerdictAgree:
		item.Status = PhotoCorrect
	case graded.Outcome.Verdict == VerdictDisagree:
		item.Status = PhotoWrong
	default:
		item.Status, item.Warning = PhotoUntrusted, "批改判定无二元结论，暂不在图片上判对错"
	}
	effects := k12storage.GradingAssessmentEffects{}
	if job.Fields.SourceKind != PracticeReturnGradingSourceKind {
		var effectsErr error
		effects, effectsErr = deps.gradingAssessmentEffects(durableCtx, gradeReq, graded)
		if effectsErr != nil {
			return item, effectsErr
		}
	}
	parentGuideInvocationID := ""
	if item.Status == PhotoWrong {
		guideRequest := parentTeachingGuideRequest(gradeReq, solved, graded.Outcome)
		rawGuide, invocationID, guideErr := executeGradingItemOperation(ctx, o, job, q,
			k12.GradingItemOperationParentGuide,
			struct {
				InputDigest string                     `json:"input_digest"`
				Request     ParentTeachingGuideRequest `json:"request"`
			}{q.InputDigest, guideRequest},
			func(callCtx context.Context) (ParentTeachingGuide, error) {
				return deps.generateParentTeachingGuide(callCtx, guideRequest)
			})
		if guideErr != nil {
			return item, guideErr
		}
		guide, guideErr := finalizeParentTeachingGuide(rawGuide, solved.Solution)
		if guideErr != nil {
			return item, guideErr
		}
		item.ParentGuide = &guide
		parentGuideInvocationID = invocationID
	}
	return commitGradingAssessmentItem(durableCtx, deps, job, q, item,
		solveInvocationID, gradeInvocationID, parentGuideInvocationID, effects)
}

func currentProblemSkipMatchesInput(
	ctx context.Context,
	deps Deps,
	job GradingJobView,
	q RecognizedQuestion,
) (bool, error) {
	revision, err := deps.Records.GetCurrentProblemSkipRevision(
		ctx, job.Record.AgentName, job.Record.RecordID, q.ProblemID,
	)
	if errors.Is(err, records.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch {
	case revision == q.ConfirmedVersion:
		return true, nil
	case revision < q.ConfirmedVersion:
		return false, nil
	default:
		return false, fmt.Errorf(
			"%w: current skip revision %d is newer than problem revision %d",
			k12storage.ErrGradingAssessmentItemConflict,
			revision,
			q.ConfirmedVersion,
		)
	}
}

func executeGradingItemOperation[T any](
	ctx context.Context,
	o *GradingOrchestrator,
	job GradingJobView,
	q RecognizedQuestion,
	operation k12.GradingItemOperation,
	request any,
	call func(context.Context) (T, error),
) (T, string, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, "", err
	}
	requestDigest := modelInvocationResultDigest(request)
	jobOperationAttempt := job.Fields.AttemptCount + 1
	currentAttempt := jobOperationAttempt
	invocations, err := o.deps.Records.ListGradingItemInvocations(ctx, qAgent(job, q), job.Record.RecordID)
	if err != nil {
		return zero, "", err
	}
	var latest *k12.GradingItemInvocation
	maxOperationAttempt := 0
	for i := range invocations {
		candidate := &invocations[i]
		if candidate.ProblemID != q.ProblemID || candidate.Operation != operation {
			continue
		}
		if candidate.OperationAttempt > maxOperationAttempt {
			maxOperationAttempt = candidate.OperationAttempt
		}
		if candidate.RequestDigest != requestDigest {
			continue
		}
		// The same request digest must never be rebound to a different item or
		// route. A different digest, however, is a legitimate immutable input
		// revision and receives a new operation attempt below.
		if err := validateGradingItemInvocationIdentity(*candidate, job, q, requestDigest); err != nil {
			return zero, candidate.InvocationID, err
		}
		if latest == nil || candidate.OperationAttempt > latest.OperationAttempt {
			latest = candidate
		}
	}
	if next := maxOperationAttempt + 1; next > currentAttempt {
		currentAttempt = next
	}
	if latest != nil {
		switch latest.Status {
		case k12.ModelInvocationSucceeded:
			if latest.ResultDigest != modelInvocationDigest([]byte(latest.ResultJSON)) {
				return zero, latest.InvocationID, fmt.Errorf("%w: invocation=%s result digest mismatch",
					ErrModelInvocationRequiresReconciliation, latest.InvocationID)
			}
			var result T
			if err := json.Unmarshal([]byte(latest.ResultJSON), &result); err != nil {
				return zero, latest.InvocationID, fmt.Errorf("%w: invocation=%s result decode: %v",
					ErrModelInvocationRequiresReconciliation, latest.InvocationID, err)
			}
			return result, latest.InvocationID, nil
		case k12.ModelInvocationPrepared:
			// The durable before-send point exists, but no external request can have
			// escaped yet. Sending this exact frozen request is safe.
		case k12.ModelInvocationFailed:
			if jobOperationAttempt <= latest.OperationAttempt {
				return zero, latest.InvocationID, fmt.Errorf("%w: invocation=%s class=%s code=%s",
					ErrGradingItemInvocationFailed, latest.InvocationID, latest.FailureClass, latest.FailureCode)
			}
			latest = nil
		case k12.ModelInvocationSent, k12.ModelInvocationOutcomeUnknown, k12.ModelInvocationReconciled:
			return zero, latest.InvocationID, fmt.Errorf("%w: invocation=%s status=%s",
				ErrModelInvocationRequiresReconciliation, latest.InvocationID, latest.Status)
		default:
			return zero, latest.InvocationID, fmt.Errorf("%w: invocation=%s unexpected status=%s",
				ErrModelInvocationRequiresReconciliation, latest.InvocationID, latest.Status)
		}
	}
	if problemSourceReconciliationOnly(ctx) {
		return zero, "", fmt.Errorf(
			"%w: reconciliation-only processing cannot create or send grading operation %s",
			ErrModelInvocationRequiresReconciliation,
			operation,
		)
	}

	var invocation k12.GradingItemInvocation
	if latest != nil {
		invocation = *latest
	} else {
		invocation, _, err = o.deps.Records.PrepareGradingItemInvocation(ctx, k12.GradingItemInvocation{
			InvocationID: "gradingitem-" + idgen.ShortID(), AgentName: job.Record.AgentName,
			JobID: job.Record.RecordID, ProblemID: q.ProblemID, AttemptID: q.AttemptID,
			Operation: operation, OperationAttempt: currentAttempt, RequestDigest: requestDigest,
			RouteSnapshot: job.Fields.ModelSnapshot, CreatedAt: o.deps.now(), UpdatedAt: o.deps.now(),
		})
		if err != nil {
			return zero, "", err
		}
		if err := validateGradingItemInvocationIdentity(invocation, job, q, requestDigest); err != nil {
			return zero, invocation.InvocationID, err
		}
		if invocation.Status != k12.ModelInvocationPrepared {
			return zero, invocation.InvocationID, fmt.Errorf("%w: invocation=%s status=%s",
				ErrModelInvocationRequiresReconciliation, invocation.InvocationID, invocation.Status)
		}
	}
	claimCtx, cancelClaim := gradingDurableCommitContext(ctx)
	invocation, claimed, err := o.deps.Records.ClaimGradingItemInvocationSent(
		claimCtx, job.Record.AgentName, invocation.InvocationID,
	)
	cancelClaim()
	if err != nil {
		return zero, invocation.InvocationID, err
	}
	if !claimed {
		return zero, invocation.InvocationID, fmt.Errorf(
			"%w: invocation=%s concurrently claimed with status=%s",
			ErrModelInvocationRequiresReconciliation, invocation.InvocationID, invocation.Status,
		)
	}
	callCtx, cancelCall := gradingIndependentCallContext(ctx, job.Fields.ModelSnapshot.TimeoutMS)
	result, callErr := call(callCtx)
	callCtxErr := callCtx.Err()
	cancelCall()
	if callErr != nil {
		ambiguousTransport := !definitiveProviderResponse(callErr)
		if invocationOutcomeUnknown(callErr) || invocationOutcomeUnknown(callCtxErr) || ambiguousTransport {
			commitCtx, cancelCommit := gradingDurableCommitContext(ctx)
			_, ledgerErr := o.deps.Records.MarkGradingItemInvocationOutcomeUnknown(
				commitCtx, job.Record.AgentName, invocation.InvocationID, "provider_transport", "outcome_unknown")
			cancelCommit()
			if ledgerErr != nil {
				return zero, invocation.InvocationID, errors.Join(callErr, ErrModelInvocationRequiresReconciliation, ledgerErr)
			}
			if ambiguousTransport && !invocationOutcomeUnknown(callErr) && !invocationOutcomeUnknown(callCtxErr) {
				return zero, invocation.InvocationID, errors.Join(
					callErr, ErrGradingPhysicalCallOutcomeUnknown, ErrModelInvocationRequiresReconciliation,
				)
			}
			return zero, invocation.InvocationID, errors.Join(callErr, ErrGradingPhysicalCallOutcomeUnknown)
		}
		statusCode, _ := definitiveProviderResponseStatus(callErr)
		failureCode := fmt.Sprintf("http_%d", statusCode)
		commitCtx, cancelCommit := gradingDurableCommitContext(ctx)
		_, ledgerErr := o.deps.Records.MarkGradingItemInvocationFailed(
			commitCtx, job.Record.AgentName, invocation.InvocationID, "provider_response", failureCode)
		cancelCommit()
		if ledgerErr != nil {
			return zero, invocation.InvocationID, errors.Join(callErr, ledgerErr)
		}
		return zero, invocation.InvocationID, callErr
	}
	// A complete, serializable response wins a simultaneous context deadline.
	// Persisting it below avoids a false ambiguous state after the upstream has
	// already supplied the full operation result.
	raw, err := json.Marshal(result)
	if err != nil {
		commitCtx, cancelCommit := gradingDurableCommitContext(ctx)
		_, _ = o.deps.Records.MarkGradingItemInvocationOutcomeUnknown(
			commitCtx, job.Record.AgentName, invocation.InvocationID, "local", "result_encode_failed")
		cancelCommit()
		return zero, invocation.InvocationID, fmt.Errorf("%w: result encode: %v", ErrModelInvocationRequiresReconciliation, err)
	}
	commitCtx, cancelCommit := gradingDurableCommitContext(ctx)
	if _, err := o.deps.Records.MarkGradingItemInvocationSucceeded(commitCtx,
		job.Record.AgentName, invocation.InvocationID, modelInvocationDigest(raw), string(raw)); err != nil {
		cancelCommit()
		unknownCtx, cancelUnknown := gradingDurableCommitContext(ctx)
		_, unknownErr := o.deps.Records.MarkGradingItemInvocationOutcomeUnknown(
			unknownCtx, job.Record.AgentName, invocation.InvocationID, "local", "result_not_durable")
		cancelUnknown()
		return zero, invocation.InvocationID, errors.Join(ErrModelInvocationRequiresReconciliation, err, unknownErr)
	}
	cancelCommit()
	return result, invocation.InvocationID, nil
}

// definitiveProviderResponse is deliberately narrow. After MarkSent, retry is
// safe only when a typed provider response proves that the upstream completed
// the request with an HTTP status. EOF/reset/generic adapter errors are
// ambiguous and must stop in outcome_unknown until reconciled.
func definitiveProviderResponse(err error) bool {
	_, ok := definitiveProviderResponseStatus(err)
	return ok

}

// sentProviderOutcomeUnknown applies only after the durable invocation ledger
// has crossed MarkSent. At that point EOF/reset/generic adapter errors cannot
// prove whether the upstream executed the request. Only a typed provider
// response makes the failure definitive enough for an ordinary retry policy.
func sentProviderOutcomeUnknown(callErr, ctxErr error) bool {
	if definitiveProviderResponse(callErr) {
		// A verifiable upstream response remains definitive even when the local
		// deadline/cancellation becomes observable at the same boundary.
		return false
	}
	if invocationOutcomeUnknown(callErr) || invocationOutcomeUnknown(ctxErr) {
		return true
	}
	return callErr != nil && !definitiveProviderResponse(callErr)
}

func definitiveProviderResponseStatus(err error) (int, bool) {
	var providerErr DefinitiveProviderResponse
	if !errors.As(err, &providerErr) || providerErr == nil {
		return 0, false
	}
	statusCode := providerErr.ProviderResponseStatusCode()
	return statusCode, statusCode > 0
}

func qAgent(job GradingJobView, _ RecognizedQuestion) string {
	return job.Record.AgentName
}

func validateGradingItemInvocationIdentity(
	invocation k12.GradingItemInvocation,
	job GradingJobView,
	q RecognizedQuestion,
	requestDigest string,
) error {
	wantRoute := k12.NormalizeGradingModelSnapshot(job.Fields.ModelSnapshot)
	gotRoute := k12.NormalizeGradingModelSnapshot(invocation.RouteSnapshot)
	if invocation.AgentName != job.Record.AgentName || invocation.JobID != job.Record.RecordID ||
		invocation.ProblemID != q.ProblemID || invocation.AttemptID != q.AttemptID ||
		invocation.RequestDigest != requestDigest || gotRoute.Provider != wantRoute.Provider ||
		gotRoute.Model != wantRoute.Model || gotRoute.Route != wantRoute.Route {
		return fmt.Errorf("%w: invocation=%s immutable identity drift", ErrModelInvocationRequiresReconciliation, invocation.InvocationID)
	}
	return nil
}

func commitGradingAssessmentItem(
	ctx context.Context,
	deps Deps,
	job GradingJobView,
	q RecognizedQuestion,
	item PhotoGradeItem,
	solveInvocationID string,
	gradeInvocationID string,
	parentGuideInvocationID string,
	effects k12storage.GradingAssessmentEffects,
) (PhotoGradeItem, error) {
	if (item.ParentGuide != nil) != (strings.TrimSpace(parentGuideInvocationID) != "") {
		return item, fmt.Errorf(
			"%w: parent guide result/invocation reference mismatch",
			ErrGradingAssessmentExactSet,
		)
	}
	if parentGuideInvocationID != "" &&
		item.Status != PhotoWrong &&
		item.Status != PhotoBlankSolved {
		return item, fmt.Errorf(
			"%w: parent guide cannot attach to status %s",
			ErrGradingAssessmentExactSet,
			item.Status,
		)
	}
	status, err := gradingAssessmentStatus(item.Status)
	if err != nil {
		return item, err
	}
	raw, err := json.Marshal(gradingAssessmentCanonicalResult(item))
	if err != nil {
		return item, err
	}
	receipt := k12.GradingAssessmentItem{
		AgentName: job.Record.AgentName, JobID: job.Record.RecordID,
		ProblemID: q.ProblemID, AttemptID: q.AttemptID,
		ConfirmedVersion: q.ConfirmedVersion, InputDigest: q.InputDigest,
		Status: status, ResultJSON: string(raw), ResultDigest: modelInvocationDigest(raw),
		SolveInvocationID: solveInvocationID, GradeInvocationID: gradeInvocationID,
		ParentGuideInvocationID: parentGuideInvocationID,
		ProjectionStatus:        k12.GradingProjectionCommitted, CreatedAt: deps.now(), UpdatedAt: deps.now(),
	}
	stored, _, err := deps.Records.CommitGradingAssessmentItem(ctx, receipt, effects)
	if err != nil {
		return item, err
	}
	return replayGradingAssessmentItem(q, stored)
}

func replayGradingAssessmentItem(q RecognizedQuestion, receipt k12.GradingAssessmentItem) (PhotoGradeItem, error) {
	if receipt.ProblemID != q.ProblemID || receipt.AttemptID != q.AttemptID ||
		receipt.ConfirmedVersion != q.ConfirmedVersion || receipt.InputDigest != q.InputDigest ||
		receipt.ResultDigest != modelInvocationDigest([]byte(receipt.ResultJSON)) {
		return PhotoGradeItem{Recognized: q}, fmt.Errorf("%w: problem=%s immutable receipt drift",
			ErrGradingAssessmentExactSet, q.ProblemID)
	}
	var item PhotoGradeItem
	if err := json.Unmarshal([]byte(receipt.ResultJSON), &item); err != nil {
		return PhotoGradeItem{Recognized: q}, fmt.Errorf("%w: problem=%s receipt decode: %v",
			ErrGradingAssessmentExactSet, q.ProblemID, err)
	}
	status, err := gradingAssessmentStatus(item.Status)
	if err != nil || status != receipt.Status || item.Recognized.ProblemID != q.ProblemID ||
		item.Recognized.AttemptID != q.AttemptID || item.Recognized.InputDigest != q.InputDigest {
		return PhotoGradeItem{Recognized: q}, fmt.Errorf("%w: problem=%s receipt payload mismatch",
			ErrGradingAssessmentExactSet, q.ProblemID)
	}
	if err := validateGradingAssessmentTerminalItem(item, receipt); err != nil {
		return PhotoGradeItem{Recognized: q}, err
	}
	// Projection metadata is storage-owned because only the atomic receipt
	// transaction knows whether the Mistake insert won or hit an existing row.
	// Overlay it after validating the immutable model-result payload.
	item.Grade.RecordID = receipt.ProjectionRecordID
	item.Grade.RecordCreated = receipt.ProjectionCreated
	if item.ResultKind == "" {
		item.ResultKind = photoItemResultKind(item.Status)
	}
	return item, nil
}

func gradingAssessmentStatus(status PhotoItemStatus) (k12.GradingAssessmentStatus, error) {
	switch status {
	case PhotoCorrect:
		return k12.GradingAssessmentCorrect, nil
	case PhotoWrong:
		return k12.GradingAssessmentWrong, nil
	case PhotoUnanswered:
		return k12.GradingAssessmentUnanswered, nil
	case PhotoAnswerUnclear:
		return k12.GradingAssessmentAnswerUnclear, nil
	case PhotoBlankSolved:
		return k12.GradingAssessmentBlankSolved, nil
	case PhotoOutOfScope:
		return k12.GradingAssessmentOutOfScope, nil
	case PhotoUntrusted:
		return k12.GradingAssessmentUntrusted, nil
	default:
		return "", fmt.Errorf("%w: non-terminal photo status %q", ErrGradingAssessmentExactSet, status)
	}
}

func validateGradingAssessmentExactSet(result PhotoGradeResult, receipts []k12.GradingAssessmentItem) error {
	if len(result.Items) != len(receipts) {
		return fmt.Errorf("%w: result_items=%d receipts=%d", ErrGradingAssessmentExactSet, len(result.Items), len(receipts))
	}
	byProblem := make(map[string]k12.GradingAssessmentItem, len(receipts))
	for _, receipt := range receipts {
		if _, duplicate := byProblem[receipt.ProblemID]; duplicate {
			return fmt.Errorf("%w: duplicate receipt problem=%s", ErrGradingAssessmentExactSet, receipt.ProblemID)
		}
		byProblem[receipt.ProblemID] = receipt
	}
	seen := make(map[string]struct{}, len(result.Items))
	for _, item := range result.Items {
		problemID := item.Recognized.ProblemID
		if _, duplicate := seen[problemID]; duplicate || strings.TrimSpace(problemID) == "" {
			return fmt.Errorf("%w: duplicate/empty result problem=%s", ErrGradingAssessmentExactSet, problemID)
		}
		seen[problemID] = struct{}{}
		receipt, ok := byProblem[problemID]
		if !ok {
			return fmt.Errorf("%w: missing receipt problem=%s", ErrGradingAssessmentExactSet, problemID)
		}
		raw, err := json.Marshal(gradingAssessmentCanonicalResult(item))
		if err != nil || receipt.ResultDigest != modelInvocationDigest(raw) {
			return fmt.Errorf("%w: result digest mismatch problem=%s", ErrGradingAssessmentExactSet, problemID)
		}
		status, statusErr := gradingAssessmentStatus(item.Status)
		if statusErr != nil || status != receipt.Status || receipt.AttemptID != item.Recognized.AttemptID ||
			receipt.ConfirmedVersion != item.Recognized.ConfirmedVersion || receipt.InputDigest != item.Recognized.InputDigest {
			return fmt.Errorf("%w: result identity mismatch problem=%s", ErrGradingAssessmentExactSet, problemID)
		}
		if err := validateGradingAssessmentTerminalItem(item, receipt); err != nil {
			return err
		}
	}
	return nil
}

func validateFrozenAssessReceiptSet(run *gradingRun, receipts []k12.GradingAssessmentItem) error {
	questions := run.questions
	if run.anchored != nil {
		questions = run.anchored
	}
	questions = RecognizedQuestionsForAssessment(cloneRecognizedQuestions(questions))
	if len(questions) != len(receipts) {
		return fmt.Errorf("%w: confirmed_questions=%d receipts=%d",
			ErrGradingAssessmentExactSet, len(questions), len(receipts))
	}
	byProblem := make(map[string]k12.GradingAssessmentItem, len(receipts))
	for _, receipt := range receipts {
		if _, duplicate := byProblem[receipt.ProblemID]; duplicate {
			return fmt.Errorf("%w: duplicate receipt problem=%s",
				ErrGradingAssessmentExactSet, receipt.ProblemID)
		}
		byProblem[receipt.ProblemID] = receipt
	}
	for _, question := range questions {
		receipt, ok := byProblem[question.ProblemID]
		if !ok {
			return fmt.Errorf("%w: missing receipt problem=%s",
				ErrGradingAssessmentExactSet, question.ProblemID)
		}
		if receipt.AttemptID != question.AttemptID ||
			receipt.ConfirmedVersion != question.ConfirmedVersion ||
			receipt.InputDigest != question.InputDigest {
			return fmt.Errorf("%w: receipt identity mismatch problem=%s",
				ErrGradingAssessmentExactSet, question.ProblemID)
		}
		if _, err := replayGradingAssessmentItem(question, receipt); err != nil {
			return err
		}
	}
	return nil
}

func validateGradingAssessmentTerminalItem(
	item PhotoGradeItem,
	receipt k12.GradingAssessmentItem,
) error {
	if err := receipt.ValidateTerminalParentGuideReference(); err != nil {
		return fmt.Errorf(
			"%w: problem=%s: %v",
			ErrGradingAssessmentExactSet,
			receipt.ProblemID,
			err,
		)
	}
	switch receipt.Status {
	case k12.GradingAssessmentWrong, k12.GradingAssessmentBlankSolved:
		if item.ParentGuide == nil {
			return fmt.Errorf(
				"%w: problem=%s status=%s requires a complete parent guide",
				ErrGradingAssessmentExactSet,
				receipt.ProblemID,
				receipt.Status,
			)
		}
		if err := validateParentTeachingGuide(*item.ParentGuide); err != nil {
			return fmt.Errorf(
				"%w: problem=%s incomplete parent guide: %v",
				ErrGradingAssessmentExactSet,
				receipt.ProblemID,
				err,
			)
		}
	default:
		if item.ParentGuide != nil {
			return fmt.Errorf(
				"%w: problem=%s status=%s must remain parent-guide-free",
				ErrGradingAssessmentExactSet,
				receipt.ProblemID,
				receipt.Status,
			)
		}
	}
	return nil
}

func gradingAssessmentCanonicalResult(item PhotoGradeItem) PhotoGradeItem {
	// ResultKind is a deterministic projection of Status. Excluding it keeps
	// pre-field receipts replayable while every live/API result still exposes
	// the explicit item semantics.
	item.ResultKind = ""
	item.Grade.RecordID = ""
	item.Grade.RecordCreated = false
	return item
}
