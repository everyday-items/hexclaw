package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func sourceReprocessNeedsConfirmation(code, format string, args ...any) error {
	return &ProblemSourceReprocessNeedsConfirmationError{
		Code: code, Detail: fmt.Sprintf(format, args...),
	}
}

// problemSourceReprocessAssetProcessor is the narrow composition contract for
// OCR-producing source actions. The processor resolves the immutable source
// from the current typed input head; ImageTask owns the authenticated
// owner-scoped PageAsset gateway and returns verified bytes to the processor.
// The queued request JSON is audit evidence only and is never an authority for
// selecting provider input after restart.
type problemSourceReprocessAssetProcessor interface {
	ProblemSourceReprocessAsset(
		context.Context,
		k12storage.ProblemSourceReprocessJob,
	) (string, error)
	ProcessProblemSourceReprocessWithAsset(
		context.Context,
		k12storage.ProblemSourceReprocessJob,
		ReadyPageAsset,
	) error
}

// ProcessProblemSourceReprocess is the ImageTask composition boundary. It
// reopens select/retake evidence through the authenticated owner-scoped ready
// PageAsset gateway before delegating to the single grading processor.
func (c *ImageTaskCoordinator) ProcessProblemSourceReprocess(
	ctx context.Context,
	work k12storage.ProblemSourceReprocessJob,
) error {
	if c == nil {
		return fmt.Errorf("usecase: image task source reprocess coordinator is unavailable")
	}
	grading, ok := c.Grading.(ProblemSourceReprocessProcessor)
	if !ok || grading == nil {
		return fmt.Errorf("usecase: grading source reprocess processor is unavailable")
	}
	if work.Action != "select_region" && work.Action != "retake" {
		return grading.ProcessProblemSourceReprocess(ctx, work)
	}
	if c.PageAssets == nil {
		return sourceReprocessNeedsConfirmation(
			"page_asset_gateway_unavailable",
			"%s cannot reopen its owner-scoped PageAsset",
			work.Action,
		)
	}
	assetProcessor, ok := c.Grading.(problemSourceReprocessAssetProcessor)
	if !ok || assetProcessor == nil {
		return fmt.Errorf("usecase: grading source OCR processor is unavailable")
	}
	pageAssetID, err := assetProcessor.ProblemSourceReprocessAsset(ctx, work)
	if err != nil {
		return err
	}
	ready, err := c.PageAssets.OpenReady(
		ctx,
		strings.TrimSpace(work.OwnerScope),
		strings.TrimSpace(work.AgentName),
		pageAssetID,
	)
	if err != nil {
		return sourceReprocessNeedsConfirmation(
			"page_asset_not_ready",
			"%s immutable PageAsset is unavailable or failed integrity verification: %v",
			work.Action,
			err,
		)
	}
	return assetProcessor.ProcessProblemSourceReprocessWithAsset(ctx, work, ready)
}

// ProblemSourceReprocessAsset resolves only from current immutable typed
// revisions. A previously committed V73 result advances the current head by
// one; that replay shape remains eligible so a crash between result commit and
// queue completion never requires another provider call.
func (o *GradingOrchestrator) ProblemSourceReprocessAsset(
	ctx context.Context,
	work k12storage.ProblemSourceReprocessJob,
) (string, error) {
	if o == nil || o.deps.Records == nil {
		return "", fmt.Errorf("usecase: grading source reprocess dependencies are unavailable")
	}
	if work.Action != "select_region" && work.Action != "retake" {
		return "", sourceReprocessNeedsConfirmation(
			"source_action_has_no_ocr_asset",
			"source action %q has no OCR PageAsset",
			work.Action,
		)
	}
	if err := validateProblemSourceReprocessIdentity(work); err != nil {
		return "", err
	}
	job, err := o.deps.GetGradingJob(ctx, work.AgentName, work.JobID)
	if err != nil {
		return "", sourceReprocessNeedsConfirmation(
			"grading_job_unavailable",
			"source reprocess grading job is unavailable: %v",
			err,
		)
	}
	if job.Record.AgentName != work.AgentName || job.Record.RecordID != work.JobID {
		return "", sourceReprocessNeedsConfirmation(
			"grading_runtime_identity_drift",
			"source reprocess grading runtime identity changed",
		)
	}
	expectedRevision := work.InputRevision
	if committed, getErr := o.deps.Records.GetProblemSourceRecognitionResultByWork(
		ctx, work.OwnerScope, work.WorkID,
	); getErr == nil {
		if err := validateProblemSourceRecognitionCommit(work, committed); err != nil {
			return "", err
		}
		expectedRevision = committed.ResultInputRevision
	} else if !errors.Is(getErr, k12storage.ErrProblemSourceRecognitionNotFound) {
		return "", getErr
	}
	inputs, err := o.deps.Records.ListCurrentProblemInputRevisions(
		ctx, work.AgentName, job.Fields.SubmissionID,
	)
	if err != nil {
		return "", err
	}
	pageAssetID, _, err := problemSourceCurrentAsset(
		work, inputs, expectedRevision,
	)
	return pageAssetID, err
}

