package usecase

import (
	"context"
	"fmt"
	"strings"
)

// InboundPhotoCoordinator 只协调 durable admission 与状态 CAS；不拥有 ACK、模型或外发 transport。
type InboundPhotoCoordinator struct {
	repository InboundPhotoRepository
}

func NewInboundPhotoCoordinator(repository InboundPhotoRepository) *InboundPhotoCoordinator {
	return &InboundPhotoCoordinator{repository: repository}
}

func (c *InboundPhotoCoordinator) Admit(
	ctx context.Context, input InboundPhotoAdmission,
) (InboundPhotoBundle, bool, error) {
	if c == nil || c.repository == nil {
		return InboundPhotoBundle{}, false, fmt.Errorf("usecase: inbound photo repository is unavailable")
	}
	return c.repository.AdmitInboundPhoto(ctx, input)
}

func (c *InboundPhotoCoordinator) Resume(
	ctx context.Context, agentName, receiptID string,
) (InboundPhotoBundle, error) {
	if c == nil || c.repository == nil {
		return InboundPhotoBundle{}, fmt.Errorf("usecase: inbound photo repository is unavailable")
	}
	return c.repository.GetInboundPhoto(
		ctx, strings.TrimSpace(agentName), strings.TrimSpace(receiptID),
	)
}

// ResumeByIdentity 按完整提供方消息身份恢复首个冻结入站聚合。
func (c *InboundPhotoCoordinator) ResumeByIdentity(
	ctx context.Context, identity InboundPhotoIdentity,
) (InboundPhotoBundle, error) {
	if c == nil || c.repository == nil {
		return InboundPhotoBundle{}, fmt.Errorf("usecase: inbound photo repository is unavailable")
	}
	return c.repository.GetInboundPhotoByIdentity(ctx, identity)
}

func (c *InboundPhotoCoordinator) Recoverable(
	ctx context.Context, limit int,
) ([]InboundPhotoBundle, error) {
	if c == nil || c.repository == nil {
		return nil, fmt.Errorf("usecase: inbound photo repository is unavailable")
	}
	return c.repository.ListRecoverableInboundPhotos(ctx, limit)
}

func (c *InboundPhotoCoordinator) advance(
	ctx context.Context,
	agentName, receiptID string,
	expectedVersion int64,
	mutate func(*InboundPhotoDispatchState) error,
) (InboundPhotoDispatch, error) {
	if c == nil || c.repository == nil {
		return InboundPhotoDispatch{}, fmt.Errorf("usecase: inbound photo repository is unavailable")
	}
	agentName = strings.TrimSpace(agentName)
	receiptID = strings.TrimSpace(receiptID)
	if agentName == "" || receiptID == "" || expectedVersion < 0 || mutate == nil {
		return InboundPhotoDispatch{}, fmt.Errorf("%w: inbound photo CAS identity is incomplete", ErrInvalidInput)
	}
	bundle, err := c.repository.GetInboundPhoto(ctx, agentName, receiptID)
	if err != nil {
		return InboundPhotoDispatch{}, err
	}
	next := bundle.Dispatch.State()
	if err := mutate(&next); err != nil {
		return InboundPhotoDispatch{}, err
	}
	return c.repository.CompareAndSwapInboundPhotoDispatch(
		ctx, agentName, receiptID, expectedVersion, next,
	)
}

func (c *InboundPhotoCoordinator) RecordImageTask(
	ctx context.Context,
	agentName, receiptID string,
	expectedVersion int64,
	imageTaskID string,
) (InboundPhotoDispatch, error) {
	imageTaskID = strings.TrimSpace(imageTaskID)
	if imageTaskID == "" {
		return InboundPhotoDispatch{}, fmt.Errorf("%w: image task identity is empty", ErrInvalidInput)
	}
	return c.advance(ctx, agentName, receiptID, expectedVersion, func(next *InboundPhotoDispatchState) error {
		next.ProcessingStatus = InboundPhotoImageTaskSubmitted
		next.ImageTaskID = imageTaskID
		return nil
	})
}

