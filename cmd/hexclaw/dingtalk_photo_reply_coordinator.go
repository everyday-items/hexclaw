package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/messagecontent"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

const k12DingtalkPhotoReplyObjectKind = "dingtalk_photo_grading_reply"

// k12DingtalkPhotoReplyBatchPort 只暴露既有 DeliveryBatch 状态机需要的两个动作：
// 首次按入站冻结的唯一目标创建图文批次，或在重启后查询已绑定批次。实现必须复用
// usecase 的批次编排和 channel.PartReceiptPort，不得在 cmd 层复制发送、重试或回执状态机。
type k12DingtalkPhotoReplyBatchPort interface {
	PrepareAndSendMessageBatchForTargets(
		ctx context.Context,
		agentName, objectKind, objectID string,
		message k12usecase.DeliveryMessage,
		targets []k12usecase.ResolvedDeliveryTarget,
	) (k12.DeliveryBatch, bool, error)
	QueryDeliveryBatch(
		ctx context.Context,
		agentName, batchID string,
	) (k12.DeliveryBatch, error)
}

// k12DingtalkPhotoReplyCommand 只携带已经持久化的最终产物和入站 direct 目标。
// DeliveryBatchID 非空表示此前已跨越批次绑定边界；此时 Message 即使存在也必须被忽略。
type k12DingtalkPhotoReplyCommand struct {
	AgentName           string
	InboundReceiptID    string
	FinalArtifactID     string
	FinalArtifactDigest string
	DeliveryBatchID     string
	Target              k12usecase.ResolvedDeliveryTarget
	Message             k12usecase.DeliveryMessage
}

type k12DingtalkPhotoReplyCoordinator struct {
	batches k12DingtalkPhotoReplyBatchPort
}

func newK12DingtalkPhotoReplyCoordinator(
	batches k12DingtalkPhotoReplyBatchPort,
) *k12DingtalkPhotoReplyCoordinator {
	return &k12DingtalkPhotoReplyCoordinator{batches: batches}
}

func (c *k12DingtalkPhotoReplyCoordinator) Deliver(
	ctx context.Context,
	command k12DingtalkPhotoReplyCommand,
) (k12.DeliveryBatch, bool, error) {
	if c == nil || c.batches == nil {
		return k12.DeliveryBatch{}, false, fmt.Errorf("K12 DingTalk photo reply delivery is unavailable")
	}
	command.AgentName = strings.TrimSpace(command.AgentName)
	command.DeliveryBatchID = strings.TrimSpace(command.DeliveryBatchID)
	if command.AgentName == "" {
		return k12.DeliveryBatch{}, false, fmt.Errorf("K12 DingTalk photo reply agent is required")
	}
	// 批次一旦绑定，重启只允许查询该批次。不得重读最终产物、准备媒体或再次发送。
	if command.DeliveryBatchID != "" {
		batch, err := c.batches.QueryDeliveryBatch(
			ctx, command.AgentName, command.DeliveryBatchID,
		)
		return batch, false, err
	}

	command.InboundReceiptID = strings.TrimSpace(command.InboundReceiptID)
	command.FinalArtifactID = strings.TrimSpace(command.FinalArtifactID)
	command.FinalArtifactDigest = strings.TrimSpace(command.FinalArtifactDigest)
	command.Target.BindingID = strings.TrimSpace(command.Target.BindingID)
	command.Target.Target.Platform = strings.ToLower(strings.TrimSpace(command.Target.Target.Platform))
	command.Target.Target.InstanceID = strings.TrimSpace(command.Target.Target.InstanceID)
	command.Target.Target.ChatID = strings.TrimSpace(command.Target.Target.ChatID)
	command.Message.Content = strings.TrimSpace(command.Message.Content)
	if command.InboundReceiptID == "" || command.FinalArtifactID == "" ||
		!validK12DingtalkPhotoReplyDigest(command.FinalArtifactDigest) {
		return k12.DeliveryBatch{}, false, fmt.Errorf("K12 DingTalk photo reply identity is incomplete")
	}
	if command.Target.BindingID == "" || command.Target.Target.Platform != "dingtalk" ||
		command.Target.Target.ChatID == "" ||
		(command.Target.Target.ChatID[0] < 0x20) {
		return k12.DeliveryBatch{}, false, fmt.Errorf("K12 DingTalk photo reply target must be one direct binding")
	}
	if command.Message.Content == "" || len(command.Message.Attachments) != 1 {
		return k12.DeliveryBatch{}, false, fmt.Errorf("K12 DingTalk photo reply must contain Markdown and one annotated image")
	}
	attachment := command.Message.Attachments[0]
	attachment.Name = strings.TrimSpace(attachment.Name)
	attachment.MIME = strings.ToLower(strings.TrimSpace(attachment.MIME))
	if attachment.Name == "" || !strings.HasPrefix(attachment.MIME, "image/") ||
		attachment.MIME == "image/" || len(attachment.Data) == 0 {
		return k12.DeliveryBatch{}, false, fmt.Errorf("K12 DingTalk photo reply annotated image is incomplete")
	}
	command.Message.Attachments[0] = attachment

	batch, created, err := c.batches.PrepareAndSendMessageBatchForTargets(
		ctx,
		command.AgentName,
		k12DingtalkPhotoReplyObjectKind,
		k12DingtalkPhotoReplyObjectID(command),
		command.Message,
		[]k12usecase.ResolvedDeliveryTarget{command.Target},
	)
	if err != nil {
		return batch, created, err
	}
	if err := validateK12DingtalkPhotoReplyBatch(batch, command.Target, attachment.MIME); err != nil {
		return batch, created, err
	}
	return batch, created, nil
}

func validK12DingtalkPhotoReplyDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func k12DingtalkPhotoReplyObjectID(command k12DingtalkPhotoReplyCommand) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		command.InboundReceiptID,
		command.FinalArtifactID,
		command.FinalArtifactDigest,
	}, "\x00")))
	return "photo-reply-" + hex.EncodeToString(sum[:])
}

func validateK12DingtalkPhotoReplyBatch(
	batch k12.DeliveryBatch,
	target k12usecase.ResolvedDeliveryTarget,
	imageMIME string,
) error {
	if strings.TrimSpace(batch.BatchID) == "" || len(batch.Receipts) != 2 {
		return fmt.Errorf("K12 DingTalk photo reply batch is incomplete")
	}
	markdown, image := batch.Receipts[0], batch.Receipts[1]
	if markdown.PartKind != messagecontent.PartMarkdown || markdown.PartOrdinal != 1 ||
		image.PartKind != messagecontent.PartArtifact || image.PartOrdinal != 2 ||
		image.PartMIME != imageMIME {
		return fmt.Errorf("K12 DingTalk photo reply batch has an invalid part set")
	}
	for _, receipt := range batch.Receipts {
		if strings.TrimSpace(receipt.DeliveryID) == "" || receipt.BindingID != target.BindingID ||
			receipt.Target != target.Target {
			return fmt.Errorf("K12 DingTalk photo reply batch changed its direct target")
		}
	}
	return nil
}
