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
