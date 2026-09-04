package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/records"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

const (
	k12DingtalkPhotoRoutingObjectKind = "dingtalk_photo_routing_confirmation"
	k12DingtalkInboundRecoveryLimit   = 100
)

var errK12InboundPhotoRoutingSnapshotUnavailable = errors.New(
	"DingTalk inbound photo routing snapshot is unavailable",
)

type k12InboundPhotoCoordinatorPort interface {
	Admit(context.Context, k12usecase.InboundPhotoAdmission) (k12usecase.InboundPhotoBundle, bool, error)
	Resume(context.Context, string, string) (k12usecase.InboundPhotoBundle, error)
	ResumeByIdentity(context.Context, k12usecase.InboundPhotoIdentity) (k12usecase.InboundPhotoBundle, error)
	Recoverable(context.Context, int) ([]k12usecase.InboundPhotoBundle, error)
	RecordImageTask(context.Context, string, string, int64, string) (k12usecase.InboundPhotoDispatch, error)
	RecordRoutingDecision(
		context.Context, string, string, int64, k12usecase.InboundPhotoRoutingDecision,
	) (k12usecase.InboundPhotoDispatch, error)
	RequestRoutingConfirmation(context.Context, string, string, int64) (k12usecase.InboundPhotoDispatch, error)
	ConfirmRouting(
		context.Context, string, string, int64, k12usecase.InboundPhotoRoutingDecision,
	) (k12usecase.InboundPhotoDispatch, error)
	RecordFinalArtifact(context.Context, string, string, int64, string) (k12usecase.InboundPhotoDispatch, error)
	BindReplyBatch(context.Context, string, string, int64, string) (k12usecase.InboundPhotoDispatch, error)
	CompleteReply(context.Context, string, string, int64) (k12usecase.InboundPhotoDispatch, error)
	FailTerminal(
		context.Context, string, string, int64,
		k12usecase.InboundPhotoTerminalStage, string,
	) (k12usecase.InboundPhotoDispatch, error)
}

type k12InboundPhotoImageTaskPort interface {
	k12ImageTaskFacade
	Retry(context.Context, string, string, int) (k12usecase.ImageTaskView, error)
}

type k12InboundPhotoFinalArtifactReader interface {
	GetGradingFinalArtifact(context.Context, string, string) (k12.GradingFinalArtifact, error)
	OpenGradingFinalAnnotatedAsset(context.Context, string, string) (k12.GradingFinalAnnotatedAsset, error)
}

type k12DingtalkPhotoReplyIdentityPort interface {
	k12DingtalkPhotoReplyBatchPort
	GetDeliveryBatchForMessageIdentity(
		context.Context,
		string,
		string,
		string,
		string,
		[]k12usecase.DeliveryAttachmentIdentity,
	) (k12.DeliveryBatch, error)
}

type k12DingtalkPhotoInboundRuntimeConfig struct {
	BaseContext       context.Context
	Router            *agentrouter.Dispatcher
	Check             func(context.Context, *adapter.Message) error
	ResolveInstanceID func(string, string) (string, error)
	Inbound           k12InboundPhotoCoordinatorPort
	ImageTasks        k12InboundPhotoImageTaskPort
	PracticeSets      k12InboundPhotoPracticeSetReader
	PracticeReturns   k12InboundPhotoPracticeReturnPort
	Artifacts         k12InboundPhotoFinalArtifactReader
	ReplyBatches      k12DingtalkPhotoReplyIdentityPort
	Now               func() int64
	PollInterval      time.Duration
	RetryInterval     time.Duration
	RestartCheckpoint k12DingtalkPhotoRestartCheckpointPort
}

// k12DingtalkPhotoInboundRuntime 只编排 V88、ImageTask、V89 与既有 DeliveryBatch；
// 各自的幂等、CAS、媒体准备与回执状态机仍由原有领域实现唯一拥有。
type k12DingtalkPhotoInboundRuntime struct {
	baseCtx           context.Context
	router            *agentrouter.Dispatcher
	check             func(context.Context, *adapter.Message) error
	resolveInstanceID func(string, string) (string, error)
	inbound           k12InboundPhotoCoordinatorPort
	imageTasks        k12InboundPhotoImageTaskPort
	practiceSets      k12InboundPhotoPracticeSetReader
	practiceReturns   k12InboundPhotoPracticeReturnPort
	artifacts         k12InboundPhotoFinalArtifactReader
	replyBatches      k12DingtalkPhotoReplyIdentityPort
	replies           *k12DingtalkPhotoReplyCoordinator
	pollInterval      time.Duration
	retryInterval     time.Duration
	now               func() int64
	restartCheckpoint k12DingtalkPhotoRestartCheckpointPort

	workerMu sync.Mutex
	running  map[string]struct{}
}

func newK12DingtalkPhotoInboundRuntime(
	config k12DingtalkPhotoInboundRuntimeConfig,
) *k12DingtalkPhotoInboundRuntime {
	baseCtx := config.BaseContext
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	pollInterval := config.PollInterval
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}
	retryInterval := config.RetryInterval
	if retryInterval <= 0 {
		retryInterval = time.Second
	}
	now := config.Now
	if now == nil {
		now = func() int64 { return time.Now().Unix() }
	}
	runtime := &k12DingtalkPhotoInboundRuntime{
		baseCtx: baseCtx, router: config.Router, check: config.Check,
		resolveInstanceID: config.ResolveInstanceID,
		inbound:           config.Inbound, imageTasks: config.ImageTasks,
		practiceSets: config.PracticeSets, practiceReturns: config.PracticeReturns,
		artifacts: config.Artifacts, replyBatches: config.ReplyBatches,
		pollInterval: pollInterval, retryInterval: retryInterval,
		now:     now,
		running: make(map[string]struct{}),
	}
	runtime.restartCheckpoint = config.RestartCheckpoint
	if runtime.restartCheckpoint == nil {
		runtime.restartCheckpoint = newK12DingtalkPhotoRestartCheckpointFromEnvironment()
	}
	if config.ReplyBatches != nil {
		runtime.replies = newK12DingtalkPhotoReplyCoordinator(config.ReplyBatches)
	}
	return runtime
}

