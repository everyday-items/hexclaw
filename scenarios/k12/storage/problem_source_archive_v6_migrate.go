package k12storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/viewcontract"
)

// ErrProblemSourceArchiveLiveWork prevents restore-as from copying a queue
// state that could issue, retry, or reconcile an external model call under a
// different Tutor identity.
var ErrProblemSourceArchiveLiveWork = errors.New("problem-source archive contains live external work")

// MigrateProblemSourceArchiveV6Owner deterministically rewrites the global and
// owner-scoped identities in a terminal source-action closure. Source facts,
// provider result digests, timestamps, and problem identities remain stable;
// process leases and provider control identifiers never cross the boundary.
func MigrateProblemSourceArchiveV6Owner(
	sourceAgent string,
	targetAgent string,
	source ProblemSourceArchiveV6,
	assetIDs map[string]string,
) (ProblemSourceArchiveV6, error) {
	sourceAgent = strings.TrimSpace(sourceAgent)
	targetAgent = strings.TrimSpace(targetAgent)
	if sourceAgent == "" || targetAgent == "" || sourceAgent == targetAgent {
		return ProblemSourceArchiveV6{}, fmt.Errorf("problem-source restore-as owner is invalid")
	}
	if err := ValidateProblemSourceArchiveV6(sourceAgent, source); err != nil {
		return ProblemSourceArchiveV6{}, err
	}
	authoritativeSubmissions, err := problemSourceArchiveAuthoritativeJobSubmissions(source)
	if err != nil {
		return ProblemSourceArchiveV6{}, err
	}
	for _, work := range source.ReprocessJobs {
		if problemSourceArchiveWorkIsLive(work) {
			return ProblemSourceArchiveV6{}, fmt.Errorf(
				"%w: work_id=%s status=%s",
				ErrProblemSourceArchiveLiveWork,
				work.WorkID,
				work.Status,
			)
		}
	}

	out := cloneProblemSourceArchiveV6(source)
	dispatchIDs := make(map[string]string, len(out.Dispatches))
	homeworkIDs := make(map[string]string, len(out.HomeworkSubmissions))
	receiptIDs := make(map[string]string, len(out.ActionReceipts))
	workIDs := make(map[string]string, len(out.ReprocessJobs))
	parentIDs := make(map[string]string, len(out.ModelInvocations))
	physicalIDs := make(map[string]string, len(out.ModelPhysicalInvocations))
	for _, item := range out.Dispatches {
		dispatchIDs[item.DispatchID] = migratedProblemSourceArchiveID(
			"dispatch", targetAgent, item.DispatchID,
		)
	}
	for _, item := range out.HomeworkSubmissions {
		homeworkIDs[item.SubmissionID] = migratedProblemSourceArchiveID(
			"homework", targetAgent, item.SubmissionID,
		)
	}
	for _, item := range out.ActionReceipts {
		receiptIDs[item.CommandReceiptID] = migratedProblemSourceArchiveID(
			"receipt", targetAgent, item.CommandReceiptID,
		)
	}
	for _, item := range out.ReprocessJobs {
		workIDs[item.WorkID] = migratedProblemSourceArchiveID(
			"work", targetAgent, item.WorkID,
		)
	}
	for _, item := range out.ModelInvocations {
		parentIDs[item.InvocationID] = migratedProblemSourceArchiveID(
			"model", targetAgent, item.InvocationID,
		)
	}
	for _, item := range out.ModelPhysicalInvocations {
		physicalIDs[item.PhysicalInvocationID] = migratedProblemSourceArchiveID(
			"physical", targetAgent, item.PhysicalInvocationID,
		)
	}

	for index := range out.PageAssets {
		item := &out.PageAssets[index]
		mapped, err := migratedProblemSourceAssetID(item.PageAssetID, assetIDs)
		if err != nil {
			return ProblemSourceArchiveV6{}, err
		}
		item.AgentName = targetAgent
		item.PageAssetID = mapped
	}
	for index := range out.Dispatches {
		item := &out.Dispatches[index]
		oldID := item.DispatchID
		item.DispatchID = dispatchIDs[oldID]
		item.AgentName = targetAgent
		var err error
		item.SourceRef, err = migrateProblemSourceArchiveAssetRef(item.SourceRef, assetIDs)
		if err != nil {
			return ProblemSourceArchiveV6{}, err
		}
		item.SourceAssetRefs, err = migrateProblemSourceArchiveAssetRefs(
			item.SourceAssetRefs, assetIDs,
		)
		if err != nil {
			return ProblemSourceArchiveV6{}, err
		}
		if item.TargetObjectType == k12.ImageTaskTargetHomeworkSubmission {
			if mapped := homeworkIDs[item.TargetObjectID]; mapped != "" {
				item.TargetObjectID = mapped
			} else if item.TargetObjectID != "" {
				item.TargetObjectID = migratedProblemSourceArchiveID(
					"homework", targetAgent, item.TargetObjectID,
				)
			}
		}
		if item.ClassificationInvocationID != "" {
			item.ClassificationInvocationID = migratedProblemSourceArchiveID(
				"classification", targetAgent, item.ClassificationInvocationID,
			)
		}
		item.IdempotencyKey = migratedProblemSourceArchiveID(
			"dispatch-key", targetAgent, oldID,
		)
		item.RequestDigest = migratedImageTaskDispatchDigest(*item)
	}
	for index := range out.DispatchOwners {
		item := &out.DispatchOwners[index]
		item.DispatchID = dispatchIDs[item.DispatchID]
		item.AgentName = targetAgent
	}
	for index := range out.HomeworkSubmissions {
		item := &out.HomeworkSubmissions[index]
		oldID := item.SubmissionID
		item.SubmissionID = homeworkIDs[oldID]
		item.DispatchID = dispatchIDs[item.DispatchID]
		item.AgentName = targetAgent
		var err error
		item.SourceRef, err = migrateProblemSourceArchiveAssetRef(item.SourceRef, assetIDs)
		if err != nil {
			return ProblemSourceArchiveV6{}, err
		}
		item.SourceAssetRefs, err = migrateProblemSourceArchiveAssetRefs(
			item.SourceAssetRefs, assetIDs,
		)
		if err != nil {
			return ProblemSourceArchiveV6{}, err
		}
		item.IdempotencyKey = migratedProblemSourceArchiveID(
			"homework-key", targetAgent, oldID,
		)
	}
	for index := range out.StructureSnapshots {
		out.StructureSnapshots[index].AgentName = targetAgent
	}
	for index := range out.StructureMembers {
		out.StructureMembers[index].AgentName = targetAgent
	}
	for index := range out.DependencyGroups {
		out.DependencyGroups[index].AgentName = targetAgent
	}

	receiptDigests := make(map[string]string, len(out.ActionReceipts))
	receiptResultRevision := make(map[string]int, len(out.ActionReceipts))
	for index := range out.ActionReceipts {
		item := &out.ActionReceipts[index]
		oldReceiptID := item.CommandReceiptID
		oldDispatchID := item.DispatchID
		request, payload, err := migrateProblemSourceActionRequest(
			item.RequestJSON, item.Action, assetIDs,
		)
		if err != nil {
			return ProblemSourceArchiveV6{}, err
		}
		item.CommandReceiptID = receiptIDs[oldReceiptID]
		item.DispatchID = dispatchIDs[oldDispatchID]
		item.AgentName = targetAgent
		item.IdempotencyKey = migratedProblemSourceArchiveID(
			"receipt-key", targetAgent, oldReceiptID,
		)
		item.RequestJSON = request
		item.RequestDigest, err = problemSourceActionDigest(
			ProblemSourceActionCommand{
				OwnerScope: item.OwnerScope, DispatchID: item.DispatchID,
				ProblemID: item.ProblemID, Action: item.Action,
				StructureVersion:      item.StructureVersion,
				ExpectedInputRevision: item.ExpectedInputRevision,
				Payload:               payload,
			},
			targetAgent,
		)
		if err != nil {
			return ProblemSourceArchiveV6{}, err
		}
		item.ResponseJSON, err = migrateProblemSourceFrozenResponse(
			item.ResponseJSON, item.CommandReceiptID, item.DispatchID, assetIDs,
		)
		if err != nil {
			return ProblemSourceArchiveV6{}, err
		}
		receiptDigests[oldReceiptID] = item.RequestDigest
		receiptResultRevision[oldReceiptID] = item.ResultInputRevision
	}

	for index := range out.InputRevisions {
		item := &out.InputRevisions[index]
		item.AgentName = targetAgent
		mapped, err := migratedProblemSourceAssetID(item.PageAssetID, assetIDs)
		if err != nil {
			return ProblemSourceArchiveV6{}, err
		}
		item.PageAssetID = mapped
		oldReceiptID := item.OriginCommandReceiptID
		if oldReceiptID != "" {
			item.OriginCommandReceiptID = receiptIDs[oldReceiptID]
			if item.InputRevision == receiptResultRevision[oldReceiptID] {
				item.InputDigest = problemSourceInputDigest(
					receiptDigests[oldReceiptID], item.ProblemID, item.InputRevision,
				)
			}
		}
	}

	workByID := make(map[string]ProblemSourceReprocessJob, len(out.ReprocessJobs))
	for index := range out.ReprocessJobs {
		item := &out.ReprocessJobs[index]
		oldWorkID := item.WorkID
		oldReceiptID := item.CommandReceiptID
		request, _, err := migrateProblemSourceActionRequest(
			item.RequestJSON, item.Action, assetIDs,
		)
		if err != nil {
			return ProblemSourceArchiveV6{}, err
		}
		item.WorkID = workIDs[oldWorkID]
		item.CommandReceiptID = receiptIDs[oldReceiptID]
		item.AgentName = targetAgent
		item.DispatchID = dispatchIDs[item.DispatchID]
		item.RequestJSON = request
		item.InputDigest = problemSourceInputDigest(
			receiptDigests[oldReceiptID],
			strings.Join(item.AffectedProblemIDs, "\x00"),
			item.InputRevision,
		)
		item.LeaseOwner = ""
		item.LeaseExpiresAtMilli = 0
		item.ReconciliationOwner = ""
		item.ReconciliationExpiresAtMilli = 0
		workByID[item.WorkID] = problemSourceArchiveJob(*item)
	}

	modelByID := make(map[string]int, len(out.ModelInvocations))
	for index := range out.ModelInvocations {
		item := &out.ModelInvocations[index]
		item.InvocationID = parentIDs[item.InvocationID]
		item.AgentName = targetAgent
		if item.ResultJSON != "" {
			migratedResult, migrateErr := migratedProblemSourceSummaryResult(
				item.ResultJSON,
				item.JobID,
				authoritativeSubmissions[item.JobID],
			)
			if migrateErr != nil {
				return ProblemSourceArchiveV6{}, migrateErr
			}
			item.ResultJSON = migratedResult
			item.ResultDigest = problemSourceArchiveModelResultDigest(migratedResult)
		}
		if item.Stage != k12.GradingStageProjecting ||
			item.Status != k12.ModelInvocationSucceeded || item.ResultJSON == "" {
			item.Status = k12.ModelInvocationReconciled
		}
		item.ProviderIdempotencyKey = ""
		item.ExternalRequestID = ""
		item.FailureKind = ""
		modelByID[item.InvocationID] = index
	}
	for index := range out.ModelPhysicalInvocations {
		item := &out.ModelPhysicalInvocations[index]
		oldRequestDigest := item.RequestDigest
		item.PhysicalInvocationID = physicalIDs[item.PhysicalInvocationID]
		item.ParentInvocationID = parentIDs[item.ParentInvocationID]
		item.AgentName = targetAgent
		item.RequestDigest = migratedProblemSourceArchiveDigest(
			"physical-request", targetAgent,
			oldRequestDigest+"\x00"+item.ParentInvocationID+"\x00"+item.PhysicalInvocationID,
		)
		item.Status = k12.ModelInvocationReconciled
		item.ExternalRequestID = ""
		item.FailureKind = ""
	}
	for index := range out.FinalizationGenerations {
		state := &out.FinalizationGenerations[index]
		state.AgentName = targetAgent
		if state.Artifact == nil {
			continue
		}
		artifact := state.Artifact
		artifact.AgentName = targetAgent
		if artifact.SummaryInvocationID != "" {
			if problemSourceFinalArtifactDigest(*artifact) != artifact.ArtifactDigest {
				return ProblemSourceArchiveV6{}, fmt.Errorf(
					"problem-source final artifact %q digest is not canonical",
					artifact.ArtifactID,
				)
			}
			mapped := parentIDs[artifact.SummaryInvocationID]
			if mapped == "" {
				return ProblemSourceArchiveV6{}, fmt.Errorf(
					"problem-source final artifact summary invocation %q is not preserved",
					artifact.SummaryInvocationID,
				)
			}
			artifact.SummaryInvocationID = mapped
			artifact.ArtifactDigest = problemSourceFinalArtifactDigest(*artifact)
		}
		artifact.ArtifactID = migratedProblemSourceFinalArtifactID(
			targetAgent, artifact.JobID, artifact.StructureVersion,
			artifact.ArtifactDigest,
		)
	}
	for index := range out.RecognitionPhysicalResults {
		item := &out.RecognitionPhysicalResults[index]
		item.WorkID = workIDs[item.WorkID]
		item.ParentInvocationID = parentIDs[item.ParentInvocationID]
		item.PhysicalInvocationID = physicalIDs[item.PhysicalInvocationID]
	}
	for index := range out.RecognitionItems {
		item := &out.RecognitionItems[index]
		item.WorkID = workIDs[item.WorkID]
		item.AgentName = targetAgent
		mapped, err := migratedProblemSourceAssetID(item.PageAssetID, assetIDs)
		if err != nil {
			return ProblemSourceArchiveV6{}, err
		}
		item.PageAssetID = mapped
	}
	for index := range out.RecognitionResults {
		item := &out.RecognitionResults[index]
		item.WorkID = workIDs[item.WorkID]
		item.CommandReceiptID = receiptIDs[item.CommandReceiptID]
		item.AgentName = targetAgent
		item.DispatchID = dispatchIDs[item.DispatchID]
		item.ParentInvocationID = parentIDs[item.ParentInvocationID]
		modelIndex, ok := modelByID[item.ParentInvocationID]
		if !ok {
			return ProblemSourceArchiveV6{}, fmt.Errorf(
				"problem-source result parent %q is missing", item.ParentInvocationID,
			)
		}
		work, ok := workByID[item.WorkID]
		if !ok {
			return ProblemSourceArchiveV6{}, fmt.Errorf(
				"problem-source result work %q is missing", item.WorkID,
			)
		}
		parent := &out.ModelInvocations[modelIndex]
		parentDigest, err := ProblemSourceRecognitionParentRequestDigest(
			work, parent.RouteSnapshot, parent.RequestPolicySnapshot,
		)
		if err != nil {
			return ProblemSourceArchiveV6{}, err
		}
		parent.RequestDigest = parentDigest
		item.ParentRequestDigest = parentDigest
		resultDigest, err := migratedProblemSourceRecognitionDigest(
			*item, out.RecognitionItems, out.RecognitionPhysicalResults,
		)
		if err != nil {
			return ProblemSourceArchiveV6{}, err
		}
		item.ResultDigest = resultDigest
		for itemIndex := range out.RecognitionItems {
			fact := &out.RecognitionItems[itemIndex]
			if fact.WorkID == item.WorkID {
				fact.InputDigest = problemSourceInputDigest(
					resultDigest, fact.ProblemID, fact.ResultInputRevision,
				)
			}
		}
	}
	// A V73-generated immutable input revision is bound to the rewritten
	// aggregate digest, not to the source action receipt digest.
	resultInputDigests := make(map[string]string, len(out.RecognitionItems))
	for _, item := range out.RecognitionItems {
		resultInputDigests[problemSourceArchiveInputKey(
			item.SubmissionID, item.StructureVersion, item.ProblemID,
			item.ResultInputRevision,
		)] = item.InputDigest
	}
	for index := range out.InputRevisions {
		item := &out.InputRevisions[index]
		if digest := resultInputDigests[problemSourceArchiveInputKey(
			item.SubmissionID, item.StructureVersion, item.ProblemID,
			item.InputRevision,
		)]; digest != "" {
			item.InputDigest = digest
		}
	}

	out = NormalizeProblemSourceArchiveV6ForRestore(out)
	if err := ValidateProblemSourceArchiveV6(targetAgent, out); err != nil {
		return ProblemSourceArchiveV6{}, err
	}
	return out, nil
}

