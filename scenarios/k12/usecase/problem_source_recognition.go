package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

const problemSourceRecognitionParentPrefix = "modelinv-source-"

type problemSourceRecognitionExecution struct {
	Parent          k12.ModelInvocation
	PhysicalResults []k12storage.ProblemSourceRecognitionPhysicalResultRef
	Recognized      []RecognizedQuestion
}

func stableProblemSourceRecognitionParentID(workID string, queueAttempt int) string {
	sum := sha256.Sum256([]byte(
		strings.TrimSpace(workID) + "\x00" + strconv.Itoa(queueAttempt),
	))
	return problemSourceRecognitionParentPrefix + hex.EncodeToString(sum[:16])
}

func problemSourceRecognitionParentDigest(
	work k12storage.ProblemSourceReprocessJob,
	route k12.GradingModelSnapshot,
	policy k12.ModelRequestPolicySnapshot,
) (string, error) {
	return k12storage.ProblemSourceRecognitionParentRequestDigest(
		work,
		route,
		policy,
	)
}

func (o *GradingOrchestrator) prepareProblemSourceRecognitionParent(
	ctx context.Context,
	work k12storage.ProblemSourceReprocessJob,
	job GradingJobView,
	image []byte,
) (k12.ModelInvocation, error) {
	if work.AttemptCount < 1 || len(image) == 0 {
		return k12.ModelInvocation{}, fmt.Errorf(
			"usecase: source recognition needs a claimed queue attempt and image",
		)
	}
	invocations, err := o.deps.Records.ListModelInvocations(
		ctx, work.AgentName, work.JobID,
	)
	if err != nil {
		return k12.ModelInvocation{}, err
	}
	currentID := stableProblemSourceRecognitionParentID(
		work.WorkID, work.AttemptCount,
	)
	expectedSourceIDs := make(map[string]struct{}, work.AttemptCount)
	for attempt := 1; attempt <= work.AttemptCount; attempt++ {
		expectedSourceIDs[stableProblemSourceRecognitionParentID(
			work.WorkID, attempt,
		)] = struct{}{}
	}
	maxRecognizingAttempt := 0
	var current *k12.ModelInvocation
	for index := range invocations {
		invocation := invocations[index]
		if invocation.Stage != k12.GradingStageRecognizing {
			continue
		}
		if invocation.Attempt > maxRecognizingAttempt {
			maxRecognizingAttempt = invocation.Attempt
		}
		if _, belongsToWork := expectedSourceIDs[invocation.InvocationID]; !belongsToWork {
			continue
		}
		if invocation.InvocationID == currentID {
			copyInvocation := invocation
			current = &copyInvocation
			continue
		}
		if err := o.problemSourceRecognitionAttemptAllowsSuccessor(
			ctx, invocation,
		); err != nil {
			return invocation, err
		}
	}
	if current != nil {
		if err := o.problemSourceRecognitionCurrentAttemptCanEnterProvider(
			ctx, *current,
		); err != nil {
			return *current, err
		}
	}

	route := k12.NormalizeGradingModelSnapshot(job.Fields.ModelSnapshot)
	policy := k12.NormalizeModelRequestPolicySnapshot(
		route.RecognizingRequestPolicy,
	)
	attempt := maxRecognizingAttempt + 1
	if current != nil {
		attempt = current.Attempt
	}
	requestDigest, err := problemSourceRecognitionParentDigest(
		work,
		route,
		policy,
	)
	if err != nil {
		return k12.ModelInvocation{}, fmt.Errorf(
			"usecase: bind source recognition parent to immutable work: %w",
			err,
		)
	}
	parent := k12.ModelInvocation{
		InvocationID:          currentID,
		AgentName:             work.AgentName,
		JobID:                 work.JobID,
		Stage:                 k12.GradingStageRecognizing,
		RequestDigest:         requestDigest,
		RouteSnapshot:         route,
		RequestPolicySnapshot: policy,
		Attempt:               attempt,
		CreatedAt:             o.deps.now(),
		UpdatedAt:             o.deps.now(),
	}
	call := k12.RecognitionPhysicalCall{
		Unit: k12.RecognitionPhysicalUnitWholePage, Image: image,
	}
	childDigest, err := recognizingPhysicalInvocationDigest(parent, call)
	if err != nil {
		return parent, err
	}
	published, child, _, err :=
		o.deps.Records.PrepareRecognizingInvocationWithInitialWholePage(
			ctx,
			parent,
			k12.ModelPhysicalInvocation{
				PhysicalInvocationID: stableRecognitionPhysicalInvocationID(
					parent.InvocationID, call.Unit,
				),
				ParentInvocationID:    parent.InvocationID,
				AgentName:             parent.AgentName,
				JobID:                 parent.JobID,
				Stage:                 parent.Stage,
				PhysicalUnit:          call.Unit,
				RequestDigest:         childDigest,
				RouteSnapshot:         parent.RouteSnapshot,
				RequestPolicySnapshot: parent.RequestPolicySnapshot,
				Attempt:               1,
				CreatedAt:             o.deps.now(),
				UpdatedAt:             o.deps.now(),
			},
		)
	if err != nil {
		return published, fmt.Errorf(
			"%w: publish source recognition parent/whole child: %v",
			ErrRecognitionPhysicalCallBeforeSend,
			err,
		)
	}
	if published.Status != k12.ModelInvocationSent ||
		child.Status != k12.ModelInvocationPrepared {
		return published, errors.Join(
			ErrModelInvocationRequiresReconciliation,
			fmt.Errorf(
				"source recognition parent=%s child=%s",
				published.Status,
				child.Status,
			),
		)
	}
	return published, nil
}

