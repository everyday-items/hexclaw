package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

var (
	ErrGradingPhysicalCallOutcomeUnknown = errors.New("grading physical call outcome unknown")
	ErrGradingGroundingUnavailable       = errors.New("grading grounding unavailable")
)

type GradingPhysicalCallSpec struct {
	Operation     k12.GradingItemOperation
	RequestDigest string
}

type GradingPhysicalCallResult struct {
	Payload      string
	InvocationID string
}

// GradingPhysicalCallExecutor is the shared durable boundary that authorizes
// exactly one physical solve/verify/grade send for a K12 grading item.
type GradingPhysicalCallExecutor interface {
	ExecuteGradingPhysicalCall(
		context.Context,
		GradingPhysicalCallSpec,
		func(context.Context) (string, error),
	) (GradingPhysicalCallResult, error)
}

type gradingPhysicalCallContextKey struct{}

func withGradingPhysicalCallExecutor(
	ctx context.Context,
	executor GradingPhysicalCallExecutor,
) context.Context {
	return WithGradingPhysicalCallExecutor(ctx, executor)
}

// WithGradingPhysicalCallExecutor binds the canonical durable physical-call
// executor to a request context. Engine adapters use the same fact to retain a
// frozen assessing deadline; they must not manufacture a second executor.
func WithGradingPhysicalCallExecutor(
	ctx context.Context,
	executor GradingPhysicalCallExecutor,
) context.Context {
	return context.WithValue(ctx, gradingPhysicalCallContextKey{}, executor)
}

func HasGradingPhysicalCallExecutor(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	executor, ok := ctx.Value(gradingPhysicalCallContextKey{}).(GradingPhysicalCallExecutor)
	return ok && executor != nil
}

const gradingGroundedPhysicalSchema = "k12_grading_grounded_physical_v1"

type gradingGroundingContextKey struct{}

type gradingProviderGrounding struct {
	snapshot       GroundingSnapshot
	text           string
	receipts       []GroundingEvidenceReceipt
	identityDigest string
}

type gradingStoredGrounding struct {
	Snapshot       GroundingSnapshot          `json:"snapshot"`
	Receipts       []GroundingEvidenceReceipt `json:"receipts"`
	IdentityDigest string                     `json:"identity_digest"`
}

type gradingGroundedPhysicalEnvelope struct {
	Schema    string                 `json:"schema"`
	Payload   json.RawMessage        `json:"payload"`
	Grounding gradingStoredGrounding `json:"grounding"`
}

// GradingGroundingForProvider 只向 Provider 适配器暴露已核验教材正文。
// 适配器不得自行读取可变教材状态，也不得把持久回执标识写入生成内容。
func GradingGroundingForProvider(ctx context.Context) (text string, ok bool) {
	if ctx == nil {
		return "", false
	}
	evidence, ok := ctx.Value(gradingGroundingContextKey{}).(gradingProviderGrounding)
	if !ok || strings.TrimSpace(evidence.text) == "" ||
		strings.TrimSpace(evidence.identityDigest) == "" {
		return "", false
	}
	return evidence.text, true
}

// WithVerifiedGradingGrounding 把已经过 pinned scope 校验的教材命中绑定到 Provider 上下文。
// 该入口只供适配器测试和批改编排复用，未命中或无完整引用时一律拒绝下发。
func WithVerifiedGradingGrounding(
	ctx context.Context,
	snapshot GroundingSnapshot,
	result GroundingSnapshotResult,
) (context.Context, error) {
	evidence, err := newGradingProviderGrounding(snapshot, result)
	if err != nil {
		return ctx, err
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: grounding context is nil", ErrGradingGroundingUnavailable)
	}
	return withGradingProviderGrounding(ctx, evidence), nil
}

func withGradingProviderGrounding(
	ctx context.Context,
	evidence gradingProviderGrounding,
) context.Context {
	return context.WithValue(ctx, gradingGroundingContextKey{}, evidence)
}

func gradingGroundingIdentityDigest(
	snapshot GroundingSnapshot,
	receipts []GroundingEvidenceReceipt,
) (string, error) {
	raw, err := json.Marshal(struct {
		Snapshot GroundingSnapshot          `json:"snapshot"`
		Receipts []GroundingEvidenceReceipt `json:"receipts"`
	}{snapshot, receipts})
	if err != nil {
		return "", err
	}
	return modelInvocationDigest([]byte("k12-grading-grounding-v1"), raw), nil
}

func newGradingProviderGrounding(
	snapshot GroundingSnapshot,
	result GroundingSnapshotResult,
) (gradingProviderGrounding, error) {
	if err := validateGradingGroundingSnapshot(snapshot); err != nil {
		return gradingProviderGrounding{}, err
	}
	if err := result.validate(snapshot); err != nil {
		return gradingProviderGrounding{}, fmt.Errorf(
			"%w: %v", ErrGradingGroundingUnavailable, err,
		)
	}
	if !result.Found {
		return gradingProviderGrounding{}, fmt.Errorf(
			"%w: pinned textbook query returned no evidence",
			ErrGradingGroundingUnavailable,
		)
	}
	digest, err := gradingGroundingIdentityDigest(snapshot, result.Receipts)
	if err != nil {
		return gradingProviderGrounding{}, err
	}
	return gradingProviderGrounding{
		snapshot:       cloneGradingGroundingSnapshot(snapshot),
		text:           strings.TrimSpace(result.Text),
		receipts:       cloneGroundingEvidenceReceipts(result.Receipts),
		identityDigest: digest,
	}, nil
}

func gradingProviderGroundingFromContext(
	ctx context.Context,
) (gradingProviderGrounding, bool) {
	if ctx == nil {
		return gradingProviderGrounding{}, false
	}
	evidence, ok := ctx.Value(gradingGroundingContextKey{}).(gradingProviderGrounding)
	if !ok || strings.TrimSpace(evidence.text) == "" ||
		strings.TrimSpace(evidence.identityDigest) == "" || len(evidence.receipts) == 0 {
		return gradingProviderGrounding{}, false
	}
	evidence.snapshot = cloneGradingGroundingSnapshot(evidence.snapshot)
	evidence.receipts = cloneGroundingEvidenceReceipts(evidence.receipts)
	return evidence, true
}

func cloneGradingGroundingSnapshot(snapshot GroundingSnapshot) GroundingSnapshot {
	snapshot.SegmentRefs = append([]string(nil), snapshot.SegmentRefs...)
	snapshot.PageRefs = append([]k12.TextbookGroundingPageRef(nil), snapshot.PageRefs...)
	for index := range snapshot.PageRefs {
		snapshot.PageRefs[index].SegmentRefs = append(
			[]string(nil), snapshot.PageRefs[index].SegmentRefs...,
		)
	}
	return snapshot
}

