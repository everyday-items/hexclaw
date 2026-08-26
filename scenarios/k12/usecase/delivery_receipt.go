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

	"github.com/hexagon-codes/hexclaw/messagecontent"
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

func deliveryPartDedupeKey(
	agentName, objectKind, objectID, bindingID string,
	partKind messagecontent.PartKind,
	partMIME string,
	partOrdinal int,
	partDigest, payloadDigest string,
) string {
	return deliveryDigest(strings.Join([]string{
		agentName, objectKind, objectID, bindingID,
		string(partKind), partMIME, fmt.Sprintf("%d", partOrdinal), partDigest, payloadDigest,
	}, "\x00"))
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

func normalizePreparedDelivery(prepared PreparedTextDelivery) PreparedTextDelivery {
	prepared.BindingID = strings.TrimSpace(prepared.BindingID)
	prepared.Target.Platform = strings.ToLower(strings.TrimSpace(prepared.Target.Platform))
	prepared.Target.InstanceID = strings.TrimSpace(prepared.Target.InstanceID)
	prepared.Target.ChatID = strings.TrimSpace(prepared.Target.ChatID)
	prepared.Target.Label = strings.TrimSpace(prepared.Target.Label)
	if prepared.PartKind == "" {
		prepared.PartKind = messagecontent.PartMarkdown
	}
	prepared.PartMIME = strings.ToLower(strings.TrimSpace(prepared.PartMIME))
	if prepared.PartOrdinal == 0 {
		prepared.PartOrdinal = 1
	}
	prepared.PartDigest = strings.ToLower(strings.TrimSpace(prepared.PartDigest))
	prepared.PayloadJSON = strings.TrimSpace(prepared.PayloadJSON)
	prepared.RenderJSON = strings.TrimSpace(prepared.RenderJSON)
	return prepared
}

func validDeliveryDigest(value string) bool {
	raw, ok := strings.CutPrefix(value, "sha256:")
	decoded, err := hex.DecodeString(raw)
	return ok && err == nil && len(decoded) == sha256.Size
}

func validatePreparedDelivery(prepared PreparedTextDelivery) error {
	if prepared.BindingID == "" || prepared.Target.Platform == "" ||
		prepared.Target.ChatID == "" || prepared.PayloadJSON == "" {
		return fmt.Errorf("%w: prepared delivery target or payload is incomplete", ErrInvalidInput)
	}
	if prepared.PartOrdinal < 1 || !validDeliveryDigest(prepared.PartDigest) {
		return fmt.Errorf("%w: prepared delivery part identity is incomplete", ErrInvalidInput)
	}
	switch prepared.PartKind {
	case messagecontent.PartMarkdown:
		if prepared.PartMIME != "" {
			return fmt.Errorf("%w: markdown delivery part cannot declare MIME", ErrInvalidInput)
		}
	case messagecontent.PartArtifact:
		if !strings.HasPrefix(prepared.PartMIME, "image/") && prepared.PartMIME != "application/pdf" {
			return fmt.Errorf("%w: unsupported delivery artifact MIME %q", ErrInvalidInput, prepared.PartMIME)
		}
	default:
		return fmt.Errorf("%w: unsupported delivery part kind %q", ErrInvalidInput, prepared.PartKind)
	}
	if !json.Valid([]byte(prepared.PayloadJSON)) {
		return fmt.Errorf("%w: prepared delivery payload is not JSON", ErrInvalidInput)
	}
	if prepared.RenderJSON != "" && !json.Valid([]byte(prepared.RenderJSON)) {
		return fmt.Errorf("%w: prepared delivery render evidence is not JSON", ErrInvalidInput)
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
	partDigest := deliveryDigest(strings.TrimSpace(content))
	for i := range prepared {
		prepared[i] = normalizePreparedDelivery(prepared[i])
		if prepared[i].PartDigest == "" {
			prepared[i].PartDigest = partDigest
		}
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
		if prepared[i].PartKind != messagecontent.PartMarkdown || prepared[i].PartMIME != "" ||
			prepared[i].PartOrdinal != 1 || prepared[i].PartDigest != partDigest {
			return nil, fmt.Errorf("%w: prepared text delivery changed its canonical part identity", ErrInvalidInput)
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
	partsPerTarget := len(message.Attachments) + 1
	if len(prepared) != len(targets)*partsPerTarget {
		return nil, fmt.Errorf(
			"%w: prepared deliveries=%d resolved target-parts=%d",
			ErrInvalidInput, len(prepared), len(targets)*partsPerTarget,
		)
	}
	for i := range prepared {
		prepared[i] = normalizePreparedDelivery(prepared[i])
		targetIndex := i / partsPerTarget
		partIndex := i % partsPerTarget
		expectedKind := messagecontent.PartMarkdown
		expectedMIME := ""
		expectedDigest := deliveryDigest(message.Content)
		if partIndex > 0 {
			attachment := message.Attachments[partIndex-1]
			expectedKind = messagecontent.PartArtifact
			expectedMIME = attachment.MIME
			expectedDigest = deliveryDigest(string(attachment.Data))
		}
		if err := validatePreparedDelivery(prepared[i]); err != nil {
			return nil, fmt.Errorf("prepared delivery %d: %w", i+1, err)
		}
		if prepared[i].BindingID != targets[targetIndex].BindingID ||
			prepared[i].Target != targets[targetIndex].Target {
			return nil, fmt.Errorf(
				"%w: prepared delivery %d changed its resolved binding target",
				ErrInvalidInput, i+1,
			)
		}
		if prepared[i].PartKind != expectedKind || prepared[i].PartMIME != expectedMIME ||
			prepared[i].PartOrdinal != partIndex+1 || prepared[i].PartDigest != expectedDigest {
			return nil, fmt.Errorf(
				"%w: prepared delivery %d changed its canonical part identity",
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
	prepared = normalizePreparedDelivery(prepared)
	if prepared.PartDigest == "" {
		prepared.PartDigest = deliveryDigest(strings.TrimSpace(content))
	}
	if err := validatePreparedDelivery(prepared); err != nil {
		return nil, err
	}
	if prepared.PartKind != messagecontent.PartMarkdown || prepared.PartMIME != "" || prepared.PartOrdinal != 1 {
		return nil, fmt.Errorf("%w: prepared text delivery changed its canonical part identity", ErrInvalidInput)
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

// PrepareAndSendMessageBatchForTargets 使用调用方已经耐久冻结的私聊目标 exact-set。
// 入站回复不得重新解析当前绑定，否则绑定变化会把结果发给不同的会话。
func (d Deps) PrepareAndSendMessageBatchForTargets(
	ctx context.Context,
	agentName, objectKind, objectID string,
	message DeliveryMessage,
	targets []ResolvedDeliveryTarget,
) (k12.DeliveryBatch, bool, error) {
	return d.prepareAndSendBatch(ctx, agentName, objectKind, objectID, message, targets)
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
			DeliveryID:   idgen.NanoID(),
			BatchID:      batchID,
			BatchOrdinal: i + 1,
			PartKind:     item.PartKind,
			PartMIME:     item.PartMIME,
			PartOrdinal:  item.PartOrdinal,
			PartDigest:   item.PartDigest,
			AgentName:    agentName,
			ObjectKind:   objectKind,
			ObjectID:     objectID,
			BindingID:    item.BindingID,
			Target:       item.Target,
			DedupeKey: deliveryPartDedupeKey(
				agentName, objectKind, objectID, item.BindingID,
				item.PartKind, item.PartMIME, item.PartOrdinal, item.PartDigest, payloadDigest,
			),
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

// prepareDeliveryBatchResources 先准备并持久化全部媒体 part；只要仍有一个媒体
// 资源缺失，整批可见消息发送次数必须保持为零。
func (d Deps) prepareDeliveryBatchResources(
	ctx context.Context,
	batch k12.DeliveryBatch,
) (k12.DeliveryBatch, error) {
	hasArtifact := false
	for _, receipt := range batch.Receipts {
		if receipt.PartKind == messagecontent.PartArtifact {
			hasArtifact = true
			break
		}
	}
	if !hasArtifact {
		return batch, nil
	}
	preparer, canPrepare := d.Delivery.(DeliveryPartResourcePreparer)
	persistCtx := context.WithoutCancel(ctx)
	failures := make([]string, 0)
	for _, receipt := range batch.Receipts {
		if receipt.PartKind != messagecontent.PartArtifact || receipt.PreparedResourceID != "" {
			continue
		}
		if receipt.Status != k12.DeliveryPending && receipt.Status != k12.DeliveryFailed {
			failures = append(failures, fmt.Sprintf("part %d already crossed the send boundary", receipt.BatchOrdinal))
			continue
		}
		if !canPrepare {
			failures = append(failures, fmt.Sprintf("part %d media preparation is unavailable", receipt.BatchOrdinal))
			continue
		}
		resourceID, err := preparer.PrepareDeliveryPartResource(ctx, receipt)
		resourceID = strings.TrimSpace(resourceID)
		if err != nil || resourceID == "" {
			detail := "media preparation returned no resource id"
			if err != nil {
				detail = err.Error()
			}
			failures = append(failures, fmt.Sprintf("part %d: %s", receipt.BatchOrdinal, detail))
			continue
		}
		if _, err := d.Records.SaveDeliveryPreparedResource(
			persistCtx, receipt.AgentName, receipt.DeliveryID, resourceID,
		); err != nil {
			failures = append(failures, fmt.Sprintf("part %d: %s", receipt.BatchOrdinal, err))
		}
	}
	detail := "delivery media preflight is incomplete"
	if len(failures) > 0 {
		detail += ": " + strings.Join(failures, "; ")
	}
	current, incomplete, err := d.Records.FailDeliveryBatchPreparationIfIncomplete(
		persistCtx, batch.AgentName, batch.BatchID, detail,
	)
	if err != nil {
		return current, err
	}
	if incomplete {
		return current, errors.New(detail)
	}
	return current, nil
}

// sendDeliveryBatch starts only children that are durably pending. It is
// intentionally separate from construction so callers cannot reach a provider
// until their complete transaction has committed.
func (d Deps) sendDeliveryBatch(
	ctx context.Context,
	batch k12.DeliveryBatch,
) (k12.DeliveryBatch, error) {
	var err error
	if _, _, err = creativeDeliveryEnvelopeGroups(batch); err != nil {
		return batch, err
	}
	batch, err = d.prepareDeliveryBatchResources(ctx, batch)
	if err != nil {
		return batch, err
	}
	groups, groupedIDs, err := creativeDeliveryEnvelopeGroups(batch)
	if err != nil {
		return batch, err
	}
	for _, group := range groups {
		if group[0].Status != k12.DeliveryPending {
			continue
		}
		if _, sendErr := d.sendPreparedDeliveryEnvelope(ctx, batch, group); sendErr != nil {
			current, getErr := d.Records.GetDeliveryBatch(
				ctx, batch.AgentName, batch.BatchID,
			)
			if getErr != nil {
				return batch, getErr
			}
			return current, sendErr
		}
	}
	for _, receipt := range batch.Receipts {
		if _, grouped := groupedIDs[receipt.DeliveryID]; grouped {
			continue
		}
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
	batch, err = d.Records.GetDeliveryBatch(ctx, batch.AgentName, batch.BatchID)
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
	if _, _, err = creativeDeliveryEnvelopeGroups(batch); err != nil {
		return batch, err
	}
	batch, err = d.prepareDeliveryBatchResources(ctx, batch)
	if err != nil {
		return batch, err
	}
	groups, groupedIDs, err := creativeDeliveryEnvelopeGroups(batch)
	if err != nil {
		return batch, err
	}
	for _, group := range groups {
		if group[0].Status != k12.DeliveryFailed {
			continue
		}
		if _, sendErr := d.sendPreparedDeliveryEnvelope(ctx, batch, group); sendErr != nil {
			current, getErr := d.GetDeliveryBatch(ctx, batch.AgentName, batch.BatchID)
			if getErr != nil {
				return k12.DeliveryBatch{}, getErr
			}
			return current, sendErr
		}
	}
	for _, receipt := range batch.Receipts {
		if _, grouped := groupedIDs[receipt.DeliveryID]; grouped {
			continue
		}
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
	groups, groupedIDs, err := creativeDeliveryEnvelopeGroups(batch)
	if err != nil {
		return batch, err
	}
	for _, group := range groups {
		if group[0].Status != k12.DeliverySending && group[0].Status != k12.DeliveryOutcomeUnknown {
			continue
		}
		if _, queryErr := d.queryPreparedDeliveryEnvelope(ctx, group); queryErr != nil {
			current, getErr := d.GetDeliveryBatch(ctx, batch.AgentName, batch.BatchID)
			if getErr != nil {
				return k12.DeliveryBatch{}, getErr
			}
			return current, queryErr
		}
	}
	for _, receipt := range batch.Receipts {
		if _, grouped := groupedIDs[receipt.DeliveryID]; grouped {
			continue
		}
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
	prepared = normalizePreparedDelivery(prepared)
	if prepared.PartDigest == "" {
		prepared.PartDigest = deliveryDigest(content)
	}
	if err := validatePreparedDelivery(prepared); err != nil {
		return k12.DeliveryReceipt{}, false, err
	}
	if prepared.PartKind != messagecontent.PartMarkdown || prepared.PartMIME != "" || prepared.PartOrdinal != 1 {
		return k12.DeliveryReceipt{}, false, fmt.Errorf("%w: prepared text delivery changed its canonical part identity", ErrInvalidInput)
	}
	payloadDigest := deliveryDigest(prepared.PayloadJSON)
	receipt, created, err := d.Records.PrepareDeliveryReceipt(ctx, k12.DeliveryReceipt{
		DeliveryID:    idgen.NanoID(),
		PartKind:      prepared.PartKind,
		PartMIME:      prepared.PartMIME,
		PartOrdinal:   prepared.PartOrdinal,
		PartDigest:    prepared.PartDigest,
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
	if receipt.BatchID != "" {
		batch, retryErr := d.RetryDeliveryBatch(ctx, receipt.AgentName, receipt.BatchID)
		if retryErr != nil {
			return receipt, retryErr
		}
		for _, child := range batch.Receipts {
			if child.DeliveryID == receipt.DeliveryID {
				return child, nil
			}
		}
		return receipt, records.ErrNotFound
	}
	return d.sendPreparedDelivery(ctx, receipt)
}

func deliveryEnvelopeTargetKey(receipt k12.DeliveryReceipt) string {
	return strings.Join([]string{
		receipt.BindingID,
		strings.ToLower(strings.TrimSpace(receipt.Target.Platform)),
		strings.TrimSpace(receipt.Target.InstanceID),
		strings.TrimSpace(receipt.Target.ChatID),
	}, "\x00")
}

type creativeEnvelopeArtifactExpectation struct {
	mime    string
	digest  string
	ordinal int
	image   bool
}

// creativeEnvelopeArtifactExpectations 从 Markdown 回执的完整 canonical 证据中
// 恢复应存在的连续图片前缀与尾随 PDF。旧版非 canonical 载荷返回 known=false。
func creativeEnvelopeArtifactExpectations(
	receipt k12.DeliveryReceipt,
) (expected []creativeEnvelopeArtifactExpectation, known bool, err error) {
	var manifest messagecontent.RenderManifest
	if json.Unmarshal([]byte(receipt.RenderJSON), &manifest) != nil {
		return nil, false, nil
	}
	var payload struct {
		MessageContent *messagecontent.MessageContent `json:"message_content"`
		LegacyContent  *messagecontent.MessageContent `json:"Content"`
	}
	if json.Unmarshal([]byte(receipt.PayloadJSON), &payload) != nil {
		return nil, false, nil
	}
	content := payload.MessageContent
	if content == nil {
		content = payload.LegacyContent
	}
	if content == nil {
		return nil, false, nil
	}
	if err := manifest.ValidateFor(*content); err != nil {
		return nil, true, fmt.Errorf("%w: invalid creative delivery canonical evidence: %v", records.ErrIllegalTransition, err)
	}
	if len(manifest.Parts) != len(content.Attachments)+1 ||
		manifest.Parts[0].Kind != messagecontent.PartMarkdown {
		return nil, true, fmt.Errorf("%w: creative delivery canonical artifact projection is incomplete", records.ErrIllegalTransition)
	}
	seenPDF := false
	for i, attachment := range content.Attachments {
		renderPart := manifest.Parts[i+1]
		if renderPart.Kind != messagecontent.PartArtifact ||
			renderPart.ArtifactRef != attachment.AssetID ||
			renderPart.ArtifactDigest != attachment.Digest ||
			renderPart.AltText != attachment.AltText {
			return nil, true, fmt.Errorf("%w: creative delivery canonical artifact identity diverged", records.ErrIllegalTransition)
		}
		mime := strings.ToLower(strings.TrimSpace(attachment.MIME))
		switch {
		case strings.HasPrefix(mime, "image/") && mime != "image/":
			if seenPDF {
				return nil, true, fmt.Errorf("%w: creative delivery canonical images are not contiguous", records.ErrIllegalTransition)
			}
			expected = append(expected, creativeEnvelopeArtifactExpectation{
				mime: mime, digest: attachment.Digest, ordinal: i + 2, image: true,
			})
		case mime == "application/pdf":
			seenPDF = true
			expected = append(expected, creativeEnvelopeArtifactExpectation{
				mime: mime, digest: attachment.Digest, ordinal: i + 2,
			})
		default:
			return nil, true, fmt.Errorf("%w: creative delivery canonical artifact MIME is unsupported", records.ErrIllegalTransition)
		}
	}
	return expected, true, nil
}

type creativeDeliveryPayloadEvidence struct {
	Kind           messagecontent.PartKind        `json:"kind"`
	MIME           string                         `json:"mime,omitempty"`
	Ordinal        int                            `json:"ordinal"`
	Digest         string                         `json:"digest"`
	Text           string                         `json:"text,omitempty"`
	Attachment     *creativeDeliveryAttachment    `json:"attachment,omitempty"`
	MessageContent *messagecontent.MessageContent `json:"message_content"`
	RenderManifest *messagecontent.RenderManifest `json:"render_manifest"`
}

type creativeDeliveryAttachment struct {
	Name string `json:"Name"`
	MIME string `json:"MIME"`
	Data []byte `json:"Data"`
}

func validateCreativeDeliveryTargetIntegrity(
	batch k12.DeliveryBatch,
	parts []k12.DeliveryReceipt,
) error {
	if len(parts) == 0 {
		return nil
	}
	first := parts[0]
	if strings.TrimSpace(first.BindingID) == "" ||
		strings.TrimSpace(first.Target.InstanceID) == "" || strings.TrimSpace(first.Target.ChatID) == "" {
		return fmt.Errorf("%w: creative delivery target identity is incomplete", records.ErrIllegalTransition)
	}
	payloads := make([]creativeDeliveryPayloadEvidence, len(parts))
	hasCanonicalPayload := false
	requiresCanonicalPayload := false
	for i, receipt := range parts {
		if receipt.AgentName != batch.AgentName || receipt.ObjectKind != batch.ObjectKind ||
			receipt.ObjectID != batch.ObjectID || receipt.BatchID != batch.BatchID {
			return fmt.Errorf("%w: creative delivery component root identity diverged", records.ErrIllegalTransition)
		}
		if receipt.BindingID != first.BindingID || receipt.Target != first.Target {
			return fmt.Errorf("%w: creative delivery component target identity diverged", records.ErrIllegalTransition)
		}
		if receipt.BatchOrdinal != first.BatchOrdinal+i || receipt.PartOrdinal != i+1 {
			return fmt.Errorf("%w: creative delivery component order diverged", records.ErrIllegalTransition)
		}
		if receipt.PartKind == messagecontent.PartMarkdown && strings.TrimSpace(receipt.PreparedResourceID) != "" {
			return fmt.Errorf("%w: creative delivery Markdown has an unexpected prepared resource", records.ErrIllegalTransition)
		}
		if receipt.PayloadDigest != deliveryDigest(receipt.PayloadJSON) {
			return fmt.Errorf("%w: creative delivery component payload digest diverged", records.ErrIllegalTransition)
		}
		if receipt.RenderJSON != first.RenderJSON {
			return fmt.Errorf("%w: creative delivery component render evidence diverged", records.ErrIllegalTransition)
		}
		if err := json.Unmarshal([]byte(receipt.PayloadJSON), &payloads[i]); err != nil {
			return fmt.Errorf("%w: creative delivery component payload is invalid", records.ErrIllegalTransition)
		}
		if payloads[i].MessageContent != nil || payloads[i].RenderManifest != nil {
			hasCanonicalPayload = true
		}
		mime := strings.ToLower(strings.TrimSpace(receipt.PartMIME))
		if receipt.PartKind == messagecontent.PartArtifact && strings.HasPrefix(mime, "image/") && mime != "image/" {
			requiresCanonicalPayload = true
		}
	}
	if !hasCanonicalPayload {
		if requiresCanonicalPayload {
			return fmt.Errorf("%w: creative delivery envelope requires canonical payload evidence", records.ErrIllegalTransition)
		}
		return nil
	}
	firstPayload := payloads[0]
	if firstPayload.MessageContent == nil || firstPayload.RenderManifest == nil {
		return fmt.Errorf("%w: creative delivery canonical payload is incomplete", records.ErrIllegalTransition)
	}
	attachmentIdentities := make([]DeliveryAttachmentIdentity, 0, len(firstPayload.MessageContent.Attachments))
	for _, attachment := range firstPayload.MessageContent.Attachments {
		attachmentIdentities = append(attachmentIdentities, DeliveryAttachmentIdentity{
			Name:          attachment.Name,
			MIME:          attachment.MIME,
			ContentDigest: attachment.Digest,
		})
	}
	contentDigest, err := deliveryMessageIdentityDigest(
		firstPayload.MessageContent.Markdown,
		attachmentIdentities,
	)
	if err != nil || batch.ContentDigest != contentDigest ||
		batch.DedupeKey != deliveryBatchDedupeKey(
			batch.AgentName,
			batch.ObjectKind,
			batch.ObjectID,
			contentDigest,
		) {
		return fmt.Errorf("%w: creative delivery batch root identity diverged", records.ErrIllegalTransition)
	}
	firstContentJSON, err := json.Marshal(firstPayload.MessageContent)
	if err != nil {
		return fmt.Errorf("%w: creative delivery canonical content is invalid", records.ErrIllegalTransition)
	}
	firstManifestJSON, err := json.Marshal(firstPayload.RenderManifest)
	if err != nil {
		return fmt.Errorf("%w: creative delivery canonical render evidence is invalid", records.ErrIllegalTransition)
	}
	for i, receipt := range parts {
		payload := payloads[i]
		if receipt.DedupeKey != deliveryPartDedupeKey(
			receipt.AgentName,
			receipt.ObjectKind,
			receipt.ObjectID,
			receipt.BindingID,
			receipt.PartKind,
			receipt.PartMIME,
			receipt.PartOrdinal,
			receipt.PartDigest,
			receipt.PayloadDigest,
		) {
			return fmt.Errorf("%w: creative delivery component dedupe identity diverged", records.ErrIllegalTransition)
		}
		if payload.MessageContent == nil || payload.RenderManifest == nil {
			return fmt.Errorf("%w: creative delivery canonical payload is incomplete", records.ErrIllegalTransition)
		}
		if payload.Kind != receipt.PartKind || payload.MIME != receipt.PartMIME ||
			payload.Ordinal != receipt.PartOrdinal || payload.Digest != receipt.PartDigest {
			return fmt.Errorf("%w: creative delivery canonical part identity diverged", records.ErrIllegalTransition)
		}
		if err := payload.RenderManifest.ValidateFor(*payload.MessageContent); err != nil {
			return fmt.Errorf("%w: invalid creative delivery canonical evidence: %v", records.ErrIllegalTransition, err)
		}
		if payload.Ordinal < 1 || payload.Ordinal > len(payload.RenderManifest.Parts) {
			return fmt.Errorf("%w: creative delivery canonical part ordinal is invalid", records.ErrIllegalTransition)
		}
		selected := payload.RenderManifest.Parts[payload.Ordinal-1]
		if selected.Kind != payload.Kind {
			return fmt.Errorf("%w: creative delivery canonical projection kind diverged", records.ErrIllegalTransition)
		}
		switch payload.Kind {
		case messagecontent.PartMarkdown:
			if payload.Ordinal != 1 || payload.MIME != "" || payload.Attachment != nil ||
				payload.Text != selected.Text || payload.Digest != deliveryDigest(payload.MessageContent.Markdown) {
				return fmt.Errorf("%w: creative delivery Markdown payload diverged", records.ErrIllegalTransition)
			}
		case messagecontent.PartArtifact:
			attachmentIndex := payload.Ordinal - 2
			if attachmentIndex < 0 || attachmentIndex >= len(payload.MessageContent.Attachments) ||
				payload.Text != "" || payload.Attachment == nil {
				return fmt.Errorf("%w: creative delivery artifact payload is incomplete", records.ErrIllegalTransition)
			}
			canonicalAttachment := payload.MessageContent.Attachments[attachmentIndex]
			if payload.Attachment.Name != canonicalAttachment.Name ||
				payload.Attachment.MIME != payload.MIME || payload.MIME != canonicalAttachment.MIME ||
				payload.Digest != canonicalAttachment.Digest ||
				deliveryDigest(string(payload.Attachment.Data)) != payload.Digest ||
				selected.ArtifactRef != canonicalAttachment.AssetID ||
				selected.ArtifactDigest != canonicalAttachment.Digest ||
				selected.AltText != canonicalAttachment.AltText {
				return fmt.Errorf("%w: creative delivery artifact payload diverged", records.ErrIllegalTransition)
			}
		default:
			return fmt.Errorf("%w: creative delivery canonical part kind is unsupported", records.ErrIllegalTransition)
		}
		contentJSON, err := json.Marshal(payload.MessageContent)
		if err != nil || string(contentJSON) != string(firstContentJSON) {
			return fmt.Errorf("%w: creative delivery canonical content diverged", records.ErrIllegalTransition)
		}
		manifestJSON, err := json.Marshal(payload.RenderManifest)
		if err != nil || string(manifestJSON) != string(firstManifestJSON) || string(manifestJSON) != receipt.RenderJSON {
			return fmt.Errorf("%w: creative delivery canonical render evidence diverged", records.ErrIllegalTransition)
		}
	}
	return nil
}

// creativeDeliveryEnvelopeGroups 为每个作品投递目标恢复有序的 Markdown+图片组件组；
// PDF 继续作为独立物理消息。
func creativeDeliveryEnvelopeGroups(
	batch k12.DeliveryBatch,
) ([][]k12.DeliveryReceipt, map[string]struct{}, error) {
	groupedIDs := make(map[string]struct{})
	if strings.TrimSpace(batch.ObjectKind) != "creative_work" {
		return nil, groupedIDs, nil
	}
	targetOrder := make([]string, 0)
	byTarget := make(map[string][]k12.DeliveryReceipt)
	for _, receipt := range batch.Receipts {
		if strings.TrimSpace(receipt.BindingID) == "" ||
			strings.TrimSpace(receipt.Target.Platform) == "" ||
			strings.TrimSpace(receipt.Target.InstanceID) == "" ||
			strings.TrimSpace(receipt.Target.ChatID) == "" {
			return nil, nil, fmt.Errorf("%w: creative delivery target identity is incomplete", records.ErrIllegalTransition)
		}
		key := deliveryEnvelopeTargetKey(receipt)
		if _, exists := byTarget[key]; !exists {
			targetOrder = append(targetOrder, key)
		}
		byTarget[key] = append(byTarget[key], receipt)
	}
	groups := make([][]k12.DeliveryReceipt, 0, len(targetOrder))
	for _, key := range targetOrder {
		parts := byTarget[key]
		if len(parts) == 0 {
			continue
		}
		if strings.ToLower(strings.TrimSpace(parts[0].Target.Platform)) != "dingtalk" {
			continue
		}
		if err := validateCreativeDeliveryTargetIntegrity(batch, parts); err != nil {
			return nil, nil, err
		}
		group := make([]k12.DeliveryReceipt, 0, len(parts))
		for _, receipt := range parts {
			switch {
			case receipt.PartKind == messagecontent.PartMarkdown && receipt.PartOrdinal == 1:
				if len(group) != 0 {
					return nil, nil, fmt.Errorf("%w: creative delivery envelope has duplicate or unordered Markdown", records.ErrIllegalTransition)
				}
				group = append(group, receipt)
			case receipt.PartKind == messagecontent.PartArtifact && strings.HasPrefix(receipt.PartMIME, "image/"):
				if len(group) == 0 || receipt.PartOrdinal != len(group)+1 {
					return nil, nil, fmt.Errorf("%w: creative delivery envelope image order is invalid", records.ErrIllegalTransition)
				}
				group = append(group, receipt)
			case receipt.PartKind == messagecontent.PartArtifact && receipt.PartMIME == "application/pdf":
				// PDF 继续沿用独立 sampleFile 投递，不进入作品图文卡片。
			default:
				return nil, nil, fmt.Errorf("%w: creative delivery envelope contains an unsupported component", records.ErrIllegalTransition)
			}
		}
		if len(group) == 0 {
			return nil, nil, fmt.Errorf("%w: creative delivery envelope has no Markdown component", records.ErrIllegalTransition)
		}
		expectedArtifacts, known, err := creativeEnvelopeArtifactExpectations(group[0])
		if err != nil {
			return nil, nil, err
		}
		if known {
			if len(parts) != len(expectedArtifacts)+1 {
				return nil, nil, fmt.Errorf("%w: creative delivery artifact component count diverged", records.ErrIllegalTransition)
			}
			expectedImageCount := 0
			for i, expected := range expectedArtifacts {
				actual := parts[i+1]
				if actual.PartKind != messagecontent.PartArtifact ||
					actual.PartOrdinal != expected.ordinal ||
					strings.ToLower(strings.TrimSpace(actual.PartMIME)) != expected.mime ||
					actual.PartDigest != expected.digest {
					return nil, nil, fmt.Errorf("%w: creative delivery artifact component identity diverged", records.ErrIllegalTransition)
				}
				if expected.image {
					expectedImageCount++
				}
			}
			if len(group) != expectedImageCount+1 {
				return nil, nil, fmt.Errorf("%w: creative delivery envelope image component count diverged", records.ErrIllegalTransition)
			}
		}
		if len(group) < 2 {
			continue
		}
		first := group[0]
		for _, receipt := range group[1:] {
			if receipt.Status != first.Status || receipt.Attempt != first.Attempt ||
				receipt.ExternalMessageID != first.ExternalMessageID || receipt.LastError != first.LastError {
				return nil, nil, fmt.Errorf("%w: creative delivery envelope component state diverged", records.ErrIllegalTransition)
			}
		}
		for _, receipt := range group {
			groupedIDs[receipt.DeliveryID] = struct{}{}
		}
		groups = append(groups, group)
	}
	return groups, groupedIDs, nil
}

func deliveryReceiptIDs(receipts []k12.DeliveryReceipt) []string {
	ids := make([]string, 0, len(receipts))
	for _, receipt := range receipts {
		ids = append(ids, receipt.DeliveryID)
	}
	return ids
}

func (d Deps) sendPreparedDeliveryEnvelope(
	ctx context.Context,
	batch k12.DeliveryBatch,
	receipts []k12.DeliveryReceipt,
) ([]k12.DeliveryReceipt, error) {
	transport, ok := d.Delivery.(DeliveryEnvelopeTransport)
	if !ok {
		return receipts, fmt.Errorf("%w: creative delivery envelope transport unavailable", ErrDeliveryUnavailable)
	}
	for _, receipt := range receipts {
		if receipt.PartKind == messagecontent.PartArtifact && strings.TrimSpace(receipt.PreparedResourceID) == "" {
			return receipts, fmt.Errorf("%w: creative delivery envelope image has no prepared provider resource", records.ErrIllegalTransition)
		}
	}
	preflight, ok := d.Delivery.(DeliveryEnvelopePreflightTransport)
	if !ok {
		return receipts, fmt.Errorf("%w: creative delivery envelope preflight unavailable", ErrDeliveryUnavailable)
	}
	if err := preflight.PreflightPreparedEnvelope(ctx, receipts); err != nil {
		return receipts, err
	}
	started, began, err := d.Records.BeginDeliveryGroupAttempt(ctx, batch, receipts)
	if err != nil {
		return receipts, err
	}
	if !began {
		return started, nil
	}
	ack, sendErr := transport.SendPreparedEnvelope(ctx, started)
	return d.persistDeliveryEnvelopeSendOutcome(context.WithoutCancel(ctx), started, ack, sendErr)
}

func (d Deps) persistDeliveryEnvelopeSendOutcome(
	ctx context.Context,
	receipts []k12.DeliveryReceipt,
	ack DeliveryTransportAck,
	sendErr error,
) ([]k12.DeliveryReceipt, error) {
	ids := deliveryReceiptIDs(receipts)
	if id := strings.TrimSpace(ack.ExternalMessageID); id != "" {
		accepted, err := d.Records.MarkDeliveryGroupAccepted(ctx, receipts[0].AgentName, ids, id)
		if err != nil {
			return receipts, err
		}
		receipts = accepted
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
		if receipts[0].ExternalMessageID == "" {
			return d.Records.MarkDeliveryGroupOutcomeUnknown(ctx, receipts[0].AgentName, ids,
				deliveryFailureDetail(ack, sendErr, "provider accepted the envelope without a query id"))
		}
		return receipts, nil
	case k12.DeliveryDelivered:
		if receipts[0].ExternalMessageID == "" {
			return d.Records.MarkDeliveryGroupOutcomeUnknown(ctx, receipts[0].AgentName, ids,
				"provider reported delivery without a verifiable message id")
		}
		return d.Records.MarkDeliveryGroupDelivered(ctx, receipts[0].AgentName, ids)
	case k12.DeliveryFailed:
		return d.Records.MarkDeliveryGroupFailed(ctx, receipts[0].AgentName, ids,
			deliveryFailureDetail(ack, sendErr, "provider rejected the creative delivery envelope"))
	case k12.DeliveryOutcomeUnknown:
		return d.Records.MarkDeliveryGroupOutcomeUnknown(ctx, receipts[0].AgentName, ids,
			deliveryFailureDetail(ack, sendErr, "creative delivery envelope outcome is unknown"))
	default:
		return d.Records.MarkDeliveryGroupOutcomeUnknown(ctx, receipts[0].AgentName, ids,
			fmt.Sprintf("provider returned unknown envelope delivery status %q", status))
	}
}

func (d Deps) queryPreparedDeliveryEnvelope(
	ctx context.Context,
	receipts []k12.DeliveryReceipt,
) ([]k12.DeliveryReceipt, error) {
	first := receipts[0]
	if first.Status == k12.DeliveryDelivered || first.Status == k12.DeliveryFailed {
		return receipts, nil
	}
	transport, ok := d.Delivery.(DeliveryEnvelopeTransport)
	if !ok {
		return receipts, fmt.Errorf("%w: creative delivery envelope query unavailable", ErrDeliveryQueryUnavailable)
	}
	if first.Status != k12.DeliverySending && first.Status != k12.DeliveryOutcomeUnknown {
		return receipts, fmt.Errorf("%w: creative delivery envelope status %s does not need a query", records.ErrIllegalTransition, first.Status)
	}
	if strings.TrimSpace(first.ExternalMessageID) == "" {
		return receipts, fmt.Errorf("%w: creative delivery envelope has no query id", ErrDeliveryQueryUnavailable)
	}
	ack, queryErr := transport.QueryPreparedEnvelope(ctx, receipts)
	if ack.ExternalMessageID == "" {
		ack.ExternalMessageID = first.ExternalMessageID
	}
	ids := deliveryReceiptIDs(receipts)
	if queryErr != nil || ack.Status == k12.DeliveryOutcomeUnknown || ack.Status == "" {
		if first.Status == k12.DeliverySending {
			return d.Records.MarkDeliveryGroupOutcomeUnknown(ctx, first.AgentName, ids,
				deliveryFailureDetail(ack, queryErr, "creative delivery envelope result is not available yet"))
		}
		return receipts, nil
	}
	switch ack.Status {
	case k12.DeliverySending:
		return d.Records.ReconcileDeliveryGroup(ctx, first.AgentName, ids,
			k12.DeliverySending, ack.ExternalMessageID, "")
	case k12.DeliveryDelivered:
		return d.Records.ReconcileDeliveryGroup(ctx, first.AgentName, ids,
			k12.DeliveryDelivered, ack.ExternalMessageID, "")
	case k12.DeliveryFailed:
		return d.Records.ReconcileDeliveryGroup(ctx, first.AgentName, ids,
			k12.DeliveryFailed, ack.ExternalMessageID,
			deliveryFailureDetail(ack, queryErr, "provider confirmed the creative delivery envelope failed"))
	default:
		return receipts, fmt.Errorf("%w: unknown creative delivery envelope query status %q", ErrInvalidInput, ack.Status)
	}
}

func (d Deps) creativeDeliveryEnvelopeForReceipt(
	ctx context.Context,
	receipt k12.DeliveryReceipt,
) ([]k12.DeliveryReceipt, bool, error) {
	if receipt.BatchID == "" || receipt.ObjectKind != "creative_work" {
		return nil, false, nil
	}
	batch, err := d.Records.GetDeliveryBatch(ctx, receipt.AgentName, receipt.BatchID)
	if err != nil {
		return nil, false, err
	}
	groups, groupedIDs, err := creativeDeliveryEnvelopeGroups(batch)
	if err != nil {
		return nil, false, err
	}
	if _, grouped := groupedIDs[receipt.DeliveryID]; !grouped {
		return nil, false, nil
	}
	for _, group := range groups {
		for _, component := range group {
			if component.DeliveryID == receipt.DeliveryID {
				return group, true, nil
			}
		}
	}
	return nil, false, fmt.Errorf("%w: creative delivery envelope group is missing", records.ErrIllegalTransition)
}

func (d Deps) sendPreparedDelivery(ctx context.Context, receipt k12.DeliveryReceipt) (k12.DeliveryReceipt, error) {
	if receipt.PartKind == messagecontent.PartArtifact && strings.TrimSpace(receipt.PreparedResourceID) == "" {
		return receipt, fmt.Errorf("%w: artifact part has no prepared provider resource", records.ErrIllegalTransition)
	}
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
	if group, grouped, groupErr := d.creativeDeliveryEnvelopeForReceipt(ctx, receipt); groupErr != nil {
		return receipt, groupErr
	} else if grouped {
		updated, queryErr := d.queryPreparedDeliveryEnvelope(ctx, group)
		for _, component := range updated {
			if component.DeliveryID == receipt.DeliveryID {
				return component, queryErr
			}
		}
		return receipt, queryErr
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
	pendingBatchCounts := make(map[string]int)
	for _, receipt := range receipts {
		if receipt.Status == k12.DeliveryPending && receipt.BatchID != "" {
			pendingBatchCounts[receipt.BatchID]++
		}
	}
	handledPendingBatches := make(map[string]struct{}, len(pendingBatchCounts))
	handledEnvelopeGroups := make(map[string]struct{})
	processed := 0
	for _, receipt := range receipts {
		select {
		case <-ctx.Done():
			return processed, ctx.Err()
		default:
		}
		group, grouped, groupErr := d.creativeDeliveryEnvelopeForReceipt(ctx, receipt)
		if groupErr != nil {
			return processed, groupErr
		}
		if grouped && receipt.Status != k12.DeliveryPending {
			groupKey := group[0].DeliveryID
			if _, handled := handledEnvelopeGroups[groupKey]; handled {
				continue
			}
			handledEnvelopeGroups[groupKey] = struct{}{}
			if group[0].Status == k12.DeliverySending && group[0].ExternalMessageID == "" {
				_, err = d.Records.MarkDeliveryGroupOutcomeUnknown(
					ctx, group[0].AgentName, deliveryReceiptIDs(group),
					"application restart found no query id for the creative delivery envelope",
				)
			} else if (group[0].Status == k12.DeliverySending || group[0].Status == k12.DeliveryOutcomeUnknown) &&
				group[0].ExternalMessageID != "" {
				_, err = d.queryPreparedDeliveryEnvelope(ctx, group)
			}
			if err != nil && !errors.Is(err, ErrDeliveryQueryUnavailable) {
				return processed, err
			}
			processed += len(group)
			continue
		}
		switch receipt.Status {
		case k12.DeliveryPending:
			if receipt.BatchID == "" {
				_, err = d.sendPreparedDelivery(ctx, receipt)
				break
			}
			if _, handled := handledPendingBatches[receipt.BatchID]; handled {
				continue
			}
			batch, getErr := d.Records.GetDeliveryBatch(ctx, receipt.AgentName, receipt.BatchID)
			if getErr != nil {
				return processed, getErr
			}
			_, err = d.sendDeliveryBatch(ctx, batch)
			if err == nil {
				handledPendingBatches[receipt.BatchID] = struct{}{}
				processed += pendingBatchCounts[receipt.BatchID]
				continue
			}
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
