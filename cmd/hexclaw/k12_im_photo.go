package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/channel"
	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/records"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

const k12TutorScenario = "k12-tutor"

const k12IMImageTaskWaitTimeout = 125 * time.Second

// k12ImageTaskFacade is the only application boundary accepted by image
// ingress adapters. Desktop, API, webhook and IM all create the same durable
// dispatch before any model call; adapters cannot fall back to GradingJob or a
// provider function after that boundary.
type k12ImageTaskFacade interface {
	PersistPageAsset(context.Context, string, string, []byte) (k12usecase.ReadyPageAsset, error)
	Create(context.Context, k12usecase.CreateImageTaskInput) (k12usecase.ImageTaskView, bool, error)
	StartAsync(agentName, dispatchID string) bool
	Get(context.Context, string, string) (k12usecase.ImageTaskView, error)
	Confirm(context.Context, k12usecase.ConfirmImageTaskInput) (k12usecase.ImageTaskView, error)
	Result(context.Context, string, string) (k12usecase.ImageTaskResult, error)
}

// k12InboundPhotoRoutingCoordinator 是 composition root 对 V88 入站收据协调器使用的窄缝。
// 图片任务门面可由装配层与该协调器组合；本入口不复制其 CAS 状态机。
type k12InboundPhotoRoutingCoordinator interface {
	Recoverable(context.Context, int) ([]k12usecase.InboundPhotoBundle, error)
	ConfirmRouting(
		context.Context,
		string,
		string,
		int64,
		k12usecase.InboundPhotoRoutingDecision,
	) (k12usecase.InboundPhotoDispatch, error)
}

// k12InboundPhotoRoutingSnapshotCoordinator 是 V91 多候选确认的可选扩展；旧装配仍可
// 继续使用单阶段 ConfirmRouting，生产装配优先走冻结快照协议。
type k12InboundPhotoRoutingSnapshotCoordinator interface {
	k12InboundPhotoRoutingCoordinator
	RequestRoutingConfirmationWithSnapshot(
		context.Context, string, string, int64, k12usecase.InboundPhotoRoutingSnapshot,
	) (k12usecase.InboundPhotoDispatch, error)
	GetRoutingSnapshot(context.Context, string, string) (k12usecase.InboundPhotoRoutingSnapshot, error)
	ConfirmRoutingSelection(
		context.Context, string, string, int64,
		k12usecase.InboundPhotoRoutingDecision, string,
	) (k12usecase.InboundPhotoDispatch, error)
}

const k12InboundPhotoRecentPracticeWindow = 14 * 24 * 60 * 60

var k12InboundPhotoPaperNoPattern = regexp.MustCompile(`(?i)P-[0-9]{4}-[0-9]{2,}`)

type k12InboundPhotoPracticeRouteInput struct {
	Now              int64
	ExplicitDecision k12usecase.InboundPhotoRoutingDecision
	RecognizedText   []string
}

type k12InboundPhotoPracticeRoute struct {
	Decision      k12usecase.InboundPhotoRoutingDecision
	PracticeSetID string
	Candidates    []k12usecase.InboundPhotoRoutingCandidate
}

type k12InboundPhotoPracticeSetReader interface {
	ListPracticeSets(context.Context, string) ([]k12usecase.PracticeSetView, error)
}

type k12InboundPhotoPracticeReturnInput struct {
	AgentName     string
	ReceiptID     string
	PracticeSetID string
	AssetID       string
	Questions     []k12usecase.RecognizedQuestion
}

type k12InboundPhotoPracticeReturnState struct {
	PracticeSetID   string
	ReturnID        string
	FinalArtifactID string
}

// k12InboundPhotoPracticeReturnPort 让 IM worker 只编排已存在的练习回传状态机。
// Resume 必须先按稳定 receipt 查找既有绑定，避免重启后重新解析到另一张卷。
type k12InboundPhotoPracticeReturnPort interface {
	ResumePracticeReturn(context.Context, string, string) (k12InboundPhotoPracticeReturnState, error)
	AdvancePracticeReturn(
		context.Context,
		k12InboundPhotoPracticeReturnInput,
	) (k12InboundPhotoPracticeReturnState, error)
}