func validateGradingGroundingSnapshot(snapshot GroundingSnapshot) error {
	if snapshot.AgentName == "" || snapshot.AgentName != strings.TrimSpace(snapshot.AgentName) ||
		snapshot.LearnerID == "" || snapshot.LearnerID != strings.TrimSpace(snapshot.LearnerID) ||
		snapshot.Subject != "数学" || strings.TrimSpace(snapshot.VectorRevisionID) == "" ||
		snapshot.VectorRevisionID != strings.TrimSpace(snapshot.VectorRevisionID) {
		return fmt.Errorf("%w: frozen textbook snapshot is incomplete", ErrGradingGroundingUnavailable)
	}
	if err := k12.ValidateTextbookGroundingScope(k12.TextbookGroundingScope{
		TextbookBindingID:  snapshot.TextbookBindingID,
		TextbookManifestID: snapshot.TextbookManifestID,
		DocumentID:         snapshot.DocumentID,
		DocumentGeneration: snapshot.DocumentGeneration,
		SourceDigest:       snapshot.SourceDigest,
		Edition:            snapshot.Edition,
		Volume:             snapshot.Volume,
		SegmentRefs:        append([]string(nil), snapshot.SegmentRefs...),
		PageRefs:           append([]k12.TextbookGroundingPageRef(nil), snapshot.PageRefs...),
	}); err != nil {
		return fmt.Errorf("%w: %v", ErrGradingGroundingUnavailable, err)
	}
	return nil
}

func validateFrozenGradingGrounding(
	requested GroundingSnapshot,
	frozen GroundingSnapshot,
) error {
	if err := validateGradingGroundingSnapshot(frozen); err != nil {
		return err
	}
	withoutRevision := cloneGradingGroundingSnapshot(frozen)
	withoutRevision.VectorRevisionID = ""
	if !reflect.DeepEqual(requested, withoutRevision) {
		return fmt.Errorf(
			"%w: grounding freeze changed the requested textbook scope",
			ErrGradingGroundingUnavailable,
		)
	}
	return nil
}

func validateStoredGradingGrounding(value gradingStoredGrounding) error {
	if strings.TrimSpace(value.IdentityDigest) == "" || len(value.Receipts) == 0 {
		return fmt.Errorf("%w: stored grounding identity is incomplete", ErrGradingGroundingUnavailable)
	}
	probe := GroundingSnapshotResult{
		Text: "verified", Found: true,
		Receipts: cloneGroundingEvidenceReceipts(value.Receipts),
	}
	if err := probe.validate(value.Snapshot); err != nil {
		return fmt.Errorf("%w: %v", ErrGradingGroundingUnavailable, err)
	}
	digest, err := gradingGroundingIdentityDigest(value.Snapshot, value.Receipts)
	if err != nil {
		return err
	}
	if digest != value.IdentityDigest {
		return fmt.Errorf("%w: stored grounding digest mismatch", ErrGradingGroundingUnavailable)
	}
	return nil
}

func encodeGroundedPhysicalPayload(
	payload string,
	evidence gradingProviderGrounding,
) (string, error) {
	if !json.Valid([]byte(payload)) {
		return "", fmt.Errorf("physical result is not valid JSON")
	}
	stored := gradingStoredGrounding{
		Snapshot:       evidence.snapshot,
		Receipts:       cloneGroundingEvidenceReceipts(evidence.receipts),
		IdentityDigest: evidence.identityDigest,
	}
	if err := validateStoredGradingGrounding(stored); err != nil {
		return "", err
	}
	raw, err := json.Marshal(gradingGroundedPhysicalEnvelope{
		Schema:    gradingGroundedPhysicalSchema,
		Payload:   json.RawMessage(payload),
		Grounding: stored,
	})
	return string(raw), err
}

func decodeGroundedPhysicalPayload(
	stored string,
	expected *gradingProviderGrounding,
) (payload string, grounding *gradingStoredGrounding, enveloped bool, err error) {
	if !json.Valid([]byte(stored)) {
		return "", nil, false, fmt.Errorf("physical result is not valid JSON")
	}
	var envelope gradingGroundedPhysicalEnvelope
	if unmarshalErr := json.Unmarshal([]byte(stored), &envelope); unmarshalErr != nil ||
		envelope.Schema != gradingGroundedPhysicalSchema {
		if expected != nil {
			return "", nil, false, fmt.Errorf(
				"%w: grounded invocation has no durable evidence envelope",
				ErrModelInvocationRequiresReconciliation,
			)
		}
		return stored, nil, false, nil
	}
	if !json.Valid(envelope.Payload) {
		return "", nil, true, fmt.Errorf(
			"%w: grounded invocation payload is invalid",
			ErrModelInvocationRequiresReconciliation,
		)
	}
	if validateErr := validateStoredGradingGrounding(envelope.Grounding); validateErr != nil {
		return "", nil, true, validateErr
	}
	if expected != nil && envelope.Grounding.IdentityDigest != expected.identityDigest {
		return "", nil, true, fmt.Errorf(
			"%w: grounded invocation evidence identity mismatch",
			ErrModelInvocationRequiresReconciliation,
		)
	}
	value := envelope.Grounding
	return string(envelope.Payload), &value, true, nil
}

type gradingGroundingSessionKey struct {
	records   *k12storage.Store
	agentName string
	jobID     string
}

type gradingGroundingItemState struct {
	mu       sync.Mutex
	resolved bool
	evidence gradingProviderGrounding
	err      error
}

type gradingGroundingSession struct {
	mu             sync.Mutex
	initialized    bool
	required       bool
	snapshot       GroundingSnapshot
	evidenceSource SnapshotGroundingEvidence
	retrieval      *k12storage.Store
	ownerID        string
	agentName      string
	jobID          string
	items          map[string]*gradingGroundingItemState
	err            error
}

type gradingGroundingInvocationInspection struct {
	snapshot           GroundingSnapshot
	found              bool
	relevantSucceeded  int
	envelopedSucceeded int
	directSucceeded    int
}

var gradingGroundingSessions sync.Map