func (o *GradingOrchestrator) problemSourceRecognitionAttemptAllowsSuccessor(
	ctx context.Context,
	parent k12.ModelInvocation,
) error {
	if parent.Status != k12.ModelInvocationFailed ||
		strings.TrimSpace(parent.FailureKind) == "" {
		return errors.Join(
			ErrModelInvocationRequiresReconciliation,
			fmt.Errorf(
				"prior source recognition parent %s remains %s",
				parent.InvocationID,
				parent.Status,
			),
		)
	}
	children, err := o.problemSourceRecognitionChildren(ctx, parent)
	if err != nil {
		return err
	}
	if len(children) == 0 {
		return errors.Join(
			ErrModelInvocationRequiresReconciliation,
			fmt.Errorf("prior source recognition parent has no physical evidence"),
		)
	}
	for _, child := range children {
		if child.Status != k12.ModelInvocationFailed ||
			strings.TrimSpace(child.FailureKind) == "" ||
			strings.TrimSpace(child.ResultDigest) != "" {
			return errors.Join(
				ErrModelInvocationRequiresReconciliation,
				fmt.Errorf(
					"prior source recognition child %s remains %s",
					child.PhysicalInvocationID,
					child.Status,
				),
			)
		}
	}
	return nil
}

func (o *GradingOrchestrator) problemSourceRecognitionCurrentAttemptCanEnterProvider(
	ctx context.Context,
	parent k12.ModelInvocation,
) error {
	children, err := o.problemSourceRecognitionChildren(ctx, parent)
	if err != nil {
		return err
	}
	if parent.Status == k12.ModelInvocationPrepared && len(children) == 0 {
		return nil
	}
	if parent.Status == k12.ModelInvocationSent && len(children) == 1 &&
		children[0].PhysicalUnit == k12.RecognitionPhysicalUnitWholePage &&
		children[0].Status == k12.ModelInvocationPrepared {
		return nil
	}
	if parent.Status == k12.ModelInvocationSent && len(children) > 0 {
		allDefinitiveFailures := true
		for _, child := range children {
			if child.Status != k12.ModelInvocationFailed ||
				strings.TrimSpace(child.FailureKind) == "" ||
				strings.TrimSpace(child.ResultDigest) != "" {
				allDefinitiveFailures = false
				break
			}
		}
		if allDefinitiveFailures {
			stored, markErr := o.deps.Records.MarkModelInvocationFailed(
				context.WithoutCancel(ctx),
				parent.AgentName,
				parent.InvocationID,
				"source_recognition_physical_failed",
			)
			if markErr != nil {
				return errors.Join(
					ErrModelInvocationRequiresReconciliation,
					markErr,
				)
			}
			return fmt.Errorf(
				"source recognition attempt %d definitively failed: %s",
				stored.Attempt,
				stored.FailureKind,
			)
		}
	}
	return errors.Join(
		ErrModelInvocationRequiresReconciliation,
		fmt.Errorf(
			"source recognition parent %s status=%s has non-replayable physical evidence",
			parent.InvocationID,
			parent.Status,
		),
	)
}

