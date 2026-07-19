package k12storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

var ErrDeliveryReceiptConflict = errors.New("delivery receipt immutable identity conflict")

const deliveryReceiptColumns = `delivery_id,agent_name,object_kind,object_id,binding_id,
    platform,instance_id,chat_id,target_label,status,dedupe_key,payload_digest,payload_json,
    render_manifest_json,external_message_id,attempt,last_error,created_at,updated_at`

func scanDeliveryReceipt(row rowScanner) (k12.DeliveryReceipt, error) {
	var receipt k12.DeliveryReceipt
	var status string
	err := row.Scan(
		&receipt.DeliveryID, &receipt.AgentName, &receipt.ObjectKind, &receipt.ObjectID,
		&receipt.BindingID, &receipt.Target.Platform, &receipt.Target.InstanceID,
		&receipt.Target.ChatID, &receipt.Target.Label, &status, &receipt.DedupeKey,
		&receipt.PayloadDigest, &receipt.PayloadJSON, &receipt.RenderJSON,
		&receipt.ExternalMessageID, &receipt.Attempt, &receipt.LastError,
		&receipt.CreatedAt, &receipt.UpdatedAt,
	)
	receipt.Status = k12.DeliveryReceiptStatus(status)
	return receipt, err
}

func normalizeDeliveryReceipt(receipt k12.DeliveryReceipt) (k12.DeliveryReceipt, error) {
	receipt.DeliveryID = strings.TrimSpace(receipt.DeliveryID)
	receipt.AgentName = strings.TrimSpace(receipt.AgentName)
	receipt.ObjectKind = strings.TrimSpace(receipt.ObjectKind)
	receipt.ObjectID = strings.TrimSpace(receipt.ObjectID)
	receipt.BindingID = strings.TrimSpace(receipt.BindingID)
	receipt.Target.Platform = strings.ToLower(strings.TrimSpace(receipt.Target.Platform))
	receipt.Target.InstanceID = strings.TrimSpace(receipt.Target.InstanceID)
	receipt.Target.ChatID = strings.TrimSpace(receipt.Target.ChatID)
	receipt.Target.Label = strings.TrimSpace(receipt.Target.Label)
	receipt.DedupeKey = strings.TrimSpace(receipt.DedupeKey)
	receipt.PayloadDigest = strings.TrimSpace(receipt.PayloadDigest)
	receipt.PayloadJSON = strings.TrimSpace(receipt.PayloadJSON)
	receipt.RenderJSON = strings.TrimSpace(receipt.RenderJSON)
	if receipt.DeliveryID == "" || receipt.AgentName == "" || receipt.ObjectKind == "" ||
		receipt.ObjectID == "" || receipt.BindingID == "" || receipt.Target.Platform == "" ||
		receipt.Target.ChatID == "" || receipt.DedupeKey == "" || receipt.PayloadDigest == "" ||
		receipt.PayloadJSON == "" {
		return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: DeliveryReceipt 缺少 id/owner/object/binding/target/dedupe/payload")
	}
	if strings.HasPrefix(receipt.Target.ChatID, "\x00") {
		return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: DeliveryReceipt 只允许 direct target")
	}
	if !json.Valid([]byte(receipt.PayloadJSON)) {
		return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: DeliveryReceipt payload_json 非法")
	}
	if receipt.RenderJSON != "" && !json.Valid([]byte(receipt.RenderJSON)) {
		return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: DeliveryReceipt render_manifest_json 非法")
	}
	if receipt.CreatedAt <= 0 {
		receipt.CreatedAt = nowUnix()
	}
	receipt.UpdatedAt = receipt.CreatedAt
	receipt.Status = k12.DeliveryPending
	receipt.ExternalMessageID = ""
	receipt.Attempt = 0
	receipt.LastError = ""
	return receipt, nil
}