func prepareGradingItemGrounding(
	ctx context.Context,
	deps Deps,
	job GradingJobView,
	q RecognizedQuestion,
	req GradeRequest,
) (context.Context, bool, error) {
	if strings.TrimSpace(req.Subject) != "数学" {
		return ctx, false, nil
	}
	key := gradingGroundingSessionKey{
		records:   deps.Records,
		agentName: strings.TrimSpace(job.Record.AgentName),
		jobID:     strings.TrimSpace(job.Record.RecordID),
	}
	loaded, _ := gradingGroundingSessions.LoadOrStore(key, &gradingGroundingSession{
		items: make(map[string]*gradingGroundingItemState),
	})
	session := loaded.(*gradingGroundingSession)
	if err := session.initialize(ctx, deps, job); err != nil {
		return ctx, session.required, err
	}
	if !session.required {
		return ctx, false, nil
	}
	evidence, err := session.resolveItem(ctx, q, req)
	if err != nil {
		return ctx, true, err
	}
	return withGradingProviderGrounding(ctx, evidence), true, nil
}

func (session *gradingGroundingSession) initialize(
	ctx context.Context,
	deps Deps,
	job GradingJobView,
) error {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.initialized {
		return session.err
	}
	session.initialized = true
	textbookOwnerID, err := resolveGradingGroundingTextbookOwner(ctx, deps, job)
	if err != nil {
		session.required = true
		session.err = err
		return err
	}
	session.retrieval = deps.Records
	session.ownerID = strings.TrimSpace(textbookOwnerID)
	session.agentName = strings.TrimSpace(job.Record.AgentName)
	session.jobID = strings.TrimSpace(job.Record.RecordID)

	inspection, err := inspectGradingGroundingInvocations(
		ctx, deps, job.Record.AgentName, job.Record.RecordID,
	)
	if err != nil {
		session.required = true
		session.err = err
		return err
	}
	if inspection.found {
		evidenceSource, ok := deps.Grounding.(SnapshotGroundingEvidence)
		if !ok {
			session.required = true
			session.err = fmt.Errorf(
				"%w: pinned grounding evidence lookup is unavailable",
				ErrGradingGroundingUnavailable,
			)
			return session.err
		}
		session.required = true
		session.snapshot = cloneGradingGroundingSnapshot(inspection.snapshot)
		session.evidenceSource = evidenceSource
		return nil
	}

	if deps.Records == nil || textbookOwnerID == "" {
		return nil
	}
	textbookScope := k12storage.TextbookScope{
		OwnerID:   textbookOwnerID,
		AgentName: strings.TrimSpace(job.Record.AgentName),
		Subject:   "math",
	}
	scope, found, resolveErr := deps.Records.GetActiveTextbookGroundingScope(ctx, textbookScope)
	if resolveErr != nil {
		session.required = true
		session.err = fmt.Errorf(
			"%w: resolve active textbook scope: %v",
			ErrGradingGroundingUnavailable, resolveErr,
		)
		return session.err
	}
	if !found {
		_, handled, catalogErr := deps.Records.GetActiveTextbookCatalog(ctx, textbookScope)
		if catalogErr != nil || handled {
			session.required = true
			if catalogErr != nil {
				session.err = fmt.Errorf(
					"%w: active textbook binding is incomplete: %v",
					ErrGradingGroundingUnavailable, catalogErr,
				)
			} else {
				session.err = fmt.Errorf(
					"%w: active textbook binding has no verified grounding scope",
					ErrGradingGroundingUnavailable,
				)
			}
			return session.err
		}
		return nil
	}
	session.required = true
	if inspection.directSucceeded > 0 {
		session.err = fmt.Errorf(
			"%w: existing item result has no durable grounding envelope",
			ErrModelInvocationRequiresReconciliation,
		)
		return session.err
	}
	if err := k12.ValidateTextbookGroundingScope(scope); err != nil {
		session.err = fmt.Errorf("%w: %v", ErrGradingGroundingUnavailable, err)
		return session.err
	}
	snapshotter, snapshotOK := deps.Grounding.(SnapshotGrounding)
	evidenceSource, evidenceOK := deps.Grounding.(SnapshotGroundingEvidence)
	if !snapshotOK || !evidenceOK {
		session.err = fmt.Errorf(
			"%w: pinned grounding freeze and evidence lookup are required",
			ErrGradingGroundingUnavailable,
		)
		return session.err
	}
	requested := GroundingSnapshot{
		AgentName:          strings.TrimSpace(job.Record.AgentName),
		LearnerID:          strings.TrimSpace(job.Record.AgentName),
		Subject:            "数学",
		TextbookBindingID:  scope.TextbookBindingID,
		TextbookManifestID: scope.TextbookManifestID,
		DocumentID:         scope.DocumentID,
		DocumentGeneration: scope.DocumentGeneration,
		SourceDigest:       scope.SourceDigest,
		Edition:            scope.Edition,
		Volume:             scope.Volume,
		SegmentRefs:        append([]string(nil), scope.SegmentRefs...),
		PageRefs:           append([]k12.TextbookGroundingPageRef(nil), scope.PageRefs...),
	}
	requested = cloneGradingGroundingSnapshot(requested)
	frozen, freezeErr := snapshotter.FreezeGroundingSnapshot(ctx, requested)
	if freezeErr != nil {
		session.err = fmt.Errorf(
			"%w: freeze textbook grounding: %v",
			ErrGradingGroundingUnavailable, freezeErr,
		)
		return session.err
	}
	if err := validateFrozenGradingGrounding(requested, frozen); err != nil {
		session.err = err
		return err
	}
	session.snapshot = cloneGradingGroundingSnapshot(frozen)
	session.evidenceSource = evidenceSource
	return nil
}

func resolveGradingGroundingTextbookOwner(
	ctx context.Context,
	deps Deps,
	job GradingJobView,
) (string, error) {
	if strings.TrimSpace(job.Fields.SourceKind) != "image_task" {
		return strings.TrimSpace(deps.TextbookOwnerID), nil
	}
	if deps.Records == nil {
		return "", fmt.Errorf(
			"%w: ImageTask owner store is unavailable",
			ErrGradingGroundingUnavailable,
		)
	}
	dispatchID, err := gradingFinalImageTaskDispatchID(job)
	if err != nil {
		return "", fmt.Errorf(
			"%w: ImageTask source identity is invalid: %v",
			ErrGradingGroundingUnavailable, err,
		)
	}
	ownerID, err := deps.Records.GetImageTaskOwnerScope(
		ctx, job.Record.AgentName, dispatchID,
	)
	if err != nil {
		return "", fmt.Errorf(
			"%w: ImageTask owner scope is unavailable: %v",
			ErrGradingGroundingUnavailable, err,
		)
	}
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return "", fmt.Errorf(
			"%w: ImageTask owner scope is empty",
			ErrGradingGroundingUnavailable,
		)
	}
	return ownerID, nil
}

