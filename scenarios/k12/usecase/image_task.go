package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

// ImageTaskClassificationInput keeps image evidence mandatory. MessageIntent
// is contextual evidence only; adapters must never classify from it alone.
type ImageTaskClassificationInput struct {
	Images        [][]byte
	MessageIntent string
}

type ImageTaskClassification struct {
	Intent                   k12.ImageTaskIntent
	IntentEvidence           []string
	Confidence               float64
	ConfirmationCandidates   []k12.ImageTaskIntent
	WorkTitleCandidate       *k12.FactCandidate
	TaskRequirementCandidate *k12.FactCandidate
}

type ImageTaskClassifier interface {
	ClassifyImageTask(context.Context, ImageTaskClassificationInput) (ImageTaskClassification, error)
}

type ImageTaskWritingOCRResult struct {
	Raw              string
	CanonicalContent string
	Confidence       float64
	RiskSegments     []k12.CreativeWorkIntakeOCRRisk
}

// ImageTaskWritingOCR returns explicit quality evidence. A string-only OCR
// result is insufficient to authorize automatic promotion.
type ImageTaskWritingOCR interface {
	RecognizeImageTaskWriting(context.Context, []byte) (ImageTaskWritingOCRResult, error)
}

type imageTaskGradingStarter interface {
	StartPhotoGradingJob(context.Context, StartPhotoGradingInput) (GradingJobView, bool, error)
	ConfirmPhotoGradingJob(context.Context, string, ConfirmPhotoGradingInput) (GradingJobView, bool, error)
	StartAsync(jobID string) bool
}

type imageTaskGradingParentWindowRetrier interface {
	CanRetryPhotoGradingWithParentAutomaticWindow(
		context.Context,
		string,
	) (bool, error)
	RetryPhotoGradingJobWithParentAutomaticWindow(
		context.Context,
		string,
		string,
		int64,
	) (GradingJobView, bool, error)
}

type ImageTaskRouteResolver func(k12.ImageTaskRouteSnapshot) (k12.ImageTaskRouteSnapshot, error)
type ImageTaskRouteDisplayResolver func(k12.ImageTaskRouteSnapshot) (string, string)
type ImageTaskAssetReader func(agentName, assetRef string) ([]byte, error)
type ImageTaskGradeResolver func(context.Context, string) (string, error)

const imageTaskDefaultProviderTimeout = 120 * time.Second

const (
	imageTaskFailureInteractiveDeadlineExceeded = "interactive_deadline_exceeded"
)

type imageTaskWorkFeedbackGenerator interface {
	GenerateWorkFeedback(context.Context, string, string) (CreativeWorkView, error)
}

type ImageTaskCoordinator struct {
	Records               *k12storage.Store
	Classifier            ImageTaskClassifier
	WritingOCR            ImageTaskWritingOCR
	Grading               imageTaskGradingStarter
	WorkFeedback          imageTaskWorkFeedbackGenerator
	ResolveRoute          ImageTaskRouteResolver
	ResolveRouteDisplay   ImageTaskRouteDisplayResolver
	ResolveGrade          ImageTaskGradeResolver
	ReadAsset             ImageTaskAssetReader
	GradingBudgetSnapshot k12.GradingBudgetSnapshot
	Now                   func() int64
	NewID                 func(kind string) string
	BaseContext           context.Context

	workerMu    sync.Mutex
	active      map[string]bool
	sealed      bool
	workerCount int
	workerIdle  chan struct{}
	runCtx      context.Context
	runCancel   context.CancelFunc
}

var ErrImageTaskCoordinatorShutdown = errors.New("image task coordinator is shut down")

type CreateImageTaskInput struct {
	AgentName         string
	LearnerID         string
	SourceKind        k12.ImageTaskSourceKind
	SourceRef         string
	SourceSessionID   string
	SourceAssetRefs   []string
	MessageIntent     string
	AttemptGeneration int
	RouteRequest      k12.ImageTaskRouteSnapshot
	CreativeEntry     *k12.ImageTaskCreativeEntry
}

type ImageTaskView struct {
	Dispatch                   k12.ImageTaskDispatch
	Homework                   *k12.HomeworkSubmission
	HomeworkProjection         *ImageTaskHomeworkProjection
	Creative                   *k12.CreativeWorkIntake
	CreativeDisplayName        string
	CreativeWork               *CreativeWorkView
	CreativeFeedback           string
	ActiveInvocationDeadlineAt int64
	feedbackInvocation         *k12.ImageTaskInvocation
}

type ImageTaskHomeworkProjection struct {
	Stage             string
	Retryable         bool
	ConfirmationState string
	AnchorState       string
	Subject           string
	Questions         []RecognizedQuestion
	Progressive       ImageTaskProgressiveSnapshot
	FinalArtifact     *k12.GradingFinalArtifact `json:"final_artifact,omitempty"`
}

type ImageTaskProgressiveSnapshot struct {
	StructureVersion int
	SnapshotRevision int
	ProblemProgress  []ImageTaskProblemProgress
	Coverage         ImageTaskProgressiveCoverage
}

type ImageTaskProblemProgress struct {
	ProblemID          string
	Status             string
	InputRevision      int
	PublishedRevision  int
	CurrentDisposition string
}

type ImageTaskProgressiveCoverage struct {
	Total              int
	Published          int
	Skipped            int
	Awaiting           int
	Failed             int
	Status             string
	ProjectionRevision int
}

type imageTaskGradingProjector interface {
	ImageTaskHomeworkProjection(
		context.Context, string, string,
	) (ImageTaskHomeworkProjection, error)
}

type imageTaskGradingCanceller interface {
	CancelImageTaskHomework(context.Context, string, string) error
}

type ImageTaskResult struct {
	Kind                string
	Dispatch            k12.ImageTaskDispatch
	Photo               *PhotoGradeResult
	Creative            *k12.CreativeWorkIntake
	CreativeDisplayName string
	CreativeWork        *CreativeWorkView
	FinalArtifact       *k12.GradingFinalArtifact `json:"final_artifact,omitempty"`
}

type imageTaskPhotoResultReader interface {
	PhotoResult(jobID string) (PhotoGradeResult, bool)
}

type ConfirmImageTaskInput struct {
	AgentName           string
	DispatchID          string
	ExpectedVersion     int
	Intent              k12.ImageTaskIntent
	Subject             string
	Grade               string
	QuestionCorrections []GradingQuestionCorrection
	CanonicalVersion    int
	CanonicalContent    string
	Creative            *ConfirmCreativeImageTaskInput
}

type CreativeImageTaskAction string

const (
	CreativeImageTaskActionFreezeOCR CreativeImageTaskAction = "freeze_ocr"
	CreativeImageTaskActionCommit    CreativeImageTaskAction = "commit"
)

type ConfirmCreativeImageTaskInput struct {
	Action             CreativeImageTaskAction
	CanonicalVersion   int
	CanonicalContent   string
	SegmentCorrections []k12.CreativeWorkIntakeOCRCorrection
	WorkTitle          string
	TaskRequirement    string
	Intent             string
	ContentMarkdown    string
}

func (c *ImageTaskCoordinator) now() int64 {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().Unix()
}

func (c *ImageTaskCoordinator) id(kind string) string {
	if c.NewID != nil {
		if value := strings.TrimSpace(c.NewID(kind)); value != "" {
			return value
		}
	}
	return idgen.NanoID()
}

func defaultImageTaskAssetReader(agentName, assetRef string) ([]byte, error) {
	owner, file, err := assetstore.Parse(assetRef)
	if err != nil || owner != agentName {
		return nil, fmt.Errorf("%w: image task 只接受当前实例真实上传的 asset:// 图片", ErrInvalidInput)
	}
	raw, _, err := assetstore.Read(agentName, file)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取 image task 图片: %v", ErrInvalidInput, err)
	}
	return raw, nil
}

func (c *ImageTaskCoordinator) validate() error {
	if c == nil || c.Records == nil {
		return fmt.Errorf("usecase: image task store 未配置")
	}
	return nil
}