func deliveryIdentityEqual(a, b k12.DeliveryReceipt) bool {
	return a.AgentName == b.AgentName && a.ObjectKind == b.ObjectKind && a.ObjectID == b.ObjectID &&
		a.BindingID == b.BindingID && a.Target == b.Target && a.DedupeKey == b.DedupeKey &&
		a.PayloadDigest == b.PayloadDigest && a.PayloadJSON == b.PayloadJSON && a.RenderJSON == b.RenderJSON
}

func (s *Store) PrepareDeliveryReceipt(ctx context.Context, input k12.DeliveryReceipt) (k12.DeliveryReceipt, bool, error) {
	receipt, err := normalizeDeliveryReceipt(input)
	if err != nil {
		return k12.DeliveryReceipt{}, false, err
	}
	if err := ensureAgentRegistered(ctx, s.db, receipt.AgentName); err != nil {
		return k12.DeliveryReceipt{}, false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO k12_delivery_receipts (`+deliveryReceiptColumns+`)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(agent_name,dedupe_key) DO NOTHING`,
		receipt.DeliveryID, receipt.AgentName, receipt.ObjectKind, receipt.ObjectID,
		receipt.BindingID, receipt.Target.Platform, receipt.Target.InstanceID,
		receipt.Target.ChatID, receipt.Target.Label, receipt.Status, receipt.DedupeKey,
		receipt.PayloadDigest, receipt.PayloadJSON, receipt.RenderJSON, "",
		0, "", receipt.CreatedAt, receipt.UpdatedAt,
	)
	if err != nil {
		return k12.DeliveryReceipt{}, false, fmt.Errorf("k12storage: prepare DeliveryReceipt: %w", err)
	}
	created, _ := res.RowsAffected()
	stored, err := s.getDeliveryReceiptByDedupe(ctx, receipt.AgentName, receipt.DedupeKey)
	if err != nil {
		return k12.DeliveryReceipt{}, false, err
	}
	if !deliveryIdentityEqual(stored, receipt) {
		return k12.DeliveryReceipt{}, false, fmt.Errorf("%w: owner=%s dedupe=%s", ErrDeliveryReceiptConflict, receipt.AgentName, receipt.DedupeKey)
	}
	return stored, created > 0, nil
}

func (s *Store) getDeliveryReceiptByDedupe(ctx context.Context, agentName, dedupeKey string) (k12.DeliveryReceipt, error) {
	receipt, err := scanDeliveryReceipt(s.db.QueryRowContext(ctx, `SELECT `+deliveryReceiptColumns+`
        FROM k12_delivery_receipts WHERE agent_name=? AND dedupe_key=?`, agentName, dedupeKey))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.DeliveryReceipt{}, records.ErrNotFound
	}
	if err != nil {
		return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: get DeliveryReceipt by dedupe: %w", err)
	}
	return receipt, nil
}

func (s *Store) GetDeliveryReceipt(ctx context.Context, agentName, deliveryID string) (k12.DeliveryReceipt, error) {
	receipt, err := scanDeliveryReceipt(s.db.QueryRowContext(ctx, `SELECT `+deliveryReceiptColumns+`
        FROM k12_delivery_receipts WHERE agent_name=? AND delivery_id=?`, agentName, deliveryID))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.DeliveryReceipt{}, records.ErrNotFound
	}
	if err != nil {
		return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: get DeliveryReceipt: %w", err)
	}
	return receipt, nil
}

func (s *Store) BeginDeliveryAttempt(ctx context.Context, agentName, deliveryID string) (k12.DeliveryReceipt, bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE k12_delivery_receipts SET
        status='sending',attempt=attempt+1,external_message_id='',last_error='',updated_at=?
        WHERE agent_name=? AND delivery_id=? AND status IN ('pending','failed')`,
		nowUnix(), agentName, deliveryID)
	if err != nil {
		return k12.DeliveryReceipt{}, false, fmt.Errorf("k12storage: begin delivery attempt: %w", err)
	}
	receipt, getErr := s.GetDeliveryReceipt(ctx, agentName, deliveryID)
	if getErr != nil {
		return k12.DeliveryReceipt{}, false, getErr
	}
	if changed, _ := res.RowsAffected(); changed == 1 {
		return receipt, true, nil
	}
	if receipt.Status == k12.DeliverySending {
		return receipt, false, nil
	}
	return receipt, false, fmt.Errorf("%w: DeliveryReceipt %s status %s cannot send", records.ErrIllegalTransition, deliveryID, receipt.Status)
}