func (c *InboundPhotoCoordinator) RecordRoutingDecision(
	ctx context.Context,
	agentName, receiptID string,
	expectedVersion int64,
	decision InboundPhotoRoutingDecision,
) (InboundPhotoDispatch, error) {
	if decision != InboundPhotoRouteRegrade && decision != InboundPhotoRouteNewSubmission {
		return InboundPhotoDispatch{}, fmt.Errorf("%w: inbound photo route is not final", ErrInvalidInput)
	}
	return c.advance(ctx, agentName, receiptID, expectedVersion, func(next *InboundPhotoDispatchState) error {
		next.RoutingDecision = decision
		next.ConfirmationStatus = InboundPhotoConfirmationNotRequired
		return nil
	})
}

func (c *InboundPhotoCoordinator) RequestRoutingConfirmation(
	ctx context.Context,
	agentName, receiptID string,
	expectedVersion int64,
) (InboundPhotoDispatch, error) {
	return c.advance(ctx, agentName, receiptID, expectedVersion, func(next *InboundPhotoDispatchState) error {
		next.RoutingDecision = InboundPhotoRouteAskedUser
		next.ConfirmationStatus = InboundPhotoConfirmationWaiting
		return nil
	})
}

func (c *InboundPhotoCoordinator) ConfirmRouting(
	ctx context.Context,
	agentName, receiptID string,
	expectedVersion int64,
	decision InboundPhotoRoutingDecision,
) (InboundPhotoDispatch, error) {
	if decision != InboundPhotoRouteRegrade && decision != InboundPhotoRouteNewSubmission {
		return InboundPhotoDispatch{}, fmt.Errorf("%w: confirmed inbound photo route is invalid", ErrInvalidInput)
	}
	return c.advance(ctx, agentName, receiptID, expectedVersion, func(next *InboundPhotoDispatchState) error {
		if next.RoutingDecision != InboundPhotoRouteAskedUser ||
			next.ConfirmationStatus != InboundPhotoConfirmationWaiting {
			return fmt.Errorf("%w: inbound photo is not waiting for route confirmation", ErrInvalidInput)
		}
		next.RoutingDecision = decision
		next.ConfirmationStatus = InboundPhotoConfirmationConfirmed
		return nil
	})
}

// RequestRoutingConfirmationWithSnapshot 在推进 waiting 状态的同时冻结候选顺序；
// 仓储不支持扩展时由调用方继续使用旧的单阶段协议。
func (c *InboundPhotoCoordinator) RequestRoutingConfirmationWithSnapshot(
	ctx context.Context,
	agentName, receiptID string,
	expectedVersion int64,
	snapshot InboundPhotoRoutingSnapshot,
) (InboundPhotoDispatch, error) {
	if c == nil || c.repository == nil {
		return InboundPhotoDispatch{}, fmt.Errorf("usecase: inbound photo repository is unavailable")
	}
	repository, ok := c.repository.(InboundPhotoRoutingSnapshotRepository)
	if !ok {
		return InboundPhotoDispatch{}, fmt.Errorf("usecase: inbound photo routing snapshot repository is unavailable")
	}
	return repository.SaveInboundPhotoRoutingSnapshot(
		ctx, strings.TrimSpace(agentName), strings.TrimSpace(receiptID), expectedVersion, snapshot,
	)
}

// GetRoutingSnapshot 返回重启后同一收据的候选快照，不重新读取练习集列表。
func (c *InboundPhotoCoordinator) GetRoutingSnapshot(
	ctx context.Context, agentName, receiptID string,
) (InboundPhotoRoutingSnapshot, error) {
	if c == nil || c.repository == nil {
		return InboundPhotoRoutingSnapshot{}, fmt.Errorf("usecase: inbound photo repository is unavailable")
	}
	repository, ok := c.repository.(InboundPhotoRoutingSnapshotRepository)
	if !ok {
		return InboundPhotoRoutingSnapshot{}, fmt.Errorf("usecase: inbound photo routing snapshot repository is unavailable")
	}
	return repository.GetInboundPhotoRoutingSnapshot(
		ctx, strings.TrimSpace(agentName), strings.TrimSpace(receiptID),
	)
}