func normalizeCreateImageTaskInput(in CreateImageTaskInput) (CreateImageTaskInput, error) {
	in.AgentName = strings.TrimSpace(in.AgentName)
	in.LearnerID = strings.TrimSpace(in.LearnerID)
	in.SourceRef = strings.TrimSpace(in.SourceRef)
	in.SourceSessionID = strings.TrimSpace(in.SourceSessionID)
	in.MessageIntent = strings.TrimSpace(in.MessageIntent)
	if in.AgentName == "" || in.LearnerID == "" || in.SourceRef == "" ||
		len(in.SourceAssetRefs) == 0 || in.AttemptGeneration < 1 {
		return in, fmt.Errorf("%w: agent/learner/source/assets/attempt_generation 不完整", ErrInvalidInput)
	}
	switch in.SourceKind {
	case k12.ImageTaskSourceDesktop, k12.ImageTaskSourceAPI, k12.ImageTaskSourceIM:
	default:
		return in, fmt.Errorf("%w: source_kind 非法", ErrInvalidInput)
	}
	for i := range in.SourceAssetRefs {
		in.SourceAssetRefs[i] = strings.TrimSpace(in.SourceAssetRefs[i])
		if in.SourceAssetRefs[i] == "" {
			return in, fmt.Errorf("%w: source_asset_refs 包含空值", ErrInvalidInput)
		}
	}
	if in.RouteRequest.SelectionSource == "" {
		in.RouteRequest.SelectionSource = "auto"
	}
	if in.CreativeEntry != nil {
		entry := *in.CreativeEntry
		entry.WorkID = strings.TrimSpace(entry.WorkID)
		entry.BaseVersionID = strings.TrimSpace(entry.BaseVersionID)
		if err := entry.Validate(); err != nil {
			return in, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		in.CreativeEntry = &entry
	}
	return in, nil
}

func digestJSON(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func imageBytesDigest(images [][]byte) string {
	hash := sha256.New()
	for _, image := range images {
		var length [8]byte
		n := uint64(len(image))
		for i := 7; i >= 0; i-- {
			length[i] = byte(n)
			n >>= 8
		}
		hash.Write(length[:])
		hash.Write(image)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func (c *ImageTaskCoordinator) Create(
	ctx context.Context,
	input CreateImageTaskInput,
) (ImageTaskView, bool, error) {
	if c == nil || c.Records == nil {
		return ImageTaskView{}, false, fmt.Errorf("usecase: image task store 未配置")
	}
	in, err := normalizeCreateImageTaskInput(input)
	if err != nil {
		return ImageTaskView{}, false, err
	}
	if in.CreativeEntry == nil && (c.Classifier == nil || c.ResolveRoute == nil) {
		return ImageTaskView{}, false, fmt.Errorf("usecase: image task classifier/route resolver 未配置")
	}
	for index, ref := range in.SourceAssetRefs {
		owner, _, parseErr := assetstore.Parse(ref)
		if parseErr != nil || owner != in.AgentName {
			return ImageTaskView{}, false, fmt.Errorf(
				"%w: source_asset_refs[%d] 不是当前实例的 immutable asset:// 图片",
				ErrInvalidInput, index,
			)
		}
	}
	idempotencyKey := fmt.Sprintf("%s:%s:g%d", in.SourceKind, in.SourceRef, in.AttemptGeneration)
	existing, lookupErr := c.Records.GetImageTaskDispatchByIdempotency(
		ctx, in.AgentName, idempotencyKey,
	)
	if lookupErr == nil {
		if existing.LearnerID != in.LearnerID ||
			existing.SourceKind != in.SourceKind ||
			existing.SourceRef != in.SourceRef ||
			existing.SourceSessionID != in.SourceSessionID ||
			!slices.Equal(existing.SourceAssetRefs, in.SourceAssetRefs) ||
			existing.MessageIntent != in.MessageIntent ||
			existing.AttemptGeneration != in.AttemptGeneration ||
			!sameCreativeEntry(existing.CreativeEntry, in.CreativeEntry) ||
			!sameCreateImageTaskRouteRequest(existing, in) {
			return ImageTaskView{}, false, k12storage.ErrImageTaskConflict
		}
		view, err := c.projectTarget(ctx, existing)
		return view, false, err
	}
	if !errors.Is(lookupErr, k12storage.ErrImageTaskNotFound) {
		return ImageTaskView{}, false, lookupErr
	}
	reader := c.ReadAsset
	if reader == nil {
		reader = defaultImageTaskAssetReader
	}
	images := make([][]byte, len(in.SourceAssetRefs))
	for index, ref := range in.SourceAssetRefs {
		images[index], err = reader(in.AgentName, ref)
		if err != nil {
			return ImageTaskView{}, false, err
		}
		if len(images[index]) == 0 {
			return ImageTaskView{}, false, fmt.Errorf("%w: source asset %d 是空图片", ErrInvalidInput, index)
		}
	}
	sourceDigest := imageBytesDigest(images)
	if in.CreativeEntry != nil {
		operationRouteRequest := in.RouteRequest
		if c.ResolveRouteDisplay != nil {
			// Parent-selected creative creation must not resolve an unexecuted
			// vision route. Freeze only the configured display facts.
			operationRouteRequest.ProviderDisplayName,
				operationRouteRequest.ModelID = c.ResolveRouteDisplay(in.RouteRequest)
		}
		requestDigest := digestJSON(struct {
			Agent, Learner, Source, Session, Message, SourceDigest string
			Assets                                                 []string
			RouteRequest                                           k12.ImageTaskRouteSnapshot
			CreativeEntry                                          *k12.ImageTaskCreativeEntry
		}{
			in.AgentName, in.LearnerID, in.SourceRef, in.SourceSessionID,
			in.MessageIntent, sourceDigest, in.SourceAssetRefs, operationRouteRequest,
			in.CreativeEntry,
		})
		now := c.now()
		dispatch := k12.ImageTaskDispatch{
			DispatchID: c.id("dispatch"), AgentName: in.AgentName, LearnerID: in.LearnerID,
			SourceKind: in.SourceKind, SourceRef: in.SourceRef, SourceSessionID: in.SourceSessionID,
			SourceAssetRefs: append([]string(nil), in.SourceAssetRefs...), SourceDigest: sourceDigest,
			MessageIntent: in.MessageIntent, TaskIntent: in.CreativeEntry.TaskIntent,
			IntentEvidence:   []string{"parent_selected:" + string(in.CreativeEntry.TaskIntent)},
			IntentConfidence: 1, Status: k12.ImageTaskStatusRouted,
			TargetObjectType:      k12.ImageTaskTargetCreativeWorkIntake,
			TargetObjectID:        c.id("creative_intake"),
			RoutingProvenance:     k12.ImageTaskRoutingParentSelected,
			CreativeEntry:         in.CreativeEntry,
			OperationRouteRequest: operationRouteRequest,
			IdempotencyKey:        idempotencyKey, RequestDigest: requestDigest,
			AttemptGeneration:         in.AttemptGeneration,
			AutomaticBudgetSeconds:    k12.ImageTaskAutomaticBudgetSeconds,
			AutomaticStartedAt:        now,
			AutomaticDeadlineAt:       now + k12.ImageTaskAutomaticBudgetSeconds,
			AutomaticRemainingSeconds: k12.ImageTaskAutomaticBudgetSeconds,
			Version:                   0, CreatedAt: now, UpdatedAt: now,
		}
		stored, intake, created, err := c.Records.PrepareParentSelectedCreativeDispatch(
			ctx, dispatch,
		)
		if err != nil {
			return ImageTaskView{}, false, err
		}
		return ImageTaskView{Dispatch: stored, Creative: intake}, created, nil
	}
	route, err := c.ResolveRoute(in.RouteRequest)
	if err != nil {
		return ImageTaskView{}, false, fmt.Errorf(
			"%w: resolve image task route: %v", ErrInvalidInput, err,
		)
	}
	route = k12.NormalizeImageTaskRouteSnapshot(route)
	if route.TimeoutMS <= 0 {
		route.TimeoutMS = int(imageTaskDefaultProviderTimeout / time.Millisecond)
	}
	if err := route.Validate(); err != nil {
		return ImageTaskView{}, false, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	requestDigest := digestJSON(struct {
		Agent, Learner, Source, Session, Message, SourceDigest string
		Assets                                                 []string
		Route                                                  k12.ImageTaskRouteSnapshot
		CreativeEntry                                          *k12.ImageTaskCreativeEntry
	}{
		in.AgentName, in.LearnerID, in.SourceRef, in.SourceSessionID,
		in.MessageIntent, sourceDigest, in.SourceAssetRefs, route, in.CreativeEntry,
	})
	now := c.now()
	dispatchID := c.id("dispatch")
	invocationID := c.id("classification")
	dispatch := k12.ImageTaskDispatch{
		DispatchID: dispatchID, AgentName: in.AgentName, LearnerID: in.LearnerID,
		SourceKind: in.SourceKind, SourceRef: in.SourceRef, SourceSessionID: in.SourceSessionID,
		SourceAssetRefs: append([]string(nil), in.SourceAssetRefs...), SourceDigest: sourceDigest,
		MessageIntent: in.MessageIntent, TaskIntent: k12.ImageTaskIntentUnknown,
		IntentEvidence: []string{}, Status: k12.ImageTaskStatusRouting,
		RoutingProvenance:           k12.ImageTaskRoutingModelClassified,
		ClassificationRouteSnapshot: route, ClassificationInvocationID: invocationID,
		RoutePolicySnapshot: route, IdempotencyKey: idempotencyKey,
		RequestDigest: requestDigest, AttemptGeneration: in.AttemptGeneration,
		AutomaticBudgetSeconds:    k12.ImageTaskAutomaticBudgetSeconds,
		AutomaticStartedAt:        now,
		AutomaticDeadlineAt:       now + k12.ImageTaskAutomaticBudgetSeconds,
		AutomaticRemainingSeconds: k12.ImageTaskAutomaticBudgetSeconds,
		Version:                   0, CreatedAt: now, UpdatedAt: now,
	}
	invocation := k12.ImageTaskInvocation{
		InvocationID: invocationID, AgentName: in.AgentName, DispatchID: dispatchID,
		Operation:     k12.ImageTaskOperationClassification,
		OperationKey:  "dispatch:" + dispatchID + ":classification",
		RequestDigest: requestDigest, RouteSnapshot: route,
		Status: k12.ImageTaskInvocationPrepared, Attempt: 1,
		DeadlineAt: dispatch.AutomaticDeadlineAt,
		CreatedAt:  now, UpdatedAt: now,
	}
	stored, created, err := c.Records.PrepareImageTaskDispatch(ctx, dispatch, invocation)
	if err != nil {
		return ImageTaskView{}, false, err
	}
	view, projectErr := c.projectTarget(ctx, stored)
	return view, created, projectErr
}

func sameCreativeEntry(a, b *k12.ImageTaskCreativeEntry) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameCreateImageTaskRouteRequest(
	existing k12.ImageTaskDispatch,
	input CreateImageTaskInput,
) bool {
	if input.CreativeEntry != nil {
		return sameImageTaskRouteRequest(
			existing.OperationRouteRequest,
			input.RouteRequest,
		)
	}
	return sameImageTaskRouteRequest(
		existing.RoutePolicySnapshot,
		input.RouteRequest,
	)
}

func sameImageTaskRouteRequest(
	frozen k12.ImageTaskRouteSnapshot,
	requested k12.ImageTaskRouteSnapshot,
) bool {
	frozen = k12.NormalizeImageTaskRouteSnapshot(frozen)
	requested = k12.NormalizeImageTaskRouteSnapshot(requested)
	if requested.SelectionSource == "" {
		requested.SelectionSource = "auto"
	}
	if frozen.SelectionSource != requested.SelectionSource {
		return false
	}
	if requested.SelectionSource == "explicit" {
		return frozen.Provider == requested.Provider &&
			frozen.Model == requested.Model
	}
	// An auto replay is identified by its request semantics, not by today's
	// mutable default. Create intentionally does not call ResolveRoute here.
	return requested.SelectionSource == "auto"
}

func gradingSnapshotFromImageRoute(route k12.ImageTaskRouteSnapshot) k12.GradingModelSnapshot {
	return k12.NormalizeGradingModelSnapshot(k12.GradingModelSnapshot{
		Provider: route.Provider, Model: route.Model, Route: route.Route,
		Capability: route.Capability, TimeoutMS: route.TimeoutMS,
		Fallback: route.FallbackPolicy,
	})
}

func imageTaskProviderContext(
	ctx context.Context,
	route k12.ImageTaskRouteSnapshot,
) (context.Context, context.CancelFunc) {
	providerCtx := k12.WithGradingModelSnapshot(
		ctx,
		gradingSnapshotFromImageRoute(route),
	)
	if route.TimeoutMS > 0 {
		return context.WithTimeout(
			providerCtx,
			time.Duration(route.TimeoutMS)*time.Millisecond,
		)
	}
	return providerCtx, func() {}
}

type imageTaskAutomaticWindowContext struct {
	DispatchID string
	DeadlineAt int64
	NowAt      int64
}

type imageTaskAutomaticWindowContextKey struct{}

func withImageTaskAutomaticWindow(
	ctx context.Context,
	dispatchID string,
	deadlineAt, nowAt int64,
) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if deadlineAt <= 0 {
		return ctx, func() {}
	}
	ctx = context.WithValue(
		ctx,
		imageTaskAutomaticWindowContextKey{},
		imageTaskAutomaticWindowContext{
			DispatchID: strings.TrimSpace(dispatchID),
			DeadlineAt: deadlineAt,
			NowAt:      nowAt,
		},
	)
	remaining := deadlineAt - nowAt
	if remaining <= 0 {
		expired, cancel := context.WithCancel(ctx)
		cancel()
		return expired, func() {}
	}
	return context.WithTimeout(ctx, time.Duration(remaining)*time.Second)
}

func imageTaskAutomaticWindowFromContext(
	ctx context.Context,
) (imageTaskAutomaticWindowContext, bool) {
	if ctx == nil {
		return imageTaskAutomaticWindowContext{}, false
	}
	window, ok := ctx.Value(imageTaskAutomaticWindowContextKey{}).(imageTaskAutomaticWindowContext)
	return window, ok && window.DeadlineAt > 0
}

func (c *ImageTaskCoordinator) expireImageTaskInvocationIfDue(
	ctx context.Context,
	dispatch k12.ImageTaskDispatch,
	invocation k12.ImageTaskInvocation,
) (ImageTaskView, bool, error) {
	deadlineAt := invocation.DeadlineAt
	if deadlineAt == 0 {
		deadlineAt = dispatch.AutomaticDeadlineAt
	}
	now := c.now()
	if deadlineAt == 0 || deadlineAt > now {
		return ImageTaskView{}, false, nil
	}
	expiredDispatch, _, _, err := c.Records.ExpireImageTaskInvocation(
		context.WithoutCancel(ctx),
		dispatch.AgentName,
		dispatch.DispatchID,
		invocation.InvocationID,
		now,
	)
	if err != nil {
		return ImageTaskView{}, false, err
	}
	view, err := c.projectTarget(context.WithoutCancel(ctx), expiredDispatch)
	return view, true, err
}

func (c *ImageTaskCoordinator) expireImageTaskGapIfDue(
	ctx context.Context,
	dispatch k12.ImageTaskDispatch,
) (ImageTaskView, bool, error) {
	now := c.now()
	if dispatch.AutomaticDeadlineAt == 0 ||
		dispatch.AutomaticDeadlineAt > now {
		return ImageTaskView{}, false, nil
	}
	expiredDispatch, _, changed, err := c.Records.ExpireImageTaskInvocation(
		context.WithoutCancel(ctx),
		dispatch.AgentName,
		dispatch.DispatchID,
		"",
		now,
	)
	if err != nil {
		return ImageTaskView{}, false, err
	}
	if !changed {
		return ImageTaskView{}, false, nil
	}
	view, err := c.projectTarget(context.WithoutCancel(ctx), expiredDispatch)
	return view, true, err
}

// StartAsync advances one already-persisted dispatch from its durable
// checkpoint. It is deliberately separate from Create: POST acceptance returns
// the dispatch identity before any classifier/OCR/feedback provider call.
func (c *ImageTaskCoordinator) StartAsync(agentName, dispatchID string) bool {
	if c == nil {
		return false
	}
	agentName = strings.TrimSpace(agentName)
	dispatchID = strings.TrimSpace(dispatchID)
	if agentName == "" || dispatchID == "" {
		return false
	}
	key := agentName + "\x00" + dispatchID
	c.workerMu.Lock()
	c.initWorkerRuntimeLocked()
	if c.sealed {
		c.workerMu.Unlock()
		return false
	}
	if c.active == nil {
		c.active = map[string]bool{}
	}
	if c.active[key] {
		c.workerMu.Unlock()
		return false
	}
	c.active[key] = true
	if c.workerCount == 0 {
		c.workerIdle = make(chan struct{})
	}
	c.workerCount++
	runCtx := c.runCtx
	c.workerMu.Unlock()
	go func() {
		defer func() {
			c.workerMu.Lock()
			delete(c.active, key)
			c.workerCount--
			if c.workerCount == 0 {
				close(c.workerIdle)
			}
			c.workerMu.Unlock()
		}()
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("K12 ImageTask worker panic; durable checkpoint retained",
					"agent", agentName, "dispatch_id", dispatchID, "panic", recovered)
			}
		}()
		if _, err := c.Run(runCtx, agentName, dispatchID); err != nil {
			slog.Warn("K12 ImageTask worker stopped at durable checkpoint",
				"agent", agentName, "dispatch_id", dispatchID, "err", err)
		}
	}()
	return true
}

func (c *ImageTaskCoordinator) initWorkerRuntimeLocked() {
	if c.workerIdle == nil {
		c.workerIdle = make(chan struct{})
		close(c.workerIdle)
	}
	if c.runCtx == nil {
		base := c.BaseContext
		if base == nil {
			base = context.Background()
		}
		c.runCtx, c.runCancel = context.WithCancel(base)
	}
}

// Wait blocks until currently scheduled ImageTask workers drain or ctx ends.
func (c *ImageTaskCoordinator) Wait(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.workerMu.Lock()
	c.initWorkerRuntimeLocked()
	done := c.workerIdle
	c.workerMu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shutdown seals the coordinator before dependencies are closed, cancels
// process-owned calls, and drains all workers. Repeated calls are safe.
func (c *ImageTaskCoordinator) Shutdown(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.workerMu.Lock()
	c.initWorkerRuntimeLocked()
	c.sealed = true
	done := c.workerIdle
	cancel := c.runCancel
	c.workerMu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *ImageTaskCoordinator) beginTrackedWorkerContext(
	parent context.Context,
) (context.Context, func(), bool) {
	if parent == nil {
		parent = context.Background()
	}
	c.workerMu.Lock()
	c.initWorkerRuntimeLocked()
	if c.sealed {
		c.workerMu.Unlock()
		return nil, nil, false
	}
	if c.workerCount == 0 {
		c.workerIdle = make(chan struct{})
	}
	c.workerCount++
	runCtx := c.runCtx
	c.workerMu.Unlock()

	ctx, cancel := context.WithCancelCause(parent)
	stop := context.AfterFunc(runCtx, func() {
		cancel(context.Cause(runCtx))
	})
	return ctx, func() {
		stop()
		cancel(context.Canceled)
		c.workerMu.Lock()
		c.workerCount--
		if c.workerCount == 0 {
			close(c.workerIdle)
		}
		c.workerMu.Unlock()
	}, true
}

// Recover scans only checkpoints that are safe to continue locally. Sent,
// outcome-unknown, retryable-failed, and parent-confirmation states remain
// parked and therefore cause zero provider calls after restart.
func (c *ImageTaskCoordinator) Recover(
	ctx context.Context,
	agents []string,
) (int, error) {
	if err := c.validate(); err != nil {
		return 0, err
	}
	trackedCtx, finish, ok := c.beginTrackedWorkerContext(ctx)
	if !ok {
		return 0, ErrImageTaskCoordinatorShutdown
	}
	defer finish()
	recovered := 0
	for _, agentName := range agents {
		dispatches, err := c.Records.ListImageTaskDispatchesForRecovery(
			trackedCtx,
			strings.TrimSpace(agentName),
		)
		if err != nil {
			return recovered, err
		}
		for _, dispatch := range dispatches {
			safe, safeErr := c.recoverySafe(trackedCtx, dispatch)
			if safeErr != nil {
				return recovered, safeErr
			}
			if safe && c.StartAsync(dispatch.AgentName, dispatch.DispatchID) {
				recovered++
			}
		}
	}
	return recovered, nil
}

func (c *ImageTaskCoordinator) recoverySafe(
	ctx context.Context,
	dispatch k12.ImageTaskDispatch,
) (bool, error) {
	if dispatch.Status == k12.ImageTaskStatusRouting {
		invocation, err := c.Records.GetImageTaskInvocation(
			ctx,
			dispatch.AgentName,
			dispatch.ClassificationInvocationID,
		)
		if err == nil && invocation.DeadlineAt > 0 &&
			invocation.DeadlineAt <= c.now() &&
			(invocation.Status == k12.ImageTaskInvocationPrepared ||
				invocation.Status == k12.ImageTaskInvocationSent) {
			_, _, _, err = c.Records.ExpireImageTaskInvocation(
				context.WithoutCancel(ctx),
				dispatch.AgentName,
				dispatch.DispatchID,
				invocation.InvocationID,
				c.now(),
			)
			return false, err
		}
		return err == nil && invocation.Status == k12.ImageTaskInvocationPrepared, err
	}
	view, err := c.projectTarget(ctx, dispatch)
	if err != nil {
		return false, err
	}
	if view.Homework != nil {
		return strings.TrimSpace(view.Homework.GradingJobID) == "", nil
	}
	if view.Creative == nil {
		return false, nil
	}
	switch view.Creative.Status {
	case k12.CreativeWorkIntakePreparing:
		invocation, invocationErr := c.Records.GetLatestWritingOCRInvocation(
			ctx,
			dispatch.AgentName,
			view.Creative.IntakeID,
		)
		if errors.Is(invocationErr, k12storage.ErrImageTaskNotFound) {
			return true, nil
		}
		if invocationErr == nil && invocation.DeadlineAt > 0 &&
			invocation.DeadlineAt <= c.now() &&
			(invocation.Status == k12.ImageTaskInvocationPrepared ||
				invocation.Status == k12.ImageTaskInvocationSent) {
			_, _, _, invocationErr = c.Records.ExpireImageTaskInvocation(
				context.WithoutCancel(ctx),
				dispatch.AgentName,
				dispatch.DispatchID,
				invocation.InvocationID,
				c.now(),
			)
			return false, invocationErr
		}
		return invocationErr == nil &&
			invocation.Status == k12.ImageTaskInvocationPrepared, invocationErr
	case k12.CreativeWorkIntakeReady:
		return true, nil
	case k12.CreativeWorkIntakePromoted:
		if view.CreativeFeedback == "feedback_ready" {
			return false, nil
		}
		if view.feedbackInvocation == nil {
			return true, nil
		}
		if view.feedbackInvocation.DeadlineAt > 0 &&
			view.feedbackInvocation.DeadlineAt <= c.now() &&
			(view.feedbackInvocation.Status == k12.ImageTaskInvocationPrepared ||
				view.feedbackInvocation.Status == k12.ImageTaskInvocationSent) {
			_, _, _, invocationErr := c.Records.ExpireImageTaskInvocation(
				context.WithoutCancel(ctx),
				dispatch.AgentName,
				dispatch.DispatchID,
				view.feedbackInvocation.InvocationID,
				c.now(),
			)
			return false, invocationErr
		}
		return view.feedbackInvocation.Status == k12.ImageTaskInvocationPrepared ||
			view.feedbackInvocation.Status == k12.ImageTaskInvocationSucceeded, nil
	default:
		return false, nil
	}
}

// Run advances exactly one dispatch from durable checkpoints. A prepared
// invocation may be sent once; sent/outcome_unknown receipts are projected
// without a blind resend.
func (c *ImageTaskCoordinator) Run(
	ctx context.Context,
	agentName, dispatchID string,
) (ImageTaskView, error) {
	if err := c.validate(); err != nil {
		return ImageTaskView{}, err
	}
	dispatch, err := c.Records.GetImageTaskDispatch(
		ctx, strings.TrimSpace(agentName), strings.TrimSpace(dispatchID),
	)
	if err != nil {
		return ImageTaskView{}, err
	}
	view, err := c.projectTarget(ctx, dispatch)
	if err != nil {
		return ImageTaskView{}, err
	}
	switch dispatch.Status {
	case k12.ImageTaskStatusRouting:
		if c.Classifier == nil {
			return view, fmt.Errorf("usecase: image task classifier 未配置")
		}
		invocation, invocationErr := c.Records.GetImageTaskInvocation(
			ctx, dispatch.AgentName, dispatch.ClassificationInvocationID,
		)
		if invocationErr != nil {
			return view, invocationErr
		}
		if invocation.Status != k12.ImageTaskInvocationPrepared {
			return view, nil
		}
		if expired, yes, expireErr := c.expireImageTaskInvocationIfDue(
			ctx,
			dispatch,
			invocation,
		); yes || expireErr != nil {
			return expired, expireErr
		}
		images, readErr := c.readDispatchImages(dispatch)
		if readErr != nil {
			return view, readErr
		}
		claimedInvocation, claimed, sendErr := c.Records.ClaimImageTaskInvocationSend(
			ctx, dispatch.AgentName, invocation.InvocationID,
			"image-task:"+dispatch.DispatchID+":classification",
			c.now(),
		)
		if sendErr != nil {
			return view, sendErr
		}
		if !claimed {
			return c.Get(ctx, dispatch.AgentName, dispatch.DispatchID)
		}
		invocation = claimedInvocation
		automaticCtx, cancelAutomatic := withImageTaskAutomaticWindow(
			ctx,
			dispatch.DispatchID,
			invocation.DeadlineAt,
			c.now(),
		)
		providerCtx, cancelProvider := imageTaskProviderContext(
			automaticCtx,
			invocation.RouteSnapshot,
		)
		classified, classifyErr := c.Classifier.ClassifyImageTask(
			providerCtx,
			ImageTaskClassificationInput{
				Images: images, MessageIntent: dispatch.MessageIntent,
			},
		)
		providerCtxErr := providerCtx.Err()
		cancelProvider()
		cancelAutomatic()
		if classifyErr != nil {
			unknown := sentProviderOutcomeUnknown(classifyErr, providerCtxErr)
			failureKind := "classification_provider_failed"
			if unknown {
				failureKind = "classification_outcome_unknown"
			}
			_ = c.Records.FailImageTaskInvocation(
				context.WithoutCancel(ctx), dispatch.AgentName, invocation.InvocationID,
				failureKind, unknown, !unknown,
			)
			failed, _ := c.Get(context.WithoutCancel(ctx), dispatch.AgentName, dispatch.DispatchID)
			return failed, fmt.Errorf("classify image task: %w", classifyErr)
		}
		routed, target, commitErr := c.Records.CommitImageTaskRouting(
			ctx, dispatch.AgentName, dispatch.DispatchID, dispatch.Version,
			k12storage.ImageTaskRoutingDecision{
				Intent:     classified.Intent,
				Evidence:   append([]string(nil), classified.IntentEvidence...),
				Confidence: classified.Confidence,
				ConfirmationCandidates: append(
					[]k12.ImageTaskIntent(nil), classified.ConfirmationCandidates...,
				),
				WorkTitleCandidate:       classified.WorkTitleCandidate,
				TaskRequirementCandidate: classified.TaskRequirementCandidate,
				InvocationResultDigest:   digestJSON(classified),
			},
		)
		if commitErr != nil {
			_ = c.Records.FailImageTaskInvocation(
				context.WithoutCancel(ctx), dispatch.AgentName, invocation.InvocationID,
				"classification_contract_invalid", false, true,
			)
			return view, commitErr
		}
		view = ImageTaskView{
			Dispatch: routed, Homework: target.HomeworkSubmission,
			Creative: target.CreativeIntake,
		}
		if routed.Status != k12.ImageTaskStatusRouted {
			return view, nil
		}
		return c.continueTarget(ctx, view, images)
	case k12.ImageTaskStatusRouted:
		needsImages := (view.Homework != nil && view.Homework.GradingJobID == "") ||
			(view.Creative != nil &&
				view.Creative.WorkType == k12.WorkTypeWriting &&
				view.Creative.Status == k12.CreativeWorkIntakePreparing)
		var images [][]byte
		if needsImages {
			images, err = c.readDispatchImages(dispatch)
			if err != nil {
				return view, err
			}
		}
		return c.continueTarget(ctx, view, images)
	default:
		return view, nil
	}
}

// projectTarget is deliberately read-only. GET/result projections must never
// send provider requests, perform OCR or promote records.
func (c *ImageTaskCoordinator) projectTarget(
	ctx context.Context,
	dispatch k12.ImageTaskDispatch,
) (ImageTaskView, error) {
	view := ImageTaskView{Dispatch: dispatch}
	if (dispatch.Status == k12.ImageTaskStatusRouting ||
		dispatch.Status == k12.ImageTaskStatusFailed) &&
		strings.TrimSpace(dispatch.ClassificationInvocationID) != "" {
		invocation, invocationErr := c.Records.GetImageTaskInvocation(
			ctx,
			dispatch.AgentName,
			dispatch.ClassificationInvocationID,
		)
		if invocationErr != nil {
			return ImageTaskView{}, invocationErr
		}
		if invocation.Status == k12.ImageTaskInvocationPrepared ||
			invocation.Status == k12.ImageTaskInvocationSent {
			view.ActiveInvocationDeadlineAt = invocation.DeadlineAt
		}
	}
	if dispatch.TargetObjectType == "" || dispatch.TargetObjectID == "" {
		return view, nil
	}
	var err error
	switch dispatch.TargetObjectType {
	case k12.ImageTaskTargetHomeworkSubmission:
		value, getErr := c.Records.GetHomeworkSubmission(ctx, dispatch.AgentName, dispatch.TargetObjectID)
		err, view.Homework = getErr, &value
	case k12.ImageTaskTargetCreativeWorkIntake:
		value, getErr := c.Records.GetCreativeWorkIntake(ctx, dispatch.AgentName, dispatch.TargetObjectID)
		err, view.Creative = getErr, &value
	default:
		err = k12storage.ErrImageTaskNotFound
	}
	if err != nil {
		return ImageTaskView{}, err
	}
	if view.Creative != nil &&
		(view.Creative.Status == k12.CreativeWorkIntakePreparing ||
			view.Creative.Status == k12.CreativeWorkIntakeFailed) {
		invocation, invocationErr := c.Records.GetLatestWritingOCRInvocation(
			ctx,
			dispatch.AgentName,
			view.Creative.IntakeID,
		)
		switch {
		case invocationErr == nil &&
			(invocation.Status == k12.ImageTaskInvocationPrepared ||
				invocation.Status == k12.ImageTaskInvocationSent):
			view.ActiveInvocationDeadlineAt = invocation.DeadlineAt
		case errors.Is(invocationErr, k12storage.ErrImageTaskNotFound):
		case invocationErr != nil:
			return ImageTaskView{}, invocationErr
		}
	}
	if view.Homework != nil && view.Homework.GradingJobID != "" {
		if projector, ok := c.Grading.(imageTaskGradingProjector); ok {
			projection, projectErr := projector.ImageTaskHomeworkProjection(
				ctx, dispatch.AgentName, view.Homework.GradingJobID,
			)
			if projectErr != nil {
				return ImageTaskView{}, projectErr
			}
			view.HomeworkProjection = &projection
		}
	}
	if view.Creative != nil && view.Creative.PromotedWorkID != "" {
		record, getErr := c.Records.Get(ctx, view.Creative.PromotedWorkID)
		if getErr != nil {
			return ImageTaskView{}, getErr
		}
		if record.AgentName != dispatch.AgentName {
			return ImageTaskView{}, k12storage.ErrImageTaskNotFound
		}
		fields, parseErr := k12.ParseCreativeWorkFields(record.Fields)
		if parseErr != nil {
			return ImageTaskView{}, parseErr
		}
		generationState, stateErr := c.Records.GetCreativeWorkGenerationState(
			ctx, dispatch.AgentName, record.RecordID,
		)
		if stateErr != nil {
			return ImageTaskView{}, stateErr
		}
		fields = overlayCurrentCreativeWorkFeedback(fields, generationState)
		view.CreativeDisplayName = fields.DisplayName
		work := CreativeWorkView{
			Record:          record,
			Fields:          fields,
			GenerationState: generationState,
		}
		view.CreativeWork = &work
		view.CreativeFeedback = creativeFeedbackProjectionState(work)
		if view.CreativeFeedback != "feedback_ready" && len(fields.Versions) > 0 {
			version := fields.Versions[len(fields.Versions)-1]
			operationKey := "work:" + record.RecordID + ":version:" +
				version.VersionID + ":feedback"
			invocation, invocationErr := c.Records.GetLatestWorkFeedbackInvocation(
				ctx, dispatch.AgentName, record.RecordID, operationKey,
			)
			switch {
			case invocationErr == nil:
				view.CreativeFeedback = publicCreativeFeedbackInvocationState(invocation)
				view.feedbackInvocation = &invocation
				if invocation.Status == k12.ImageTaskInvocationPrepared ||
					invocation.Status == k12.ImageTaskInvocationSent {
					view.ActiveInvocationDeadlineAt = invocation.DeadlineAt
				}
			case !errors.Is(invocationErr, k12storage.ErrImageTaskNotFound):
				return ImageTaskView{}, invocationErr
			}
		}
	}
	return view, nil
}

func creativeFeedbackProjectionState(work CreativeWorkView) string {
	if work.GenerationState.Latest != nil &&
		work.GenerationState.Latest.Status == k12.WorkFeedbackSucceeded &&
		work.GenerationState.Latest.Feedback != nil {
		return "feedback_ready"
	}
	if work.Record != nil && work.Record.Status == k12.WorkStatusFeedbackReady &&
		len(work.Fields.Versions) > 0 {
		version := work.Fields.Versions[len(work.Fields.Versions)-1]
		if version.StructuredFeedback != nil &&
			strings.TrimSpace(version.StructuredFeedback.ProjectionMarkdown) != "" {
			return "feedback_ready"
		}
	}
	return "feedback_pending"
}

func publicCreativeFeedbackInvocationState(invocation k12.ImageTaskInvocation) string {
	switch invocation.Status {
	case k12.ImageTaskInvocationPrepared,
		k12.ImageTaskInvocationSent,
		k12.ImageTaskInvocationSucceeded:
		return "feedback_pending"
	case k12.ImageTaskInvocationFailed:
		return "feedback_failed"
	case k12.ImageTaskInvocationOutcomeUnknown:
		return "feedback_outcome_unknown"
	default:
		return "feedback_pending"
	}
}

func imageTaskWorkFeedbackContext(
	ctx context.Context,
	snapshot k12.ImageTaskRouteSnapshot,
) context.Context {
	snapshot = k12.NormalizeImageTaskRouteSnapshot(snapshot)
	if err := snapshot.Validate(); err != nil {
		return ctx
	}
	return withWorkFeedbackRouteSnapshot(ctx, snapshot)
}

func (c *ImageTaskCoordinator) continueCreativeFeedback(
	ctx context.Context,
	view ImageTaskView,
) (ImageTaskView, error) {
	if view.Creative == nil ||
		view.Creative.Status != k12.CreativeWorkIntakePromoted ||
		strings.TrimSpace(view.Creative.PromotedWorkID) == "" {
		return view, nil
	}
	projected, err := c.projectTarget(ctx, view.Dispatch)
	if err != nil {
		return view, err
	}
	if projected.CreativeFeedback == "feedback_ready" ||
		projected.CreativeFeedback == "feedback_failed" ||
		projected.CreativeFeedback == "feedback_outcome_unknown" {
		return projected, nil
	}
	if projected.feedbackInvocation != nil {
		switch projected.feedbackInvocation.Status {
		case k12.ImageTaskInvocationSent,
			k12.ImageTaskInvocationOutcomeUnknown,
			k12.ImageTaskInvocationFailed:
			return projected, nil
		}
	}
	if c.WorkFeedback == nil {
		return projected, fmt.Errorf("usecase: creative image task work feedback 未配置")
	}
	automaticCtx, cancelAutomatic := withImageTaskAutomaticWindow(
		ctx,
		view.Dispatch.DispatchID,
		view.Dispatch.AutomaticDeadlineAt,
		c.now(),
	)
	defer cancelAutomatic()
	feedbackCtx := imageTaskWorkFeedbackContext(
		automaticCtx,
		view.Dispatch.RoutePolicySnapshot,
	)
	if _, err := c.WorkFeedback.GenerateWorkFeedback(
		feedbackCtx, view.Dispatch.AgentName, view.Creative.PromotedWorkID,
	); err != nil {
		return projected, err
	}
	return c.projectTarget(ctx, view.Dispatch)
}

func (c *ImageTaskCoordinator) continueTarget(
	ctx context.Context,
	view ImageTaskView,
	images [][]byte,
) (ImageTaskView, error) {
	if expired, yes, err := c.expireImageTaskGapIfDue(
		ctx,
		view.Dispatch,
	); yes || err != nil {
		return expired, err
	}
	if view.Homework != nil {
		if view.Homework.GradingJobID != "" {
			return view, nil
		}
		if c.Grading == nil {
			return view, fmt.Errorf("usecase: homework image task grading orchestrator 未配置")
		}
		grade := ""
		if c.ResolveGrade != nil {
			var gradeErr error
			grade, gradeErr = c.ResolveGrade(ctx, view.Dispatch.AgentName)
			if gradeErr != nil {
				return view, gradeErr
			}
		}
		job, _, err := c.Grading.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
			Photo: PhotoGradeRequest{
				AgentName: view.Dispatch.AgentName, SourceSession: view.Dispatch.SourceSessionID,
				Image:      append([]byte(nil), images[0]...),
				TaskIntent: photoTaskIntentFromDispatch(view.Dispatch.TaskIntent),
				Grade:      strings.TrimSpace(grade),
			},
			SourceKind: "image_task", SourceKey: view.Dispatch.DispatchID,
			ModelSnapshot:  gradingSnapshotFromImageRoute(view.Dispatch.RoutePolicySnapshot),
			BudgetSnapshot: c.GradingBudgetSnapshot,
			ParentAutomaticAttemptID: fmt.Sprintf(
				"%s:%d",
				view.Dispatch.DispatchID,
				view.Dispatch.AutomaticStartedAt,
			),
			ParentAutomaticDeadlineAt: view.Dispatch.AutomaticDeadlineAt,
		})
		if err != nil {
			return view, err
		}
		if job.Record == nil || strings.TrimSpace(job.Record.RecordID) == "" {
			return view, fmt.Errorf("usecase: grading orchestrator 未返回 durable job identity")
		}
		submission, err := c.Records.BindHomeworkSubmissionGradingJob(
			ctx, view.Dispatch.AgentName, view.Homework.SubmissionID,
			job.Record.RecordID, view.Homework.Version,
		)
		if err != nil {
			return view, err
		}
		view.Homework = &submission
		c.Grading.StartAsync(job.Record.RecordID)
		return view, nil
	}
	if view.Creative == nil {
		return view, nil
	}
	intake := *view.Creative
	if intake.Status == k12.CreativeWorkIntakePromoted {
		return c.continueCreativeFeedback(ctx, view)
	}
	if intake.WorkType == k12.WorkTypeWriting && intake.Status == k12.CreativeWorkIntakePreparing {
		if c.WritingOCR == nil {
			return view, fmt.Errorf("usecase: writing OCR 未配置")
		}
		prepared, err := c.Records.GetLatestWritingOCRInvocation(
			ctx,
			intake.AgentName,
			intake.IntakeID,
		)
		if errors.Is(err, k12storage.ErrImageTaskNotFound) {
			ocrRoute := intake.RoutePolicySnapshot
			if intake.PromotionPolicy == k12.CreativeWorkPromotionExplicitCommit {
				if c.ResolveRoute == nil {
					return view, fmt.Errorf("usecase: writing OCR route resolver 未配置")
				}
				ocrRoute, err = c.ResolveRoute(view.Dispatch.OperationRouteRequest)
				if err != nil {
					return view, fmt.Errorf(
						"%w: resolve writing OCR route: %v",
						ErrInvalidInput,
						err,
					)
				}
				ocrRoute = k12.NormalizeImageTaskRouteSnapshot(ocrRoute)
				if ocrRoute.TimeoutMS <= 0 {
					ocrRoute.TimeoutMS = int(imageTaskDefaultProviderTimeout / time.Millisecond)
				}
			}
			ocrRoute.PromptVersion = "creative-work-writing-ocr-v1"
			ocrRoute.Capability = "vision"
			if err := ocrRoute.Validate(); err != nil {
				return view, fmt.Errorf("%w: writing OCR route: %v", ErrInvalidInput, err)
			}
			invocation := k12.ImageTaskInvocation{
				InvocationID: c.id("writing_ocr"), AgentName: intake.AgentName,
				IntakeID:     intake.IntakeID,
				Operation:    k12.ImageTaskOperationWritingOCR,
				OperationKey: "intake:" + intake.IntakeID + ":writing-ocr",
				RequestDigest: digestJSON(struct{ Intake, Source string }{
					intake.IntakeID,
					intake.SourceDigest,
				}),
				RouteSnapshot: ocrRoute, Status: k12.ImageTaskInvocationPrepared, Attempt: 1,
				DeadlineAt: view.Dispatch.AutomaticDeadlineAt,
				CreatedAt:  c.now(), UpdatedAt: c.now(),
			}
			prepared, _, err = c.Records.PrepareImageTaskInvocation(ctx, invocation)
			if err != nil {
				return view, err
			}
		} else if err != nil {
			return view, err
		}
		switch prepared.Status {
		case k12.ImageTaskInvocationPrepared:
			if expired, yes, expireErr := c.expireImageTaskInvocationIfDue(
				ctx,
				view.Dispatch,
				prepared,
			); yes || expireErr != nil {
				return expired, expireErr
			}
			intake, err = c.executeWritingOCR(
				ctx,
				view.Dispatch,
				intake,
				prepared,
				images,
			)
			if err != nil {
				return view, err
			}
			view.Creative = &intake
		case k12.ImageTaskInvocationSucceeded:
			updated, getErr := c.Records.GetCreativeWorkIntake(
				ctx, intake.AgentName, intake.IntakeID,
			)
			if getErr != nil {
				return view, getErr
			}
			view.Creative = &updated
		case k12.ImageTaskInvocationSent,
			k12.ImageTaskInvocationOutcomeUnknown,
			k12.ImageTaskInvocationFailed:
			// A prepared request may be sent exactly once. A sent or uncertain
			// receipt must be reconciled by provider request key; a retryable
			// failure may advance only through the explicit retry command.
			return view, nil
		default:
			return view, k12storage.ErrImageTaskInvalidState
		}
	}
	if view.Creative.Status == k12.CreativeWorkIntakeReady {
		if view.Creative.PromotionPolicy == k12.CreativeWorkPromotionExplicitCommit {
			return view, nil
		}
		// File IO stays outside the promotion transaction. Re-read immediately
		// before the CAS so deleted/replaced content-addressed evidence cannot be
		// promoted from an earlier in-memory copy.
		if _, err := c.readDispatchImages(view.Dispatch); err != nil {
			return view, err
		}
		workID, _, err := c.Records.PromoteCreativeWorkIntake(
			ctx, view.Creative.AgentName, view.Creative.IntakeID, view.Creative.Version,
		)
		if err != nil {
			return view, err
		}
		updated, err := c.Records.GetCreativeWorkIntake(ctx, view.Creative.AgentName, view.Creative.IntakeID)
		if err != nil {
			return view, err
		}
		if updated.PromotedWorkID != workID {
			return view, fmt.Errorf("usecase: promoted work projection drift")
		}
		view.Creative = &updated
		return c.continueCreativeFeedback(ctx, view)
	}
	return view, nil
}

func photoTaskIntentFromDispatch(intent k12.ImageTaskIntent) PhotoTaskIntent {
	if intent == k12.ImageTaskIntentBlankWorksheet {
		return PhotoTaskBlankWorksheet
	}
	return PhotoTaskCompletedHomework
}

func (c *ImageTaskCoordinator) executeWritingOCR(
	ctx context.Context,
	dispatch k12.ImageTaskDispatch,
	intake k12.CreativeWorkIntake,
	invocation k12.ImageTaskInvocation,
	images [][]byte,
) (k12.CreativeWorkIntake, error) {
	if len(images) == 0 {
		return intake, fmt.Errorf("%w: writing OCR resume 缺少 immutable source image", ErrInvalidInput)
	}
	if invocation.Status == k12.ImageTaskInvocationPrepared {
		claimedInvocation, claimed, err := c.Records.ClaimImageTaskInvocationSend(
			ctx, intake.AgentName, invocation.InvocationID,
			"image-task:"+intake.DispatchID+":writing-ocr",
			c.now(),
		)
		if err != nil {
			return intake, err
		}
		if !claimed {
			return c.Records.GetCreativeWorkIntake(
				ctx,
				intake.AgentName,
				intake.IntakeID,
			)
		}
		invocation = claimedInvocation
	}
	automaticCtx, cancelAutomatic := withImageTaskAutomaticWindow(
		ctx,
		dispatch.DispatchID,
		invocation.DeadlineAt,
		c.now(),
	)
	providerCtx, cancelProvider := imageTaskProviderContext(
		automaticCtx,
		invocation.RouteSnapshot,
	)
	ocr, err := c.WritingOCR.RecognizeImageTaskWriting(providerCtx, images[0])
	providerCtxErr := providerCtx.Err()
	cancelProvider()
	cancelAutomatic()
	if err != nil {
		unknown := sentProviderOutcomeUnknown(err, providerCtxErr)
		failureKind := "writing_ocr_provider_failed"
		if unknown {
			failureKind = "writing_ocr_outcome_unknown"
		}
		_ = c.Records.FailImageTaskInvocation(
			context.WithoutCancel(ctx), intake.AgentName, invocation.InvocationID,
			failureKind, unknown, !unknown,
		)
		return intake, err
	}
	ocr.Raw = strings.TrimSpace(ocr.Raw)
	ocr.CanonicalContent = strings.TrimSpace(ocr.CanonicalContent)
	if ocr.CanonicalContent == "" {
		ocr.CanonicalContent = ocr.Raw
	}
	sum := sha256.Sum256([]byte(ocr.CanonicalContent))
	evidence := k12.CreativeWorkIntakeOCREvidence{
		Raw: ocr.Raw, CanonicalContent: ocr.CanonicalContent, CanonicalVersion: 1,
		CanonicalDigest: "sha256:" + hex.EncodeToString(sum[:]),
		Confidence:      ocr.Confidence,
		RiskSegments:    append([]k12.CreativeWorkIntakeOCRRisk(nil), ocr.RiskSegments...),
	}
	if intake.PromotionPolicy == k12.CreativeWorkPromotionExplicitCommit {
		return c.Records.HoldCreativeWorkIntakeOCRConfirmation(
			ctx, intake.AgentName, intake.IntakeID, intake.Version,
			invocation.InvocationID, evidence,
		)
	}
	if ocr.Confidence >= 0.95 && len(ocr.RiskSegments) == 0 {
		evidence.ConfirmationProvenance = k12.CreativeWorkEvidenceAutoFreeze
		evidence.FrozenAt = c.now()
		return c.Records.FreezeCreativeWorkIntakeOCR(
			ctx, intake.AgentName, intake.IntakeID, intake.Version,
			invocation.InvocationID, evidence, k12.CreativeWorkEvidenceAutoFreeze,
		)
	}
	if len(evidence.RiskSegments) == 0 {
		evidence.RiskSegments = []k12.CreativeWorkIntakeOCRRisk{{
			SegmentID: "document", RawText: evidence.Raw,
			Reasons: []string{"confidence_below_auto_freeze_threshold"},
		}}
	}
	return c.Records.HoldCreativeWorkIntakeOCRConfirmation(
		ctx, intake.AgentName, intake.IntakeID, intake.Version,
		invocation.InvocationID, evidence,
	)
}

func (c *ImageTaskCoordinator) Get(
	ctx context.Context,
	agentName, dispatchID string,
) (ImageTaskView, error) {
	if c == nil || c.Records == nil {
		return ImageTaskView{}, fmt.Errorf("usecase: image task store 未配置")
	}
	dispatch, err := c.Records.GetImageTaskDispatch(ctx, strings.TrimSpace(agentName), strings.TrimSpace(dispatchID))
	if err != nil {
		return ImageTaskView{}, err
	}
	return c.projectTarget(ctx, dispatch)
}

func (c *ImageTaskCoordinator) Result(
	ctx context.Context,
	agentName, dispatchID string,
) (ImageTaskResult, error) {
	view, err := c.Get(ctx, agentName, dispatchID)
	if err != nil {
		return ImageTaskResult{}, err
	}
	result := ImageTaskResult{
		Kind: "pending", Dispatch: view.Dispatch, Creative: view.Creative,
		CreativeDisplayName: view.CreativeDisplayName, CreativeWork: view.CreativeWork,
	}
	if view.Dispatch.Status == k12.ImageTaskStatusAwaitingConfirmation ||
		(view.Creative != nil && view.Creative.Status == k12.CreativeWorkIntakeAwaitingConfirmation) {
		result.Kind = "awaiting_confirmation"
		return result, nil
	}
	if view.Creative != nil && view.Creative.Status == k12.CreativeWorkIntakePromoted &&
		(view.CreativeFeedback == "feedback_ready" ||
			view.Creative.PromotionPolicy == k12.CreativeWorkPromotionExplicitCommit) {
		result.Kind = "creative"
		return result, nil
	}
	if view.Homework != nil && view.Homework.GradingJobID != "" {
		finalArtifact, finalArtifactErr := c.Records.GetGradingFinalArtifactByJob(
			ctx, agentName, view.Homework.GradingJobID,
		)
		if finalArtifactErr == nil {
			result.Kind = string(view.Dispatch.TaskIntent)
			result.FinalArtifact = &finalArtifact
		} else if !errors.Is(finalArtifactErr, records.ErrNotFound) {
			return ImageTaskResult{}, finalArtifactErr
		}
		reader, ok := c.Grading.(imageTaskPhotoResultReader)
		if !ok {
			return result, nil
		}
		photo, ok := reader.PhotoResult(view.Homework.GradingJobID)
		if ok {
			result.Kind = string(view.Dispatch.TaskIntent)
			result.Photo = &photo
		}
	}
	return result, nil
}

// ResolveTutoringTipsGradingJob is an internal server-side bridge. Public
// callers address only the ImageTaskDispatch; the GradingJob identity never
// crosses the HTTP boundary.
func (c *ImageTaskCoordinator) ResolveTutoringTipsGradingJob(
	ctx context.Context,
	agentName, dispatchID string,
) (string, error) {
	view, err := c.Get(ctx, strings.TrimSpace(agentName), strings.TrimSpace(dispatchID))
	if err != nil {
		return "", err
	}
	if view.Dispatch.Status != k12.ImageTaskStatusRouted ||
		(view.Dispatch.TaskIntent != k12.ImageTaskIntentCompletedHomework &&
			view.Dispatch.TaskIntent != k12.ImageTaskIntentBlankWorksheet) ||
		view.Dispatch.TargetObjectType != k12.ImageTaskTargetHomeworkSubmission ||
		view.Homework == nil ||
		strings.TrimSpace(view.Homework.GradingJobID) == "" {
		return "", k12storage.ErrImageTaskInvalidState
	}
	return view.Homework.GradingJobID, nil
}

func (c *ImageTaskCoordinator) readDispatchImages(
	dispatch k12.ImageTaskDispatch,
) ([][]byte, error) {
	reader := c.ReadAsset
	if reader == nil {
		reader = defaultImageTaskAssetReader
	}
	images := make([][]byte, len(dispatch.SourceAssetRefs))
	for index, ref := range dispatch.SourceAssetRefs {
		owner, _, parseErr := assetstore.Parse(ref)
		if parseErr != nil || owner != dispatch.AgentName {
			return nil, fmt.Errorf("%w: retry source asset owner mismatch", ErrInvalidInput)
		}
		var err error
		images[index], err = reader(dispatch.AgentName, ref)
		if err != nil {
			return nil, err
		}
		if len(images[index]) == 0 {
			return nil, fmt.Errorf("%w: retry source image %d empty", ErrInvalidInput, index)
		}
	}
	if digest := imageBytesDigest(images); digest != dispatch.SourceDigest {
		return nil, fmt.Errorf(
			"%w: immutable source digest mismatch: frozen=%s actual=%s",
			ErrInvalidInput, dispatch.SourceDigest, digest,
		)
	}
	return images, nil
}

func (c *ImageTaskCoordinator) Confirm(
	ctx context.Context,
	input ConfirmImageTaskInput,
) (ImageTaskView, error) {
	if c == nil || c.Records == nil {
		return ImageTaskView{}, fmt.Errorf("usecase: image task store 未配置")
	}
	dispatch, err := c.Records.GetImageTaskDispatch(ctx, input.AgentName, input.DispatchID)
	if err != nil {
		return ImageTaskView{}, err
	}
	if dispatch.Version != input.ExpectedVersion {
		return ImageTaskView{}, k12storage.ErrImageTaskVersionConflict
	}
	if dispatch.Status == k12.ImageTaskStatusAwaitingConfirmation {
		if input.Creative != nil || input.Subject != "" || input.Grade != "" ||
			len(input.QuestionCorrections) != 0 {
			return ImageTaskView{}, fmt.Errorf("%w: intent confirmation cannot mix target command", ErrInvalidInput)
		}
		images, err := c.readDispatchImages(dispatch)
		if err != nil {
			return ImageTaskView{}, err
		}
		routed, target, err := c.Records.ConfirmImageTaskIntent(
			ctx, input.AgentName, input.DispatchID, input.ExpectedVersion, input.Intent,
		)
		if err != nil {
			return ImageTaskView{}, err
		}
		view := ImageTaskView{
			Dispatch: routed, Homework: target.HomeworkSubmission, Creative: target.CreativeIntake,
		}
		return c.continueTarget(ctx, view, images)
	}
	view, err := c.projectTarget(ctx, dispatch)
	if err != nil {
		return ImageTaskView{}, err
	}
	if view.Homework != nil {
		if input.Creative != nil || input.Intent != "" {
			return view, fmt.Errorf("%w: homework/creative confirmation branches are exclusive", ErrInvalidInput)
		}
		if c.Grading == nil || view.Homework.GradingJobID == "" {
			return view, k12storage.ErrImageTaskInvalidState
		}
		if _, ok, err := c.Grading.ConfirmPhotoGradingJob(
			ctx, view.Homework.GradingJobID,
			ConfirmPhotoGradingInput{
				Subject: input.Subject, Grade: input.Grade,
				Corrections: input.QuestionCorrections,
			},
		); err != nil || !ok {
			if err == nil {
				err = k12storage.ErrImageTaskInvalidState
			}
			return view, err
		}
		return view, nil
	}
	if view.Creative == nil || input.Creative == nil ||
		input.Intent != "" || input.Subject != "" || input.Grade != "" ||
		len(input.QuestionCorrections) != 0 {
		return view, k12storage.ErrImageTaskInvalidState
	}
	creative := input.Creative
	switch creative.Action {
	case CreativeImageTaskActionFreezeOCR:
		if strings.TrimSpace(creative.WorkTitle) != "" ||
			strings.TrimSpace(creative.TaskRequirement) != "" ||
			strings.TrimSpace(creative.Intent) != "" ||
			strings.TrimSpace(creative.ContentMarkdown) != "" {
			return view, fmt.Errorf("%w: freeze_ocr cannot carry commit fields", ErrInvalidInput)
		}
		if view.Creative.Status != k12.CreativeWorkIntakeAwaitingConfirmation {
			return view, k12storage.ErrImageTaskInvalidState
		}
		images, err := c.readDispatchImages(dispatch)
		if err != nil {
			return view, err
		}
		intake, err := c.Records.ConfirmCreativeWorkIntakeOCR(
			ctx, input.AgentName, view.Creative.IntakeID, view.Creative.Version,
			creative.CanonicalVersion, creative.CanonicalContent,
			creative.SegmentCorrections,
		)
		if err != nil {
			return view, err
		}
		view.Creative = &intake
		return c.continueTarget(ctx, view, images)
	case CreativeImageTaskActionCommit:
		if creative.CanonicalVersion != 0 ||
			strings.TrimSpace(creative.CanonicalContent) != "" ||
			len(creative.SegmentCorrections) != 0 {
			return view, fmt.Errorf("%w: commit cannot carry freeze_ocr fields", ErrInvalidInput)
		}
		command := k12.CreativeWorkCommitCommand{
			WorkTitle:       strings.TrimSpace(creative.WorkTitle),
			TaskRequirement: strings.TrimSpace(creative.TaskRequirement),
			Intent:          strings.TrimSpace(creative.Intent),
			ContentMarkdown: strings.TrimSpace(creative.ContentMarkdown),
		}
		if view.Creative.WorkType == k12.WorkTypeWriting &&
			command.ContentMarkdown != "" &&
			(view.Creative.OCREvidence == nil ||
				command.ContentMarkdown != view.Creative.OCREvidence.CanonicalContent) {
			return view, fmt.Errorf("%w: writing content changed after freeze_ocr", ErrInvalidInput)
		}
		command.CommandDigest = digestJSON(struct {
			DispatchID, IntakeID, WorkTitle, TaskRequirement, Intent, ContentMarkdown string
		}{
			dispatch.DispatchID, view.Creative.IntakeID, command.WorkTitle,
			command.TaskRequirement, command.Intent, command.ContentMarkdown,
		})
		intake, err := c.Records.CommitManualCreativeWorkIntake(
			ctx, input.AgentName, view.Creative.IntakeID,
			view.Creative.Version, command,
		)
		if err != nil {
			return view, err
		}
		view.Creative = &intake
		return c.projectTarget(ctx, dispatch)
	default:
		return view, fmt.Errorf("%w: creative action must be freeze_ocr or commit", ErrInvalidInput)
	}
}

// Retry executes only a durably retry-safe failed invocation and reuses its
// frozen route snapshot. GET/result never call this path.
func (c *ImageTaskCoordinator) Retry(
	ctx context.Context,
	agentName, dispatchID string,
	expectedVersion int,
) (ImageTaskView, error) {
	if err := c.validate(); err != nil {
		return ImageTaskView{}, err
	}
	original, err := c.Records.GetImageTaskDispatch(ctx, agentName, dispatchID)
	if err != nil {
		return ImageTaskView{}, err
	}
	if original.Version != expectedVersion {
		return ImageTaskView{}, k12storage.ErrImageTaskVersionConflict
	}
	current, err := c.projectTarget(ctx, original)
	if err != nil {
		return ImageTaskView{}, err
	}
	if current.Homework != nil &&
		strings.TrimSpace(current.Homework.GradingJobID) != "" {
		retrier, ok := c.Grading.(imageTaskGradingParentWindowRetrier)
		if !ok {
			return current, k12storage.ErrImageTaskInvalidState
		}
		allowed, preflightErr := retrier.CanRetryPhotoGradingWithParentAutomaticWindow(
			ctx,
			current.Homework.GradingJobID,
		)
		if preflightErr != nil {
			return current, preflightErr
		}
		if !allowed {
			return current, k12storage.ErrImageTaskInvalidState
		}
		restarted, restartErr := c.Records.RestartImageTaskAutomaticWindow(
			ctx,
			agentName,
			dispatchID,
			expectedVersion,
			c.now(),
		)
		if restartErr != nil {
			return current, restartErr
		}
		parentAttemptID := fmt.Sprintf(
			"%s:%d",
			restarted.DispatchID,
			restarted.AutomaticStartedAt,
		)
		if _, started, retryErr := retrier.RetryPhotoGradingJobWithParentAutomaticWindow(
			ctx,
			current.Homework.GradingJobID,
			parentAttemptID,
			restarted.AutomaticDeadlineAt,
		); retryErr != nil || !started {
			if retryErr == nil {
				retryErr = k12storage.ErrImageTaskInvalidState
			}
			return current, retryErr
		}
		return c.projectTarget(ctx, restarted)
	}
	if current.Creative != nil &&
		current.Creative.Status == k12.CreativeWorkIntakePromoted {
		if current.CreativeFeedback != "feedback_failed" ||
			c.WorkFeedback == nil {
			return current, k12storage.ErrImageTaskInvalidState
		}
		dispatch := current.Dispatch
		if dispatch.Status == k12.ImageTaskStatusFailed && dispatch.RetrySafe {
			dispatch, err = c.Records.RestartImageTaskAutomaticWindow(
				ctx,
				agentName,
				dispatchID,
				expectedVersion,
				c.now(),
			)
			if err != nil {
				return current, err
			}
			current.Dispatch = dispatch
		}
		automaticCtx, cancelAutomatic := withImageTaskAutomaticWindow(
			ctx,
			dispatch.DispatchID,
			dispatch.AutomaticDeadlineAt,
			c.now(),
		)
		defer cancelAutomatic()
		feedbackCtx := imageTaskWorkFeedbackContext(
			automaticCtx,
			dispatch.RoutePolicySnapshot,
		)
		if _, err := c.WorkFeedback.GenerateWorkFeedback(
			feedbackCtx, agentName, current.Creative.PromotedWorkID,
		); err != nil {
			return current, err
		}
		return c.projectTarget(ctx, dispatch)
	}
	if original.Status == k12.ImageTaskStatusFailed &&
		original.RetrySafe &&
		original.TargetObjectType != "" &&
		original.TargetObjectID != "" {
		restarted, restartErr := c.Records.RestartImageTaskAutomaticWindow(
			ctx,
			agentName,
			dispatchID,
			expectedVersion,
			c.now(),
		)
		if restartErr != nil {
			return current, restartErr
		}
		current.Dispatch = restarted
		var images [][]byte
		if current.Homework != nil ||
			(current.Creative != nil &&
				current.Creative.WorkType == k12.WorkTypeWriting &&
				current.Creative.Status == k12.CreativeWorkIntakePreparing) {
			images, err = c.readDispatchImages(restarted)
			if err != nil {
				return current, err
			}
		}
		return c.continueTarget(ctx, current, images)
	}
	images, err := c.readDispatchImages(original)
	if err != nil {
		return ImageTaskView{}, err
	}
	dispatch, invocation, err := c.Records.PrepareImageTaskRetry(
		ctx, agentName, dispatchID, expectedVersion, c.id(string(invocationKindForDispatch(original))+"_retry"),
	)
	if err != nil {
		return ImageTaskView{}, err
	}
	switch invocation.Operation {
	case k12.ImageTaskOperationClassification:
		if c.Classifier == nil {
			return ImageTaskView{}, fmt.Errorf("usecase: image task classifier 未配置")
		}
		claimedInvocation, claimed, err := c.Records.ClaimImageTaskInvocationSend(
			ctx, agentName, invocation.InvocationID,
			"image-task:"+dispatchID+":classification:retry",
			c.now(),
		)
		if err != nil {
			return ImageTaskView{}, err
		}
		if !claimed {
			return c.Get(ctx, agentName, dispatchID)
		}
		invocation = claimedInvocation
		automaticCtx, cancelAutomatic := withImageTaskAutomaticWindow(
			ctx,
			dispatch.DispatchID,
			invocation.DeadlineAt,
			c.now(),
		)
		providerCtx, cancelProvider := imageTaskProviderContext(
			automaticCtx,
			invocation.RouteSnapshot,
		)
		classified, err := c.Classifier.ClassifyImageTask(
			providerCtx,
			ImageTaskClassificationInput{Images: images, MessageIntent: dispatch.MessageIntent},
		)
		providerCtxErr := providerCtx.Err()
		cancelProvider()
		cancelAutomatic()
		if err != nil {
			unknown := sentProviderOutcomeUnknown(err, providerCtxErr)
			_ = c.Records.FailImageTaskInvocation(
				context.WithoutCancel(ctx), agentName, invocation.InvocationID,
				"classification_retry_failed", unknown, !unknown,
			)
			return ImageTaskView{}, err
		}
		routed, target, err := c.Records.CommitImageTaskRouting(
			ctx, agentName, dispatchID, dispatch.Version,
			k12storage.ImageTaskRoutingDecision{
				Intent: classified.Intent, Evidence: classified.IntentEvidence,
				Confidence:               classified.Confidence,
				ConfirmationCandidates:   classified.ConfirmationCandidates,
				WorkTitleCandidate:       classified.WorkTitleCandidate,
				TaskRequirementCandidate: classified.TaskRequirementCandidate,
				InvocationResultDigest:   digestJSON(classified),
			},
		)
		if err != nil {
			_ = c.Records.FailImageTaskInvocation(
				context.WithoutCancel(ctx), agentName, invocation.InvocationID,
				"classification_retry_contract_invalid", false, true,
			)
			return ImageTaskView{}, err
		}
		view := ImageTaskView{
			Dispatch: routed, Homework: target.HomeworkSubmission, Creative: target.CreativeIntake,
		}
		if routed.Status == k12.ImageTaskStatusRouted {
			return c.continueTarget(ctx, view, images)
		}
		return view, nil
	case k12.ImageTaskOperationWritingOCR:
		view, err := c.projectTarget(ctx, dispatch)
		if err != nil || view.Creative == nil {
			return view, err
		}
		intake, err := c.executeWritingOCR(
			ctx,
			dispatch,
			*view.Creative,
			invocation,
			images,
		)
		if err != nil {
			return view, err
		}
		view.Creative = &intake
		return c.continueTarget(ctx, view, images)
	default:
		return ImageTaskView{}, k12storage.ErrImageTaskInvalidState
	}
}

func invocationKindForDispatch(dispatch k12.ImageTaskDispatch) k12.ImageTaskOperation {
	if dispatch.Status == k12.ImageTaskStatusFailed {
		return k12.ImageTaskOperationClassification
	}
	return k12.ImageTaskOperationWritingOCR
}

func (c *ImageTaskCoordinator) Cancel(
	ctx context.Context,
	agentName, dispatchID string,
	expectedVersion int,
) (ImageTaskView, error) {
	if c == nil || c.Records == nil {
		return ImageTaskView{}, fmt.Errorf("usecase: image task store 未配置")
	}
	current, err := c.Get(ctx, agentName, dispatchID)
	if err != nil {
		return ImageTaskView{}, err
	}
	if current.Dispatch.Status == k12.ImageTaskStatusCancelled {
		return current, nil
	}
	if current.Dispatch.Version != expectedVersion {
		return ImageTaskView{}, k12storage.ErrImageTaskVersionConflict
	}
	if current.Homework != nil && current.Homework.GradingJobID != "" {
		canceller, ok := c.Grading.(imageTaskGradingCanceller)
		if !ok {
			return ImageTaskView{}, fmt.Errorf(
				"usecase: homework image task cancellation 未配置",
			)
		}
		if err := canceller.CancelImageTaskHomework(
			ctx, agentName, current.Homework.GradingJobID,
		); err != nil {
			return ImageTaskView{}, err
		}
	}
	dispatch, err := c.Records.CancelImageTaskDispatch(ctx, agentName, dispatchID, expectedVersion)
	if err != nil {
		return ImageTaskView{}, err
	}
	return c.projectTarget(ctx, dispatch)
}