// ProcessProblemSourceReprocess is OCR-free for correct_text/resume. An OCR
// action must enter through ImageTask's owner-scoped PageAsset composition.
func (o *GradingOrchestrator) ProcessProblemSourceReprocess(
	ctx context.Context,
	work k12storage.ProblemSourceReprocessJob,
) error {
	if work.Action == "select_region" || work.Action == "retake" {
		return sourceReprocessNeedsConfirmation(
			"page_asset_required",
			"%s requires verified owner-scoped PageAsset bytes",
			work.Action,
		)
	}
	return o.processProblemSourceReprocess(ctx, work, nil)
}

func (o *GradingOrchestrator) ProcessProblemSourceReprocessWithAsset(
	ctx context.Context,
	work k12storage.ProblemSourceReprocessJob,
	ready ReadyPageAsset,
) error {
	if work.Action != "select_region" && work.Action != "retake" {
		return sourceReprocessNeedsConfirmation(
			"unexpected_page_asset",
			"source action %q must not receive OCR PageAsset bytes",
			work.Action,
		)
	}
	return o.processProblemSourceReprocess(ctx, work, &ready)
}

func validateProblemSourceReprocessIdentity(
	work k12storage.ProblemSourceReprocessJob,
) error {
	agentName := strings.TrimSpace(work.AgentName)
	jobID := strings.TrimSpace(work.JobID)
	if strings.TrimSpace(work.WorkID) == "" ||
		strings.TrimSpace(work.CommandReceiptID) == "" ||
		strings.TrimSpace(work.OwnerScope) == "" ||
		strings.TrimSpace(work.DispatchID) == "" ||
		agentName == "" || jobID == "" || work.StructureVersion < 1 ||
		work.InputRevision < 1 || strings.TrimSpace(work.InputDigest) == "" ||
		len(work.AffectedProblemIDs) == 0 {
		return sourceReprocessNeedsConfirmation(
			"invalid_work_identity",
			"source reprocess work has incomplete immutable identity",
		)
	}
	seen := make(map[string]struct{}, len(work.AffectedProblemIDs))
	for _, rawProblemID := range work.AffectedProblemIDs {
		problemID := strings.TrimSpace(rawProblemID)
		if problemID == "" {
			return sourceReprocessNeedsConfirmation(
				"affected_exact_set_invalid",
				"source reprocess affected exact-set contains an empty problem",
			)
		}
		if _, duplicate := seen[problemID]; duplicate {
			return sourceReprocessNeedsConfirmation(
				"affected_exact_set_invalid",
				"source reprocess affected exact-set contains duplicate %s",
				problemID,
			)
		}
		seen[problemID] = struct{}{}
	}
	if _, ok := seen[strings.TrimSpace(work.ProblemID)]; !ok {
		return sourceReprocessNeedsConfirmation(
			"affected_exact_set_invalid",
			"source reprocess path target is absent from its affected exact-set",
		)
	}
	return nil
}

