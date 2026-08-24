package usecase

import (
	"context"

	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

// 入站图片聚合类型由耐久仓储定义；用例层通过这些别名保持单一合同面。
type InboundPhotoIdentity = k12storage.InboundPhotoIdentity
type InboundPhotoAdmission = k12storage.InboundPhotoAdmission
type InboundPhotoReceipt = k12storage.InboundPhotoReceipt
type InboundPhotoAsset = k12storage.InboundPhotoAsset
type InboundPhotoDispatchState = k12storage.InboundPhotoDispatchState
type InboundPhotoDispatch = k12storage.InboundPhotoDispatch
type InboundPhotoBundle = k12storage.InboundPhotoBundle
type InboundPhotoProcessingStatus = k12storage.InboundPhotoProcessingStatus
type InboundPhotoRoutingDecision = k12storage.InboundPhotoRoutingDecision
type InboundPhotoConfirmationStatus = k12storage.InboundPhotoConfirmationStatus
type InboundPhotoReplyStatus = k12storage.InboundPhotoReplyStatus

// ErrInboundPhotoConflict 保留仓储与用例层一致的外部消息冲突判定。
var ErrInboundPhotoConflict = k12storage.ErrInboundPhotoConflict

const (
	InboundPhotoAdmitted           = k12storage.InboundPhotoAdmitted
	InboundPhotoImageTaskSubmitted = k12storage.InboundPhotoImageTaskSubmitted
	InboundPhotoFinalArtifactReady = k12storage.InboundPhotoFinalArtifactReady

	InboundPhotoRoutePending       = k12storage.InboundPhotoRoutePending
	InboundPhotoRouteRegrade       = k12storage.InboundPhotoRouteRegrade
	InboundPhotoRouteNewSubmission = k12storage.InboundPhotoRouteNewSubmission
	InboundPhotoRouteAskedUser     = k12storage.InboundPhotoRouteAskedUser

	InboundPhotoConfirmationNotRequired = k12storage.InboundPhotoConfirmationNotRequired
	InboundPhotoConfirmationWaiting     = k12storage.InboundPhotoConfirmationWaiting
	InboundPhotoConfirmationConfirmed   = k12storage.InboundPhotoConfirmationConfirmed

	InboundPhotoReplyPending   = k12storage.InboundPhotoReplyPending
	InboundPhotoReplyReady     = k12storage.InboundPhotoReplyReady
	InboundPhotoReplyBound     = k12storage.InboundPhotoReplyBound
	InboundPhotoReplyDelivered = k12storage.InboundPhotoReplyDelivered
)

// InboundPhotoRepository 是 callback admission 与重启 worker 共享的唯一耐久 port。
type InboundPhotoRepository interface {
	AdmitInboundPhoto(
		context.Context, InboundPhotoAdmission,
	) (InboundPhotoBundle, bool, error)
	GetInboundPhoto(context.Context, string, string) (InboundPhotoBundle, error)
	GetInboundPhotoByIdentity(
		context.Context, InboundPhotoIdentity,
	) (InboundPhotoBundle, error)
	CompareAndSwapInboundPhotoDispatch(
		context.Context, string, string, int64, InboundPhotoDispatchState,
	) (InboundPhotoDispatch, error)
	ListRecoverableInboundPhotos(context.Context, int) ([]InboundPhotoBundle, error)
}
