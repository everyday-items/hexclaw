package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func deliveryDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func normalizeDeliveryMessage(message DeliveryMessage) (DeliveryMessage, error) {
	message.Content = strings.TrimSpace(message.Content)
	if message.Content == "" {
		return DeliveryMessage{}, fmt.Errorf("%w: delivery content required", ErrInvalidInput)
	}
	attachments := make([]DeliveryAttachment, len(message.Attachments))
	for i, attachment := range message.Attachments {
		attachment.Name = strings.TrimSpace(attachment.Name)
		attachment.MIME = strings.ToLower(strings.TrimSpace(attachment.MIME))
		if attachment.Name == "" || attachment.MIME == "" || len(attachment.Data) == 0 {
			return DeliveryMessage{}, fmt.Errorf(
				"%w: delivery attachment %d is incomplete", ErrInvalidInput, i+1,
			)
		}
		attachment.Data = append([]byte(nil), attachment.Data...)
		attachments[i] = attachment
	}
	message.Attachments = attachments
	return message, nil
}

func deliveryMessageDigest(message DeliveryMessage) string {
	identities := make([]DeliveryAttachmentIdentity, 0, len(message.Attachments))
	for _, attachment := range message.Attachments {
		identities = append(identities, DeliveryAttachmentIdentity{
			Name:          attachment.Name,
			MIME:          attachment.MIME,
			ContentDigest: deliveryDigest(string(attachment.Data)),
		})
	}
	digest, _ := deliveryMessageIdentityDigest(message.Content, identities)
	return digest
}

func deliveryMessageIdentityDigest(
	content string,
	attachments []DeliveryAttachmentIdentity,
) (string, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return "", fmt.Errorf("%w: delivery content required", ErrInvalidInput)
	}
	if len(attachments) == 0 {
		return deliveryDigest(content), nil
	}
	type attachmentDigest struct {
		Name   string `json:"name"`
		MIME   string `json:"mime"`
		Digest string `json:"digest"`
	}
	normalized := make([]attachmentDigest, 0, len(attachments))
	for i, attachment := range attachments {
		attachment.Name = strings.TrimSpace(attachment.Name)
		attachment.MIME = strings.ToLower(strings.TrimSpace(attachment.MIME))
		attachment.ContentDigest = strings.ToLower(strings.TrimSpace(attachment.ContentDigest))
		rawDigest, ok := strings.CutPrefix(attachment.ContentDigest, "sha256:")
		decoded, err := hex.DecodeString(rawDigest)
		if attachment.Name == "" || attachment.MIME == "" || !ok || err != nil || len(decoded) != sha256.Size {
			return "", fmt.Errorf(
				"%w: delivery attachment identity %d is incomplete", ErrInvalidInput, i+1,
			)
		}
		normalized = append(normalized, attachmentDigest{
			Name: attachment.Name, MIME: attachment.MIME, Digest: attachment.ContentDigest,
		})
	}
	payload, _ := json.Marshal(struct {
		Content     string             `json:"content"`
		Attachments []attachmentDigest `json:"attachments"`
	}{Content: content, Attachments: normalized})
	return deliveryDigest(string(payload)), nil
}

func deliveryDedupeKey(agentName, objectKind, objectID, bindingID, payloadDigest string) string {
	return deliveryDigest(strings.Join([]string{agentName, objectKind, objectID, bindingID, payloadDigest}, "\x00"))
}

func deliveryBatchDedupeKey(agentName, objectKind, objectID, contentDigest string) string {
	return deliveryDigest(strings.Join([]string{agentName, objectKind, objectID, contentDigest}, "\x00"))
}