func (o *GradingOrchestrator) problemSourceRecognitionChildren(
	ctx context.Context,
	parent k12.ModelInvocation,
) ([]k12.ModelPhysicalInvocation, error) {
	all, err := o.deps.Records.ListModelPhysicalInvocations(
		ctx, parent.AgentName, parent.JobID,
	)
	if err != nil {
		return nil, err
	}
	children := make([]k12.ModelPhysicalInvocation, 0, len(all))
	for _, child := range all {
		if child.ParentInvocationID == parent.InvocationID {
			children = append(children, child)
		}
	}
	return children, nil
}

func (o *GradingOrchestrator) settleProblemSourceRecognitionError(
	ctx context.Context,
	parent k12.ModelInvocation,
	cause error,
) error {
	children, err := o.problemSourceRecognitionChildren(
		context.WithoutCancel(ctx), parent,
	)
	if err != nil {
		return errors.Join(
			cause,
			ErrModelInvocationRequiresReconciliation,
			err,
		)
	}
	if len(children) == 1 &&
		children[0].Status == k12.ModelInvocationPrepared {
		closed, closeErr := o.deps.Records.MarkModelPhysicalInvocationNotSent(
			context.WithoutCancel(ctx),
			parent.AgentName,
			children[0].PhysicalInvocationID,
		)
		if closeErr != nil {
			return errors.Join(
				cause,
				ErrModelInvocationRequiresReconciliation,
				closeErr,
			)
		}
		children[0] = closed
	}
	allDefinitiveFailures := len(children) > 0
	for _, child := range children {
		if child.Status != k12.ModelInvocationFailed ||
			strings.TrimSpace(child.FailureKind) == "" ||
			strings.TrimSpace(child.ResultDigest) != "" {
			allDefinitiveFailures = false
			break
		}
	}
	if !allDefinitiveFailures {
		return errors.Join(
			cause,
			ErrModelInvocationRequiresReconciliation,
		)
	}
	if _, err := o.deps.Records.MarkModelInvocationFailed(
		context.WithoutCancel(ctx),
		parent.AgentName,
		parent.InvocationID,
		"source_recognition_physical_failed",
	); err != nil {
		return errors.Join(
			cause,
			ErrModelInvocationRequiresReconciliation,
			err,
		)
	}
	return cause
}

func (o *GradingOrchestrator) executeProblemSourceRecognition(
	ctx context.Context,
	work k12storage.ProblemSourceReprocessJob,
	job GradingJobView,
	image []byte,
) (problemSourceRecognitionExecution, error) {
	parent, err := o.prepareProblemSourceRecognitionParent(
		ctx, work, job, image,
	)
	if err != nil {
		return problemSourceRecognitionExecution{}, err
	}
	executor := newDurableRecognitionPhysicalCallExecutor(o, parent)
	providerCtx := k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
	recognized, err := o.deps.RecognizeHomework(providerCtx, image)
	if err != nil {
		return problemSourceRecognitionExecution{},
			o.settleProblemSourceRecognitionError(ctx, parent, err)
	}
	physical, err := o.recognitionPhysicalSuccessSet(
		context.WithoutCancel(ctx), parent, image,
	)
	if err != nil {
		return problemSourceRecognitionExecution{}, errors.Join(
			err,
			ErrModelInvocationRequiresReconciliation,
		)
	}
	refs := make(
		[]k12storage.ProblemSourceRecognitionPhysicalResultRef,
		0,
		len(physical),
	)
	for _, child := range physical {
		refs = append(refs,
			k12storage.ProblemSourceRecognitionPhysicalResultRef{
				PhysicalInvocationID: child.PhysicalInvocationID,
				PhysicalUnit:         string(child.PhysicalUnit),
				ResultDigest:         child.ResultDigest,
			},
		)
	}
	return problemSourceRecognitionExecution{
		Parent: parent, PhysicalResults: refs,
		Recognized: cloneRecognizedQuestions(recognized),
	}, nil
}