type k12DingtalkInboundPhotoCommand struct {
	SchemaVersion   int    `json:"schema_version"`
	SourceSessionID string `json:"source_session_id"`
	MessageIntent   string `json:"message_intent"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
}

func normalizeK12DingtalkInboundPhotoRoute(provider, model string) (string, string, bool) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	return provider, model, provider != "" && model != "" &&
		!strings.EqualFold(provider, "auto") && !strings.EqualFold(model, "auto")
}

func k12DingtalkInboundIdentity(msg *adapter.Message) (k12usecase.InboundPhotoIdentity, error) {
	if msg == nil {
		return k12usecase.InboundPhotoIdentity{}, fmt.Errorf("DingTalk inbound photo message is required")
	}
	identity := k12usecase.InboundPhotoIdentity{
		Platform:          strings.ToLower(strings.TrimSpace(string(msg.Platform))),
		InstanceID:        strings.TrimSpace(msg.InstanceID),
		ChatID:            strings.TrimSpace(msg.ChatID),
		ProviderMessageID: strings.TrimSpace(msg.ID),
	}
	if identity.Platform != string(adapter.PlatformDingtalk) || identity.InstanceID == "" ||
		identity.ChatID == "" || identity.ProviderMessageID == "" {
		return k12usecase.InboundPhotoIdentity{}, fmt.Errorf("DingTalk inbound photo identity is incomplete")
	}
	return identity, nil
}

// AdmitInboundPhoto 在 callback ACK 前只做安全校验与 V88 原子接纳；模型与投递均由进程 worker 接续。
func (r *k12DingtalkPhotoInboundRuntime) AdmitInboundPhoto(
	ctx context.Context,
	msg *adapter.Message,
) (bool, error) {
	startedAt := time.Now()
	if r == nil || r.inbound == nil {
		return false, fmt.Errorf("DingTalk inbound photo runtime is unavailable")
	}
	if msg == nil || msg.Metadata["conversation_type"] == "2" ||
		len(msg.Attachments) != 1 || strings.TrimSpace(msg.Attachments[0].Type) != "image" {
		return false, nil
	}
	identity, err := k12DingtalkInboundIdentity(msg)
	if err != nil {
		return false, err
	}
	raw, err := decodeK12PhotoAttachment(msg.Attachments[0])
	if err != nil {
		return false, err
	}

	// 完整 provider identity 的首次冻结值优先于当前可变路由。
	existing, resumeErr := r.inbound.ResumeByIdentity(ctx, identity)
	if resumeErr == nil {
		if r.check != nil {
			if err := r.check(ctx, msg); err != nil {
				return false, err
			}
		}
		bundle, _, err := r.inbound.Admit(ctx, k12usecase.InboundPhotoAdmission{
			OwnerScope: existing.Receipt.OwnerScope, AgentName: existing.Receipt.AgentName,
			BindingID: existing.Receipt.BindingID, Identity: existing.Receipt.Identity,
			AssetName: existing.Asset.Name, AssetMIME: existing.Asset.MIME,
			AssetBytes: raw, CommandJSON: existing.Receipt.CommandJSON,
		})
		if err != nil {
			return false, err
		}
		slog.Info("K12 DingTalk inbound photo admitted",
			"stage", "admitted",
			"agent_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Receipt.AgentName),
			"receipt_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Receipt.ReceiptID),
			"resumed", true,
			"elapsed_ms", time.Since(startedAt).Milliseconds(),
		)
		r.schedule(bundle.Receipt.AgentName, bundle.Receipt.ReceiptID)
		return true, nil
	}
	if !errors.Is(resumeErr, records.ErrNotFound) {
		return false, resumeErr
	}

	routed := routeK12DingtalkPhotoTutor(msg, r.router)
	if routed == nil {
		return false, nil
	}
	if r.check != nil {
		if err := r.check(ctx, msg); err != nil {
			return false, err
		}
	}
	provider, model, exactRoute := normalizeK12DingtalkInboundPhotoRoute(
		routed.AgentConfig.Provider, routed.AgentConfig.Model,
	)
	if !exactRoute {
		return false, fmt.Errorf("DingTalk inbound TutorAgent route is incomplete")
	}
	commandJSON, err := json.Marshal(k12DingtalkInboundPhotoCommand{
		SchemaVersion: 2, SourceSessionID: k12PhotoSourceSession(msg),
		MessageIntent: strings.TrimSpace(msg.Content), Provider: provider, Model: model,
	})
	if err != nil {
		return false, fmt.Errorf("encode DingTalk inbound photo command: %w", err)
	}
	attachment := msg.Attachments[0]
	assetName := strings.TrimSpace(attachment.Name)
	if assetName == "" {
		assetName = "dingtalk-picture"
	}
	bundle, _, err := r.inbound.Admit(ctx, k12usecase.InboundPhotoAdmission{
		OwnerScope: k12usecase.DefaultLocalOwnerScope,
		AgentName:  routed.AgentName, BindingID: stableBindingID(*routed.Rule),
		Identity: identity, AssetName: assetName,
		AssetMIME:  strings.ToLower(strings.TrimSpace(attachment.Mime)),
		AssetBytes: raw, CommandJSON: string(commandJSON),
	})
	if err != nil {
		return false, err
	}
	slog.Info("K12 DingTalk inbound photo admitted",
		"stage", "admitted",
		"agent_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Receipt.AgentName),
		"receipt_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Receipt.ReceiptID),
		"resumed", false,
		"elapsed_ms", time.Since(startedAt).Milliseconds(),
	)
	r.schedule(bundle.Receipt.AgentName, bundle.Receipt.ReceiptID)
	return true, nil
}

func (r *k12DingtalkPhotoInboundRuntime) workerKey(agentName, receiptID string) string {
	return strings.TrimSpace(agentName) + "\x00" + strings.TrimSpace(receiptID)
}

func (r *k12DingtalkPhotoInboundRuntime) schedule(agentName, receiptID string) {
	if r == nil || r.inbound == nil || r.imageTasks == nil {
		return
	}
	key := r.workerKey(agentName, receiptID)
	if key == "\x00" {
		return
	}
	r.workerMu.Lock()
	if _, exists := r.running[key]; exists {
		r.workerMu.Unlock()
		return
	}
	r.running[key] = struct{}{}
	r.workerMu.Unlock()
	go func() {
		defer func() {
			r.workerMu.Lock()
			delete(r.running, key)
			r.workerMu.Unlock()
		}()
		r.run(agentName, receiptID)
	}()
}

func (r *k12DingtalkPhotoInboundRuntime) run(agentName, receiptID string) {
	for {
		if err := r.baseCtx.Err(); err != nil {
			return
		}
		attemptStartedAt := time.Now()
		bundle, err := r.inbound.Resume(r.baseCtx, agentName, receiptID)
		done := false
		if err == nil {
			done, err = r.advance(r.baseCtx, bundle)
		}
		if done {
			return
		}
		delay := r.pollInterval
		if err != nil {
			if !errors.Is(err, records.ErrVersionConflict) {
				slog.Warn("K12 DingTalk inbound photo worker will retry",
					"agent", agentName,
					"receipt_id", receiptID,
					"dispatch_id", bundle.Dispatch.DispatchID,
					"image_task_id", bundle.Dispatch.ImageTaskID,
					"delivery_batch_id", bundle.Dispatch.DeliveryBatchID,
					"agent_ref", k12DingtalkPhotoRestartCheckpointValueDigest(agentName),
					"receipt_ref", k12DingtalkPhotoRestartCheckpointValueDigest(receiptID),
					"dispatch_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Dispatch.DispatchID),
					"image_task_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Dispatch.ImageTaskID),
					"delivery_batch_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Dispatch.DeliveryBatchID),
					"processing_status", bundle.Dispatch.ProcessingStatus,
					"routing_decision", bundle.Dispatch.RoutingDecision,
					"reply_status", bundle.Dispatch.ReplyStatus,
					"terminal_status", bundle.Dispatch.TerminalStatus,
					"elapsed_ms", time.Since(attemptStartedAt).Milliseconds(),
					"error_type", fmt.Sprintf("%T", err),
					"error", err,
				)
			}
			delay = r.retryInterval
		}
		timer := time.NewTimer(delay)
		select {
		case <-r.baseCtx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Recover 只在平台实例已启动后调用；扫描结果按收据去重进入同一个进程 worker。
func (r *k12DingtalkPhotoInboundRuntime) Recover(ctx context.Context) (int, error) {
	if r == nil || r.inbound == nil {
		return 0, fmt.Errorf("DingTalk inbound photo runtime is unavailable")
	}
	bundles, err := r.inbound.Recoverable(ctx, k12DingtalkInboundRecoveryLimit)
	if err != nil {
		return 0, err
	}
	for _, bundle := range bundles {
		r.schedule(bundle.Receipt.AgentName, bundle.Receipt.ReceiptID)
	}
	return len(bundles), nil
}

func (r *k12DingtalkPhotoInboundRuntime) advance(
	ctx context.Context,
	bundle k12usecase.InboundPhotoBundle,
) (bool, error) {
	if bundle.Dispatch.TerminalStatus == k12usecase.InboundPhotoTerminalFailed {
		return true, nil
	}
	if bundle.Dispatch.ReplyStatus == k12usecase.InboundPhotoReplyDelivered {
		return true, nil
	}
	if bundle.Dispatch.ProcessingStatus == k12usecase.InboundPhotoFinalArtifactReady {
		if bundle.Dispatch.ReplyStatus == k12usecase.InboundPhotoReplyReady &&
			strings.TrimSpace(bundle.Dispatch.DeliveryBatchID) == "" {
			if err := r.reachRestartCheckpoint(
				ctx, k12DingtalkPhotoRestartCheckpointBeforeDeliverySend, bundle, "",
			); err != nil {
				return false, err
			}
		}
		return r.advanceFinalReply(ctx, bundle)
	}
	switch bundle.Dispatch.ProcessingStatus {
	case k12usecase.InboundPhotoAdmitted:
		if err := r.reachRestartCheckpoint(
			ctx, k12DingtalkPhotoRestartCheckpointAdmissionCommitted, bundle, "",
		); err != nil {
			return false, err
		}
		return false, r.createAndBindImageTask(ctx, bundle)
	case k12usecase.InboundPhotoImageTaskSubmitted:
		return r.advanceImageTask(ctx, bundle)
	default:
		return false, fmt.Errorf("DingTalk inbound photo processing status is invalid")
	}
}

func (r *k12DingtalkPhotoInboundRuntime) createAndBindImageTask(
	ctx context.Context,
	bundle k12usecase.InboundPhotoBundle,
) error {
	var command k12DingtalkInboundPhotoCommand
	if err := json.Unmarshal([]byte(bundle.Receipt.CommandJSON), &command); err != nil {
		return fmt.Errorf("decode DingTalk inbound photo command: %w", err)
	}
	provider, model, exactRoute := normalizeK12DingtalkInboundPhotoRoute(
		command.Provider, command.Model,
	)
	if command.SchemaVersion != 2 || !exactRoute {
		return fmt.Errorf("DingTalk inbound photo frozen route is incomplete")
	}
	command.Provider = provider
	command.Model = model
	ready, err := r.imageTasks.PersistPageAsset(
		ctx, bundle.Receipt.OwnerScope, bundle.Receipt.AgentName, bundle.Asset.Bytes,
	)
	if err != nil {
		return err
	}
	view, _, err := r.imageTasks.Create(ctx, k12usecase.CreateImageTaskInput{
		OwnerScope: bundle.Receipt.OwnerScope, AgentName: bundle.Receipt.AgentName,
		LearnerID: bundle.Receipt.AgentName, SourceKind: k12.ImageTaskSourceIM,
		SourceRef:       "dingtalk-inbound:" + bundle.Receipt.ReceiptID,
		SourceSessionID: command.SourceSessionID,
		SourceAssetRefs: []string{ready.Metadata.PageAssetID},
		MessageIntent:   command.MessageIntent, AttemptGeneration: 1,
		RouteRequest: k12.ImageTaskRouteSnapshot{
			Provider: command.Provider, Model: command.Model, SelectionSource: "explicit",
		},
	})
	if err != nil {
		return err
	}
	if _, err := r.inbound.RecordImageTask(
		ctx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID,
		bundle.Dispatch.Version, view.Dispatch.DispatchID,
	); err != nil {
		return err
	}
	slog.Info("K12 DingTalk inbound photo image task submitted",
		"stage", "image_task_submitted",
		"agent_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Receipt.AgentName),
		"receipt_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Receipt.ReceiptID),
		"dispatch_ref", k12DingtalkPhotoRestartCheckpointValueDigest(view.Dispatch.DispatchID),
		"page_asset_ref", k12DingtalkPhotoRestartCheckpointValueDigest(ready.Metadata.PageAssetID),
		"provider", command.Provider,
		"model", command.Model,
	)
	// V88 已冻结 task identity 后才允许模型 worker 越过执行边界。
	r.imageTasks.StartAsync(bundle.Receipt.AgentName, view.Dispatch.DispatchID)
	return nil
}

func (r *k12DingtalkPhotoInboundRuntime) advanceImageTask(
	ctx context.Context,
	bundle k12usecase.InboundPhotoBundle,
) (bool, error) {
	view, err := r.imageTasks.Get(
		ctx, bundle.Receipt.AgentName, bundle.Dispatch.ImageTaskID,
	)
	if err != nil {
		return false, err
	}
	homeworkCompleted := view.HomeworkProjection != nil &&
		view.HomeworkProjection.Stage == k12.GradingStageCompleted
	if view.Dispatch.Status == k12.ImageTaskStatusFailed &&
		!homeworkCompleted &&
		!view.Dispatch.RetrySafe &&
		view.ClassificationInvocationStatus != k12.ImageTaskInvocationOutcomeUnknown {
		failureKind := strings.TrimSpace(view.Dispatch.FailureKind)
		if failureKind == "" {
			failureKind = "image_task_failed"
		}
		_, err := r.inbound.FailTerminal(
			ctx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID, bundle.Dispatch.Version,
			k12usecase.InboundPhotoTerminalStageImageTask, failureKind,
		)
		return err == nil, err
	}
	if view.Dispatch.Status == k12.ImageTaskStatusFailed &&
		!homeworkCompleted &&
		view.Dispatch.RetrySafe &&
		bundle.Dispatch.RoutingDecision == k12usecase.InboundPhotoRouteNewSubmission {
		retryStartedAt := time.Now()
		slog.Info("K12 DingTalk inbound photo image task retry started",
			"agent", bundle.Receipt.AgentName,
			"receipt_id", bundle.Receipt.ReceiptID,
			"dispatch_id", bundle.Dispatch.DispatchID,
			"image_task_id", bundle.Dispatch.ImageTaskID,
			"image_task_version", view.Dispatch.Version,
			"failure_kind", view.Dispatch.FailureKind,
		)
		retried, retryErr := r.imageTasks.Retry(
			ctx,
			bundle.Receipt.AgentName,
			bundle.Dispatch.ImageTaskID,
			view.Dispatch.Version,
		)
		if retryErr != nil {
			slog.Warn("K12 DingTalk inbound photo image task retry failed",
				"agent", bundle.Receipt.AgentName,
				"receipt_id", bundle.Receipt.ReceiptID,
				"dispatch_id", bundle.Dispatch.DispatchID,
				"image_task_id", bundle.Dispatch.ImageTaskID,
				"image_task_version", view.Dispatch.Version,
				"elapsed_ms", time.Since(retryStartedAt).Milliseconds(),
				"error_type", fmt.Sprintf("%T", retryErr),
				"error", retryErr,
			)
			return false, retryErr
		}
		gradingJobID := ""
		if retried.Homework != nil {
			gradingJobID = retried.Homework.GradingJobID
		}
		slog.Info("K12 DingTalk inbound photo image task retry completed",
			"agent", bundle.Receipt.AgentName,
			"receipt_id", bundle.Receipt.ReceiptID,
			"dispatch_id", bundle.Dispatch.DispatchID,
			"image_task_id", bundle.Dispatch.ImageTaskID,
			"image_task_status", retried.Dispatch.Status,
			"grading_job_id", gradingJobID,
			"elapsed_ms", time.Since(retryStartedAt).Milliseconds(),
		)
		return false, nil
	}
	if view.HomeworkProjection != nil &&
		view.HomeworkProjection.Stage == k12.GradingStageFailedTerminal {
		_, err := r.inbound.FailTerminal(
			ctx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID, bundle.Dispatch.Version,
			k12usecase.InboundPhotoTerminalStageGrading, "grading_failed_terminal",
		)
		return err == nil, err
	}
	practiceRoutingConfigured := r.practiceSets != nil || r.practiceReturns != nil
	if practiceRoutingConfigured && (r.practiceSets == nil || r.practiceReturns == nil) {
		return false, fmt.Errorf("DingTalk practice-return routing dependencies are incomplete")
	}
	if bundle.Dispatch.RoutingDecision == k12usecase.InboundPhotoRouteRegrade {
		if !practiceRoutingConfigured {
			return false, fmt.Errorf("DingTalk practice-return routing is unavailable")
		}
		return r.advancePracticeReturn(ctx, bundle, view, "")
	}
	if practiceRoutingConfigured &&
		bundle.Dispatch.RoutingDecision == k12usecase.InboundPhotoRouteAskedUser {
		if err := r.sendRoutingConfirmation(ctx, bundle); err != nil {
			return false, err
		}
		return true, nil
	}
	if practiceRoutingConfigured &&
		bundle.Dispatch.RoutingDecision == k12usecase.InboundPhotoRoutePending &&
		view.Dispatch.TaskIntent == k12.ImageTaskIntentCompletedHomework {
		route, err := r.resolvePracticeRoute(ctx, bundle, view)
		if err != nil {
			return false, err
		}
		switch route.Decision {
		case k12usecase.InboundPhotoRouteRegrade:
			return r.advancePracticeReturn(ctx, bundle, view, route.PracticeSetID)
		case k12usecase.InboundPhotoRouteAskedUser:
			dispatch, err := r.requestRoutingConfirmation(ctx, bundle, route.Candidates)
			if err != nil {
				return false, err
			}
			bundle.Dispatch = dispatch
			if err := r.sendRoutingConfirmation(ctx, bundle); err != nil {
				return false, err
			}
			return true, nil
		case k12usecase.InboundPhotoRouteNewSubmission:
			dispatch, err := r.inbound.RecordRoutingDecision(
				ctx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID,
				bundle.Dispatch.Version, k12usecase.InboundPhotoRouteNewSubmission,
			)
			if err != nil {
				return false, err
			}
			bundle.Dispatch = dispatch
		default:
			return false, fmt.Errorf("DingTalk practice-return routing is unresolved")
		}
	}
	switch view.Dispatch.Status {
	case k12.ImageTaskStatusFailed:
		// 子批改任务已完成时直接读取其最终产物，旧的父任务失败态不再触发重试。
		if !homeworkCompleted {
			return false, fmt.Errorf("DingTalk inbound photo image task failed")
		}
	case k12.ImageTaskStatusCancelled:
		return true, nil
	case k12.ImageTaskStatusAwaitingConfirmation:
		switch bundle.Dispatch.RoutingDecision {
		case k12usecase.InboundPhotoRoutePending:
			dispatch, err := r.requestRoutingConfirmation(ctx, bundle, nil)
			if err != nil {
				return false, err
			}
			bundle.Dispatch = dispatch
			if err := r.sendRoutingConfirmation(ctx, bundle); err != nil {
				return false, err
			}
			return true, nil
		case k12usecase.InboundPhotoRouteAskedUser:
			if err := r.sendRoutingConfirmation(ctx, bundle); err != nil {
				return false, err
			}
			return true, nil
		case k12usecase.InboundPhotoRouteNewSubmission:
			if _, err := r.imageTasks.Confirm(ctx, k12usecase.ConfirmImageTaskInput{
				AgentName:       bundle.Receipt.AgentName,
				DispatchID:      bundle.Dispatch.ImageTaskID,
				ExpectedVersion: view.Dispatch.Version,
				Intent:          k12.ImageTaskIntentCompletedHomework,
			}); err != nil {
				return false, err
			}
			r.imageTasks.StartAsync(bundle.Receipt.AgentName, bundle.Dispatch.ImageTaskID)
			return false, nil
		case k12usecase.InboundPhotoRouteRegrade:
			return false, fmt.Errorf("DingTalk practice-return routing is unavailable")
		}
	}
	if view.HomeworkProjection != nil &&
		view.HomeworkProjection.Stage == k12.GradingStageAwaitingConfirmation {
		if _, err := r.imageTasks.Confirm(ctx, k12usecase.ConfirmImageTaskInput{
			AgentName:       bundle.Receipt.AgentName,
			DispatchID:      bundle.Dispatch.ImageTaskID,
			ExpectedVersion: view.Dispatch.Version,
			Subject:         view.HomeworkProjection.Subject,
		}); err != nil {
			return false, err
		}
		r.imageTasks.StartAsync(bundle.Receipt.AgentName, bundle.Dispatch.ImageTaskID)
		return false, nil
	}
	result, err := r.imageTasks.Result(
		ctx, bundle.Receipt.AgentName, bundle.Dispatch.ImageTaskID,
	)
	if err != nil {
		return false, err
	}
	if result.FinalArtifact == nil {
		if view.Homework == nil || strings.TrimSpace(view.Homework.GradingJobID) == "" {
			r.imageTasks.StartAsync(bundle.Receipt.AgentName, bundle.Dispatch.ImageTaskID)
		}
		return false, nil
	}
	if bundle.Dispatch.RoutingDecision == k12usecase.InboundPhotoRoutePending {
		_, err = r.inbound.RecordRoutingDecision(
			ctx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID,
			bundle.Dispatch.Version, k12usecase.InboundPhotoRouteNewSubmission,
		)
		return false, err
	}
	if bundle.Dispatch.RoutingDecision != k12usecase.InboundPhotoRouteRegrade &&
		bundle.Dispatch.RoutingDecision != k12usecase.InboundPhotoRouteNewSubmission {
		return false, fmt.Errorf("DingTalk inbound photo routing is unresolved")
	}
	validated := bundle
	validated.Dispatch.FinalArtifactID = result.FinalArtifact.ArtifactID
	if _, _, err := r.openValidatedFinalArtifact(ctx, validated); err != nil {
		return false, err
	}
	if err := r.reachRestartCheckpoint(
		ctx, k12DingtalkPhotoRestartCheckpointGradingModelCompleted,
		bundle, result.FinalArtifact.ArtifactID,
	); err != nil {
		return false, err
	}
	_, err = r.inbound.RecordFinalArtifact(
		ctx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID,
		bundle.Dispatch.Version, result.FinalArtifact.ArtifactID,
	)
	if err != nil {
		return false, err
	}
	slog.Info("K12 DingTalk inbound photo final artifact ready",
		"stage", "final_artifact_ready",
		"agent_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Receipt.AgentName),
		"receipt_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Receipt.ReceiptID),
		"image_task_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Dispatch.ImageTaskID),
		"final_artifact_ref", k12DingtalkPhotoRestartCheckpointValueDigest(result.FinalArtifact.ArtifactID),
	)
	return false, nil
}

func (r *k12DingtalkPhotoInboundRuntime) resolvePracticeRoute(
	ctx context.Context,
	bundle k12usecase.InboundPhotoBundle,
	view k12usecase.ImageTaskView,
) (k12InboundPhotoPracticeRoute, error) {
	sets, err := r.practiceSets.ListPracticeSets(ctx, bundle.Receipt.AgentName)
	if err != nil {
		return k12InboundPhotoPracticeRoute{}, err
	}
	evidence := make([]string, 0, len(view.Dispatch.IntentEvidence)+1)
	evidence = append(evidence, view.Dispatch.MessageIntent)
	evidence = append(evidence, view.Dispatch.IntentEvidence...)
	return resolveK12InboundPhotoPracticeRoute(k12InboundPhotoPracticeRouteInput{
		Now: r.now(),
		ExplicitDecision: k12InboundPhotoExplicitRoutingDecision(
			view.Dispatch.MessageIntent,
		),
		RecognizedText: evidence,
	}, sets), nil
}

func (r *k12DingtalkPhotoInboundRuntime) requestRoutingConfirmation(
	ctx context.Context,
	bundle k12usecase.InboundPhotoBundle,
	candidates []k12usecase.InboundPhotoRoutingCandidate,
) (k12usecase.InboundPhotoDispatch, error) {
	if coordinator, ok := r.inbound.(k12InboundPhotoRoutingSnapshotCoordinator); ok {
		return coordinator.RequestRoutingConfirmationWithSnapshot(
			ctx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID, bundle.Dispatch.Version,
			k12usecase.InboundPhotoRoutingSnapshot{
				ReceiptID:  bundle.Receipt.ReceiptID,
				Stage:      k12usecase.InboundPhotoRoutingStageIntent,
				Candidates: append([]k12usecase.InboundPhotoRoutingCandidate(nil), candidates...),
			},
		)
	}
	return r.inbound.RequestRoutingConfirmation(
		ctx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID, bundle.Dispatch.Version,
	)
}

func (r *k12DingtalkPhotoInboundRuntime) advancePracticeReturn(
	ctx context.Context,
	bundle k12usecase.InboundPhotoBundle,
	view k12usecase.ImageTaskView,
	practiceSetID string,
) (bool, error) {
	state, err := r.practiceReturns.ResumePracticeReturn(
		ctx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID,
	)
	if errors.Is(err, records.ErrNotFound) {
		if strings.TrimSpace(practiceSetID) == "" {
			// 多候选确认后的复批必须消费持久化的选中卷号；只有旧的
			// 单候选协议没有快照时，才允许回退到实时练习集解析。
			if snapshotCoordinator, ok := r.inbound.(k12InboundPhotoRoutingSnapshotCoordinator); ok {
				snapshot, snapshotErr := snapshotCoordinator.GetRoutingSnapshot(
					ctx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID,
				)
				if snapshotErr == nil && snapshot.Stage == k12usecase.InboundPhotoRoutingStageCandidate {
					practiceSetID = strings.TrimSpace(snapshot.SelectedPracticeSetID)
					if practiceSetID == "" {
						return false, fmt.Errorf("DingTalk practice-return selected candidate is incomplete")
					}
				} else if snapshotErr != nil &&
					!errors.Is(snapshotErr, records.ErrNotFound) &&
					!errors.Is(snapshotErr, errK12InboundPhotoRoutingSnapshotUnavailable) {
					return false, snapshotErr
				}
			}
		}
		if strings.TrimSpace(practiceSetID) == "" {
			route, routeErr := r.resolvePracticeRoute(ctx, bundle, view)
			if routeErr != nil {
				return false, routeErr
			}
			if route.Decision != k12usecase.InboundPhotoRouteRegrade ||
				strings.TrimSpace(route.PracticeSetID) == "" {
				return false, fmt.Errorf("DingTalk practice-return binding is unresolved")
			}
			practiceSetID = route.PracticeSetID
		}
		questions := []k12usecase.RecognizedQuestion(nil)
		if view.HomeworkProjection != nil {
			questions = append(questions, view.HomeworkProjection.Questions...)
		}
		if len(view.Dispatch.SourceAssetRefs) != 1 ||
			strings.TrimSpace(view.Dispatch.SourceAssetRefs[0]) == "" {
			return false, fmt.Errorf("DingTalk practice-return source asset is invalid")
		}
		state, err = r.practiceReturns.AdvancePracticeReturn(
			ctx,
			k12InboundPhotoPracticeReturnInput{
				AgentName: bundle.Receipt.AgentName, ReceiptID: bundle.Receipt.ReceiptID,
				PracticeSetID: practiceSetID, AssetID: view.Dispatch.SourceAssetRefs[0],
				Questions: questions,
			},
		)
	}
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(state.PracticeSetID) == "" || strings.TrimSpace(state.ReturnID) == "" {
		return false, fmt.Errorf("DingTalk practice-return binding is incomplete")
	}
	if bundle.Dispatch.RoutingDecision == k12usecase.InboundPhotoRoutePending {
		dispatch, err := r.inbound.RecordRoutingDecision(
			ctx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID,
			bundle.Dispatch.Version, k12usecase.InboundPhotoRouteRegrade,
		)
		if err != nil {
			return false, err
		}
		bundle.Dispatch = dispatch
	}
	if strings.TrimSpace(state.FinalArtifactID) == "" {
		return false, nil
	}
	validated := bundle
	validated.Dispatch.FinalArtifactID = state.FinalArtifactID
	if _, _, err := r.openValidatedFinalArtifact(ctx, validated); err != nil {
		return false, err
	}
	if err := r.reachRestartCheckpoint(
		ctx, k12DingtalkPhotoRestartCheckpointGradingModelCompleted,
		bundle, state.FinalArtifactID,
	); err != nil {
		return false, err
	}
	_, err = r.inbound.RecordFinalArtifact(
		ctx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID,
		bundle.Dispatch.Version, state.FinalArtifactID,
	)
	return false, err
}

func inboundPhotoFrozenTarget(bundle k12usecase.InboundPhotoBundle) k12usecase.ResolvedDeliveryTarget {
	return k12usecase.ResolvedDeliveryTarget{
		BindingID: bundle.Receipt.BindingID,
		Target: k12.DeliveryTarget{
			Platform:   bundle.Receipt.Identity.Platform,
			InstanceID: bundle.Receipt.Identity.InstanceID,
			ChatID:     bundle.Receipt.Identity.ChatID,
		},
	}
}

func (r *k12DingtalkPhotoInboundRuntime) resolveInboundPhotoFrozenTarget(
	bundle k12usecase.InboundPhotoBundle,
) (k12usecase.ResolvedDeliveryTarget, error) {
	target := inboundPhotoFrozenTarget(bundle)
	if r.resolveInstanceID == nil {
		return target, nil
	}
	instanceID, err := r.resolveInstanceID(target.Target.Platform, target.Target.InstanceID)
	if err != nil {
		return k12usecase.ResolvedDeliveryTarget{}, fmt.Errorf("resolve inbound photo instance: %w", err)
	}
	target.Target.InstanceID = strings.TrimSpace(instanceID)
	if target.Target.InstanceID == "" {
		return k12usecase.ResolvedDeliveryTarget{}, fmt.Errorf("resolve inbound photo instance: stable instance ID is required")
	}
	return target, nil
}

func (r *k12DingtalkPhotoInboundRuntime) sendRoutingConfirmation(
	ctx context.Context,
	bundle k12usecase.InboundPhotoBundle,
) error {
	if r.replyBatches == nil {
		return fmt.Errorf("DingTalk photo routing confirmation delivery is unavailable")
	}
	content := k12PhotoRoutingConfirmationText
	if coordinator, ok := r.inbound.(k12InboundPhotoRoutingSnapshotCoordinator); ok {
		snapshot, snapshotErr := coordinator.GetRoutingSnapshot(
			ctx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID,
		)
		if snapshotErr != nil &&
			!errors.Is(snapshotErr, records.ErrNotFound) &&
			!errors.Is(snapshotErr, errK12InboundPhotoRoutingSnapshotUnavailable) {
			return snapshotErr
		}
		if snapshotErr == nil && snapshot.Stage == k12usecase.InboundPhotoRoutingStageCandidate {
			content = k12PhotoRoutingCandidateText(snapshot)
		}
	}
	target, err := r.resolveInboundPhotoFrozenTarget(bundle)
	if err != nil {
		return err
	}
	startedAt := time.Now()
	batch, _, err := r.replyBatches.PrepareAndSendMessageBatchForTargets(
		ctx, bundle.Receipt.AgentName, k12DingtalkPhotoRoutingObjectKind,
		bundle.Receipt.ReceiptID,
		k12usecase.DeliveryMessage{Content: content},
		[]k12usecase.ResolvedDeliveryTarget{target},
	)
	if err != nil {
		return err
	}
	slog.Info("K12 DingTalk inbound photo routing confirmation send result",
		"stage", "routing_confirmation_send_result",
		"agent_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Receipt.AgentName),
		"receipt_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Receipt.ReceiptID),
		"delivery_batch_ref", k12DingtalkPhotoRestartCheckpointValueDigest(batch.BatchID),
		"delivery_status", batch.Status,
		"elapsed_ms", time.Since(startedAt).Milliseconds(),
	)
	return nil
}

func validateInboundFinalArtifact(
	bundle k12usecase.InboundPhotoBundle,
	artifact k12.GradingFinalArtifact,
	asset k12.GradingFinalAnnotatedAsset,
) error {
	if artifact.AgentName != bundle.Receipt.AgentName ||
		artifact.ArtifactID != bundle.Dispatch.FinalArtifactID ||
		artifact.ArtifactDigest != k12.ComputeGradingFinalArtifactDigest(artifact) {
		return fmt.Errorf("DingTalk photo final artifact identity drifted")
	}
	if err := artifact.Validate(); err != nil {
		return fmt.Errorf("DingTalk photo final artifact is invalid: %w", err)
	}
	if !artifact.HasAnnotatedAsset() {
		return fmt.Errorf("DingTalk photo final artifact has no annotated asset")
	}
	if asset.OwnerScope != artifact.AnnotatedAssetOwnerScope ||
		asset.AssetID != artifact.AnnotatedAssetID || asset.MIME != artifact.AnnotatedMIME ||
		asset.Digest != artifact.AnnotatedDigest ||
		asset.OriginalSourceDigest != artifact.OriginalSourceDigest || len(asset.Data) == 0 {
		return fmt.Errorf("DingTalk photo annotated asset identity drifted")
	}
	annotatedSum := sha256.Sum256(asset.Data)
	if hex.EncodeToString(annotatedSum[:]) != asset.Digest {
		return fmt.Errorf("DingTalk photo annotated asset bytes drifted")
	}
	originalDigest := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(bundle.Asset.Digest)), "sha256:")
	if originalDigest == "" || originalDigest != asset.OriginalSourceDigest {
		return fmt.Errorf("DingTalk photo annotated asset source drifted")
	}
	return nil
}

func (r *k12DingtalkPhotoInboundRuntime) completeBoundReply(
	ctx context.Context,
	bundle k12usecase.InboundPhotoBundle,
) (bool, error) {
	target, err := r.resolveInboundPhotoFrozenTarget(bundle)
	if err != nil {
		return false, err
	}
	batch, _, err := r.replies.Deliver(ctx, k12DingtalkPhotoReplyCommand{
		AgentName:       bundle.Receipt.AgentName,
		DeliveryBatchID: bundle.Dispatch.DeliveryBatchID,
	})
	if err != nil {
		return false, err
	}
	if len(batch.Receipts) != 2 || !strings.HasPrefix(batch.Receipts[1].PartMIME, "image/") {
		return false, fmt.Errorf("DingTalk photo bound reply batch is invalid")
	}
	if err := validateK12DingtalkPhotoReplyBatch(batch, target, batch.Receipts[1].PartMIME); err != nil {
		return false, err
	}
	if batch.Status == k12.DeliveryBatchFailed || batch.Status == k12.DeliveryBatchPartialFailed {
		failureKind := "delivery_batch_failed"
		if batch.Status == k12.DeliveryBatchPartialFailed {
			failureKind = "delivery_batch_partial_failed"
		}
		_, err := r.inbound.FailTerminal(
			ctx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID, bundle.Dispatch.Version,
			k12usecase.InboundPhotoTerminalStageDelivery, failureKind,
		)
		return err == nil, err
	}
	if batch.Status != k12.DeliveryBatchDelivered {
		return false, nil
	}
	completed, err := r.inbound.CompleteReply(
		ctx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID, bundle.Dispatch.Version,
	)
	if err != nil {
		return false, err
	}
	bundle.Dispatch = completed
	if err := r.reachRestartCheckpoint(
		ctx, k12DingtalkPhotoRestartCheckpointAfterDeliverySend, bundle, "",
	); err != nil {
		return false, err
	}
	slog.Info("K12 DingTalk inbound photo reply delivered",
		"stage", "reply_delivered",
		"agent_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Receipt.AgentName),
		"receipt_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Receipt.ReceiptID),
		"delivery_batch_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Dispatch.DeliveryBatchID),
		"delivery_status", batch.Status,
		"part_count", len(batch.Receipts),
	)
	return true, nil
}

func (r *k12DingtalkPhotoInboundRuntime) openValidatedFinalArtifact(
	ctx context.Context,
	bundle k12usecase.InboundPhotoBundle,
) (k12.GradingFinalArtifact, k12.GradingFinalAnnotatedAsset, error) {
	if r.artifacts == nil {
		return k12.GradingFinalArtifact{}, k12.GradingFinalAnnotatedAsset{},
			fmt.Errorf("DingTalk photo final artifact reader is unavailable")
	}
	artifact, err := r.artifacts.GetGradingFinalArtifact(
		ctx, bundle.Receipt.AgentName, bundle.Dispatch.FinalArtifactID,
	)
	if err != nil {
		return k12.GradingFinalArtifact{}, k12.GradingFinalAnnotatedAsset{}, err
	}
	asset, err := r.artifacts.OpenGradingFinalAnnotatedAsset(
		ctx, bundle.Receipt.AgentName, bundle.Dispatch.FinalArtifactID,
	)
	if err != nil {
		return k12.GradingFinalArtifact{}, k12.GradingFinalAnnotatedAsset{}, err
	}
	if err := validateInboundFinalArtifact(bundle, artifact, asset); err != nil {
		return k12.GradingFinalArtifact{}, k12.GradingFinalAnnotatedAsset{}, err
	}
	return artifact, asset, nil
}

func (r *k12DingtalkPhotoInboundRuntime) advanceFinalReply(
	ctx context.Context,
	bundle k12usecase.InboundPhotoBundle,
) (bool, error) {
	if r.inbound == nil || r.replies == nil || r.replyBatches == nil {
		return false, fmt.Errorf("DingTalk photo final reply runtime is unavailable")
	}
	// V88 已绑定批次后只允许读冻结批次并查询回执，不再打开 V89 字节。
	if strings.TrimSpace(bundle.Dispatch.DeliveryBatchID) != "" {
		return r.completeBoundReply(ctx, bundle)
	}
	artifact, asset, err := r.openValidatedFinalArtifact(ctx, bundle)
	if err != nil {
		return false, err
	}
	target, err := r.resolveInboundPhotoFrozenTarget(bundle)
	if err != nil {
		return false, err
	}
	command := k12DingtalkPhotoReplyCommand{
		AgentName:           bundle.Receipt.AgentName,
		InboundReceiptID:    bundle.Receipt.ReceiptID,
		FinalArtifactID:     artifact.ArtifactID,
		FinalArtifactDigest: artifact.ArtifactDigest,
		Target:              target,
		Message: k12usecase.DeliveryMessage{
			Content: artifact.CanonicalMarkdown,
			Attachments: []k12usecase.DeliveryAttachment{{
				Name: correctedPhotoFilename(asset.MIME), MIME: asset.MIME,
				Data: append([]byte(nil), asset.Data...),
			}},
		},
	}
	objectID := k12DingtalkPhotoReplyObjectID(command)
	identities := []k12usecase.DeliveryAttachmentIdentity{{
		Name:          command.Message.Attachments[0].Name,
		MIME:          asset.MIME,
		ContentDigest: "sha256:" + asset.Digest,
	}}
	existing, lookupErr := r.replyBatches.GetDeliveryBatchForMessageIdentity(
		ctx, command.AgentName, k12DingtalkPhotoReplyObjectKind, objectID,
		artifact.CanonicalMarkdown, identities,
	)
	if lookupErr == nil {
		if err := validateK12DingtalkPhotoReplyBatch(existing, target, asset.MIME); err != nil {
			return false, err
		}
		bound, err := r.inbound.BindReplyBatch(
			ctx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID,
			bundle.Dispatch.Version, existing.BatchID,
		)
		if err != nil {
			return false, err
		}
		bundle.Dispatch = bound
		slog.Info("K12 DingTalk inbound photo delivery batch bound",
			"stage", "delivery_batch_bound",
			"agent_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Receipt.AgentName),
			"receipt_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Receipt.ReceiptID),
			"final_artifact_ref", k12DingtalkPhotoRestartCheckpointValueDigest(artifact.ArtifactID),
			"delivery_batch_ref", k12DingtalkPhotoRestartCheckpointValueDigest(existing.BatchID),
			"delivery_status", existing.Status,
			"reused", true,
		)
		return r.completeBoundReply(ctx, bundle)
	}
	if !errors.Is(lookupErr, records.ErrNotFound) {
		return false, lookupErr
	}
	deliveryStartedAt := time.Now()
	batch, _, deliverErr := r.replies.Deliver(ctx, command)
	if strings.TrimSpace(batch.BatchID) != "" {
		persistCtx := context.WithoutCancel(ctx)
		bound, bindErr := r.inbound.BindReplyBatch(
			persistCtx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID,
			bundle.Dispatch.Version, batch.BatchID,
		)
		if bindErr != nil {
			return false, errors.Join(deliverErr, bindErr)
		}
		bundle.Dispatch = bound
		slog.Info("K12 DingTalk inbound photo delivery batch bound",
			"stage", "delivery_batch_bound",
			"agent_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Receipt.AgentName),
			"receipt_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Receipt.ReceiptID),
			"final_artifact_ref", k12DingtalkPhotoRestartCheckpointValueDigest(artifact.ArtifactID),
			"delivery_batch_ref", k12DingtalkPhotoRestartCheckpointValueDigest(batch.BatchID),
			"delivery_status", batch.Status,
			"reused", false,
			"elapsed_ms", time.Since(deliveryStartedAt).Milliseconds(),
			"send_error", deliverErr != nil,
		)
	}
	if deliverErr != nil {
		return false, deliverErr
	}
	if bundle.Dispatch.DeliveryBatchID == "" {
		return false, fmt.Errorf("DingTalk photo reply returned no durable batch identity")
	}
	if batch.Status != k12.DeliveryBatchDelivered {
		return false, nil
	}
	completed, err := r.inbound.CompleteReply(
		ctx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID, bundle.Dispatch.Version,
	)
	if err != nil {
		return false, err
	}
	bundle.Dispatch = completed
	if err := r.reachRestartCheckpoint(
		ctx, k12DingtalkPhotoRestartCheckpointAfterDeliverySend, bundle, "",
	); err != nil {
		return false, err
	}
	slog.Info("K12 DingTalk inbound photo reply delivered",
		"stage", "reply_delivered",
		"agent_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Receipt.AgentName),
		"receipt_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Receipt.ReceiptID),
		"final_artifact_ref", k12DingtalkPhotoRestartCheckpointValueDigest(artifact.ArtifactID),
		"delivery_batch_ref", k12DingtalkPhotoRestartCheckpointValueDigest(bundle.Dispatch.DeliveryBatchID),
		"delivery_status", batch.Status,
		"part_count", len(batch.Receipts),
	)
	return true, nil
}

func (r *k12DingtalkPhotoInboundRuntime) reachRestartCheckpoint(
	ctx context.Context,
	stage k12DingtalkPhotoRestartCheckpointStage,
	bundle k12usecase.InboundPhotoBundle,
	finalArtifactID string,
) error {
	if r == nil || r.restartCheckpoint == nil {
		return nil
	}
	return r.restartCheckpoint.Reach(ctx, k12DingtalkPhotoRestartCheckpoint{
		Stage: stage, Bundle: bundle, FinalArtifactID: strings.TrimSpace(finalArtifactID),
	})
}

// 下列方法让文字确认入口与旧同步入口消费同一个运行时门面。
func (r *k12DingtalkPhotoInboundRuntime) PersistPageAsset(
	ctx context.Context, ownerScope, agentName string, data []byte,
) (k12usecase.ReadyPageAsset, error) {
	return r.imageTasks.PersistPageAsset(ctx, ownerScope, agentName, data)
}

func (r *k12DingtalkPhotoInboundRuntime) Create(
	ctx context.Context, input k12usecase.CreateImageTaskInput,
) (k12usecase.ImageTaskView, bool, error) {
	return r.imageTasks.Create(ctx, input)
}

func (r *k12DingtalkPhotoInboundRuntime) StartAsync(agentName, dispatchID string) bool {
	return r.imageTasks.StartAsync(agentName, dispatchID)
}

// AllowIMCompletedHomeworkGrading 是 ImageTask 在创建 GradingJob 前的只读门；
// 只有 V88 已冻结为新作业时放行，其他分流由入站 worker 继续处理。
func (r *k12DingtalkPhotoInboundRuntime) AllowIMCompletedHomeworkGrading(
	ctx context.Context,
	dispatch k12.ImageTaskDispatch,
) (bool, error) {
	if r == nil || r.inbound == nil {
		return false, fmt.Errorf("DingTalk inbound photo routing gate is unavailable")
	}
	if dispatch.SourceKind != k12.ImageTaskSourceIM ||
		dispatch.TaskIntent != k12.ImageTaskIntentCompletedHomework {
		return false, fmt.Errorf("DingTalk inbound photo routing gate received an invalid task")
	}
	const sourcePrefix = "dingtalk-inbound:"
	if !strings.HasPrefix(dispatch.SourceRef, sourcePrefix) {
		return false, fmt.Errorf("DingTalk inbound photo routing identity is invalid")
	}
	receiptID := strings.TrimSpace(strings.TrimPrefix(dispatch.SourceRef, sourcePrefix))
	if receiptID == "" {
		return false, fmt.Errorf("DingTalk inbound photo routing identity is invalid")
	}
	bundle, err := r.inbound.Resume(ctx, dispatch.AgentName, receiptID)
	if err != nil {
		return false, err
	}
	if bundle.Receipt.AgentName != dispatch.AgentName ||
		bundle.Receipt.ReceiptID != receiptID ||
		bundle.Dispatch.ImageTaskID != dispatch.DispatchID {
		return false, fmt.Errorf("DingTalk inbound photo routing identity drifted")
	}
	switch bundle.Dispatch.RoutingDecision {
	case k12usecase.InboundPhotoRouteNewSubmission:
		return true, nil
	case k12usecase.InboundPhotoRoutePending,
		k12usecase.InboundPhotoRouteAskedUser,
		k12usecase.InboundPhotoRouteRegrade:
		return false, nil
	default:
		return false, fmt.Errorf("DingTalk inbound photo routing decision is invalid")
	}
}

func (r *k12DingtalkPhotoInboundRuntime) Get(
	ctx context.Context, agentName, dispatchID string,
) (k12usecase.ImageTaskView, error) {
	return r.imageTasks.Get(ctx, agentName, dispatchID)
}

func (r *k12DingtalkPhotoInboundRuntime) Confirm(
	ctx context.Context, input k12usecase.ConfirmImageTaskInput,
) (k12usecase.ImageTaskView, error) {
	return r.imageTasks.Confirm(ctx, input)
}

func (r *k12DingtalkPhotoInboundRuntime) Result(
	ctx context.Context, agentName, dispatchID string,
) (k12usecase.ImageTaskResult, error) {
	return r.imageTasks.Result(ctx, agentName, dispatchID)
}

func (r *k12DingtalkPhotoInboundRuntime) Recoverable(
	ctx context.Context, limit int,
) ([]k12usecase.InboundPhotoBundle, error) {
	return r.inbound.Recoverable(ctx, limit)
}

func (r *k12DingtalkPhotoInboundRuntime) ConfirmRouting(
	ctx context.Context,
	agentName, receiptID string,
	expectedVersion int64,
	decision k12usecase.InboundPhotoRoutingDecision,
) (k12usecase.InboundPhotoDispatch, error) {
	dispatch, err := r.inbound.ConfirmRouting(
		ctx, agentName, receiptID, expectedVersion, decision,
	)
	if err == nil {
		r.schedule(agentName, receiptID)
	}
	return dispatch, err
}

// RequestRoutingConfirmationWithSnapshot 将二阶段候选快照转发到耐久协调器；
// runtime 只负责把确认成功后的 worker 重新纳入同一收据，不复制存储状态机。
func (r *k12DingtalkPhotoInboundRuntime) RequestRoutingConfirmationWithSnapshot(
	ctx context.Context,
	agentName, receiptID string,
	expectedVersion int64,
	snapshot k12usecase.InboundPhotoRoutingSnapshot,
) (k12usecase.InboundPhotoDispatch, error) {
	if r == nil || r.inbound == nil {
		return k12usecase.InboundPhotoDispatch{}, errK12InboundPhotoRoutingSnapshotUnavailable
	}
	coordinator, ok := r.inbound.(interface {
		RequestRoutingConfirmationWithSnapshot(
			context.Context, string, string, int64, k12usecase.InboundPhotoRoutingSnapshot,
		) (k12usecase.InboundPhotoDispatch, error)
	})
	if !ok {
		return k12usecase.InboundPhotoDispatch{}, errK12InboundPhotoRoutingSnapshotUnavailable
	}
	return coordinator.RequestRoutingConfirmationWithSnapshot(
		ctx, agentName, receiptID, expectedVersion, snapshot,
	)
}

// GetRoutingSnapshot 只读取冻结候选，不重新读取可变练习集列表。
func (r *k12DingtalkPhotoInboundRuntime) GetRoutingSnapshot(
	ctx context.Context, agentName, receiptID string,
) (k12usecase.InboundPhotoRoutingSnapshot, error) {
	if r == nil || r.inbound == nil {
		return k12usecase.InboundPhotoRoutingSnapshot{}, errK12InboundPhotoRoutingSnapshotUnavailable
	}
	coordinator, ok := r.inbound.(interface {
		GetRoutingSnapshot(context.Context, string, string) (k12usecase.InboundPhotoRoutingSnapshot, error)
	})
	if !ok {
		return k12usecase.InboundPhotoRoutingSnapshot{}, errK12InboundPhotoRoutingSnapshotUnavailable
	}
	return coordinator.GetRoutingSnapshot(ctx, agentName, receiptID)
}

// ConfirmRoutingSelection 只提交冻结候选中的 practice set，并让恢复 worker 接续复批。
func (r *k12DingtalkPhotoInboundRuntime) ConfirmRoutingSelection(
	ctx context.Context,
	agentName, receiptID string,
	expectedVersion int64,
	decision k12usecase.InboundPhotoRoutingDecision,
	practiceSetID string,
) (k12usecase.InboundPhotoDispatch, error) {
	if r == nil || r.inbound == nil {
		return k12usecase.InboundPhotoDispatch{}, errK12InboundPhotoRoutingSnapshotUnavailable
	}
	coordinator, ok := r.inbound.(interface {
		ConfirmRoutingSelection(
			context.Context, string, string, int64, k12usecase.InboundPhotoRoutingDecision, string,
		) (k12usecase.InboundPhotoDispatch, error)
	})
	if !ok {
		return k12usecase.InboundPhotoDispatch{}, errK12InboundPhotoRoutingSnapshotUnavailable
	}
	dispatch, err := coordinator.ConfirmRoutingSelection(
		ctx, agentName, receiptID, expectedVersion, decision, practiceSetID,
	)
	if err == nil {
		r.schedule(agentName, receiptID)
	}
	return dispatch, err
}

type k12InboundPhotoQueryPort interface {
	ResumeByIdentity(context.Context, k12usecase.InboundPhotoIdentity) (k12usecase.InboundPhotoBundle, error)
}

type k12InboundPhotoIdentityProjection struct {
	Platform          string `json:"platform"`
	InstanceID        string `json:"instance_id"`
	ChatID            string `json:"chat_id"`
	ProviderMessageID string `json:"provider_message_id"`
}

type k12InboundPhotoReceiptProjection struct {
	Identity      k12InboundPhotoIdentityProjection `json:"identity"`
	AgentName     string                            `json:"agent_name"`
	ReceiptID     string                            `json:"receipt_id"`
	BindingID     string                            `json:"binding_id"`
	CommandDigest string                            `json:"command_digest"`
}

type k12InboundPhotoAssetProjection struct {
	AssetID   string `json:"asset_id"`
	ReceiptID string `json:"receipt_id"`
	MIME      string `json:"mime"`
	Size      int    `json:"size"`
	Digest    string `json:"digest"`
}

type k12InboundPhotoDispatchProjection struct {
	ReceiptID          string `json:"receipt_id"`
	DispatchID         string `json:"dispatch_id"`
	ProcessingStatus   string `json:"processing_status"`
	RoutingDecision    string `json:"routing_decision"`
	ConfirmationStatus string `json:"confirmation_status"`
	ReplyStatus        string `json:"reply_status"`
	Version            int64  `json:"version"`
	ImageTaskID        string `json:"image_task_id,omitempty"`
	FinalArtifactID    string `json:"final_artifact_id,omitempty"`
	DeliveryBatchID    string `json:"delivery_batch_id,omitempty"`
}

type k12InboundPhotoBundleProjection struct {
	Receipt  k12InboundPhotoReceiptProjection  `json:"receipt"`
	Asset    k12InboundPhotoAssetProjection    `json:"asset"`
	Dispatch k12InboundPhotoDispatchProjection `json:"dispatch"`
}

func projectK12InboundPhotoBundle(bundle k12usecase.InboundPhotoBundle) k12InboundPhotoBundleProjection {
	identity := bundle.Receipt.Identity
	return k12InboundPhotoBundleProjection{
		Receipt: k12InboundPhotoReceiptProjection{
			Identity: k12InboundPhotoIdentityProjection{
				Platform: identity.Platform, InstanceID: identity.InstanceID,
				ChatID: identity.ChatID, ProviderMessageID: identity.ProviderMessageID,
			},
			AgentName: bundle.Receipt.AgentName, ReceiptID: bundle.Receipt.ReceiptID,
			BindingID: bundle.Receipt.BindingID, CommandDigest: bundle.Receipt.CommandDigest,
		},
		Asset: k12InboundPhotoAssetProjection{
			AssetID: bundle.Asset.AssetID, ReceiptID: bundle.Asset.ReceiptID,
			MIME: bundle.Asset.MIME, Size: bundle.Asset.Size, Digest: bundle.Asset.Digest,
		},
		Dispatch: k12InboundPhotoDispatchProjection{
			ReceiptID: bundle.Dispatch.ReceiptID, DispatchID: bundle.Dispatch.DispatchID,
			ProcessingStatus:   string(bundle.Dispatch.ProcessingStatus),
			RoutingDecision:    string(bundle.Dispatch.RoutingDecision),
			ConfirmationStatus: string(bundle.Dispatch.ConfirmationStatus),
			ReplyStatus:        string(bundle.Dispatch.ReplyStatus), Version: bundle.Dispatch.Version,
			ImageTaskID:     bundle.Dispatch.ImageTaskID,
			FinalArtifactID: bundle.Dispatch.FinalArtifactID,
			DeliveryBatchID: bundle.Dispatch.DeliveryBatchID,
		},
	}
}

// newK12DingtalkPhotoInboundQueryHandler 暴露可信 principal 下的完整身份只读查询。
// owner、命令与图片字节永不进入公开 DTO。
func newK12DingtalkPhotoInboundQueryHandler(
	next http.Handler,
	query k12InboundPhotoQueryPort,
	ownerScope string,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/dingtalk-inbound" {
			next.ServeHTTP(w, req)
			return
		}
		if req.Method != http.MethodGet || query == nil {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		identity := k12usecase.InboundPhotoIdentity{
			Platform:          req.URL.Query().Get("platform"),
			InstanceID:        req.URL.Query().Get("instance_id"),
			ChatID:            req.URL.Query().Get("chat_id"),
			ProviderMessageID: req.URL.Query().Get("provider_message_id"),
		}
		bundle, err := query.ResumeByIdentity(req.Context(), identity)
		if errors.Is(err, records.ErrNotFound) || err == nil &&
			(bundle.Receipt.OwnerScope != strings.TrimSpace(ownerScope) ||
				bundle.Receipt.AgentName != strings.TrimSpace(req.URL.Query().Get("agent"))) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(projectK12InboundPhotoBundle(bundle)); err != nil {
			return
		}
	})
}