// GetDeliveryBatchForMessageIdentity 只读取与当前正文及附件身份完全一致的冻结批次。
// 它不解析目标、不准备载荷，也不启动 pending 子回执或跨越渠道边界。
func (d Deps) GetDeliveryBatchForMessageIdentity(
	ctx context.Context,
	agentName, objectKind, objectID, content string,
	attachments []DeliveryAttachmentIdentity,
) (k12.DeliveryBatch, error) {
	agentName = strings.TrimSpace(agentName)
	objectKind = strings.TrimSpace(objectKind)
	objectID = strings.TrimSpace(objectID)
	if agentName == "" || objectKind == "" || objectID == "" {
		return k12.DeliveryBatch{}, fmt.Errorf("%w: agent/object/content required", ErrInvalidInput)
	}
	if d.Records == nil {
		return k12.DeliveryBatch{}, ErrDeliveryUnavailable
	}
	contentDigest, err := deliveryMessageIdentityDigest(content, attachments)
	if err != nil {
		return k12.DeliveryBatch{}, err
	}
	return d.Records.GetDeliveryBatchByDedupe(
		ctx,
		agentName,
		deliveryBatchDedupeKey(agentName, objectKind, objectID, contentDigest),
	)
}

// ReplayDeliveryBatchForMessageIdentity 在任何附件读取或渠道准备前重放已冻结批次。
// 已尝试的子回执保持不动，只有从未尝试的 pending 子回执会继续发送。
func (d Deps) ReplayDeliveryBatchForMessageIdentity(
	ctx context.Context,
	agentName, objectKind, objectID, content string,
	attachments []DeliveryAttachmentIdentity,
) (k12.DeliveryBatch, error) {
	batch, err := d.GetDeliveryBatchForMessageIdentity(
		ctx, agentName, objectKind, objectID, content, attachments,
	)
	if err != nil {
		return k12.DeliveryBatch{}, err
	}
	return d.sendDeliveryBatch(ctx, batch)
}

func validatePreparedDelivery(prepared PreparedTextDelivery) error {
	if strings.TrimSpace(prepared.BindingID) == "" || strings.TrimSpace(prepared.Target.Platform) == "" ||
		strings.TrimSpace(prepared.Target.ChatID) == "" || strings.TrimSpace(prepared.PayloadJSON) == "" {
		return fmt.Errorf("%w: 投递目标或冻结载荷不完整", ErrInvalidInput)
	}
	if !json.Valid([]byte(prepared.PayloadJSON)) {
		return fmt.Errorf("%w: 冻结投递载荷不是 JSON", ErrInvalidInput)
	}
	if prepared.RenderJSON != "" && !json.Valid([]byte(prepared.RenderJSON)) {
		return fmt.Errorf("%w: 冻结渲染证据不是 JSON", ErrInvalidInput)
	}
	return nil
}

func (d Deps) ResolveDeliveryTargets(
	ctx context.Context,
	agentName string,
) ([]ResolvedDeliveryTarget, error) {
	if d.Delivery == nil {
		return nil, ErrDeliveryUnavailable
	}
	batchTransport, ok := d.Delivery.(BatchDeliveryTransport)
	if !ok {
		return nil, ErrDeliveryUnavailable
	}
	targets, err := batchTransport.ResolveTextTargets(ctx, strings.TrimSpace(agentName))
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, ErrNoActiveDirectBindings
	}
	for i := range targets {
		targets[i].BindingID = strings.TrimSpace(targets[i].BindingID)
		targets[i].Target.Platform = strings.ToLower(strings.TrimSpace(targets[i].Target.Platform))
		targets[i].Target.InstanceID = strings.TrimSpace(targets[i].Target.InstanceID)
		targets[i].Target.ChatID = strings.TrimSpace(targets[i].Target.ChatID)
		targets[i].Target.Label = strings.TrimSpace(targets[i].Target.Label)
		if targets[i].BindingID == "" || targets[i].Target.Platform == "" ||
			targets[i].Target.ChatID == "" ||
			(targets[i].Target.ChatID[0] < 0x20) {
			return nil, fmt.Errorf("%w: resolved direct binding %d is invalid", ErrInvalidInput, i+1)
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		left, right := targets[i], targets[j]
		if left.Target.Platform != right.Target.Platform {
			return left.Target.Platform < right.Target.Platform
		}
		if left.Target.InstanceID != right.Target.InstanceID {
			return left.Target.InstanceID < right.Target.InstanceID
		}
		if left.Target.ChatID != right.Target.ChatID {
			return left.Target.ChatID < right.Target.ChatID
		}
		return left.BindingID < right.BindingID
	})
	for i := 1; i < len(targets); i++ {
		if targets[i-1].Target.Platform == targets[i].Target.Platform &&
			targets[i-1].Target.InstanceID == targets[i].Target.InstanceID &&
			targets[i-1].Target.ChatID == targets[i].Target.ChatID {
			return nil, fmt.Errorf("%w: resolved direct bindings contain duplicate target", ErrInvalidInput)
		}
	}
	return targets, nil
}