func migratedProblemSourceSummaryResult(
	raw string,
	jobID string,
	submissionID string,
) (string, error) {
	jobID = strings.TrimSpace(jobID)
	submissionID = strings.TrimSpace(submissionID)
	if jobID == "" || submissionID == "" {
		return "", fmt.Errorf(
			"problem-source typed summary has no authoritative target job/submission identity",
		)
	}
	summary, err := decodeProblemSourceArchiveSummaryResult(raw)
	if err != nil {
		return "", fmt.Errorf("decode problem-source typed summary: %w", err)
	}
	summary.GradingJobID = jobID
	summary.SubmissionID = submissionID
	encoded, err := json.Marshal(summary)
	if err != nil {
		return "", fmt.Errorf("encode migrated problem-source typed summary: %w", err)
	}
	return string(encoded), nil
}

func problemSourceFinalArtifactDigest(artifact k12.GradingFinalArtifact) string {
	raw, _ := json.Marshal(struct {
		StructureVersion          int
		CoverageStatus            k12.GradingFinalArtifactCoverageStatus
		TotalCount                int
		PublishedCount            int
		SkippedCount              int
		OrderedCurrentDigestsJSON string
		CanonicalMarkdown         string
		SummaryInvocationID       string
	}{
		artifact.StructureVersion,
		artifact.CoverageStatus,
		artifact.TotalCount,
		artifact.PublishedCount,
		artifact.SkippedCount,
		artifact.OrderedCurrentDigestsJSON,
		artifact.CanonicalMarkdown,
		artifact.SummaryInvocationID,
	})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func migratedProblemSourceFinalArtifactID(
	agentName string,
	jobID string,
	structureVersion int,
	artifactDigest string,
) string {
	sum := sha256.Sum256([]byte(
		agentName + "\x00" + jobID + "\x00" +
			strconv.Itoa(structureVersion) + "\x00" + artifactDigest,
	))
	return "grading-final-" + hex.EncodeToString(sum[:])
}

func problemSourceArchiveWorkIsLive(work ProblemSourceArchiveReprocessJob) bool {
	switch work.Status {
	case ProblemSourceReprocessPrepared,
		ProblemSourceReprocessQueued,
		ProblemSourceReprocessRunning,
		ProblemSourceReprocessOutcomeUnknown:
		return true
	case ProblemSourceReprocessFailed:
		return work.NextAttemptAtMilli > 0
	default:
		return false
	}
}

func migratedProblemSourceArchiveID(kind, targetAgent, sourceID string) string {
	return "hexbakv6-" + kind + "-" + migratedProblemSourceArchiveDigest(
		kind, targetAgent, sourceID,
	)[len("sha256:"):len("sha256:")+32]
}

func migratedProblemSourceArchiveDigest(kind, targetAgent, source string) string {
	sum := sha256.Sum256([]byte(
		"hexclaw.hexbak.v6.restore_as." + kind + "\x00" +
			targetAgent + "\x00" + source,
	))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func migratedProblemSourceAssetID(source string, mapping map[string]string) (string, error) {
	if target := strings.TrimSpace(mapping[source]); target != "" {
		return target, nil
	}
	return "", fmt.Errorf("problem-source PageAsset %q has no target mapping", source)
}

func migrateProblemSourceArchiveAssetRef(source string, mapping map[string]string) (string, error) {
	if target := mapping[source]; target != "" {
		return target, nil
	}
	if strings.HasPrefix(strings.TrimSpace(source), "asset://") {
		return "", fmt.Errorf("problem-source asset reference %q has no target mapping", source)
	}
	return source, nil
}

func migrateProblemSourceArchiveAssetRefs(source []string, mapping map[string]string) ([]string, error) {
	out := make([]string, len(source))
	for index, item := range source {
		var err error
		out[index], err = migrateProblemSourceArchiveAssetRef(item, mapping)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func migratedImageTaskDispatchDigest(dispatch k12.ImageTaskDispatch) string {
	var payload any
	if dispatch.CreativeEntry != nil {
		payload = struct {
			Agent, Learner, Source, Session, Message, SourceDigest string
			Assets                                                 []string
			RouteRequest                                           k12.ImageTaskRouteSnapshot
			CreativeEntry                                          *k12.ImageTaskCreativeEntry
		}{
			dispatch.AgentName, dispatch.LearnerID, dispatch.SourceRef,
			dispatch.SourceSessionID, dispatch.MessageIntent, dispatch.SourceDigest,
			dispatch.SourceAssetRefs, dispatch.OperationRouteRequest,
			dispatch.CreativeEntry,
		}
	} else {
		payload = struct {
			Agent, Learner, Source, Session, Message, SourceDigest string
			Assets                                                 []string
			Route                                                  k12.ImageTaskRouteSnapshot
			CreativeEntry                                          *k12.ImageTaskCreativeEntry
		}{
			dispatch.AgentName, dispatch.LearnerID, dispatch.SourceRef,
			dispatch.SourceSessionID, dispatch.MessageIntent, dispatch.SourceDigest,
			dispatch.SourceAssetRefs, dispatch.ClassificationRouteSnapshot,
			dispatch.CreativeEntry,
		}
	}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type problemSourceArchiveActionRequest struct {
	Action                string          `json:"action"`
	StructureVersion      int             `json:"structure_version"`
	ExpectedInputRevision int             `json:"expected_input_revision"`
	Payload               json.RawMessage `json:"payload"`
}

func migrateProblemSourceActionRequest(
	raw json.RawMessage,
	action string,
	assetIDs map[string]string,
) (json.RawMessage, json.RawMessage, error) {
	var request problemSourceArchiveActionRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, nil, fmt.Errorf("decode problem-source action request: %w", err)
	}
	if request.Action != action {
		return nil, nil, fmt.Errorf("problem-source action request identity mismatch")
	}
	switch action {
	case "select_region":
		var payload selectRegionProblemSourceActionPayload
		if err := json.Unmarshal(request.Payload, &payload); err != nil {
			return nil, nil, err
		}
		mapped, err := migratedProblemSourceAssetID(payload.PageAssetID, assetIDs)
		if err != nil {
			return nil, nil, err
		}
		payload.PageAssetID = mapped
		request.Payload, _ = json.Marshal(payload)
	case "retake":
		var payload retakeProblemSourceActionPayload
		if err := json.Unmarshal(request.Payload, &payload); err != nil {
			return nil, nil, err
		}
		mapped, err := migratedProblemSourceAssetID(payload.PageAssetID, assetIDs)
		if err != nil {
			return nil, nil, err
		}
		payload.PageAssetID = mapped
		request.Payload, _ = json.Marshal(payload)
	}
	canonical, err := canonicalProblemSourceActionPayload(action, request.Payload)
	if err != nil {
		return nil, nil, err
	}
	request.Payload = canonical
	migrated, err := json.Marshal(request)
	if err != nil {
		return nil, nil, err
	}
	return migrated, canonical, nil
}

func migrateProblemSourceFrozenResponse(
	raw json.RawMessage,
	receiptID string,
	dispatchID string,
	assetIDs map[string]string,
) (json.RawMessage, error) {
	frozen, err := viewcontract.ParseFrozenProblemSourceActionResponse(raw)
	if err != nil {
		return nil, err
	}
	frozen.CommandReceiptID = receiptID
	frozen.DispatchID = dispatchID
	for index := range frozen.ProgressiveSnapshot.ProblemProgress {
		progress := &frozen.ProgressiveSnapshot.ProblemProgress[index]
		if progress.PageAssetID == "" {
			continue
		}
		progress.PageAssetID, err = migratedProblemSourceAssetID(
			progress.PageAssetID, assetIDs,
		)
		if err != nil {
			return nil, err
		}
	}
	migrated, err := viewcontract.FreezeProblemSourceActionResponse(
		frozen.ProblemSourceActionResponse,
	)
	if err != nil {
		return nil, err
	}
	return migrated.JSON, nil
}

func problemSourceArchiveJob(item ProblemSourceArchiveReprocessJob) ProblemSourceReprocessJob {
	return ProblemSourceReprocessJob{
		WorkID: item.WorkID, CommandReceiptID: item.CommandReceiptID,
		OwnerScope: item.OwnerScope, AgentName: item.AgentName,
		DispatchID: item.DispatchID, JobID: item.JobID, ProblemID: item.ProblemID,
		Action: item.Action, StructureVersion: item.StructureVersion,
		InputRevision: item.InputRevision, InputDigest: item.InputDigest,
		AffectedProblemIDs: append([]string(nil), item.AffectedProblemIDs...),
		RequestJSON:        append(json.RawMessage(nil), item.RequestJSON...),
		Status:             item.Status, LeaseOwner: item.LeaseOwner, LeaseEpoch: item.LeaseEpoch,
		LeaseExpiresAtMilli: item.LeaseExpiresAtMilli, AttemptCount: item.AttemptCount,
		NextAttemptAtMilli:           item.NextAttemptAtMilli,
		ReconciliationOwner:          item.ReconciliationOwner,
		ReconciliationEpoch:          item.ReconciliationEpoch,
		ReconciliationExpiresAtMilli: item.ReconciliationExpiresAtMilli,
		ReconciliationAttemptCount:   item.ReconciliationAttemptCount,
		NextReconcileAtMilli:         item.NextReconcileAtMilli,
		FailureCode:                  item.FailureCode, FailureDetail: item.FailureDetail,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
	}
}

func migratedProblemSourceRecognitionDigest(
	result ProblemSourceArchiveRecognitionResult,
	allItems []ProblemSourceArchiveRecognitionItem,
	allPhysical []ProblemSourceArchiveRecognitionPhysicalResult,
) (string, error) {
	input, err := problemSourceArchiveRecognitionInput(result, allItems, allPhysical)
	if err != nil {
		return "", err
	}
	_, digest, err := normalizeProblemSourceRecognitionResult(input)
	return digest, err
}

func problemSourceArchiveRecognitionInput(
	result ProblemSourceArchiveRecognitionResult,
	allItems []ProblemSourceArchiveRecognitionItem,
	allPhysical []ProblemSourceArchiveRecognitionPhysicalResult,
) (ProblemSourceRecognitionResult, error) {
	input := ProblemSourceRecognitionResult{
		MappingState:       ProblemSourceRecognitionMappingState(result.MappingState),
		ParentInvocationID: result.ParentInvocationID,
	}
	physical := make([]ProblemSourceArchiveRecognitionPhysicalResult, 0)
	for _, item := range allPhysical {
		if item.WorkID == result.WorkID {
			physical = append(physical, item)
		}
	}
	sort.Slice(physical, func(left, right int) bool {
		return physical[left].Ordinal < physical[right].Ordinal
	})
	for _, item := range physical {
		input.PhysicalResults = append(input.PhysicalResults,
			ProblemSourceRecognitionPhysicalResultRef{
				PhysicalInvocationID: item.PhysicalInvocationID,
				PhysicalUnit:         item.PhysicalUnit, ResultDigest: item.ResultDigest,
			},
		)
	}
	items := make([]ProblemSourceArchiveRecognitionItem, 0)
	for _, item := range allItems {
		if item.WorkID == result.WorkID {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(left, right int) bool {
		return items[left].Ordinal < items[right].Ordinal
	})
	for _, item := range items {
		converted, err := problemSourceArchiveRecognitionItem(item)
		if err != nil {
			return ProblemSourceRecognitionResult{}, err
		}
		input.Items = append(input.Items, converted)
	}
	return input, nil
}

func problemSourceArchiveRecognitionItem(
	item ProblemSourceArchiveRecognitionItem,
) (ProblemSourceRecognitionItem, error) {
	converted := ProblemSourceRecognitionItem{
		ProblemID: item.ProblemID, StemRaw: item.StemRaw,
		QuestionCanonicalMarkdown: item.QuestionCanonicalMarkdown,
		AnswerState:               item.AnswerState, AnswerRaw: item.AnswerRaw,
		AnswerCanonicalMarkdown: item.AnswerCanonicalMarkdown,
		Subject:                 item.Subject, RecognitionConfidence: item.RecognitionConfidence,
		ConfirmationRequired: item.ConfirmationRequired,
	}
	if item.AnswerBBoxJSON != "" {
		var bbox k12.AttemptBBox
		if err := json.Unmarshal([]byte(item.AnswerBBoxJSON), &bbox); err != nil {
			return ProblemSourceRecognitionItem{}, err
		}
		converted.AnswerBBox = &bbox
	}
	arrays := []struct {
		raw    json.RawMessage
		target *[]string
	}{
		{item.KnowledgePointsJSON, &converted.KnowledgePoints},
		{item.OCRSignalsJSON, &converted.OCRSignals},
		{item.EvidenceTranscriptionsJSON, &converted.EvidenceTranscriptions},
		{item.AnswerEvidenceTranscriptionsJSON, &converted.AnswerEvidenceTranscriptions},
		{item.ConfirmationReasonsJSON, &converted.ConfirmationReasons},
	}
	for _, array := range arrays {
		if err := json.Unmarshal(array.raw, array.target); err != nil {
			return ProblemSourceRecognitionItem{}, err
		}
	}
	return converted, nil
}

func problemSourceArchiveInputKey(
	submissionID string,
	structureVersion int,
	problemID string,
	inputRevision int,
) string {
	return fmt.Sprintf(
		"%s\x00%d\x00%s\x00%d",
		submissionID, structureVersion, problemID, inputRevision,
	)
}