// resolveK12InboundPhotoPracticeRoute 只读取已固化练习卷事实并输出分流；
// PracticeSet 回传追加和复批继续由既有用例状态机负责。
func resolveK12InboundPhotoPracticeRoute(
	input k12InboundPhotoPracticeRouteInput,
	sets []k12usecase.PracticeSetView,
) k12InboundPhotoPracticeRoute {
	if input.ExplicitDecision == k12usecase.InboundPhotoRouteNewSubmission {
		return k12InboundPhotoPracticeRoute{Decision: k12usecase.InboundPhotoRouteNewSubmission}
	}
	recognized := make(map[string]struct{})
	for _, evidence := range input.RecognizedText {
		for _, paperNo := range k12InboundPhotoPaperNoPattern.FindAllString(evidence, -1) {
			recognized[strings.ToUpper(paperNo)] = struct{}{}
		}
	}
	if len(recognized) > 0 {
		if len(recognized) != 1 {
			return k12InboundPhotoPracticeRoute{Decision: k12usecase.InboundPhotoRouteAskedUser}
		}
		var paperNo string
		for candidate := range recognized {
			paperNo = candidate
		}
		matches := make([]k12usecase.PracticeSetView, 0, 1)
		for _, set := range sets {
			if k12InboundPhotoUnreturnedPracticeSet(set) &&
				strings.EqualFold(strings.TrimSpace(set.Fields.PaperNo), paperNo) {
				matches = append(matches, set)
			}
		}
		if len(matches) == 1 {
			return k12InboundPhotoPracticeRoute{
				Decision:      k12usecase.InboundPhotoRouteRegrade,
				PracticeSetID: strings.TrimSpace(matches[0].Record.RecordID),
			}
		}
		return k12InboundPhotoPracticeRoute{
			Decision:   k12usecase.InboundPhotoRouteAskedUser,
			Candidates: k12InboundPhotoPracticeCandidates(matches),
		}
	}

	recent := make([]k12usecase.PracticeSetView, 0, 1)
	for _, set := range sets {
		if !k12InboundPhotoUnreturnedPracticeSet(set) || set.Fields.FinalizedAt <= 0 ||
			input.Now < set.Fields.FinalizedAt ||
			input.Now-set.Fields.FinalizedAt > k12InboundPhotoRecentPracticeWindow {
			continue
		}
		recent = append(recent, set)
	}
	if len(recent) == 1 {
		return k12InboundPhotoPracticeRoute{
			Decision:      k12usecase.InboundPhotoRouteRegrade,
			PracticeSetID: strings.TrimSpace(recent[0].Record.RecordID),
		}
	}
	return k12InboundPhotoPracticeRoute{
		Decision:   k12usecase.InboundPhotoRouteAskedUser,
		Candidates: k12InboundPhotoPracticeCandidates(recent),
	}
}

func k12InboundPhotoPracticeCandidates(
	sets []k12usecase.PracticeSetView,
) []k12usecase.InboundPhotoRoutingCandidate {
	candidates := make([]k12usecase.InboundPhotoRoutingCandidate, 0, len(sets))
	for _, set := range sets {
		if set.Record == nil || strings.TrimSpace(set.Record.RecordID) == "" ||
			strings.TrimSpace(set.Fields.PaperNo) == "" || set.Fields.FinalizedAt <= 0 {
			continue
		}
		title := strings.TrimSpace(set.Fields.Title)
		candidates = append(candidates, k12usecase.InboundPhotoRoutingCandidate{
			PracticeSetID: strings.TrimSpace(set.Record.RecordID),
			PaperNo:       strings.TrimSpace(set.Fields.PaperNo),
			Title:         title,
			SentAt:        set.Fields.FinalizedAt,
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].SentAt != candidates[j].SentAt {
			return candidates[i].SentAt > candidates[j].SentAt
		}
		if candidates[i].PaperNo != candidates[j].PaperNo {
			return candidates[i].PaperNo < candidates[j].PaperNo
		}
		return candidates[i].PracticeSetID < candidates[j].PracticeSetID
	})
	return candidates
}