func (d Deps) prepareTextForResolvedTargets(
	ctx context.Context,
	content string,
	targets []ResolvedDeliveryTarget,
) ([]PreparedTextDelivery, error) {
	batchTransport, ok := d.Delivery.(BatchDeliveryTransport)
	if !ok {
		return nil, ErrDeliveryUnavailable
	}
	prepared, err := batchTransport.PrepareTextForTargets(ctx, content, targets)
	if err != nil {
		return nil, err
	}
	if len(prepared) != len(targets) {
		return nil, fmt.Errorf(
			"%w: prepared deliveries=%d resolved targets=%d",
			ErrInvalidInput, len(prepared), len(targets),
		)
	}
	for i := range prepared {
		if err := validatePreparedDelivery(prepared[i]); err != nil {
			return nil, fmt.Errorf("prepared delivery %d: %w", i+1, err)
		}
		if prepared[i].BindingID != targets[i].BindingID ||
			prepared[i].Target != targets[i].Target {
			return nil, fmt.Errorf(
				"%w: prepared delivery %d changed its resolved binding target",
				ErrInvalidInput, i+1,
			)
		}
	}
	return prepared, nil
}

func (d Deps) prepareMessageForResolvedTargets(
	ctx context.Context,
	message DeliveryMessage,
	targets []ResolvedDeliveryTarget,
) ([]PreparedTextDelivery, error) {
	batchTransport, ok := d.Delivery.(BatchMessageDeliveryTransport)
	if !ok {
		return nil, ErrDeliveryUnavailable
	}
	prepared, err := batchTransport.PrepareMessageForTargets(ctx, message, targets)
	if err != nil {
		return nil, err
	}
	if len(prepared) != len(targets) {
		return nil, fmt.Errorf(
			"%w: prepared deliveries=%d resolved targets=%d",
			ErrInvalidInput, len(prepared), len(targets),
		)
	}
	for i := range prepared {
		if err := validatePreparedDelivery(prepared[i]); err != nil {
			return nil, fmt.Errorf("prepared delivery %d: %w", i+1, err)
		}
		if prepared[i].BindingID != targets[i].BindingID ||
			prepared[i].Target != targets[i].Target {
			return nil, fmt.Errorf(
				"%w: prepared delivery %d changed its resolved binding target",
				ErrInvalidInput, i+1,
			)
		}
	}
	return prepared, nil
}

func (d Deps) resolveAndPrepareTextBatch(
	ctx context.Context,
	agentName, content string,
) ([]PreparedTextDelivery, error) {
	if _, ok := d.Delivery.(BatchDeliveryTransport); ok {
		targets, err := d.ResolveDeliveryTargets(ctx, agentName)
		if err != nil {
			return nil, err
		}
		return d.prepareTextForResolvedTargets(ctx, content, targets)
	}
	prepared, err := d.Delivery.PrepareText(ctx, agentName, content)
	if err != nil {
		return nil, err
	}
	if err := validatePreparedDelivery(prepared); err != nil {
		return nil, err
	}
	return []PreparedTextDelivery{prepared}, nil
}

func (d Deps) resolveAndPrepareMessageBatch(
	ctx context.Context,
	agentName string,
	message DeliveryMessage,
) ([]PreparedTextDelivery, error) {
	if _, ok := d.Delivery.(BatchDeliveryTransport); !ok {
		return nil, ErrDeliveryUnavailable
	}
	targets, err := d.ResolveDeliveryTargets(ctx, agentName)
	if err != nil {
		return nil, err
	}
	return d.prepareMessageForResolvedTargets(ctx, message, targets)
}

// PrepareAndSendTextBatch freezes one logical command and every current active
// direct binding as child receipts in one transaction, then starts each pending
// provider request. A command replay returns the frozen batch before consulting
// mutable bindings and therefore cannot add recipients or resend.
func (d Deps) PrepareAndSendTextBatch(
	ctx context.Context,
	agentName, objectKind, objectID, content string,
) (k12.DeliveryBatch, bool, error) {
	return d.prepareAndSendBatch(
		ctx, agentName, objectKind, objectID, DeliveryMessage{Content: content}, nil,
	)
}