func validateProblemSourceRecognitionCommit(
	work k12storage.ProblemSourceReprocessJob,
	commit k12storage.ProblemSourceRecognitionCommit,
) error {
	if commit.WorkID != work.WorkID ||
		commit.CommandReceiptID != work.CommandReceiptID ||
		commit.OwnerScope != work.OwnerScope ||
		commit.AgentName != work.AgentName ||
		commit.DispatchID != work.DispatchID ||
		commit.JobID != work.JobID ||
		commit.PathProblemID != work.ProblemID ||
		commit.Action != work.Action ||
		commit.StructureVersion != work.StructureVersion ||
		commit.SourceInputRevision != work.InputRevision ||
		commit.ResultInputRevision != work.InputRevision+1 ||
		commit.MappingState != k12storage.ProblemSourceRecognitionMappingStableExactSet ||
		!equalProblemSourceRecognitionPath(
			commit.AffectedProblemIDs,
			work.AffectedProblemIDs,
		) {
		return sourceReprocessNeedsConfirmation(
			"recognition_result_identity_drift",
			"source recognition result no longer matches immutable work %s",
			work.WorkID,
		)
	}
	return nil
}

func problemSourceCurrentAsset(
	work k12storage.ProblemSourceReprocessJob,
	inputs map[string]k12storage.CurrentProblemInputRevision,
	expectedRevision int,
) (string, *k12.SourcePixelRegion, error) {
	var pageAssetID string
	var sourceRegion *k12.SourcePixelRegion
	for _, rawProblemID := range work.AffectedProblemIDs {
		problemID := strings.TrimSpace(rawProblemID)
		current, ok := inputs[problemID]
		if !ok || current.InputRevision != expectedRevision ||
			strings.TrimSpace(current.InputDigest) == "" ||
			strings.TrimSpace(current.PageAssetID) == "" {
			return "", nil, sourceReprocessNeedsConfirmation(
				"input_revision_changed",
				"source reprocess problem %s no longer has current immutable input revision %d",
				problemID,
				expectedRevision,
			)
		}
		if current.SourceWidth <= 0 || current.SourceHeight <= 0 {
			return "", nil, sourceReprocessNeedsConfirmation(
				"page_asset_not_ready",
				"source reprocess problem %s has no ready PageAsset dimensions",
				problemID,
			)
		}
		if work.Action == "select_region" && current.SourceRegion == nil {
			return "", nil, sourceReprocessNeedsConfirmation(
				"source_region_missing",
				"selected source revision for problem %s has no verified source region",
				problemID,
			)
		}
		if work.Action == "retake" && current.SourceRegion != nil {
			return "", nil, sourceReprocessNeedsConfirmation(
				"source_region_drift",
				"retake source revision for problem %s unexpectedly contains a crop",
				problemID,
			)
		}
		if pageAssetID == "" {
			pageAssetID = strings.TrimSpace(current.PageAssetID)
			sourceRegion = cloneProblemSourceRegion(current.SourceRegion)
			continue
		}
		if pageAssetID != strings.TrimSpace(current.PageAssetID) ||
			!sameProblemSourceRegion(sourceRegion, current.SourceRegion) {
			return "", nil, sourceReprocessNeedsConfirmation(
				"source_exact_set_drift",
				"source reprocess affected exact-set no longer shares one immutable image region",
			)
		}
	}
	return pageAssetID, sourceRegion, nil
}

func cloneProblemSourceRegion(
	region *k12.SourcePixelRegion,
) *k12.SourcePixelRegion {
	if region == nil {
		return nil
	}
	copyRegion := *region
	return &copyRegion
}

func sameProblemSourceRegion(
	left, right *k12.SourcePixelRegion,
) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func overlayCurrentProblemInputRevisions(
	questions []RecognizedQuestion,
	currentInputs map[string]k12storage.CurrentProblemInputRevision,
) []RecognizedQuestion {
	questions = cloneRecognizedQuestions(questions)
	for index := range questions {
		current, ok := currentInputs[questions[index].ProblemID]
		if !ok {
			continue
		}
		questions[index].PageAssetID = current.PageAssetID
		questions[index].SourceWidth = current.SourceWidth
		questions[index].SourceHeight = current.SourceHeight
		questions[index].SourceRegion = cloneProblemSourceRegion(current.SourceRegion)
		questions[index].RawTranscription = current.StemRaw
		questions[index].AnswerRawTranscription = current.AnswerRaw
		questions[index].CanonicalMarkdown = current.QuestionCanonicalMarkdown
		questions[index].AnswerCanonicalMarkdown = current.AnswerCanonicalMarkdown
		questions[index].InputDigest = current.InputDigest
		questions[index].CanonicalVersion = current.InputRevision
		questions[index].ConfirmedVersion = current.InputRevision
	}
	return questions
}