func (s *Store) MarkDeliveryAccepted(ctx context.Context, agentName, deliveryID, externalMessageID string) (k12.DeliveryReceipt, error) {
	externalMessageID = strings.TrimSpace(externalMessageID)
	if externalMessageID == "" {
		return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: accepted delivery requires external_message_id")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE k12_delivery_receipts SET external_message_id=?,updated_at=?
        WHERE agent_name=? AND delivery_id=? AND status='sending'
          AND (external_message_id='' OR external_message_id=?)`,
		externalMessageID, nowUnix(), agentName, deliveryID, externalMessageID)
	if err != nil {
		return k12.DeliveryReceipt{}, err
	}
	receipt, getErr := s.GetDeliveryReceipt(ctx, agentName, deliveryID)
	if getErr != nil {
		return k12.DeliveryReceipt{}, getErr
	}
	if changed, _ := res.RowsAffected(); changed == 0 &&
		(receipt.Status != k12.DeliverySending || receipt.ExternalMessageID != externalMessageID) {
		return receipt, fmt.Errorf("%w: DeliveryReceipt %s cannot accept external id", records.ErrIllegalTransition, deliveryID)
	}
	return receipt, nil
}

func (s *Store) MarkDeliveryDelivered(ctx context.Context, agentName, deliveryID string) (k12.DeliveryReceipt, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE k12_delivery_receipts SET status='delivered',last_error='',updated_at=?
        WHERE agent_name=? AND delivery_id=? AND status IN ('sending','outcome_unknown')
          AND external_message_id!=''`, nowUnix(), agentName, deliveryID)
	if err != nil {
		return k12.DeliveryReceipt{}, err
	}
	return s.deliveryTransitionResult(ctx, agentName, deliveryID, res, k12.DeliveryDelivered)
}

func (s *Store) MarkDeliveryFailed(ctx context.Context, agentName, deliveryID, detail string) (k12.DeliveryReceipt, error) {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: failed delivery requires detail")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE k12_delivery_receipts SET status='failed',last_error=?,updated_at=?
        WHERE agent_name=? AND delivery_id=? AND status='sending'`,
		detail, nowUnix(), agentName, deliveryID)
	if err != nil {
		return k12.DeliveryReceipt{}, err
	}
	return s.deliveryTransitionResult(ctx, agentName, deliveryID, res, k12.DeliveryFailed)
}

func (s *Store) MarkDeliveryOutcomeUnknown(ctx context.Context, agentName, deliveryID, detail string) (k12.DeliveryReceipt, error) {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: outcome_unknown delivery requires detail")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE k12_delivery_receipts SET status='outcome_unknown',last_error=?,updated_at=?
        WHERE agent_name=? AND delivery_id=? AND status='sending'`,
		detail, nowUnix(), agentName, deliveryID)
	if err != nil {
		return k12.DeliveryReceipt{}, err
	}
	return s.deliveryTransitionResult(ctx, agentName, deliveryID, res, k12.DeliveryOutcomeUnknown)
}

func (s *Store) deliveryTransitionResult(ctx context.Context, agentName, deliveryID string, res sql.Result, want k12.DeliveryReceiptStatus) (k12.DeliveryReceipt, error) {
	receipt, err := s.GetDeliveryReceipt(ctx, agentName, deliveryID)
	if err != nil {
		return k12.DeliveryReceipt{}, err
	}
	if changed, _ := res.RowsAffected(); changed == 0 && receipt.Status != want {
		return receipt, fmt.Errorf("%w: DeliveryReceipt %s status %s -> %s", records.ErrIllegalTransition, deliveryID, receipt.Status, want)
	}
	return receipt, nil
}