// PrepareAndSendMessageBatch 冻结同一份正文与附件，为全部当前有效私聊目标创建子回执后发送。
func (d Deps) PrepareAndSendMessageBatch(
	ctx context.Context,
	agentName, objectKind, objectID string,
	message DeliveryMessage,
) (k12.DeliveryBatch, bool, error) {
	return d.prepareAndSendBatch(ctx, agentName, objectKind, objectID, message, nil)
}

func (d Deps) prepareAndSendTextBatchWithTargets(
	ctx context.Context,
	agentName, objectKind, objectID, content string,
	targets []ResolvedDeliveryTarget,
) (k12.DeliveryBatch, bool, error) {
	if len(targets) == 0 {
		return k12.DeliveryBatch{}, false, ErrNoActiveDirectBindings
	}
	return d.prepareAndSendBatch(
		ctx, agentName, objectKind, objectID, DeliveryMessage{Content: content}, targets,
	)
}

func (d Deps) prepareAndSendBatch(
	ctx context.Context,
	agentName, objectKind, objectID string,
	message DeliveryMessage,
	targets []ResolvedDeliveryTarget,
) (k12.DeliveryBatch, bool, error) {
	agentName = strings.TrimSpace(agentName)
	objectKind = strings.TrimSpace(objectKind)
	objectID = strings.TrimSpace(objectID)
	var err error
	message, err = normalizeDeliveryMessage(message)
	if err != nil {
		return k12.DeliveryBatch{}, false, err
	}
	if agentName == "" || objectKind == "" || objectID == "" {
		return k12.DeliveryBatch{}, false, fmt.Errorf("%w: agent/object/content required", ErrInvalidInput)
	}
	if d.Records == nil || d.Delivery == nil {
		return k12.DeliveryBatch{}, false, ErrDeliveryUnavailable
	}
	contentDigest := deliveryMessageDigest(message)
	dedupeKey := deliveryBatchDedupeKey(agentName, objectKind, objectID, contentDigest)
	existing, err := d.Records.GetDeliveryBatchByDedupe(ctx, agentName, dedupeKey)
	if err == nil {
		existing, err = d.sendDeliveryBatch(ctx, existing)
		return existing, false, err
	}
	if !errors.Is(err, records.ErrNotFound) {
		return k12.DeliveryBatch{}, false, err
	}
	batch, err := d.buildPreparedMessageBatch(
		ctx, agentName, objectKind, objectID, message, targets,
	)
	if err != nil {
		return k12.DeliveryBatch{}, false, err
	}
	batch, created, err := d.Records.PrepareDeliveryBatch(ctx, batch)
	if err != nil {
		return batch, created, err
	}
	if !created {
		batch, err = d.sendDeliveryBatch(ctx, batch)
		return batch, false, err
	}
	batch, err = d.sendDeliveryBatch(ctx, batch)
	return batch, true, err
}

// buildPreparedTextBatch freezes the target-specific payloads and constructs
// the durable root/children value without writing the database or calling a
// provider. Aggregate transactions may safely invoke it before their commit.
func (d Deps) buildPreparedTextBatch(
	ctx context.Context,
	agentName, objectKind, objectID, content string,
	targets []ResolvedDeliveryTarget,
) (k12.DeliveryBatch, error) {
	return d.buildPreparedMessageBatch(
		ctx, agentName, objectKind, objectID, DeliveryMessage{Content: content}, targets,
	)
}