// ConfirmRoutingSelection 只接受冻结候选中的 practiceSetID，避免确认后再次按可变列表漂移。
func (c *InboundPhotoCoordinator) ConfirmRoutingSelection(
	ctx context.Context,
	agentName, receiptID string,
	expectedVersion int64,
	decision InboundPhotoRoutingDecision,
	practiceSetID string,
) (InboundPhotoDispatch, error) {
	if c == nil || c.repository == nil {
		return InboundPhotoDispatch{}, fmt.Errorf("usecase: inbound photo repository is unavailable")
	}
	repository, ok := c.repository.(InboundPhotoRoutingSnapshotRepository)
	if !ok {
		return InboundPhotoDispatch{}, fmt.Errorf("usecase: inbound photo routing snapshot repository is unavailable")
	}
	return repository.ConfirmInboundPhotoRoutingSelection(
		ctx, strings.TrimSpace(agentName), strings.TrimSpace(receiptID), expectedVersion, decision,
		strings.TrimSpace(practiceSetID),
	)
}

func (c *InboundPhotoCoordinator) RecordFinalArtifact(
	ctx context.Context,
	agentName, receiptID string,
	expectedVersion int64,
	finalArtifactID string,
) (InboundPhotoDispatch, error) {
	finalArtifactID = strings.TrimSpace(finalArtifactID)
	if finalArtifactID == "" {
		return InboundPhotoDispatch{}, fmt.Errorf("%w: final artifact identity is empty", ErrInvalidInput)
	}
	return c.advance(ctx, agentName, receiptID, expectedVersion, func(next *InboundPhotoDispatchState) error {
		next.ProcessingStatus = InboundPhotoFinalArtifactReady
		next.FinalArtifactID = finalArtifactID
		next.ReplyStatus = InboundPhotoReplyReady
		return nil
	})
}

func (c *InboundPhotoCoordinator) BindReplyBatch(
	ctx context.Context,
	agentName, receiptID string,
	expectedVersion int64,
	deliveryBatchID string,
) (InboundPhotoDispatch, error) {
	deliveryBatchID = strings.TrimSpace(deliveryBatchID)
	if deliveryBatchID == "" {
		return InboundPhotoDispatch{}, fmt.Errorf("%w: delivery batch identity is empty", ErrInvalidInput)
	}
	return c.advance(ctx, agentName, receiptID, expectedVersion, func(next *InboundPhotoDispatchState) error {
		next.ReplyStatus = InboundPhotoReplyBound
		next.DeliveryBatchID = deliveryBatchID
		return nil
	})
}

func (c *InboundPhotoCoordinator) CompleteReply(
	ctx context.Context,
	agentName, receiptID string,
	expectedVersion int64,
) (InboundPhotoDispatch, error) {
	return c.advance(ctx, agentName, receiptID, expectedVersion, func(next *InboundPhotoDispatchState) error {
		next.ReplyStatus = InboundPhotoReplyDelivered
		return nil
	})
}

// FailTerminal 只冻结内部永久失败事实，不改变已完成的处理或回复检查点。
func (c *InboundPhotoCoordinator) FailTerminal(
	ctx context.Context,
	agentName, receiptID string,
	expectedVersion int64,
	stage InboundPhotoTerminalStage,
	failureKind string,
) (InboundPhotoDispatch, error) {
	failureKind = strings.TrimSpace(failureKind)
	if stage == "" || failureKind == "" {
		return InboundPhotoDispatch{}, fmt.Errorf("%w: inbound photo terminal facts are incomplete", ErrInvalidInput)
	}
	return c.advance(ctx, agentName, receiptID, expectedVersion, func(next *InboundPhotoDispatchState) error {
		next.TerminalStatus = InboundPhotoTerminalFailed
		next.TerminalStage = stage
		next.FailureKind = failureKind
		return nil
	})
}
