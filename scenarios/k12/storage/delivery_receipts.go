package k12storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

var ErrDeliveryReceiptConflict = errors.New("delivery receipt immutable identity conflict")
var ErrDeliveryBatchConflict = errors.New("delivery batch immutable identity conflict")

const deliveryReceiptColumns = `delivery_id,batch_id,batch_ordinal,part_kind,part_mime,part_ordinal,
    part_digest,prepared_resource_id,agent_name,object_kind,object_id,binding_id,
    platform,instance_id,chat_id,target_label,status,dedupe_key,payload_digest,payload_json,
    render_manifest_json,external_message_id,attempt,last_error,created_at,updated_at`

func scanDeliveryReceipt(row rowScanner) (k12.DeliveryReceipt, error) {
	var receipt k12.DeliveryReceipt
	var status string
	err := row.Scan(
		&receipt.DeliveryID, &receipt.BatchID, &receipt.BatchOrdinal,
		&receipt.PartKind, &receipt.PartMIME, &receipt.PartOrdinal,
		&receipt.PartDigest, &receipt.PreparedResourceID,
		&receipt.AgentName, &receipt.ObjectKind, &receipt.ObjectID,
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
	receipt.BatchID = strings.TrimSpace(receipt.BatchID)
	if receipt.PartKind == "" {
		receipt.PartKind = messagecontent.PartMarkdown
	}
	receipt.PartMIME = strings.ToLower(strings.TrimSpace(receipt.PartMIME))
	if receipt.PartOrdinal == 0 {
		receipt.PartOrdinal = 1
	}
	receipt.PartDigest = strings.TrimSpace(receipt.PartDigest)
	receipt.PreparedResourceID = strings.TrimSpace(receipt.PreparedResourceID)
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
	if receipt.PartDigest == "" {
		receipt.PartDigest = receipt.PayloadDigest
	}
	if receipt.DeliveryID == "" || receipt.AgentName == "" || receipt.ObjectKind == "" ||
		receipt.ObjectID == "" || receipt.BindingID == "" || receipt.Target.Platform == "" ||
		receipt.Target.ChatID == "" || receipt.DedupeKey == "" || receipt.PayloadDigest == "" ||
		receipt.PayloadJSON == "" {
		return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: DeliveryReceipt 缺少 id/owner/object/binding/target/dedupe/payload")
	}
	if receipt.BatchOrdinal < 0 || (receipt.BatchID != "" && receipt.BatchOrdinal < 1) {
		return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: DeliveryReceipt batch ordinal 非法")
	}
	if receipt.PartOrdinal < 1 {
		return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: delivery receipt part ordinal is invalid")
	}
	switch receipt.PartKind {
	case messagecontent.PartMarkdown:
		if receipt.PartMIME != "" {
			return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: markdown delivery part cannot declare MIME")
		}
	case messagecontent.PartArtifact:
		if !strings.HasPrefix(receipt.PartMIME, "image/") && receipt.PartMIME != "application/pdf" {
			return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: unsupported delivery artifact MIME %q", receipt.PartMIME)
		}
	default:
		return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: unsupported delivery part kind %q", receipt.PartKind)
	}
	if receipt.PartDigest == "" {
		return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: delivery receipt part digest is required")
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
	receipt.PreparedResourceID = ""
	receipt.ExternalMessageID = ""
	receipt.Attempt = 0
	receipt.LastError = ""
	return receipt, nil
}

func deliveryIdentityEqual(a, b k12.DeliveryReceipt) bool {
	return a.BatchID == b.BatchID && a.BatchOrdinal == b.BatchOrdinal &&
		a.PartKind == b.PartKind && a.PartMIME == b.PartMIME &&
		a.PartOrdinal == b.PartOrdinal && a.PartDigest == b.PartDigest &&
		a.AgentName == b.AgentName && a.ObjectKind == b.ObjectKind && a.ObjectID == b.ObjectID &&
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
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(agent_name,dedupe_key) DO NOTHING`,
		receipt.DeliveryID, receipt.BatchID, receipt.BatchOrdinal,
		receipt.PartKind, receipt.PartMIME, receipt.PartOrdinal,
		receipt.PartDigest, receipt.PreparedResourceID,
		receipt.AgentName, receipt.ObjectKind, receipt.ObjectID,
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

// SaveDeliveryPreparedResource 持久化媒体 part 的 provider 资源引用。
// 该引用一经写入不得替换，失败批次重试只补仍为空的媒体 part。
func (s *Store) SaveDeliveryPreparedResource(
	ctx context.Context,
	agentName, deliveryID, preparedResourceID string,
) (k12.DeliveryReceipt, error) {
	agentName = strings.TrimSpace(agentName)
	deliveryID = strings.TrimSpace(deliveryID)
	preparedResourceID = strings.TrimSpace(preparedResourceID)
	if preparedResourceID == "" {
		return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: prepared resource id is required")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE k12_delivery_receipts SET
		prepared_resource_id=?,updated_at=?
		WHERE agent_name=? AND delivery_id=? AND part_kind='artifact'
		  AND status IN ('pending','failed')
		  AND (prepared_resource_id='' OR prepared_resource_id=?)`,
		preparedResourceID, nowUnix(), agentName, deliveryID, preparedResourceID,
	)
	if err != nil {
		return k12.DeliveryReceipt{}, fmt.Errorf("k12storage: save prepared delivery resource: %w", err)
	}
	receipt, getErr := s.GetDeliveryReceipt(ctx, agentName, deliveryID)
	if getErr != nil {
		return k12.DeliveryReceipt{}, getErr
	}
	if changed, _ := res.RowsAffected(); changed == 1 {
		return receipt, nil
	}
	if receipt.PartKind != messagecontent.PartArtifact {
		return receipt, fmt.Errorf("%w: delivery receipt %s is not an artifact part", records.ErrIllegalTransition, deliveryID)
	}
	if receipt.PreparedResourceID != "" && receipt.PreparedResourceID != preparedResourceID {
		return receipt, fmt.Errorf("%w: delivery receipt %s prepared resource differs", ErrDeliveryReceiptConflict, deliveryID)
	}
	if receipt.PreparedResourceID == preparedResourceID {
		return receipt, nil
	}
	return receipt, fmt.Errorf("%w: delivery receipt %s cannot prepare resource from status %s", records.ErrIllegalTransition, deliveryID, receipt.Status)
}

const deliveryBatchColumns = `batch_id,agent_name,object_kind,object_id,dedupe_key,
    content_digest,created_at,updated_at`

func scanDeliveryBatchRoot(row rowScanner) (k12.DeliveryBatch, error) {
	var batch k12.DeliveryBatch
	err := row.Scan(
		&batch.BatchID, &batch.AgentName, &batch.ObjectKind, &batch.ObjectID,
		&batch.DedupeKey, &batch.ContentDigest, &batch.CreatedAt, &batch.UpdatedAt,
	)
	return batch, err
}

func normalizeDeliveryBatch(input k12.DeliveryBatch) (k12.DeliveryBatch, error) {
	input.BatchID = strings.TrimSpace(input.BatchID)
	input.AgentName = strings.TrimSpace(input.AgentName)
	input.ObjectKind = strings.TrimSpace(input.ObjectKind)
	input.ObjectID = strings.TrimSpace(input.ObjectID)
	input.DedupeKey = strings.TrimSpace(input.DedupeKey)
	input.ContentDigest = strings.TrimSpace(input.ContentDigest)
	if input.BatchID == "" || input.AgentName == "" || input.ObjectKind == "" ||
		input.ObjectID == "" || input.DedupeKey == "" || input.ContentDigest == "" {
		return k12.DeliveryBatch{}, fmt.Errorf("k12storage: DeliveryBatch 缺少 id/owner/object/dedupe/content")
	}
	if len(input.Receipts) == 0 {
		return k12.DeliveryBatch{}, fmt.Errorf("k12storage: DeliveryBatch 至少需要一个子回执")
	}
	if input.CreatedAt <= 0 {
		input.CreatedAt = nowUnix()
	}
	input.UpdatedAt = input.CreatedAt
	input.Status = ""
	closedTargets := make(map[string]struct{}, len(input.Receipts))
	bindingTargets := make(map[string]string, len(input.Receipts))
	normalized := make([]k12.DeliveryReceipt, 0, len(input.Receipts))
	currentTarget := ""
	currentBinding := ""
	currentParts := make([]struct {
		kind messagecontent.PartKind
		mime string
	}, 0)
	var partTemplate []struct {
		kind messagecontent.PartKind
		mime string
	}
	finishTarget := func() error {
		if currentTarget == "" {
			return nil
		}
		if partTemplate == nil {
			partTemplate = append(partTemplate, currentParts...)
		} else {
			if len(currentParts) != len(partTemplate) {
				return fmt.Errorf("k12storage: delivery batch targets have different part counts")
			}
			for i := range currentParts {
				if currentParts[i] != partTemplate[i] {
					return fmt.Errorf("k12storage: delivery batch targets have different part manifests")
				}
			}
		}
		closedTargets[currentTarget] = struct{}{}
		return nil
	}
	for i, child := range input.Receipts {
		child.BatchID = input.BatchID
		child.BatchOrdinal = i + 1
		child.AgentName = input.AgentName
		child.ObjectKind = input.ObjectKind
		child.ObjectID = input.ObjectID
		child.CreatedAt = input.CreatedAt
		child, err := normalizeDeliveryReceipt(child)
		if err != nil {
			return k12.DeliveryBatch{}, fmt.Errorf("k12storage: normalize DeliveryBatch child %d: %w", i+1, err)
		}
		targetKey := strings.Join([]string{
			child.Target.Platform, child.Target.InstanceID, child.Target.ChatID,
		}, "\x00")
		if currentTarget != targetKey {
			if err := finishTarget(); err != nil {
				return k12.DeliveryBatch{}, err
			}
			if _, exists := closedTargets[targetKey]; exists {
				return k12.DeliveryBatch{}, fmt.Errorf("k12storage: delivery batch target parts are not contiguous")
			}
			currentTarget = targetKey
			currentBinding = child.BindingID
			currentParts = currentParts[:0]
		}
		if child.BindingID != currentBinding {
			return k12.DeliveryBatch{}, fmt.Errorf("k12storage: delivery batch target changed binding between parts")
		}
		if existingTarget, exists := bindingTargets[child.BindingID]; exists && existingTarget != targetKey {
			return k12.DeliveryBatch{}, fmt.Errorf("k12storage: delivery batch binding maps to multiple targets")
		}
		bindingTargets[child.BindingID] = targetKey
		if child.PartOrdinal != len(currentParts)+1 {
			return k12.DeliveryBatch{}, fmt.Errorf("k12storage: delivery batch part ordinals must be contiguous")
		}
		currentParts = append(currentParts, struct {
			kind messagecontent.PartKind
			mime string
		}{kind: child.PartKind, mime: child.PartMIME})
		normalized = append(normalized, child)
	}
	if err := finishTarget(); err != nil {
		return k12.DeliveryBatch{}, err
	}
	input.Receipts = normalized
	return input, nil
}

func deliveryBatchIdentityEqual(a, b k12.DeliveryBatch) bool {
	return a.AgentName == b.AgentName && a.ObjectKind == b.ObjectKind &&
		a.ObjectID == b.ObjectID && a.DedupeKey == b.DedupeKey &&
		a.ContentDigest == b.ContentDigest
}

func insertDeliveryBatchRoot(
	ctx context.Context,
	ex dbExecer,
	batch k12.DeliveryBatch,
	ignoreDedupeConflict bool,
) (bool, error) {
	query := `INSERT INTO k12_delivery_batches (` + deliveryBatchColumns + `)
        VALUES(?,?,?,?,?,?,?,?)`
	if ignoreDedupeConflict {
		query += ` ON CONFLICT(agent_name,dedupe_key) DO NOTHING`
	}
	res, err := ex.ExecContext(ctx, query,
		batch.BatchID, batch.AgentName, batch.ObjectKind, batch.ObjectID,
		batch.DedupeKey, batch.ContentDigest, batch.CreatedAt, batch.UpdatedAt,
	)
	if err != nil {
		return false, fmt.Errorf("k12storage: prepare DeliveryBatch: %w", err)
	}
	created, _ := res.RowsAffected()
	return created > 0, nil
}

func insertDeliveryBatchChildren(
	ctx context.Context,
	ex dbExecer,
	batch k12.DeliveryBatch,
) error {
	for _, receipt := range batch.Receipts {
		if _, err := ex.ExecContext(ctx, `INSERT INTO k12_delivery_receipts (`+deliveryReceiptColumns+`)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			receipt.DeliveryID, receipt.BatchID, receipt.BatchOrdinal,
			receipt.PartKind, receipt.PartMIME, receipt.PartOrdinal,
			receipt.PartDigest, receipt.PreparedResourceID,
			receipt.AgentName, receipt.ObjectKind, receipt.ObjectID,
			receipt.BindingID, receipt.Target.Platform, receipt.Target.InstanceID,
			receipt.Target.ChatID, receipt.Target.Label, receipt.Status, receipt.DedupeKey,
			receipt.PayloadDigest, receipt.PayloadJSON, receipt.RenderJSON,
			receipt.ExternalMessageID, receipt.Attempt, receipt.LastError,
			receipt.CreatedAt, receipt.UpdatedAt,
		); err != nil {
			return fmt.Errorf(
				"k12storage: prepare DeliveryBatch child %d: %w", receipt.BatchOrdinal, err,
			)
		}
	}
	return nil
}

// PrepareDeliveryBatch atomically freezes the logical batch root and every
// child receipt before any provider call. The first writer wins a concurrent
// binding-snapshot race; later command replays return that frozen batch.
func (s *Store) PrepareDeliveryBatch(ctx context.Context, input k12.DeliveryBatch) (k12.DeliveryBatch, bool, error) {
	batch, err := normalizeDeliveryBatch(input)
	if err != nil {
		return k12.DeliveryBatch{}, false, err
	}
	// Keep the deferred SQLite transaction write-first. A transactional read
	// followed by INSERT can fail its lock upgrade under concurrent writers.
	if err := ensureAgentRegistered(ctx, s.db, batch.AgentName); err != nil {
		return k12.DeliveryBatch{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.DeliveryBatch{}, false, fmt.Errorf("k12storage: begin DeliveryBatch: %w", err)
	}
	defer tx.Rollback()
	created, err := insertDeliveryBatchRoot(ctx, tx, batch, true)
	if err != nil {
		return k12.DeliveryBatch{}, false, err
	}
	storedRoot := batch
	if !created {
		storedRoot, err = scanDeliveryBatchRoot(tx.QueryRowContext(ctx,
			`SELECT `+deliveryBatchColumns+` FROM k12_delivery_batches
             WHERE agent_name=? AND dedupe_key=?`,
			batch.AgentName, batch.DedupeKey,
		))
		if errors.Is(err, sql.ErrNoRows) {
			return k12.DeliveryBatch{}, false, records.ErrNotFound
		}
		if err != nil {
			return k12.DeliveryBatch{}, false, fmt.Errorf("k12storage: replay DeliveryBatch: %w", err)
		}
		if !deliveryBatchIdentityEqual(storedRoot, batch) {
			return k12.DeliveryBatch{}, false, fmt.Errorf(
				"%w: owner=%s dedupe=%s", ErrDeliveryBatchConflict, batch.AgentName, batch.DedupeKey,
			)
		}
	} else {
		if err := insertDeliveryBatchChildren(ctx, tx, batch); err != nil {
			return k12.DeliveryBatch{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return k12.DeliveryBatch{}, false, fmt.Errorf("k12storage: commit DeliveryBatch: %w", err)
	}
	stored, err := s.GetDeliveryBatch(ctx, batch.AgentName, storedRoot.BatchID)
	return stored, created, err
}

func (s *Store) GetDeliveryBatchByDedupe(ctx context.Context, agentName, dedupeKey string) (k12.DeliveryBatch, error) {
	return getDeliveryBatchByDedupeVia(
		ctx, s.db, strings.TrimSpace(agentName), strings.TrimSpace(dedupeKey),
	)
}

func getDeliveryBatchByDedupeVia(
	ctx context.Context,
	q dbQueryer,
	agentName, dedupeKey string,
) (k12.DeliveryBatch, error) {
	root, err := scanDeliveryBatchRoot(q.QueryRowContext(ctx,
		`SELECT `+deliveryBatchColumns+` FROM k12_delivery_batches
         WHERE agent_name=? AND dedupe_key=?`,
		agentName, dedupeKey,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.DeliveryBatch{}, records.ErrNotFound
	}
	if err != nil {
		return k12.DeliveryBatch{}, fmt.Errorf("k12storage: get DeliveryBatch by dedupe: %w", err)
	}
	return attachDeliveryBatchReceiptsVia(ctx, q, root)
}

func (s *Store) GetDeliveryBatch(ctx context.Context, agentName, batchID string) (k12.DeliveryBatch, error) {
	return getDeliveryBatchVia(
		ctx, s.db, strings.TrimSpace(agentName), strings.TrimSpace(batchID),
	)
}

// FailDeliveryBatchPreparationIfIncomplete 在任一媒体 part 仍未准备时，原子地把
// 尚未跨越可见发送边界的 children 标记为明确失败。全部资源已就绪时不写入。
func (s *Store) FailDeliveryBatchPreparationIfIncomplete(
	ctx context.Context,
	agentName, batchID, detail string,
) (k12.DeliveryBatch, bool, error) {
	agentName = strings.TrimSpace(agentName)
	batchID = strings.TrimSpace(batchID)
	detail = strings.TrimSpace(detail)
	if agentName == "" || batchID == "" || detail == "" {
		return k12.DeliveryBatch{}, false, fmt.Errorf("k12storage: batch preparation failure requires owner, batch and detail")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE k12_delivery_receipts SET
		status=CASE WHEN status='pending' THEN 'failed' ELSE status END,
		last_error=?,updated_at=?
		WHERE agent_name=? AND batch_id=?
		  AND (status='pending' OR (status='failed' AND attempt=0))
		  AND EXISTS (
			SELECT 1 FROM k12_delivery_receipts missing
			WHERE missing.agent_name=? AND missing.batch_id=?
			  AND missing.part_kind='artifact' AND missing.prepared_resource_id=''
		  )`,
		detail, nowUnix(), agentName, batchID, agentName, batchID,
	)
	if err != nil {
		return k12.DeliveryBatch{}, false, fmt.Errorf("k12storage: fail incomplete delivery batch preparation: %w", err)
	}
	batch, getErr := s.GetDeliveryBatch(ctx, agentName, batchID)
	if getErr != nil {
		return k12.DeliveryBatch{}, false, getErr
	}
	incomplete := false
	for _, receipt := range batch.Receipts {
		if receipt.PartKind == messagecontent.PartArtifact && receipt.PreparedResourceID == "" {
			incomplete = true
			if receipt.Status != k12.DeliveryPending && receipt.Status != k12.DeliveryFailed {
				return batch, true, fmt.Errorf("%w: unprepared artifact part %s already crossed send boundary", ErrDeliveryBatchConflict, receipt.DeliveryID)
			}
		}
	}
	if incomplete {
		if changed, _ := res.RowsAffected(); changed == 0 {
			return batch, true, fmt.Errorf("%w: incomplete delivery batch has no retryable children", ErrDeliveryBatchConflict)
		}
		return batch, true, nil
	}
	return batch, false, nil
}

func getDeliveryBatchVia(
	ctx context.Context,
	q dbQueryer,
	agentName, batchID string,
) (k12.DeliveryBatch, error) {
	root, err := scanDeliveryBatchRoot(q.QueryRowContext(ctx,
		`SELECT `+deliveryBatchColumns+` FROM k12_delivery_batches
         WHERE agent_name=? AND batch_id=?`,
		agentName, batchID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.DeliveryBatch{}, records.ErrNotFound
	}
	if err != nil {
		return k12.DeliveryBatch{}, fmt.Errorf("k12storage: get DeliveryBatch: %w", err)
	}
	return attachDeliveryBatchReceiptsVia(ctx, q, root)
}

func (s *Store) attachDeliveryBatchReceipts(ctx context.Context, batch k12.DeliveryBatch) (k12.DeliveryBatch, error) {
	return attachDeliveryBatchReceiptsVia(ctx, s.db, batch)
}

func attachDeliveryBatchReceiptsVia(
	ctx context.Context,
	q dbQueryer,
	batch k12.DeliveryBatch,
) (k12.DeliveryBatch, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+deliveryReceiptColumns+`
        FROM k12_delivery_receipts WHERE agent_name=? AND batch_id=?
        ORDER BY batch_ordinal,delivery_id`, batch.AgentName, batch.BatchID)
	if err != nil {
		return k12.DeliveryBatch{}, fmt.Errorf("k12storage: list DeliveryBatch children: %w", err)
	}
	defer rows.Close()
	batch.Receipts = nil
	for rows.Next() {
		receipt, scanErr := scanDeliveryReceipt(rows)
		if scanErr != nil {
			return k12.DeliveryBatch{}, scanErr
		}
		batch.Receipts = append(batch.Receipts, receipt)
		if receipt.UpdatedAt > batch.UpdatedAt {
			batch.UpdatedAt = receipt.UpdatedAt
		}
	}
	if err := rows.Err(); err != nil {
		return k12.DeliveryBatch{}, err
	}
	if len(batch.Receipts) == 0 {
		return k12.DeliveryBatch{}, fmt.Errorf(
			"%w: DeliveryBatch %s has no child receipts", ErrDeliveryBatchConflict, batch.BatchID,
		)
	}
	batch.Status = k12.DeliveryBatchStatusOf(batch.Receipts)
	return batch, nil
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