func (session *gradingGroundingSession) resolveItem(
	ctx context.Context,
	q RecognizedQuestion,
	req GradeRequest,
) (gradingProviderGrounding, error) {
	itemKey := modelInvocationDigest(
		[]byte(strings.TrimSpace(q.ProblemID)),
		[]byte(strings.TrimSpace(q.AttemptID)),
		[]byte(fmt.Sprintf("%d", q.ConfirmedVersion)),
		[]byte(strings.TrimSpace(q.InputDigest)),
	)
	session.mu.Lock()
	state := session.items[itemKey]
	if state == nil {
		state = &gradingGroundingItemState{}
		session.items[itemKey] = state
	}
	snapshot := cloneGradingGroundingSnapshot(session.snapshot)
	evidenceSource := session.evidenceSource
	session.mu.Unlock()

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.resolved {
		return state.evidence, state.err
	}
	state.resolved = true
	query := gradingItemGroundingQuery(q, req)
	if session.retrieval != nil && session.ownerID != "" && session.agentName != "" && session.jobID != "" {
		claim := groundingRetrievalClaim(session.ownerID, session.agentName, session.jobID, q, itemKey, snapshot, query)
		invocation, claimErr := session.retrieval.ClaimGroundingRetrievalInvocation(ctx, claim)
		if claimErr == nil {
			if !invocation.Fresh {
				if invocation.Status != k12storage.GroundingRetrievalInvocationStatusSucceeded {
					state.err = fmt.Errorf(
						"%w: grounding retrieval invocation=%s status=%s",
						ErrModelInvocationRequiresReconciliation, invocation.InvocationID, invocation.Status,
					)
					return gradingProviderGrounding{}, state.err
				}
				storedResult, decodeErr := decodeGroundingRetrievalResult(invocation, snapshot, claim)
				if decodeErr != nil {
					state.err = decodeErr
					return gradingProviderGrounding{}, state.err
				}
				state.evidence, state.err = newGradingProviderGrounding(snapshot, storedResult)
				return state.evidence, state.err
			}
			result, queryErr := evidenceSource.GroundSnapshotWithEvidence(
				ctx, snapshot, query, strings.TrimSpace(req.Grade),
			)
			if queryErr != nil {
				if marker, ok := any(session.retrieval).(interface {
					MarkGroundingRetrievalInvocationOutcomeUnknown(context.Context, k12storage.GroundingRetrievalInvocation, string) error
				}); ok {
					_ = marker.MarkGroundingRetrievalInvocationOutcomeUnknown(ctx, invocation, queryErr.Error())
				}
				state.err = fmt.Errorf(
					"%w: pinned textbook query failed: %v",
					ErrGradingGroundingUnavailable, queryErr,
				)
				return gradingProviderGrounding{}, state.err
			}
			evidence, evidenceErr := newGradingProviderGrounding(snapshot, result)
			if evidenceErr != nil {
				state.err = evidenceErr
				return gradingProviderGrounding{}, state.err
			}
			resultJSON, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				state.err = fmt.Errorf("%w: encode grounding retrieval result: %v", ErrGradingGroundingUnavailable, marshalErr)
				return gradingProviderGrounding{}, state.err
			}
			queryReceiptDigest, hitSetDigest, citationSetDigest := groundingRetrievalResultDigests(result)
			if saveErr := session.retrieval.SaveGroundingRetrievalInvocation(ctx, invocation,
				k12storage.GroundingRetrievalInvocationResult{
					ResultJSON: string(resultJSON), QueryReceiptDigest: queryReceiptDigest,
					HitSetDigest: hitSetDigest, CitationSetDigest: citationSetDigest,
				}); saveErr != nil {
				state.err = fmt.Errorf("%w: persist grounding retrieval result: %v", ErrGradingGroundingUnavailable, saveErr)
				return gradingProviderGrounding{}, state.err
			}
			state.evidence = evidence
			return state.evidence, nil
		} else if !errors.Is(claimErr, k12storage.ErrGroundingRetrievalInvocationLedgerUnavailable) &&
			!strings.Contains(strings.ToLower(claimErr.Error()), "no such table") {
			state.err = fmt.Errorf("%w: claim grounding retrieval invocation: %v", ErrGradingGroundingUnavailable, claimErr)
			return gradingProviderGrounding{}, state.err
		}
	}
	result, err := evidenceSource.GroundSnapshotWithEvidence(
		ctx, snapshot, query, strings.TrimSpace(req.Grade),
	)
	if err != nil {
		state.err = fmt.Errorf(
			"%w: pinned textbook query failed: %v",
			ErrGradingGroundingUnavailable, err,
		)
		return gradingProviderGrounding{}, state.err
	}
	state.evidence, state.err = newGradingProviderGrounding(snapshot, result)
	return state.evidence, state.err
}

func groundingRetrievalClaim(
	ownerID, agentName, jobID string,
	q RecognizedQuestion,
	itemKey string,
	snapshot GroundingSnapshot,
	query string,
) k12storage.GroundingRetrievalInvocationClaim {
	problemID := strings.TrimSpace(q.ProblemID)
	if problemID == "" {
		problemID = itemKey
	}
	snapshotRaw, _ := json.Marshal(snapshot)
	queryRaw := []byte(query)
	scopeRaw, _ := json.Marshal(struct {
		BindingID  string                         `json:"binding_id"`
		Manifest   string                         `json:"manifest_id"`
		Document   string                         `json:"document_id"`
		Generation int64                          `json:"generation"`
		Segments   []string                       `json:"segments"`
		Pages      []k12.TextbookGroundingPageRef `json:"pages"`
	}{snapshot.TextbookBindingID, snapshot.TextbookManifestID, snapshot.DocumentID,
		snapshot.DocumentGeneration, snapshot.SegmentRefs, snapshot.PageRefs})
	return k12storage.GroundingRetrievalInvocationClaim{
		OwnerID: ownerID, AgentName: agentName, JobID: jobID, ProblemID: problemID,
		Operation:               "k12_grounding_retrieval",
		GroundingSnapshotDigest: strings.TrimPrefix(modelInvocationDigest(snapshotRaw), "sha256:"),
		QueryDigest:             strings.TrimPrefix(modelInvocationDigest(queryRaw), "sha256:"),
		DocumentID:              snapshot.DocumentID, DocumentGeneration: snapshot.DocumentGeneration,
		RevisionID: snapshot.VectorRevisionID, ScopeDigest: strings.TrimPrefix(modelInvocationDigest(scopeRaw), "sha256:"),
	}
}