func (d Deps) buildPreparedMessageBatch(
	ctx context.Context,
	agentName, objectKind, objectID string,
	message DeliveryMessage,
	targets []ResolvedDeliveryTarget,
) (k12.DeliveryBatch, error) {
	agentName = strings.TrimSpace(agentName)
	objectKind = strings.TrimSpace(objectKind)
	objectID = strings.TrimSpace(objectID)
	var err error
	message, err = normalizeDeliveryMessage(message)
	if err != nil {
		return k12.DeliveryBatch{}, err
	}
	if agentName == "" || objectKind == "" || objectID == "" {
		return k12.DeliveryBatch{}, fmt.Errorf(
			"%w: agent/object/content required", ErrInvalidInput,
		)
	}
	if d.Delivery == nil {
		return k12.DeliveryBatch{}, ErrDeliveryUnavailable
	}
	var prepared []PreparedTextDelivery
	if targets == nil {
		if len(message.Attachments) == 0 {
			prepared, err = d.resolveAndPrepareTextBatch(ctx, agentName, message.Content)
		} else {
			prepared, err = d.resolveAndPrepareMessageBatch(ctx, agentName, message)
		}
	} else if len(message.Attachments) == 0 {
		prepared, err = d.prepareTextForResolvedTargets(ctx, message.Content, targets)
	} else {
		prepared, err = d.prepareMessageForResolvedTargets(ctx, message, targets)
	}
	if err != nil {
		return k12.DeliveryBatch{}, err
	}
	if len(prepared) == 0 {
		return k12.DeliveryBatch{}, ErrNoActiveDirectBindings
	}
	contentDigest := deliveryMessageDigest(message)
	dedupeKey := deliveryBatchDedupeKey(agentName, objectKind, objectID, contentDigest)
	batchID := idgen.NanoID()
	receipts := make([]k12.DeliveryReceipt, 0, len(prepared))
	for i, item := range prepared {
		payloadDigest := deliveryDigest(item.PayloadJSON)
		receipts = append(receipts, k12.DeliveryReceipt{
			DeliveryID:    idgen.NanoID(),
			BatchID:       batchID,
			BatchOrdinal:  i + 1,
			AgentName:     agentName,
			ObjectKind:    objectKind,
			ObjectID:      objectID,
			BindingID:     item.BindingID,
			Target:        item.Target,
			DedupeKey:     deliveryDedupeKey(agentName, objectKind, objectID, item.BindingID, payloadDigest),
			PayloadDigest: payloadDigest,
			PayloadJSON:   item.PayloadJSON,
			RenderJSON:    item.RenderJSON,
		})
	}
	return k12.DeliveryBatch{
		BatchID:       batchID,
		AgentName:     agentName,
		ObjectKind:    objectKind,
		ObjectID:      objectID,
		DedupeKey:     dedupeKey,
		ContentDigest: contentDigest,
		Receipts:      receipts,
	}, nil
}

// sendDeliveryBatch starts only children that are durably pending. It is
// intentionally separate from construction so callers cannot reach a provider
// until their complete transaction has committed.
func (d Deps) sendDeliveryBatch(
	ctx context.Context,
	batch k12.DeliveryBatch,
) (k12.DeliveryBatch, error) {
	for _, receipt := range batch.Receipts {
		if receipt.Status != k12.DeliveryPending {
			continue
		}
		if _, err := d.sendPreparedDelivery(ctx, receipt); err != nil {
			current, getErr := d.Records.GetDeliveryBatch(
				ctx, batch.AgentName, batch.BatchID,
			)
			if getErr != nil {
				return batch, getErr
			}
			return current, err
		}
	}
	batch, err := d.Records.GetDeliveryBatch(ctx, batch.AgentName, batch.BatchID)
	return batch, err
}

func (d Deps) GetDeliveryBatch(ctx context.Context, agentName, batchID string) (k12.DeliveryBatch, error) {
	if d.Records == nil {
		return k12.DeliveryBatch{}, ErrDeliveryUnavailable
	}
	return d.Records.GetDeliveryBatch(ctx, strings.TrimSpace(agentName), strings.TrimSpace(batchID))
}

// RetryDeliveryBatch retries only children with explicit failed evidence.
// Delivered, sending and outcome_unknown children are never sent here.
func (d Deps) RetryDeliveryBatch(ctx context.Context, agentName, batchID string) (k12.DeliveryBatch, error) {
	if d.Records == nil || d.Delivery == nil {
		return k12.DeliveryBatch{}, ErrDeliveryUnavailable
	}
	batch, err := d.GetDeliveryBatch(ctx, agentName, batchID)
	if err != nil {
		return k12.DeliveryBatch{}, err
	}
	for _, receipt := range batch.Receipts {
		if receipt.Status != k12.DeliveryFailed {
			continue
		}
		if _, sendErr := d.sendPreparedDelivery(ctx, receipt); sendErr != nil {
			current, getErr := d.GetDeliveryBatch(ctx, batch.AgentName, batch.BatchID)
			if getErr != nil {
				return k12.DeliveryBatch{}, getErr
			}
			return current, sendErr
		}
	}
	return d.GetDeliveryBatch(ctx, batch.AgentName, batch.BatchID)
}