func k12InboundPhotoUnreturnedPracticeSet(set k12usecase.PracticeSetView) bool {
	if set.Record == nil || strings.TrimSpace(set.Record.RecordID) == "" ||
		strings.TrimSpace(set.Fields.PaperNo) == "" ||
		(set.Record.Status != k12.PracticeStatusAssigned &&
			set.Record.Status != k12.PracticeStatusSubmitted) {
		return false
	}
	if set.Record.Status == k12.PracticeStatusAssigned && len(set.Fields.ReturnAssets) == 0 {
		return true
	}
	for _, item := range set.Fields.Items {
		if !k12.PracticeItemPublishable(item) {
			continue
		}
		if !item.Returned {
			return true
		}
	}
	return false
}

func k12InboundPhotoExplicitRoutingDecision(
	messageIntent string,
) k12usecase.InboundPhotoRoutingDecision {
	switch strings.TrimSpace(messageIntent) {
	case "新作业", "新作业批改":
		return k12usecase.InboundPhotoRouteNewSubmission
	default:
		return k12usecase.InboundPhotoRoutePending
	}
}

// maybeHandleK12DingtalkRuntimeMessage 让 composition root 只把文字确认交给同步 handler；
// 图片 callback 已在 ACK 前进入 V88，禁止再次等待模型或返回附件回复。
func maybeHandleK12DingtalkRuntimeMessage(
	ctx context.Context,
	msg *adapter.Message,
	router *agentrouter.Dispatcher,
	runtime *k12DingtalkPhotoInboundRuntime,
) (*adapter.Reply, bool, error) {
	if runtime == nil || msg == nil || len(msg.Attachments) != 0 {
		return nil, false, nil
	}
	return maybeHandleK12DingtalkPhoto(ctx, msg, router, runtime)
}

// maybeHandleK12DingtalkPhoto is the composition-root seam between generic IM
// delivery and the unified K12 ImageTask facade. It deliberately requires an
// explicit direct-message routing rule: a K12 default agent must not steal an
// unrelated picture. Once matched, failure is surfaced honestly; there is no
// second provider path that could duplicate model work or bind idempotency to a
// different route.
func maybeHandleK12DingtalkPhoto(
	ctx context.Context,
	msg *adapter.Message,
	router *agentrouter.Dispatcher,
	imageTasks k12ImageTaskFacade,
) (*adapter.Reply, bool, error) {
	// 文字确认必须按入站照片收据中冻结的 Agent 恢复，不能因当前绑定已切换
	// 而把原命令交给通用会话。只有 direct 钉钉文字且内容确实像确认动作时
	// 才读取待确认收据，避免拦截普通消息或群聊消息。
	if msg != nil && msg.Platform == adapter.PlatformDingtalk &&
		msg.Metadata["conversation_type"] != "2" {
		if coordinator, ok := imageTasks.(k12InboundPhotoRoutingCoordinator); ok {
			decision, matched := k12PhotoRoutingConfirmationDecision(msg)
			if !matched && k12PhotoRoutingCandidateSelectionMessage(msg) {
				// 多候选阶段使用纯序号；具体候选身份由冻结快照校验，不能在入口猜测。
				matched = true
			}
			if matched {
				// agentName 为空表示按 durable receipt 的冻结 Agent 匹配；
				// current router 只代表当前配置，不能覆盖已接纳任务的路由事实。
				reply, handled, err := confirmK12PendingPhotoRoute(
					ctx, msg, "", decision, coordinator,
				)
				if handled || err != nil {
					return reply, true, err
				}
			}
		}
	}
	routed := routeK12DingtalkPhotoTutor(msg, router)
	if routed == nil {
		return nil, false, nil
	}
	if coordinator, ok := imageTasks.(k12InboundPhotoRoutingCoordinator); ok {
		reply, blocked, err := guardK12PendingPhotoRoute(
			ctx, msg, routed.AgentName, coordinator,
		)
		if blocked || err != nil {
			return reply, true, err
		}
	}
	if imageTasks == nil {
		return nil, true, fmt.Errorf("K12 图片任务服务未配置")
	}
	raw, err := decodeK12PhotoAttachment(msg.Attachments[0])
	if err != nil {
		return nil, true, err
	}
	readyAsset, err := imageTasks.PersistPageAsset(
		ctx,
		k12usecase.DefaultLocalOwnerScope,
		routed.AgentName,
		raw,
	)
	if err != nil {
		return nil, true, fmt.Errorf("K12 钉钉图片入库: %w", err)
	}
	assetRef := readyAsset.Metadata.PageAssetID
	sourceRef := strings.TrimSpace(msg.ID)
	sourceSession := k12PhotoSourceSession(msg)
	if sourceRef == "" {
		sourceRef = sourceSession
	}
	view, created, err := imageTasks.Create(ctx, k12usecase.CreateImageTaskInput{
		OwnerScope: k12usecase.DefaultLocalOwnerScope,
		AgentName:  routed.AgentName, LearnerID: routed.AgentName,
		SourceKind: k12.ImageTaskSourceIM, SourceRef: sourceRef,
		SourceSessionID: sourceSession, SourceAssetRefs: []string{assetRef},
		MessageIntent: strings.TrimSpace(msg.Content), AttemptGeneration: 1,
	})
	if err != nil {
		return nil, true, err
	}
	if started := imageTasks.StartAsync(
		routed.AgentName, view.Dispatch.DispatchID,
	); created && !started {
		return nil, true, fmt.Errorf(
			"K12 图片任务已创建但未能启动（dispatch=%s）",
			view.Dispatch.DispatchID,
		)
	}

	waitCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, k12IMImageTaskWaitTimeout)
		defer cancel()
	}
	reply, err := waitK12IMImageTaskResult(
		waitCtx, imageTasks, routed.AgentName, view.Dispatch.DispatchID,
	)
	return reply, true, err
}