func groundingRetrievalResultDigests(result GroundingSnapshotResult) (string, string, string) {
	receiptsRaw, _ := json.Marshal(result.Receipts)
	hitIDs := make([]string, 0, len(result.Receipts))
	citations := make([]string, 0, len(result.Receipts))
	for _, receipt := range result.Receipts {
		hitIDs = append(hitIDs, receipt.ChunkID)
		citations = append(citations, receipt.CitationDigest)
	}
	hitRaw, _ := json.Marshal(hitIDs)
	citationRaw, _ := json.Marshal(citations)
	return strings.TrimPrefix(modelInvocationDigest(receiptsRaw), "sha256:"),
		strings.TrimPrefix(modelInvocationDigest(hitRaw), "sha256:"),
		strings.TrimPrefix(modelInvocationDigest(citationRaw), "sha256:")
}

func decodeGroundingRetrievalResult(
	invocation k12storage.GroundingRetrievalInvocation,
	snapshot GroundingSnapshot,
	claim k12storage.GroundingRetrievalInvocationClaim,
) (GroundingSnapshotResult, error) {
	var result GroundingSnapshotResult
	if !json.Valid([]byte(invocation.ResultJSON)) {
		return GroundingSnapshotResult{}, fmt.Errorf("%w: grounding retrieval result is invalid JSON", ErrModelInvocationRequiresReconciliation)
	}
	if err := json.Unmarshal([]byte(invocation.ResultJSON), &result); err != nil {
		return GroundingSnapshotResult{}, fmt.Errorf("%w: decode grounding retrieval result: %v", ErrModelInvocationRequiresReconciliation, err)
	}
	if err := result.validate(snapshot); err != nil {
		return GroundingSnapshotResult{}, fmt.Errorf("%w: stored grounding retrieval result: %v", ErrModelInvocationRequiresReconciliation, err)
	}
	queryReceiptDigest, hitSetDigest, citationSetDigest := groundingRetrievalResultDigests(result)
	if invocation.QueryDigest != claim.QueryDigest || invocation.GroundingSnapshotDigest != claim.GroundingSnapshotDigest ||
		invocation.QueryReceiptDigest != queryReceiptDigest || invocation.HitSetDigest != hitSetDigest ||
		invocation.CitationSetDigest != citationSetDigest {
		return GroundingSnapshotResult{}, fmt.Errorf("%w: grounding retrieval result digest mismatch", ErrModelInvocationRequiresReconciliation)
	}
	return result, nil
}

func gradingItemGroundingQuery(q RecognizedQuestion, req GradeRequest) string {
	values := make([]string, 0, len(req.KnowledgePoints))
	for _, value := range req.KnowledgePoints {
		if value = strings.TrimSpace(value); value != "" {
			values = append(values, value)
		}
	}
	if len(values) > 0 {
		return strings.Join(values, "、")
	}
	if value := strings.TrimSpace(req.Problem); value != "" {
		return value
	}
	return strings.TrimSpace(q.Question)
}

func inspectGradingGroundingInvocations(
	ctx context.Context,
	deps Deps,
	agentName string,
	jobID string,
) (gradingGroundingInvocationInspection, error) {
	var inspection gradingGroundingInvocationInspection
	if deps.Records == nil {
		return inspection, nil
	}
	invocations, err := deps.Records.ListGradingItemInvocations(ctx, agentName, jobID)
	if err != nil {
		return inspection, err
	}
	for _, invocation := range invocations {
		if invocation.Status != k12.ModelInvocationSucceeded ||
			!gradingGroundingRelevantOperation(invocation.Operation) {
			continue
		}
		inspection.relevantSucceeded++
		if invocation.ResultDigest != modelInvocationDigest([]byte(invocation.ResultJSON)) {
			return inspection, fmt.Errorf(
				"%w: invocation=%s result digest mismatch",
				ErrModelInvocationRequiresReconciliation, invocation.InvocationID,
			)
		}
		_, storedGrounding, enveloped, decodeErr := decodeGroundedPhysicalPayload(
			invocation.ResultJSON, nil,
		)
		if decodeErr != nil {
			return inspection, decodeErr
		}
		if !enveloped {
			if localDeterministicGradingInvocation(invocation) {
				continue
			}
			inspection.directSucceeded++
			continue
		}
		inspection.envelopedSucceeded++
		if storedGrounding == nil {
			return inspection, fmt.Errorf(
				"%w: invocation=%s grounding envelope is incomplete",
				ErrModelInvocationRequiresReconciliation, invocation.InvocationID,
			)
		}
		snapshot := cloneGradingGroundingSnapshot(storedGrounding.Snapshot)
		if err := validateGradingGroundingSnapshot(snapshot); err != nil {
			return inspection, err
		}
		if !inspection.found {
			inspection.snapshot = snapshot
			inspection.found = true
			continue
		}
		if !reflect.DeepEqual(inspection.snapshot, snapshot) {
			return inspection, fmt.Errorf(
				"%w: item invocations carry inconsistent grounding snapshots",
				ErrModelInvocationRequiresReconciliation,
			)
		}
	}
	if inspection.envelopedSucceeded > 0 && inspection.directSucceeded > 0 {
		return inspection, fmt.Errorf(
			"%w: item invocations only partially carry grounding envelopes",
			ErrModelInvocationRequiresReconciliation,
		)
	}
	return inspection, nil
}

// localDeterministicGradingInvocation 只识别本机 numeric_exec 的 solve / grade 回执；
// 它们不属于 Provider 物理调用，不参与教材证据封套完整性计数。
func localDeterministicGradingInvocation(invocation k12.GradingItemInvocation) bool {
	if invocation.Operation != k12.GradingItemOperationSolve &&
		invocation.Operation != k12.GradingItemOperationGrade {
		return false
	}
	var result struct {
		Evidence SolveEvidence `json:"Evidence"`
	}
	if err := json.Unmarshal([]byte(invocation.ResultJSON), &result); err != nil {
		return false
	}
	return result.Evidence.EvidenceType == EvidenceNumericExec
}

func gradingGroundingRelevantOperation(operation k12.GradingItemOperation) bool {
	switch operation {
	case k12.GradingItemOperationSolve,
		k12.GradingItemOperationSolveGenerate,
		k12.GradingItemOperationSolveVerify,
		k12.GradingItemOperationGrade:
		return true
	default:
		return false
	}
}