func overlayProblemSourceRecognitionFacts(
	questions []RecognizedQuestion,
	facts map[string]k12storage.ProblemSourceRecognitionFact,
	affectedProblemIDs []string,
	expectedRevision int,
) ([]RecognizedQuestion, error) {
	byIndex := make(map[string]int, len(questions))
	for index := range questions {
		byIndex[questions[index].ProblemID] = index
	}
	for _, rawProblemID := range affectedProblemIDs {
		problemID := strings.TrimSpace(rawProblemID)
		fact, ok := facts[problemID]
		questionIndex, inStructure := byIndex[problemID]
		if !ok || !inStructure || fact.ProblemID != problemID ||
			fact.InputRevision != expectedRevision ||
			strings.TrimSpace(fact.InputDigest) == "" {
			return nil, sourceReprocessNeedsConfirmation(
				"recognition_result_not_current",
				"problem %s has no current typed source-recognition fact at revision %d",
				problemID,
				expectedRevision,
			)
		}
		question := questions[questionIndex]
		question.PageAssetID = fact.Source.PageAssetID
		question.SourceWidth = fact.Source.PixelWidth
		question.SourceHeight = fact.Source.PixelHeight
		question.SourceRegion = cloneProblemSourceRegion(fact.Source.Region)
		question.RawTranscription = fact.StemRaw
		question.CanonicalMarkdown = fact.QuestionCanonicalMarkdown
		question.AnswerState = AnswerState(fact.AnswerState)
		question.AnswerRawTranscription = fact.AnswerRaw
		question.AnswerCanonicalMarkdown = fact.AnswerCanonicalMarkdown
		question.Subject = fact.Subject
		question.KnowledgePoints = append([]string(nil), fact.KnowledgePoints...)
		question.RecognitionConfidence = fact.RecognitionConfidence
		question.OCRSignals = append([]string(nil), fact.OCRSignals...)
		question.EvidenceTranscriptions = append(
			[]string(nil), fact.EvidenceTranscriptions...,
		)
		question.AnswerEvidenceTranscriptions = append(
			[]string(nil), fact.AnswerEvidenceTranscriptions...,
		)
		question.ConfirmationRequired = fact.ConfirmationRequired
		question.ConfirmationReasons = make(
			[]OCRRiskReason, len(fact.ConfirmationReasons),
		)
		for reasonIndex, reason := range fact.ConfirmationReasons {
			question.ConfirmationReasons[reasonIndex] = OCRRiskReason(reason)
		}
		question.BBox = nil
		if fact.AnswerBBox != nil {
			question.BBox = &BBox{
				X: fact.AnswerBBox.X, Y: fact.AnswerBBox.Y,
				W: fact.AnswerBBox.W, H: fact.AnswerBBox.H,
			}
		}
		question.CanonicalVersion = fact.InputRevision
		question.ConfirmedVersion = fact.InputRevision
		question.InputDigest = fact.InputDigest
		questions[questionIndex] = question
	}
	return questions, nil
}