func k12PhotoRoutingConfirmationDecision(
	msg *adapter.Message,
) (k12usecase.InboundPhotoRoutingDecision, bool) {
	if msg == nil || len(msg.Attachments) != 0 {
		return "", false
	}
	if action := strings.TrimSpace(msg.Metadata["interactive_action"]); action != "" {
		switch action {
		case string(k12usecase.InboundPhotoRouteRegrade):
			return k12usecase.InboundPhotoRouteRegrade, true
		case string(k12usecase.InboundPhotoRouteNewSubmission):
			return k12usecase.InboundPhotoRouteNewSubmission, true
		default:
			return "", false
		}
	}
	switch strings.TrimSpace(msg.Content) {
	case "1":
		return k12usecase.InboundPhotoRouteRegrade, true
	case "2":
		return k12usecase.InboundPhotoRouteNewSubmission, true
	default:
		return "", false
	}
}

func k12PhotoRoutingCandidateSelectionMessage(msg *adapter.Message) bool {
	if msg == nil || len(msg.Attachments) != 0 {
		return false
	}
	if strings.TrimSpace(msg.Metadata["routing_candidate_index"]) != "" {
		return true
	}
	return regexp.MustCompile(`^[0-9]+$`).MatchString(strings.TrimSpace(msg.Content))
}