func gradingGroundingSnapshotFromItemInvocations(
	ctx context.Context,
	deps Deps,
	agentName string,
	jobID string,
) (GroundingSnapshot, bool, error) {
	inspection, err := inspectGradingGroundingInvocations(ctx, deps, agentName, jobID)
	if err != nil {
		return GroundingSnapshot{}, false, err
	}
	gradingGroundingSessions.Delete(gradingGroundingSessionKey{
		records: deps.Records, agentName: strings.TrimSpace(agentName), jobID: strings.TrimSpace(jobID),
	})
	if !inspection.found {
		return GroundingSnapshot{}, false, nil
	}
	return cloneGradingGroundingSnapshot(inspection.snapshot), true, nil
}

func ExecuteGradingPhysicalCall(
	ctx context.Context,
	spec GradingPhysicalCallSpec,
	send func(context.Context) (string, error),
) (GradingPhysicalCallResult, error) {
	if ctx != nil {
		if executor, ok := ctx.Value(gradingPhysicalCallContextKey{}).(GradingPhysicalCallExecutor); ok &&
			executor != nil {
			return executor.ExecuteGradingPhysicalCall(ctx, spec, send)
		}
	}
	payload, err := send(ctx)
	return GradingPhysicalCallResult{Payload: payload}, err
}

type gradingPhysicalNoRetryError struct {
	cause error
}

func (e gradingPhysicalNoRetryError) Error() string {
	return fmt.Sprintf("%v: %v", ErrGradingPhysicalCallOutcomeUnknown, e.cause)
}
func (e gradingPhysicalNoRetryError) Unwrap() error           { return e.cause }
func (e gradingPhysicalNoRetryError) SubAgentRetryable() bool { return false }

type durableGradingPhysicalCallExecutor struct {
	o   *GradingOrchestrator
	job GradingJobView
	q   RecognizedQuestion

	mu   sync.Mutex
	last map[k12.GradingItemOperation]string
}

func newDurableGradingPhysicalCallExecutor(
	o *GradingOrchestrator,
	job GradingJobView,
	q RecognizedQuestion,
) *durableGradingPhysicalCallExecutor {
	return &durableGradingPhysicalCallExecutor{
		o: o, job: job, q: q, last: map[k12.GradingItemOperation]string{},
	}
}

func (e *durableGradingPhysicalCallExecutor) remember(
	operation k12.GradingItemOperation,
	invocationID string,
) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.last[operation] = invocationID
}

func (e *durableGradingPhysicalCallExecutor) lastInvocation(
	operations ...k12.GradingItemOperation,
) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, operation := range operations {
		if invocationID := e.last[operation]; invocationID != "" {
			return invocationID
		}
	}
	return ""
}