func (o *GradingOrchestrator) processProblemSourceReprocess(
	ctx context.Context,
	work k12storage.ProblemSourceReprocessJob,
	ready *ReadyPageAsset,
) error {
	if o == nil || o.deps.Records == nil {
		return fmt.Errorf("usecase: grading source reprocess dependencies are unavailable")
	}
	if err := validateProblemSourceReprocessIdentity(work); err != nil {
		return err
	}
	switch work.Action {
	case "correct_text", "resume":
		if ready != nil {
			return sourceReprocessNeedsConfirmation(
				"unexpected_page_asset",
				"%s must remain OCR-free",
				work.Action,
			)
		}
	case "select_region", "retake":
		if ready == nil {
			return sourceReprocessNeedsConfirmation(
				"page_asset_required",
				"%s requires verified owner-scoped PageAsset bytes",
				work.Action,
			)
		}
	default:
		return sourceReprocessNeedsConfirmation(
			"unsupported_source_action",
			"unsupported source reprocess action %q",
			work.Action,
		)
	}

	agentName := strings.TrimSpace(work.AgentName)
	jobID := strings.TrimSpace(work.JobID)
	run, err := o.ensureRun(ctx, jobID)
	if err != nil {
		return sourceReprocessNeedsConfirmation(
			"grading_runtime_unavailable",
			"source reprocess cannot restore its frozen grading runtime: %v",
			err,
		)
	}
	if run.agentName != agentName {
		return sourceReprocessNeedsConfirmation(
			"grading_runtime_identity_drift",
			"source reprocess grading runtime owner changed",
		)
	}

	// 与主批改链共用原 Job 锁；局部清晰题在锁外调用模型时先等待，
	// 避免恢复中的源重处理与冻结执行集并发修改同一输入和产物。
	jobLock := o.jobLock(jobID)
	for {
		jobLock.Lock()
		if run.clearAssessmentDone == nil {
			break
		}
		done := run.clearAssessmentDone
		jobLock.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer jobLock.Unlock()

	job, err := o.deps.GetGradingJob(ctx, agentName, jobID)
	if err != nil {
		return sourceReprocessNeedsConfirmation(
			"grading_job_unavailable",
			"source reprocess grading job is unavailable: %v",
			err,
		)
	}
	if job.Record.AgentName != agentName || job.Record.RecordID != jobID {
		return sourceReprocessNeedsConfirmation(
			"grading_job_not_processable",
			"source reprocess grading job identity changed",
		)
	}
	if job.Record.Status == k12.GradingStageCompleted {
		return o.requireCurrentSourceReprocessFinalArtifact(ctx, job)
	}
	if job.Record.Status == k12.GradingStageFailedRetryable && work.Action == "correct_text" {
		invocations, err := o.deps.Records.ListGradingItemInvocations(ctx, agentName, jobID)
		if err != nil {
			return err
		}
		affectedProblemIDs := make(map[string]struct{}, len(work.AffectedProblemIDs))
		for _, problemID := range work.AffectedProblemIDs {
			affectedProblemIDs[strings.TrimSpace(problemID)] = struct{}{}
		}
		// 新输入摘要不能绕过同题的历史未决调用；组外调用不扩大局部重处理范围。
		for _, invocation := range invocations {
			if _, affected := affectedProblemIDs[invocation.ProblemID]; !affected {
				continue
			}
			if invocation.Status == k12.ModelInvocationSent || invocation.Status == k12.ModelInvocationOutcomeUnknown {
				return sourceReprocessNeedsConfirmation(
					"grading_job_not_processable",
					"source reprocess affected problem %s invocation %s status %s requires reconciliation",
					invocation.ProblemID, invocation.InvocationID, invocation.Status,
				)
			}
		}
	} else if job.Record.Status != k12.GradingStageAssessing {
		return sourceReprocessNeedsConfirmation(
			"grading_job_not_processable",
			"source reprocess grading job stage %s is not automatically processable",
			job.Record.Status,
		)
	}

	// Read the V19 snapshot only for stable structure/Attempt identity. Current
	// source text and V73 recognition facts are overlaid from immutable current
	// revisions below; old OCR observations are never mistaken for new input.
	snapshot, err := o.deps.Records.GetProblemAttemptSnapshot(
		ctx, agentName, job.Fields.SubmissionID,
	)
	if err != nil {
		return sourceReprocessNeedsConfirmation(
			"recognition_facts_unavailable",
			"source reprocess durable recognition facts are unavailable: %v",
			err,
		)
	}
	structureQuestions, err := RecognizedQuestionsFromProblemAttemptSnapshot(snapshot)
	if err != nil {
		return sourceReprocessNeedsConfirmation(
			"recognition_facts_invalid",
			"source reprocess durable recognition facts are invalid: %v",
			err,
		)
	}
	progressive, err := o.deps.Records.GetGradingProgressiveProjection(
		ctx, agentName, jobID,
	)
	if err != nil {
		return err
	}
	if progressive.ProgressiveSnapshot.StructureVersion != work.StructureVersion {
		return sourceReprocessNeedsConfirmation(
			"structure_version_changed",
			"source reprocess structure version changed from %d to %d",
			work.StructureVersion,
			progressive.ProgressiveSnapshot.StructureVersion,
		)
	}

	currentInputs, err := o.deps.Records.ListCurrentProblemInputRevisions(
		ctx, agentName, job.Fields.SubmissionID,
	)
	if err != nil {
		return err
	}
	expectedRevision := work.InputRevision
	var recognitionCommit k12storage.ProblemSourceRecognitionCommit
	hasRecognitionCommit := false
	if work.Action == "select_region" || work.Action == "retake" {
		recognitionCommit, err = o.deps.Records.GetProblemSourceRecognitionResultByWork(
			ctx, work.OwnerScope, work.WorkID,
		)
		switch {
		case err == nil:
			if err := validateProblemSourceRecognitionCommit(work, recognitionCommit); err != nil {
				return err
			}
			hasRecognitionCommit = true
			expectedRevision = recognitionCommit.ResultInputRevision
		case errors.Is(err, k12storage.ErrProblemSourceRecognitionNotFound):
			err = nil
		default:
			return err
		}
		pageAssetID, sourceRegion, sourceErr := problemSourceCurrentAsset(
			work, currentInputs, expectedRevision,
		)
		if sourceErr != nil {
			return sourceErr
		}
		if ready.Metadata.OwnerScope != strings.TrimSpace(work.OwnerScope) ||
			ready.Metadata.AgentName != agentName ||
			ready.Metadata.PageAssetID != pageAssetID {
			return sourceReprocessNeedsConfirmation(
				"page_asset_identity_drift",
				"verified PageAsset does not match the current source revision",
			)
		}
		if !hasRecognitionCommit {
			if problemSourceReconciliationOnly(ctx) {
				return errors.Join(
					ErrModelInvocationRequiresReconciliation,
					fmt.Errorf(
						"outcome_unknown work %s has no committed V73 recognition result",
						work.WorkID,
					),
				)
			}
			ocrRegion := sourceRegion
			if work.Action == "retake" {
				ocrRegion = nil
			}
			ocrImage, imageErr := normalizeProblemSourceOCRImage(*ready, ocrRegion)
			if imageErr != nil {
				return sourceReprocessNeedsConfirmation(
					"source_image_invalid",
					"source image cannot be normalized safely: %v",
					imageErr,
				)
			}
			execution, executeErr := o.executeProblemSourceRecognition(
				ctx, work, job, ocrImage.Data,
			)
			if executeErr != nil {
				return executeErr
			}
			items, mapErr := mapProblemSourceRecognitionExactSet(
				work, structureQuestions, execution.Recognized,
			)
			if mapErr != nil {
				return mapErr
			}
			typedResult := k12storage.ProblemSourceRecognitionResult{
				MappingState: k12storage.
					ProblemSourceRecognitionMappingStableExactSet,
				ParentInvocationID: execution.Parent.InvocationID,
				PhysicalResults:    execution.PhysicalResults,
				Items:              items,
			}
			typedResultDigest, digestErr :=
				k12storage.ProblemSourceRecognitionTypedResultDigest(typedResult)
			if digestErr != nil {
				return digestErr
			}
			parent, markErr := o.deps.Records.MarkModelInvocationSucceeded(
				context.WithoutCancel(ctx),
				execution.Parent.AgentName,
				execution.Parent.InvocationID,
				typedResultDigest,
				"",
			)
			if markErr != nil || parent.Status != k12.ModelInvocationSucceeded ||
				parent.ResultDigest != typedResultDigest {
				return errors.Join(
					ErrModelInvocationRequiresReconciliation,
					markErr,
				)
			}
			commitCtx, cancelCommit := gradingDurableCommitContext(ctx)
			recognitionCommit, _, err = o.deps.Records.CommitProblemSourceRecognitionResult(
				commitCtx,
				work.Lease(),
				typedResult,
			)
			cancelCommit()
			if err != nil {
				switch {
				case errors.Is(err, k12storage.ErrProblemSourceRecognitionConflict),
					errors.Is(err, k12storage.ErrProblemSourceRecognitionInvalid),
					errors.Is(err, k12storage.ErrProblemSourceRecognitionUnstableMapping):
					return sourceReprocessNeedsConfirmation(
						"recognition_result_conflict",
						"source recognition result cannot be committed safely: %v",
						err,
					)
				default:
					return err
				}
			}
			if err := validateProblemSourceRecognitionCommit(work, recognitionCommit); err != nil {
				return err
			}
			expectedRevision = recognitionCommit.ResultInputRevision
			currentInputs, err = o.deps.Records.ListCurrentProblemInputRevisions(
				ctx, agentName, job.Fields.SubmissionID,
			)
			if err != nil {
				return err
			}
		}
	}

	questions, err := o.deps.loadCurrentConfirmedQuestions(
		ctx, agentName, job.Fields.SubmissionID,
	)
	if err != nil {
		return err
	}
	if err := validateNormalizedRecognizedProblems(questions); err != nil {
		return sourceReprocessNeedsConfirmation(
			"structure_mapping_changed",
			"source reprocess current structure is not safely mappable: %v",
			err,
		)
	}
	assessmentQuestions := RecognizedQuestionsForAssessment(questions)
	byProblem := make(map[string]RecognizedQuestion, len(assessmentQuestions))
	for _, question := range assessmentQuestions {
		if _, duplicate := byProblem[question.ProblemID]; duplicate {
			return sourceReprocessNeedsConfirmation(
				"structure_mapping_changed",
				"source reprocess current structure contains duplicate problem %s",
				question.ProblemID,
			)
		}
		byProblem[question.ProblemID] = question
	}
	affected := make([]RecognizedQuestion, 0, len(work.AffectedProblemIDs))
	for _, rawProblemID := range work.AffectedProblemIDs {
		problemID := strings.TrimSpace(rawProblemID)
		question, ok := byProblem[problemID]
		if !ok {
			return sourceReprocessNeedsConfirmation(
				"structure_mapping_changed",
				"source reprocess affected problem %s is not in the current answerable structure",
				problemID,
			)
		}
		if question.ConfirmedVersion != expectedRevision ||
			question.CanonicalVersion != expectedRevision ||
			strings.TrimSpace(question.InputDigest) == "" {
			return sourceReprocessNeedsConfirmation(
				"input_revision_changed",
				"source reprocess problem %s current input revision no longer matches revision %d",
				problemID,
				expectedRevision,
			)
		}
		if question.ConfirmationRequired {
			return sourceReprocessNeedsConfirmation(
				"source_risk_requires_confirmation",
				"source reprocess problem %s retains OCR risk: %v",
				problemID,
				question.ConfirmationReasons,
			)
		}
		affected = append(affected, question)
	}

	req := run.req
	mode := classifyPhotoMode(assessmentQuestions)
	switch req.TaskIntent {
	case "":
	case PhotoTaskCompletedHomework:
		mode = PhotoModeGrade
	case PhotoTaskBlankWorksheet:
		mode = PhotoModeSolve
	default:
		return sourceReprocessNeedsConfirmation(
			"task_intent_changed",
			"source reprocess has an unsupported frozen task intent %q",
			req.TaskIntent,
		)
	}

	// Refresh the process-local checkpoint under jobLock before provider work so
	// a subsequent RunGradingJob cannot replay stale questions.
	canonicalQuestions := cloneRecognizedQuestions(questions)
	anchoredQuestions := cloneRecognizedQuestions(questions)
	hasAnchoredGeometry := false
	for index := range canonicalQuestions {
		if anchoredQuestions[index].BBox != nil {
			hasAnchoredGeometry = true
		}
		canonicalQuestions[index].BBox = nil
	}
	run.questions = canonicalQuestions
	if hasAnchoredGeometry {
		run.anchored = anchoredQuestions
	} else {
		run.anchored = nil
	}
	run.result = nil
	run.renderFailure = ""
	if err := o.persistRun(jobID, run); err != nil {
		return fmt.Errorf("usecase: persist current source reprocess runtime: %w", err)
	}
	for _, question := range affected {
		if _, err := o.assessDurablePhotoItem(
			ctx, o.deps, job, req, mode, question,
		); err != nil {
			return fmt.Errorf(
				"usecase: assess source-reprocessed problem %s: %w",
				question.ProblemID,
				err,
			)
		}
	}

	// A source worker owns only its frozen affected exact-set. It must never
	// resume runLoop here: runLoop's assessing stage legitimately claims every
	// missing current question for a normal page run, which would send unrelated
	// source-action questions to providers. The shared finalizer is still the
	// only terminal authority; it returns ErrGradingFinalizationIncomplete until
	// every current question has exactly one durable assessment or skip receipt.
	completed, finalized, err := o.finalizeSourceReprocessIfCurrentExactSet(
		ctx, run, job,
	)
	if err != nil {
		return err
	}
	if !finalized {
		return nil
	}
	return o.requireCurrentSourceReprocessFinalArtifact(ctx, completed)
}

// finalizeSourceReprocessIfCurrentExactSet finishes a source-action replay
// only after the canonical finalizer has proved the complete current exact-set.
// It intentionally contains no item assessment or OCR path. Source work has no
// process-local full-page render result, so the post-finalizer checkpoint
// transitions consume the already durable immutable artifact rather than
// invoking the normal runner's rendering/assessing branches.
func (o *GradingOrchestrator) finalizeSourceReprocessIfCurrentExactSet(
	ctx context.Context,
	run *gradingRun,
	job GradingJobView,
) (GradingJobView, bool, error) {
	artifact, err := o.finalizeGradingPage(ctx, run, job)
	if errors.Is(err, ErrGradingFinalizationIncomplete) {
		return job, false, nil
	}
	if err != nil {
		return job, false, err
	}
	// A stale artifact is never enough to transition the Job. Re-read through
	// the aggregate-generation fence after the finalizer commit so a source
	// action cannot complete from an older immutable result.
	current, err := o.deps.Records.GetCurrentGradingFinalArtifactByJob(
		ctx, job.Record.AgentName, job.Record.RecordID,
	)
	if errors.Is(err, records.ErrNotFound) {
		return job, false, sourceReprocessNeedsConfirmation(
			"finalization_not_current",
			"source reprocess finalizer did not leave a current final artifact",
		)
	}
	if err != nil {
		return job, false, err
	}
	if current.ArtifactID != artifact.ArtifactID ||
		current.ArtifactDigest != artifact.ArtifactDigest || current.Validate() != nil {
		return job, false, sourceReprocessNeedsConfirmation(
			"finalization_not_current",
			"source reprocess finalizer artifact changed before terminal publication",
		)
	}

	completed := job
	for completed.Record != nil && completed.Record.Status != k12.GradingStageCompleted {
		switch completed.Record.Status {
		case k12.GradingStageAssessing, k12.GradingStageRendering:
			completed, err = o.advanceOK(
				ctx,
				run,
				completed.Record.RecordID,
				"source-reprocess-final:"+current.ArtifactDigest,
			)
		case k12.GradingStageProjecting:
			completed, err = o.completeFinalizedGrading(
				ctx,
				run,
				completed.Record.RecordID,
				current.ArtifactDigest,
			)
		default:
			return completed, false, sourceReprocessNeedsConfirmation(
				"grading_job_not_processable",
				"source reprocess finalizer cannot publish from grading stage %s",
				completed.Record.Status,
			)
		}
		if err != nil {
			return completed, false, err
		}
	}
	if completed.Record == nil || completed.Record.Status != k12.GradingStageCompleted {
		return completed, false, fmt.Errorf(
			"usecase: source reprocess canonical grading job %s stopped before completion",
			job.Record.RecordID,
		)
	}
	return completed, true, nil
}

func (o *GradingOrchestrator) requireCurrentSourceReprocessFinalArtifact(
	ctx context.Context,
	job GradingJobView,
) error {
	artifact, err := o.deps.Records.GetCurrentGradingFinalArtifactByJob(
		ctx,
		job.Record.AgentName,
		job.Record.RecordID,
	)
	if errors.Is(err, records.ErrNotFound) {
		return sourceReprocessNeedsConfirmation(
			"finalization_not_current",
			"completed source reprocess has no final artifact at the current aggregate generation",
		)
	}
	if err != nil {
		return err
	}
	if artifact.AgentName != job.Record.AgentName ||
		artifact.JobID != job.Record.RecordID ||
		artifact.Validate() != nil {
		return sourceReprocessNeedsConfirmation(
			"finalization_not_current",
			"completed source reprocess final artifact is invalid",
		)
	}
	return nil
}