func confirmK12PendingPhotoRoute(
	ctx context.Context,
	msg *adapter.Message,
	agentName string,
	decision k12usecase.InboundPhotoRoutingDecision,
	coordinator k12InboundPhotoRoutingCoordinator,
) (*adapter.Reply, bool, error) {
	bundles, err := coordinator.Recoverable(ctx, 100)
	if err != nil {
		return nil, true, err
	}
	candidates := pendingK12PhotoRouteBundles(bundles, msg, agentName)
	if len(candidates) == 0 {
		// 第二阶段的数字回复不能在确认后重新解释成第一阶段的
		// `2=新作业批改`。已确认收据仍保留候选快照，重投时先按
		// 冻结快照核对同一 ordinal，命中后直接幂等结束。
		if snapshotCoordinator, ok := coordinator.(k12InboundPhotoRoutingSnapshotCoordinator); ok {
			for _, bundle := range bundles {
				if !k12InboundPhotoBundleMatchesMessage(bundle, msg, agentName) ||
					bundle.Dispatch.RoutingDecision != k12usecase.InboundPhotoRouteRegrade ||
					bundle.Dispatch.ConfirmationStatus != k12storage.InboundPhotoConfirmationConfirmed {
					continue
				}
				snapshot, snapshotErr := snapshotCoordinator.GetRoutingSnapshot(
					ctx, bundle.Receipt.AgentName, bundle.Receipt.ReceiptID,
				)
				if snapshotErr != nil || snapshot.Stage != k12usecase.InboundPhotoRoutingStageCandidate ||
					snapshot.SelectedPracticeSetID == "" {
					continue
				}
				index, valid := k12PhotoRoutingCandidateIndex(msg, len(snapshot.Candidates))
				if valid && snapshot.Candidates[index-1].PracticeSetID == snapshot.SelectedPracticeSetID {
					return nil, true, nil
				}
			}
		}
		for _, bundle := range bundles {
			if k12InboundPhotoBundleMatchesMessage(bundle, msg, agentName) &&
				bundle.Dispatch.RoutingDecision == decision &&
				bundle.Dispatch.ConfirmationStatus == k12storage.InboundPhotoConfirmationConfirmed {
				return nil, true, nil
			}
		}
		return nil, false, nil
	}
	if len(candidates) != 1 {
		reply, replyErr := k12PhotoRoutingConfirmationReply()
		return reply, true, replyErr
	}
	pending := candidates[0]
	if snapshotCoordinator, ok := coordinator.(k12InboundPhotoRoutingSnapshotCoordinator); ok {
		snapshot, snapshotErr := snapshotCoordinator.GetRoutingSnapshot(
			ctx, pending.Receipt.AgentName, pending.Receipt.ReceiptID,
		)
		if snapshotErr != nil &&
			!errors.Is(snapshotErr, records.ErrNotFound) &&
			!errors.Is(snapshotErr, errK12InboundPhotoRoutingSnapshotUnavailable) {
			return nil, true, snapshotErr
		}
		if snapshotErr == nil {
			if snapshot.Stage == k12usecase.InboundPhotoRoutingStageCandidate {
				index, valid := k12PhotoRoutingCandidateIndex(msg, len(snapshot.Candidates))
				if !valid {
					reply, replyErr := k12PhotoRoutingCandidateReply(snapshot)
					return reply, true, replyErr
				}
				_, err := snapshotCoordinator.ConfirmRoutingSelection(
					ctx, pending.Receipt.AgentName, pending.Receipt.ReceiptID,
					pending.Dispatch.Version, k12usecase.InboundPhotoRouteRegrade,
					snapshot.Candidates[index-1].PracticeSetID,
				)
				return nil, true, err
			}
			if snapshot.Stage == k12usecase.InboundPhotoRoutingStageIntent &&
				decision == k12usecase.InboundPhotoRouteRegrade && len(snapshot.Candidates) > 1 {
				snapshot.Stage = k12usecase.InboundPhotoRoutingStageCandidate
				dispatch, err := snapshotCoordinator.RequestRoutingConfirmationWithSnapshot(
					ctx, pending.Receipt.AgentName, pending.Receipt.ReceiptID,
					pending.Dispatch.Version, snapshot,
				)
				if err != nil {
					return nil, true, err
				}
				_ = dispatch
				reply, replyErr := k12PhotoRoutingCandidateReply(snapshot)
				return reply, true, replyErr
			}
			if snapshot.Stage == k12usecase.InboundPhotoRoutingStageIntent &&
				decision == k12usecase.InboundPhotoRouteRegrade && len(snapshot.Candidates) == 1 {
				_, err := snapshotCoordinator.ConfirmRoutingSelection(
					ctx, pending.Receipt.AgentName, pending.Receipt.ReceiptID,
					pending.Dispatch.Version, decision, snapshot.Candidates[0].PracticeSetID,
				)
				return nil, true, err
			}
		}
	}
	if decision == "" {
		reply, replyErr := k12PhotoRoutingConfirmationReply()
		return reply, true, replyErr
	}
	_, err = coordinator.ConfirmRouting(
		ctx,
		pending.Receipt.AgentName,
		pending.Receipt.ReceiptID,
		pending.Dispatch.Version,
		decision,
	)
	return nil, true, err
}