// QueryDeliveryBatch queries only in-flight or outcome_unknown children.
// It never calls SendPrepared and terminal children remain untouched.
func (d Deps) QueryDeliveryBatch(ctx context.Context, agentName, batchID string) (k12.DeliveryBatch, error) {
	if d.Records == nil || d.Delivery == nil {
		return k12.DeliveryBatch{}, ErrDeliveryUnavailable
	}
	batch, err := d.GetDeliveryBatch(ctx, agentName, batchID)
	if err != nil {
		return k12.DeliveryBatch{}, err
	}
	for _, receipt := range batch.Receipts {
		if receipt.Status != k12.DeliverySending && receipt.Status != k12.DeliveryOutcomeUnknown {
			continue
		}
		if _, queryErr := d.QueryDeliveryReceipt(ctx, receipt.AgentName, receipt.DeliveryID); queryErr != nil {
			current, getErr := d.GetDeliveryBatch(ctx, batch.AgentName, batch.BatchID)
			if getErr != nil {
				return k12.DeliveryBatch{}, getErr
			}
			return current, queryErr
		}
	}
	return d.GetDeliveryBatch(ctx, batch.AgentName, batch.BatchID)
}

// PrepareAndSendText freezes target/payload/render evidence, creates the
// durable Receipt, then starts the provider request. Replaying the same
// object/payload/binding returns the existing Receipt without another send.
func (d Deps) PrepareAndSendText(ctx context.Context, agentName, objectKind, objectID, content string) (k12.DeliveryReceipt, bool, error) {
	agentName = strings.TrimSpace(agentName)
	objectKind = strings.TrimSpace(objectKind)
	objectID = strings.TrimSpace(objectID)
	content = strings.TrimSpace(content)
	if agentName == "" || objectKind == "" || objectID == "" || content == "" {
		return k12.DeliveryReceipt{}, false, fmt.Errorf("%w: agent/object/content required", ErrInvalidInput)
	}
	if d.Records == nil || d.Delivery == nil {
		return k12.DeliveryReceipt{}, false, ErrDeliveryUnavailable
	}
	prepared, err := d.Delivery.PrepareText(ctx, agentName, content)
	if err != nil {
		return k12.DeliveryReceipt{}, false, err
	}
	if err := validatePreparedDelivery(prepared); err != nil {
		return k12.DeliveryReceipt{}, false, err
	}
	payloadDigest := deliveryDigest(prepared.PayloadJSON)
	receipt, created, err := d.Records.PrepareDeliveryReceipt(ctx, k12.DeliveryReceipt{
		DeliveryID:    idgen.NanoID(),
		AgentName:     agentName,
		ObjectKind:    objectKind,
		ObjectID:      objectID,
		BindingID:     prepared.BindingID,
		Target:        prepared.Target,
		DedupeKey:     deliveryDedupeKey(agentName, objectKind, objectID, prepared.BindingID, payloadDigest),
		PayloadDigest: payloadDigest,
		PayloadJSON:   prepared.PayloadJSON,
		RenderJSON:    prepared.RenderJSON,
	})
	if err != nil || !created {
		return receipt, created, err
	}
	receipt, err = d.sendPreparedDelivery(ctx, receipt)
	return receipt, true, err
}

func (d Deps) GetDeliveryReceipt(ctx context.Context, agentName, deliveryID string) (k12.DeliveryReceipt, error) {
	if d.Records == nil {
		return k12.DeliveryReceipt{}, ErrDeliveryUnavailable
	}
	return d.Records.GetDeliveryReceipt(ctx, strings.TrimSpace(agentName), strings.TrimSpace(deliveryID))
}