// ReconcileDeliveryReceipt is the only path out of outcome_unknown. It records
// provider query evidence and never sends another message.
func (s *Store) ReconcileDeliveryReceipt(ctx context.Context, agentName, deliveryID string, status k12.DeliveryReceiptStatus, externalMessageID, detail string) (k12.DeliveryReceipt, error) {
	receipt, err := s.GetDeliveryReceipt(ctx, agentName, deliveryID)
	if err != nil {
		return k12.DeliveryReceipt{}, err
	}
	externalMessageID = strings.TrimSpace(externalMessageID)
	// Concurrent provider queries may observe the same terminal evidence. The
	// second writer is an idempotent replay, not an illegal transition.
	if receipt.Status == status && (status == k12.DeliveryDelivered || status == k12.DeliveryFailed) {
		if externalMessageID == "" || receipt.ExternalMessageID == "" || receipt.ExternalMessageID == externalMessageID {
			return receipt, nil
		}
	}
	if receipt.Status != k12.DeliveryOutcomeUnknown && receipt.Status != k12.DeliverySending {
		return receipt, fmt.Errorf("%w: DeliveryReceipt %s status %s cannot reconcile", records.ErrIllegalTransition, deliveryID, receipt.Status)
	}
	if externalMessageID == "" {
		externalMessageID = receipt.ExternalMessageID
	}
	switch status {
	case k12.DeliveryDelivered:
		if externalMessageID == "" {
			return receipt, fmt.Errorf("k12storage: delivered reconciliation requires external_message_id")
		}
		_, err = s.db.ExecContext(ctx, `UPDATE k12_delivery_receipts SET
            status='delivered',external_message_id=?,last_error='',updated_at=?
            WHERE agent_name=? AND delivery_id=? AND status IN ('sending','outcome_unknown')`,
			externalMessageID, nowUnix(), agentName, deliveryID)
	case k12.DeliveryFailed:
		if strings.TrimSpace(detail) == "" {
			return receipt, fmt.Errorf("k12storage: failed reconciliation requires detail")
		}
		_, err = s.db.ExecContext(ctx, `UPDATE k12_delivery_receipts SET
            status='failed',external_message_id=?,last_error=?,updated_at=?
            WHERE agent_name=? AND delivery_id=? AND status IN ('sending','outcome_unknown')`,
			externalMessageID, strings.TrimSpace(detail), nowUnix(), agentName, deliveryID)
	case k12.DeliverySending:
		if externalMessageID == "" {
			return receipt, fmt.Errorf("k12storage: accepted reconciliation requires external_message_id")
		}
		_, err = s.db.ExecContext(ctx, `UPDATE k12_delivery_receipts SET
            status='sending',external_message_id=?,last_error='',updated_at=?
            WHERE agent_name=? AND delivery_id=? AND status IN ('sending','outcome_unknown')`,
			externalMessageID, nowUnix(), agentName, deliveryID)
	default:
		return receipt, fmt.Errorf("k12storage: unsupported reconciliation status %q", status)
	}
	if err != nil {
		return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: reconcile DeliveryReceipt: %w", err)
	}
	return s.GetDeliveryReceipt(ctx, agentName, deliveryID)
}

func (s *Store) ListRecoverableDeliveryReceipts(ctx context.Context, agentName string) ([]k12.DeliveryReceipt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deliveryReceiptColumns+`
        FROM k12_delivery_receipts WHERE agent_name=?
          AND status IN ('pending','sending','outcome_unknown')
        ORDER BY created_at,delivery_id`, agentName)
	if err != nil {
		return nil, fmt.Errorf("k12storage: list recoverable DeliveryReceipts: %w", err)
	}
	defer rows.Close()
	out := make([]k12.DeliveryReceipt, 0)
	for rows.Next() {
		receipt, scanErr := scanDeliveryReceipt(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, receipt)
	}
	return out, rows.Err()
}