func k12PhotoRoutingCandidateIndex(msg *adapter.Message, total int) (int, bool) {
	if msg == nil || total <= 0 {
		return 0, false
	}
	value := strings.TrimSpace(msg.Metadata["routing_candidate_index"])
	if value == "" {
		value = strings.TrimSpace(msg.Content)
	}
	var index int
	if _, err := fmt.Sscanf(value, "%d", &index); err != nil || index < 1 || index > total {
		return 0, false
	}
	if fmt.Sprintf("%d", index) != value {
		return 0, false
	}
	return index, true
}

func guardK12PendingPhotoRoute(
	ctx context.Context,
	msg *adapter.Message,
	agentName string,
	coordinator k12InboundPhotoRoutingCoordinator,
) (*adapter.Reply, bool, error) {
	bundles, err := coordinator.Recoverable(ctx, 100)
	if err != nil {
		return nil, true, err
	}
	if len(pendingK12PhotoRouteBundles(bundles, msg, agentName)) == 0 {
		return nil, false, nil
	}
	reply, replyErr := k12PhotoRoutingConfirmationReply()
	return reply, true, replyErr
}

func pendingK12PhotoRouteBundles(
	bundles []k12usecase.InboundPhotoBundle,
	msg *adapter.Message,
	agentName string,
) []k12usecase.InboundPhotoBundle {
	if msg == nil {
		return nil
	}
	candidates := make([]k12usecase.InboundPhotoBundle, 0, 1)
	for _, bundle := range bundles {
		if !k12InboundPhotoBundleMatchesMessage(bundle, msg, agentName) {
			continue
		}
		if bundle.Dispatch.RoutingDecision == k12usecase.InboundPhotoRouteAskedUser &&
			bundle.Dispatch.ConfirmationStatus == k12storage.InboundPhotoConfirmationWaiting {
			candidates = append(candidates, bundle)
		}
	}
	return candidates
}

func k12InboundPhotoBundleMatchesMessage(
	bundle k12usecase.InboundPhotoBundle,
	msg *adapter.Message,
	agentName string,
) bool {
	if msg == nil {
		return false
	}
	identity := bundle.Receipt.Identity
	return (strings.TrimSpace(agentName) == "" ||
		bundle.Receipt.AgentName == strings.TrimSpace(agentName)) &&
		strings.ToLower(strings.TrimSpace(identity.Platform)) == string(msg.Platform) &&
		strings.TrimSpace(identity.InstanceID) == strings.TrimSpace(msg.InstanceID) &&
		strings.TrimSpace(identity.ChatID) == strings.TrimSpace(msg.ChatID)
}