// RetryDeliveryReceipt is intentionally legal only from failed. In-flight and
// outcome_unknown receipts must be queried so a lost response cannot duplicate
// a message.
func (d Deps) RetryDeliveryReceipt(ctx context.Context, agentName, deliveryID string) (k12.DeliveryReceipt, error) {
	if d.Records == nil || d.Delivery == nil {
		return k12.DeliveryReceipt{}, ErrDeliveryUnavailable
	}
	receipt, err := d.Records.GetDeliveryReceipt(ctx, strings.TrimSpace(agentName), strings.TrimSpace(deliveryID))
	if err != nil {
		return k12.DeliveryReceipt{}, err
	}
	if receipt.Status != k12.DeliveryFailed {
		return receipt, fmt.Errorf("%w: 只有明确失败的消息可以重试；当前状态 %s 请查询结果", records.ErrIllegalTransition, receipt.Status)
	}
	return d.sendPreparedDelivery(ctx, receipt)
}

func (d Deps) sendPreparedDelivery(ctx context.Context, receipt k12.DeliveryReceipt) (k12.DeliveryReceipt, error) {
	started, began, err := d.Records.BeginDeliveryAttempt(ctx, receipt.AgentName, receipt.DeliveryID)
	if err != nil {
		return receipt, err
	}
	if !began {
		return started, nil
	}
	ack, sendErr := d.Delivery.SendPrepared(ctx, started)
	return d.persistDeliverySendOutcome(context.WithoutCancel(ctx), started, ack, sendErr)
}

func deliveryFailureDetail(ack DeliveryTransportAck, err error, fallback string) string {
	if detail := strings.TrimSpace(ack.Detail); detail != "" {
		return detail
	}
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		return err.Error()
	}
	return fallback
}

func (d Deps) persistDeliverySendOutcome(ctx context.Context, receipt k12.DeliveryReceipt, ack DeliveryTransportAck, sendErr error) (k12.DeliveryReceipt, error) {
	if id := strings.TrimSpace(ack.ExternalMessageID); id != "" {
		accepted, err := d.Records.MarkDeliveryAccepted(ctx, receipt.AgentName, receipt.DeliveryID, id)
		if err != nil {
			return receipt, err
		}
		receipt = accepted
	}
	status := ack.Status
	if status == "" {
		if errors.Is(sendErr, context.Canceled) || errors.Is(sendErr, context.DeadlineExceeded) {
			status = k12.DeliveryOutcomeUnknown
		} else {
			status = k12.DeliveryFailed
		}
	}
	switch status {
	case k12.DeliverySending:
		if receipt.ExternalMessageID == "" {
			return d.Records.MarkDeliveryOutcomeUnknown(ctx, receipt.AgentName, receipt.DeliveryID,
				deliveryFailureDetail(ack, sendErr, "平台已受理但没有返回可查询编号"))
		}
		return receipt, nil
	case k12.DeliveryDelivered:
		if receipt.ExternalMessageID == "" {
			return d.Records.MarkDeliveryOutcomeUnknown(ctx, receipt.AgentName, receipt.DeliveryID,
				"平台声称送达但没有返回可核验编号")
		}
		return d.Records.MarkDeliveryDelivered(ctx, receipt.AgentName, receipt.DeliveryID)
	case k12.DeliveryFailed:
		return d.Records.MarkDeliveryFailed(ctx, receipt.AgentName, receipt.DeliveryID,
			deliveryFailureDetail(ack, sendErr, "平台明确拒绝本次发送"))
	case k12.DeliveryOutcomeUnknown:
		return d.Records.MarkDeliveryOutcomeUnknown(ctx, receipt.AgentName, receipt.DeliveryID,
			deliveryFailureDetail(ack, sendErr, "发送结果未知，请查询后再决定"))
	default:
		return d.Records.MarkDeliveryOutcomeUnknown(ctx, receipt.AgentName, receipt.DeliveryID,
			fmt.Sprintf("平台返回未知投递状态 %q", status))
	}
}