func mapProblemSourceRecognitionExactSet(
	work k12storage.ProblemSourceReprocessJob,
	currentQuestions []RecognizedQuestion,
	recognized []RecognizedQuestion,
) ([]k12storage.ProblemSourceRecognitionItem, error) {
	normalized, err := NormalizeRecognizedProblems(
		"source-reprocess:"+strings.TrimSpace(work.WorkID),
		recognized,
	)
	if err != nil {
		return nil, sourceReprocessNeedsConfirmation(
			"recognition_structure_changed",
			"source recognition structure is invalid: %v",
			err,
		)
	}
	candidates := RecognizedQuestionsForAssessment(normalized)
	currentAnswerable := RecognizedQuestionsForAssessment(currentQuestions)
	currentByID := make(map[string]RecognizedQuestion, len(currentAnswerable))
	for _, question := range currentAnswerable {
		currentByID[question.ProblemID] = question
	}
	items := make([]k12storage.ProblemSourceRecognitionItem, 0, len(work.AffectedProblemIDs))
	used := make(map[int]struct{}, len(work.AffectedProblemIDs))
	for _, problemID := range work.AffectedProblemIDs {
		current, ok := currentByID[problemID]
		if !ok {
			return nil, sourceReprocessNeedsConfirmation(
				"recognition_structure_changed",
				"affected problem %s is absent from the current structure",
				problemID,
			)
		}
		matches := make([]int, 0, 1)
		for index, candidate := range candidates {
			if _, alreadyUsed := used[index]; alreadyUsed {
				continue
			}
			if problemSourceRecognitionStructureMatches(
				current,
				candidate,
				work.Action == "select_region" &&
					len(work.AffectedProblemIDs) == 1 && len(candidates) == 1,
			) {
				matches = append(matches, index)
			}
		}
		if len(matches) != 1 {
			return nil, sourceReprocessNeedsConfirmation(
				"recognition_structure_changed",
				"affected problem %s has %d stable recognition mappings",
				problemID,
				len(matches),
			)
		}
		candidateIndex := matches[0]
		used[candidateIndex] = struct{}{}
		candidate := NormalizeRecognizedQuestion(candidates[candidateIndex])
		reasons := make([]string, len(candidate.ConfirmationReasons))
		for index, reason := range candidate.ConfirmationReasons {
			reasons[index] = string(reason)
		}
		var bbox *k12.AttemptBBox
		if candidate.BBox != nil {
			bbox = &k12.AttemptBBox{
				X: candidate.BBox.X, Y: candidate.BBox.Y,
				W: candidate.BBox.W, H: candidate.BBox.H,
			}
		}
		items = append(items, k12storage.ProblemSourceRecognitionItem{
			ProblemID:                    problemID,
			StemRaw:                      candidate.RawTranscription,
			QuestionCanonicalMarkdown:    candidate.CanonicalMarkdown,
			AnswerState:                  string(candidate.AnswerState),
			AnswerRaw:                    candidate.AnswerRawTranscription,
			AnswerCanonicalMarkdown:      candidate.AnswerCanonicalMarkdown,
			AnswerBBox:                   bbox,
			Subject:                      candidate.Subject,
			KnowledgePoints:              append([]string(nil), candidate.KnowledgePoints...),
			RecognitionConfidence:        candidate.RecognitionConfidence,
			OCRSignals:                   append([]string(nil), candidate.OCRSignals...),
			EvidenceTranscriptions:       append([]string(nil), candidate.EvidenceTranscriptions...),
			AnswerEvidenceTranscriptions: append([]string(nil), candidate.AnswerEvidenceTranscriptions...),
			ConfirmationRequired:         candidate.ConfirmationRequired,
			ConfirmationReasons:          reasons,
		})
	}
	return items, nil
}

func problemSourceRecognitionStructureMatches(
	current RecognizedQuestion,
	candidate RecognizedQuestion,
	singleExplicitRegion bool,
) bool {
	if singleExplicitRegion {
		return true
	}
	if len(current.SourceNumberPath) > 0 || len(candidate.SourceNumberPath) > 0 {
		return equalProblemSourceRecognitionPath(
			current.SourceNumberPath,
			candidate.SourceNumberPath,
		) && strings.TrimSpace(current.SubproblemNo) ==
			strings.TrimSpace(candidate.SubproblemNo)
	}
	if len(current.SourceSectionPath) == 0 ||
		len(candidate.SourceSectionPath) == 0 ||
		!equalProblemSourceRecognitionPath(
			current.SourceSectionPath,
			candidate.SourceSectionPath,
		) {
		return false
	}
	return current.SystemSectionOrdinal > 0 &&
		current.SystemSectionOrdinal == candidate.SystemSectionOrdinal
}

func equalProblemSourceRecognitionPath(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if strings.TrimSpace(left[index]) != strings.TrimSpace(right[index]) {
			return false
		}
	}
	return true
}