func (e *durableGradingPhysicalCallExecutor) ExecuteGradingPhysicalCall(
	ctx context.Context,
	spec GradingPhysicalCallSpec,
	send func(context.Context) (string, error),
) (GradingPhysicalCallResult, error) {
	var zero GradingPhysicalCallResult
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	spec.RequestDigest = strings.TrimSpace(spec.RequestDigest)
	if !spec.Operation.Valid() || spec.Operation == k12.GradingItemOperationSolve ||
		spec.RequestDigest == "" {
		return zero, fmt.Errorf("%w: invalid physical grading call identity", ErrInvalidInput)
	}
	var expectedGrounding *gradingProviderGrounding
	if grounding, ok := gradingProviderGroundingFromContext(ctx); ok {
		expectedGrounding = &grounding
		spec.RequestDigest = modelInvocationDigest(
			[]byte("k12-grading-grounded-request-v1"),
			[]byte(spec.RequestDigest),
			[]byte(grounding.identityDigest),
		)
	}

	commitCtx, cancelCommit := gradingDurableCommitContext(ctx)
	invocations, err := e.o.deps.Records.ListGradingItemInvocations(
		commitCtx, e.job.Record.AgentName, e.job.Record.RecordID,
	)
	cancelCommit()
	if err != nil {
		return zero, err
	}
	currentGeneration := e.job.Fields.AttemptCount + 1
	currentBase := currentGeneration * 1000
	nextOrdinal := 1
	var matching *k12.GradingItemInvocation
	for i := range invocations {
		candidate := &invocations[i]
		if candidate.ProblemID != e.q.ProblemID || candidate.Operation != spec.Operation {
			continue
		}
		if candidate.OperationAttempt/1000 == currentGeneration {
			if ordinal := candidate.OperationAttempt % 1000; ordinal >= nextOrdinal {
				nextOrdinal = ordinal + 1
			}
		}
		if candidate.RequestDigest == spec.RequestDigest &&
			(matching == nil || candidate.OperationAttempt > matching.OperationAttempt) {
			matching = candidate
		}
	}
	if matching != nil {
		switch matching.Status {
		case k12.ModelInvocationSucceeded:
			if matching.ResultDigest != modelInvocationDigest([]byte(matching.ResultJSON)) {
				return zero, fmt.Errorf("%w: invocation=%s result digest mismatch",
					ErrModelInvocationRequiresReconciliation, matching.InvocationID)
			}
			payload, _, _, decodeErr := decodeGroundedPhysicalPayload(
				matching.ResultJSON, expectedGrounding,
			)
			if decodeErr != nil {
				return zero, decodeErr
			}
			e.remember(spec.Operation, matching.InvocationID)
			return GradingPhysicalCallResult{
				Payload: payload, InvocationID: matching.InvocationID,
			}, nil
		case k12.ModelInvocationPrepared:
			// Claim below.
		case k12.ModelInvocationFailed:
			if matching.OperationAttempt/1000 >= currentGeneration {
				return zero, fmt.Errorf("%w: invocation=%s class=%s code=%s",
					ErrGradingItemInvocationFailed, matching.InvocationID,
					matching.FailureClass, matching.FailureCode)
			}
			matching = nil
		case k12.ModelInvocationSent, k12.ModelInvocationOutcomeUnknown, k12.ModelInvocationReconciled:
			return zero, gradingPhysicalNoRetryError{cause: fmt.Errorf(
				"%w: invocation=%s status=%s; provider query unavailable",
				ErrModelInvocationRequiresReconciliation, matching.InvocationID, matching.Status,
			)}
		default:
			return zero, fmt.Errorf("%w: invocation=%s unexpected status=%s",
				ErrModelInvocationRequiresReconciliation, matching.InvocationID, matching.Status)
		}
	}
	if problemSourceReconciliationOnly(ctx) {
		return zero, gradingPhysicalNoRetryError{cause: fmt.Errorf(
			"%w: reconciliation-only processing cannot create or send a grading invocation",
			ErrModelInvocationRequiresReconciliation,
		)}
	}

	var invocation k12.GradingItemInvocation
	if matching != nil {
		invocation = *matching
	} else {
		operationAttempt := currentBase + nextOrdinal
		invocationID := stableGradingPhysicalInvocationID(
			e.job, e.q, spec, operationAttempt,
		)
		commitCtx, cancelCommit = gradingDurableCommitContext(ctx)
		invocation, _, err = e.o.deps.Records.PrepareGradingItemInvocation(
			commitCtx,
			k12.GradingItemInvocation{
				InvocationID: invocationID, AgentName: e.job.Record.AgentName,
				JobID: e.job.Record.RecordID, ProblemID: e.q.ProblemID, AttemptID: e.q.AttemptID,
				Operation: spec.Operation, OperationAttempt: operationAttempt,
				RequestDigest: spec.RequestDigest, RouteSnapshot: e.job.Fields.ModelSnapshot,
				CreatedAt: e.o.deps.now(), UpdatedAt: e.o.deps.now(),
			},
		)
		cancelCommit()
		if err != nil {
			return zero, err
		}
	}
	commitCtx, cancelCommit = gradingDurableCommitContext(ctx)
	invocation, claimed, err := e.o.deps.Records.ClaimGradingItemInvocationSent(
		commitCtx, e.job.Record.AgentName, invocation.InvocationID,
	)
	cancelCommit()
	if err != nil {
		return zero, err
	}
	if !claimed {
		return zero, gradingPhysicalNoRetryError{cause: fmt.Errorf(
			"%w: invocation=%s concurrently claimed with status=%s",
			ErrModelInvocationRequiresReconciliation, invocation.InvocationID, invocation.Status,
		)}
	}

	callCtx, cancelCall := gradingIndependentCallContext(ctx, e.job.Fields.ModelSnapshot.TimeoutMS)
	payload, callErr := send(callCtx)
	callCtxErr := callCtx.Err()
	cancelCall()
	if callErr != nil {
		commitCtx, cancelCommit = gradingDurableCommitContext(ctx)
		defer cancelCommit()
		if sentProviderOutcomeUnknown(callErr, callCtxErr) {
			_, ledgerErr := e.o.deps.Records.MarkGradingItemInvocationOutcomeUnknown(
				commitCtx, e.job.Record.AgentName, invocation.InvocationID,
				"provider_transport", "outcome_unknown",
			)
			if ledgerErr != nil {
				return zero, errors.Join(
					gradingPhysicalNoRetryError{cause: callErr},
					ErrModelInvocationRequiresReconciliation,
					ledgerErr,
				)
			}
			return zero, gradingPhysicalNoRetryError{cause: errors.Join(
				ErrGradingPhysicalCallOutcomeUnknown, callErr,
			)}
		}
		statusCode, _ := definitiveProviderResponseStatus(callErr)
		_, ledgerErr := e.o.deps.Records.MarkGradingItemInvocationFailed(
			commitCtx, e.job.Record.AgentName, invocation.InvocationID,
			"provider_response", fmt.Sprintf("http_%d", statusCode),
		)
		if ledgerErr != nil {
			return zero, errors.Join(callErr, ledgerErr)
		}
		return zero, callErr
	}
	if !json.Valid([]byte(payload)) {
		commitCtx, cancelCommit = gradingDurableCommitContext(ctx)
		defer cancelCommit()
		_, ledgerErr := e.o.deps.Records.MarkGradingItemInvocationOutcomeUnknown(
			commitCtx, e.job.Record.AgentName, invocation.InvocationID,
			"local", "result_encode_failed",
		)
		return zero, errors.Join(
			gradingPhysicalNoRetryError{cause: errors.New("physical result is not valid JSON")},
			ledgerErr,
		)
	}
	storedPayload := payload
	if expectedGrounding != nil {
		storedPayload, err = encodeGroundedPhysicalPayload(payload, *expectedGrounding)
		if err != nil {
			commitCtx, cancelCommit = gradingDurableCommitContext(ctx)
			defer cancelCommit()
			_, ledgerErr := e.o.deps.Records.MarkGradingItemInvocationOutcomeUnknown(
				commitCtx, e.job.Record.AgentName, invocation.InvocationID,
				"local", "grounding_envelope_encode_failed",
			)
			return zero, errors.Join(gradingPhysicalNoRetryError{cause: err}, ledgerErr)
		}
	}
	commitCtx, cancelCommit = gradingDurableCommitContext(ctx)
	stored, err := e.o.deps.Records.MarkGradingItemInvocationSucceeded(
		commitCtx, e.job.Record.AgentName, invocation.InvocationID,
		modelInvocationDigest([]byte(storedPayload)), storedPayload,
	)
	cancelCommit()
	if err != nil {
		unknownCtx, cancelUnknown := gradingDurableCommitContext(ctx)
		_, unknownErr := e.o.deps.Records.MarkGradingItemInvocationOutcomeUnknown(
			unknownCtx, e.job.Record.AgentName, invocation.InvocationID,
			"local", "result_not_durable",
		)
		cancelUnknown()
		return zero, errors.Join(
			gradingPhysicalNoRetryError{cause: ErrGradingPhysicalCallOutcomeUnknown},
			err,
			unknownErr,
		)
	}
	returnedPayload, _, _, decodeErr := decodeGroundedPhysicalPayload(
		stored.ResultJSON, expectedGrounding,
	)
	if decodeErr != nil {
		return zero, decodeErr
	}
	e.remember(spec.Operation, stored.InvocationID)
	return GradingPhysicalCallResult{
		Payload: returnedPayload, InvocationID: stored.InvocationID,
	}, nil
}

func stableGradingPhysicalInvocationID(
	job GradingJobView,
	q RecognizedQuestion,
	spec GradingPhysicalCallSpec,
	operationAttempt int,
) string {
	identity := strings.Join([]string{
		job.Record.AgentName,
		job.Record.RecordID,
		q.ProblemID,
		q.AttemptID,
		string(spec.Operation),
		fmt.Sprintf("%d", operationAttempt),
		spec.RequestDigest,
		job.Fields.ModelSnapshot.Route,
	}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	return "gradingitem-" + hex.EncodeToString(sum[:16])
}

func gradingIndependentCallContext(
	parent context.Context,
	timeoutMS int,
) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(parent)
	// The caller supplies the durable stage context. Its deadline is the single
	// budget authority for nested assessing operations; TimeoutMS may carry the
	// DD-036 recognizing limit and must not introduce a shorter hidden cap here.
	if parentDeadline, ok := parent.Deadline(); ok {
		return context.WithDeadline(base, parentDeadline)
	}
	timeout := 3 * time.Minute
	if timeoutMS > 0 {
		timeout = time.Duration(timeoutMS) * time.Millisecond
	}
	return context.WithTimeout(base, timeout)
}

func gradingDurableCommitContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
}

type physicalGradingCaller interface {
	UsesGradingPhysicalCalls() bool
}

func usesGradingPhysicalCalls(candidate any) bool {
	caller, ok := candidate.(physicalGradingCaller)
	return ok && caller.UsesGradingPhysicalCalls()
}

func executeDurableSolveOperation(
	ctx context.Context,
	o *GradingOrchestrator,
	deps Deps,
	job GradingJobView,
	q RecognizedQuestion,
	gradeReq GradeRequest,
) (SolveHomeworkResult, string, error) {
	groundedCtx, groundingRequired, groundingErr := prepareGradingItemGrounding(
		ctx, deps, job, q, gradeReq,
	)
	if groundingErr != nil {
		return SolveHomeworkResult{}, "", groundingErr
	}
	ctx = groundedCtx
	if !usesGradingPhysicalCalls(deps.Solver) {
		if groundingRequired {
			return SolveHomeworkResult{}, "", fmt.Errorf(
				"%w: grounded grading requires durable physical solver calls",
				ErrGradingGroundingUnavailable,
			)
		}
		return executeGradingItemOperationWithKind(ctx, o, job, q,
			k12.GradingItemOperationSolve,
			k12.GradingExecutionProvider,
			struct {
				InputDigest string       `json:"input_digest"`
				Request     GradeRequest `json:"request"`
			}{q.InputDigest, gradeReq},
			func(callCtx context.Context) (SolveHomeworkResult, error) {
				return deps.SolveHomeworkProblem(callCtx, gradeReq)
			})
	}
	if err := ctx.Err(); err != nil {
		return SolveHomeworkResult{}, "", err
	}
	executor := newDurableGradingPhysicalCallExecutor(o, job, q)
	itemCtx, cancelItem := gradingIndependentCallContext(ctx, job.Fields.ModelSnapshot.TimeoutMS)
	defer cancelItem()
	itemCtx = withGradingPhysicalCallExecutor(itemCtx, executor)
	result, err := deps.SolveHomeworkProblem(itemCtx, gradeReq)
	invocationID := executor.lastInvocation(
		k12.GradingItemOperationSolveVerify,
		k12.GradingItemOperationSolveGenerate,
	)
	if err == nil && invocationID == "" {
		if result.Evidence.EvidenceType != EvidenceNumericExec {
			err = fmt.Errorf("%w: physical solver returned without a durable invocation",
				ErrModelInvocationRequiresReconciliation)
		} else {
			return executeGradingItemOperationWithKind(ctx, o, job, q,
				k12.GradingItemOperationSolve,
				k12.GradingExecutionLocalDeterministic,
				struct {
					ExecutionKind k12.GradingExecutionKind `json:"execution_kind"`
					InputDigest   string                   `json:"input_digest"`
					Request       GradeRequest             `json:"request"`
				}{k12.GradingExecutionLocalDeterministic, q.InputDigest, gradeReq},
				func(context.Context) (SolveHomeworkResult, error) {
					return result, nil
				})
		}
	}
	return result, invocationID, err
}

func executeDurableGradeOperation(
	ctx context.Context,
	o *GradingOrchestrator,
	deps Deps,
	job GradingJobView,
	q RecognizedQuestion,
	gradeReq GradeRequest,
	solved SolveHomeworkResult,
) (GradeResult, string, error) {
	groundedCtx, groundingRequired, groundingErr := prepareGradingItemGrounding(
		ctx, deps, job, q, gradeReq,
	)
	if groundingErr != nil {
		return GradeResult{}, "", groundingErr
	}
	ctx = groundedCtx
	gradeCaller := any(deps.Grader)
	if deps.VerifiedGrader != nil {
		gradeCaller = deps.VerifiedGrader
	}
	if !usesGradingPhysicalCalls(gradeCaller) {
		if groundingRequired {
			return GradeResult{}, "", fmt.Errorf(
				"%w: grounded grading requires durable physical grader calls",
				ErrGradingGroundingUnavailable,
			)
		}
		return executeGradingItemOperationWithKind(ctx, o, job, q,
			k12.GradingItemOperationGrade,
			k12.GradingExecutionProvider,
			struct {
				InputDigest string              `json:"input_digest"`
				Request     GradeRequest        `json:"request"`
				Solved      SolveHomeworkResult `json:"solved"`
			}{q.InputDigest, gradeReq, solved},
			func(callCtx context.Context) (GradeResult, error) {
				return deps.gradeSolvedHomeworkProblem(callCtx, gradeReq, solved)
			})
	}
	if err := ctx.Err(); err != nil {
		return GradeResult{}, "", err
	}
	executor := newDurableGradingPhysicalCallExecutor(o, job, q)
	itemCtx, cancelItem := gradingIndependentCallContext(ctx, job.Fields.ModelSnapshot.TimeoutMS)
	defer cancelItem()
	itemCtx = withGradingPhysicalCallExecutor(itemCtx, executor)
	result, err := deps.gradeSolvedHomeworkProblem(itemCtx, gradeReq, solved)
	invocationID := executor.lastInvocation(k12.GradingItemOperationGrade)
	if err == nil && invocationID == "" {
		if result.Evidence.EvidenceType != EvidenceNumericExec {
			err = fmt.Errorf("%w: physical grader returned without a durable invocation",
				ErrModelInvocationRequiresReconciliation)
		} else {
			return executeGradingItemOperationWithKind(ctx, o, job, q,
				k12.GradingItemOperationGrade,
				k12.GradingExecutionLocalDeterministic,
				struct {
					ExecutionKind k12.GradingExecutionKind `json:"execution_kind"`
					InputDigest   string                   `json:"input_digest"`
					Request       GradeRequest             `json:"request"`
					Solved        SolveHomeworkResult      `json:"solved"`
				}{k12.GradingExecutionLocalDeterministic, q.InputDigest, gradeReq, solved},
				func(context.Context) (GradeResult, error) {
					return result, nil
				})
		}
	}
	return result, invocationID, err
}