// QueryDeliveryReceipt is the sole convergence path for sending and
// outcome_unknown. It never calls SendPrepared.
func (d Deps) QueryDeliveryReceipt(ctx context.Context, agentName, deliveryID string) (k12.DeliveryReceipt, error) {
	if d.Records == nil || d.Delivery == nil {
		return k12.DeliveryReceipt{}, ErrDeliveryUnavailable
	}
	receipt, err := d.Records.GetDeliveryReceipt(ctx, strings.TrimSpace(agentName), strings.TrimSpace(deliveryID))
	if err != nil {
		return k12.DeliveryReceipt{}, err
	}
	if receipt.Status == k12.DeliveryDelivered || receipt.Status == k12.DeliveryFailed {
		return receipt, nil
	}
	if receipt.Status != k12.DeliverySending && receipt.Status != k12.DeliveryOutcomeUnknown {
		return receipt, fmt.Errorf("%w: 状态 %s 不需要查询", records.ErrIllegalTransition, receipt.Status)
	}
	if strings.TrimSpace(receipt.ExternalMessageID) == "" {
		return receipt, fmt.Errorf("%w: 平台没有返回查询编号，禁止盲目重发", ErrDeliveryQueryUnavailable)
	}
	ack, queryErr := d.Delivery.QueryPrepared(ctx, receipt)
	if ack.ExternalMessageID == "" {
		ack.ExternalMessageID = receipt.ExternalMessageID
	}
	if queryErr != nil || ack.Status == k12.DeliveryOutcomeUnknown || ack.Status == "" {
		if receipt.Status == k12.DeliverySending {
			return d.Records.MarkDeliveryOutcomeUnknown(ctx, receipt.AgentName, receipt.DeliveryID,
				deliveryFailureDetail(ack, queryErr, "暂时查不到送达结果，请稍后再查"))
		}
		return receipt, nil
	}
	switch ack.Status {
	case k12.DeliverySending:
		return d.Records.ReconcileDeliveryReceipt(ctx, receipt.AgentName, receipt.DeliveryID,
			k12.DeliverySending, ack.ExternalMessageID, "")
	case k12.DeliveryDelivered:
		return d.Records.ReconcileDeliveryReceipt(ctx, receipt.AgentName, receipt.DeliveryID,
			k12.DeliveryDelivered, ack.ExternalMessageID, "")
	case k12.DeliveryFailed:
		return d.Records.ReconcileDeliveryReceipt(ctx, receipt.AgentName, receipt.DeliveryID,
			k12.DeliveryFailed, ack.ExternalMessageID,
			deliveryFailureDetail(ack, queryErr, "平台确认发送失败"))
	default:
		return receipt, fmt.Errorf("%w: 未知查询状态 %q", ErrInvalidInput, ack.Status)
	}
}

// RecoverDeliveryReceipts resumes one owner's durable ledger after process
// reconstruction. Pending was never attempted and can be sent once; sending or
// unknown is query-only. A sending row without external evidence becomes
// outcome_unknown instead of risking a duplicate.
func (d Deps) RecoverDeliveryReceipts(ctx context.Context, agentName string) (int, error) {
	if d.Records == nil || d.Delivery == nil {
		return 0, ErrDeliveryUnavailable
	}
	receipts, err := d.Records.ListRecoverableDeliveryReceipts(ctx, strings.TrimSpace(agentName))
	if err != nil {
		return 0, err
	}
	processed := 0
	for _, receipt := range receipts {
		select {
		case <-ctx.Done():
			return processed, ctx.Err()
		default:
		}
		switch receipt.Status {
		case k12.DeliveryPending:
			_, err = d.sendPreparedDelivery(ctx, receipt)
		case k12.DeliverySending:
			if receipt.ExternalMessageID == "" {
				_, err = d.Records.MarkDeliveryOutcomeUnknown(ctx, receipt.AgentName, receipt.DeliveryID,
					"应用重启时该次发送没有可查询编号，禁止盲目重发")
			} else {
				_, err = d.QueryDeliveryReceipt(ctx, receipt.AgentName, receipt.DeliveryID)
			}
		case k12.DeliveryOutcomeUnknown:
			if receipt.ExternalMessageID != "" {
				_, err = d.QueryDeliveryReceipt(ctx, receipt.AgentName, receipt.DeliveryID)
			}
		}
		if err != nil && !errors.Is(err, ErrDeliveryQueryUnavailable) {
			return processed, err
		}
		processed++
	}
	return processed, nil
}