func waitK12IMImageTaskResult(
	ctx context.Context,
	imageTasks k12ImageTaskFacade,
	agentName, dispatchID string,
) (*adapter.Reply, error) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	homeworkConfirmed := false
	for {
		view, err := imageTasks.Get(ctx, agentName, dispatchID)
		if err != nil {
			return nil, err
		}
		switch view.Dispatch.Status {
		case k12.ImageTaskStatusFailed:
			return nil, fmt.Errorf("K12 图片任务失败: %s", view.Dispatch.FailureKind)
		case k12.ImageTaskStatusCancelled:
			return nil, fmt.Errorf("K12 图片任务已取消")
		case k12.ImageTaskStatusAwaitingConfirmation:
			return k12PhotoRoutingConfirmationReply()
		}
		if !homeworkConfirmed && view.HomeworkProjection != nil &&
			view.HomeworkProjection.Stage == k12.GradingStageAwaitingConfirmation {
			if _, err := imageTasks.Confirm(ctx, k12usecase.ConfirmImageTaskInput{
				AgentName: agentName, DispatchID: dispatchID,
				ExpectedVersion: view.Dispatch.Version, Intent: view.Dispatch.TaskIntent,
				Subject: view.HomeworkProjection.Subject,
			}); err != nil {
				return nil, err
			}
			homeworkConfirmed = true
		}
		result, err := imageTasks.Result(ctx, agentName, dispatchID)
		if err != nil {
			return nil, err
		}
		switch result.Kind {
		case string(k12.ImageTaskIntentCompletedHomework), string(k12.ImageTaskIntentBlankWorksheet):
			if result.Photo == nil {
				return nil, fmt.Errorf("K12 图片任务已完成但结果缺失")
			}
			return k12PhotoReply(*result.Photo), nil
		case "creative":
			return k12CreativeWorkReply(result)
		case "awaiting_confirmation":
			return k12PhotoRoutingConfirmationReply()
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("等待 K12 图片任务结果: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

const k12PhotoRoutingConfirmationText = "回复 1=练习卷回传，2=新作业批改"

// k12PhotoRoutingConfirmationReply 用同一份渠道中立 Markdown 投影原生按钮与文字降级；
// 两种入口只携带等价 route action，不在回调内猜测最终分流。
func k12PhotoRoutingConfirmationReply() (*adapter.Reply, error) {
	message, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12,
		"zh-CN",
		k12PhotoRoutingConfirmationText,
		k12PhotoRoutingConfirmationText,
		"",
		nil,
	)
	if err != nil {
		return nil, err
	}
	reply := adapterReplyFromChannelMessage(message)
	reply.Interactive = &adapter.InteractivePayload{
		Type:   adapter.InteractiveTypeButtons,
		Prompt: k12PhotoRoutingConfirmationText,
		Buttons: []adapter.InteractiveButton{
			{Label: "练习卷回传", Action: string(k12usecase.InboundPhotoRouteRegrade), Variant: adapter.ButtonPrimary},
			{Label: "新作业批改", Action: string(k12usecase.InboundPhotoRouteNewSubmission), Variant: adapter.ButtonSecondary},
		},
	}
	return reply, nil
}

func k12PhotoRoutingCandidateText(snapshot k12usecase.InboundPhotoRoutingSnapshot) string {
	lines := []string{"检测到多份未回传练习卷，请回复序号选择要回传的卷："}
	for index, candidate := range snapshot.Candidates {
		line := fmt.Sprintf("%d. ", index+1)
		if title := strings.TrimSpace(candidate.Title); title != "" {
			line += title + " · "
		}
		line += "卷面号 " + strings.TrimSpace(candidate.PaperNo)
		if candidate.SentAt > 0 {
			line += " · 发送日期 " + time.Unix(candidate.SentAt, 0).Format("2006-01-02")
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func k12PhotoRoutingCandidateReply(snapshot k12usecase.InboundPhotoRoutingSnapshot) (*adapter.Reply, error) {
	text := k12PhotoRoutingCandidateText(snapshot)
	message, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12, "zh-CN", text, text, "", nil,
	)
	if err != nil {
		return nil, err
	}
	return adapterReplyFromChannelMessage(message), nil
}

func k12CreativeWorkReply(result k12usecase.ImageTaskResult) (*adapter.Reply, error) {
	if result.CreativeWork == nil || len(result.CreativeWork.Fields.Versions) == 0 {
		return nil, fmt.Errorf("K12 作品任务已完成但点评缺失")
	}
	version := result.CreativeWork.Fields.Versions[len(result.CreativeWork.Fields.Versions)-1]
	markdown := strings.TrimSpace(version.Feedback)
	if version.StructuredFeedback != nil {
		markdown = strings.TrimSpace(version.StructuredFeedback.ProjectionMarkdown)
	}
	if markdown == "" {
		return nil, fmt.Errorf("K12 作品任务已完成但点评投影为空")
	}
	projected := imLaTeXFallback(markdown, "k12_creative_feedback")
	fallbackReason := ""
	if projected != markdown {
		fallbackReason = messagecontent.FallbackMathToReadableText
	}
	msg, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12, "zh-CN", markdown, projected, fallbackReason, nil,
	)
	if err != nil {
		return nil, err
	}
	return adapterReplyFromChannelMessage(msg), nil
}

// routeK12DingtalkPhotoTutor 路由门禁：仅钉钉单图 + 显式绑定 K12 辅导 Agent 时接管。
// K12-INV-015：当前渠道只允许 direct，群 conversation 永不进入业务流——群聊消息
// （钉钉 conversation_type="2"）在此静默交还通用路径，绝不触发批改。
func routeK12DingtalkPhotoTutor(msg *adapter.Message, router *agentrouter.Dispatcher) *agentrouter.RoutingResult {
	if msg == nil || len(msg.Attachments) != 1 || strings.TrimSpace(msg.Attachments[0].Type) != "image" {
		return nil
	}
	return routeK12DingtalkTutor(msg, router)
}

func routeK12DingtalkTutor(msg *adapter.Message, router *agentrouter.Dispatcher) *agentrouter.RoutingResult {
	if msg == nil || router == nil || msg.Platform != adapter.PlatformDingtalk {
		return nil
	}
	if msg.Metadata["conversation_type"] == "2" { // 钉钉群聊约定值（adapter/dingtalk 同一口径）
		return nil
	}
	routed := router.Route(agentrouter.RouteRequest{
		Platform:   string(msg.Platform),
		InstanceID: msg.InstanceID,
		UserID:     msg.UserID,
		ChatID:     msg.ChatID,
	})
	if routed == nil || routed.Rule == nil || routed.AgentConfig == nil ||
		strings.TrimSpace(routed.AgentConfig.Metadata["scenario"]) != k12TutorScenario {
		return nil
	}
	return routed
}

func k12PhotoSourceSession(msg *adapter.Message) string {
	sourceSession := strings.TrimSpace(msg.SessionID)
	if sourceSession == "" {
		sourceSession = strings.TrimSpace(msg.ChatID)
	}
	if sourceSession == "" {
		sourceSession = strings.TrimSpace(msg.ID)
	}
	return sourceSession
}

// k12PhotoReply 按既有投递逻辑组装 IM 回复：先产 ChannelNeutralMessage（§6.10 ChannelPort
// 收敛，批改结果 Markdown + 可选批注图），再经钉钉投影为 adapter.Reply——
// 拍照批改是入站消息的同步回复，传输仍走 adapter 回执链，仅消息构造收敛到通道中立层，
// 附件名/MIME/base64 编码与直连时代逐字节一致。
func k12PhotoReply(result k12usecase.PhotoGradeResult) *adapter.Reply {
	return adapterReplyFromChannelMessage(k12PhotoChannelMessage(result))
}

// k12PhotoChannelMessage 把批改产物装配为通道中立图文消息（发图文：批改结果+批注图）。
// 批改 Markdown 在此过 IM 出口 LaTeX→Unicode 兜底（钉钉不渲染 LaTeX；识别/批改侧
// 真机取证过模型会违反提示词的 Unicode 约束——BUG-20260712-U）；桌面 HTTP 面的
// 批改结果不经此路径，保持原文。
func k12PhotoChannelMessage(result k12usecase.PhotoGradeResult) channel.Message {
	attachments := make([]channel.Attachment, 0, 1)
	if result.AnnotatedImage != nil && len(result.AnnotatedImage.Data) > 0 {
		mime := strings.TrimSpace(result.AnnotatedImage.MIME)
		if mime == "" {
			mime = "image/png"
		}
		attachments = append(attachments, channel.Attachment{
			Name: correctedPhotoFilename(mime),
			MIME: mime,
			Data: result.AnnotatedImage.Data,
		})
	}
	projected := imLaTeXFallback(result.Markdown, "k12_photo_grading")
	fallbackReason := ""
	if projected != result.Markdown {
		fallbackReason = messagecontent.FallbackMathToReadableText
	}
	msg, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12,
		"zh-CN",
		result.Markdown,
		projected,
		fallbackReason,
		attachments,
	)
	if err != nil {
		return channel.Message{Text: "批改结果渲染失败，请重试。"}
	}
	return msg
}

func correctedPhotoFilename(mime string) string {
	if strings.EqualFold(strings.TrimSpace(mime), "image/jpeg") || strings.EqualFold(strings.TrimSpace(mime), "image/jpg") {
		return "批改后的作业.jpg"
	}
	return "批改后的作业.png"
}

func decodeK12PhotoAttachment(att adapter.Attachment) ([]byte, error) {
	encoded := strings.TrimSpace(att.Data)
	if encoded == "" {
		return nil, fmt.Errorf("K12 钉钉拍照批改: 图片数据为空")
	}
	if strings.HasPrefix(encoded, "data:") {
		comma := strings.IndexByte(encoded, ',')
		if comma < 0 {
			return nil, fmt.Errorf("K12 钉钉拍照批改: 图片 data URL 无效")
		}
		encoded = encoded[comma+1:]
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("K12 钉钉拍照批改: 解码图片: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("K12 钉钉拍照批改: 图片数据为空")
	}
	return raw, nil
}
