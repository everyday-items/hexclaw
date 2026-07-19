package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func deliveryDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func deliveryDedupeKey(agentName, objectKind, objectID, bindingID, payloadDigest string) string {
	return deliveryDigest(strings.Join([]string{agentName, objectKind, objectID, bindingID, payloadDigest}, "\x00"))
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
	return d.persistDeliverySendOutcome(ctx, started, ack, sendErr)
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
