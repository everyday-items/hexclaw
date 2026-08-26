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

func normalizeDeliveryGroupIdentity(agentName string, deliveryIDs []string) (string, []string, error) {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" || len(deliveryIDs) < 2 {
		return "", nil, fmt.Errorf("%w: creative delivery group requires owner, markdown and image", ErrDeliveryBatchConflict)
	}
	normalized := make([]string, len(deliveryIDs))
	seen := make(map[string]struct{}, len(deliveryIDs))
	for index, deliveryID := range deliveryIDs {
		deliveryID = strings.TrimSpace(deliveryID)
		if deliveryID == "" {
			return "", nil, fmt.Errorf("%w: creative delivery group contains an empty delivery id", ErrDeliveryBatchConflict)
		}
		if _, exists := seen[deliveryID]; exists {
			return "", nil, fmt.Errorf("%w: creative delivery group contains duplicate delivery id %s", ErrDeliveryBatchConflict, deliveryID)
		}
		seen[deliveryID] = struct{}{}
		normalized[index] = deliveryID
	}
	return agentName, normalized, nil
}

func getDeliveryGroupReceiptsVia(
	ctx context.Context,
	q dbQueryer,
	agentName string,
	deliveryIDs []string,
) ([]k12.DeliveryReceipt, error) {
	receipts := make([]k12.DeliveryReceipt, 0, len(deliveryIDs))
	for _, deliveryID := range deliveryIDs {
		receipt, err := scanDeliveryReceipt(q.QueryRowContext(ctx, `SELECT `+deliveryReceiptColumns+`
            FROM k12_delivery_receipts WHERE agent_name=? AND delivery_id=?`, agentName, deliveryID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: creative delivery group is missing delivery id %s", ErrDeliveryBatchConflict, deliveryID)
		}
		if err != nil {
			return nil, fmt.Errorf("k12storage: get creative delivery group receipt: %w", err)
		}
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}

func deliveryGroupMutableStateEqual(a, b k12.DeliveryReceipt) bool {
	return a.Status == b.Status && a.Attempt == b.Attempt &&
		a.ExternalMessageID == b.ExternalMessageID && a.LastError == b.LastError
}

func deliveryGroupImmutableIdentityEqual(a, b k12.DeliveryReceipt) bool {
	return a.BatchID == b.BatchID && a.AgentName == b.AgentName &&
		a.ObjectKind == b.ObjectKind && a.ObjectID == b.ObjectID &&
		a.BindingID == b.BindingID && a.Target == b.Target
}

func deliveryGroupFrozenSnapshotEqual(current, expected k12.DeliveryReceipt) bool {
	return current.DeliveryID == expected.DeliveryID &&
		deliveryIdentityEqual(current, expected) &&
		current.PreparedResourceID == expected.PreparedResourceID &&
		current.CreatedAt == expected.CreatedAt &&
		deliveryGroupMutableStateEqual(current, expected)
}

func deliveryGroupIDsFromSnapshot(
	agentName string,
	expected []k12.DeliveryReceipt,
) (string, []string, error) {
	deliveryIDs := make([]string, len(expected))
	for index, receipt := range expected {
		deliveryIDs[index] = receipt.DeliveryID
	}
	agentName, deliveryIDs, err := normalizeDeliveryGroupIdentity(agentName, deliveryIDs)
	if err != nil {
		return "", nil, err
	}
	return agentName, deliveryIDs, nil
}

func validateDeliveryGroupFrozenSnapshot(
	current []k12.DeliveryReceipt,
	expected []k12.DeliveryReceipt,
) error {
	if len(current) != len(expected) {
		return fmt.Errorf("%w: creative delivery group frozen snapshot size differs", ErrDeliveryBatchConflict)
	}
	for index := range current {
		if !deliveryGroupFrozenSnapshotEqual(current[index], expected[index]) {
			return fmt.Errorf("%w: creative delivery group frozen snapshot differs at ordinal %d", ErrDeliveryBatchConflict, index+1)
		}
	}
	return nil
}

func deliveryGroupBatchRootSnapshotEqual(current, expected k12.DeliveryBatch) bool {
	// GetDeliveryBatch 会把 UpdatedAt 投影为子回执的最大更新时间，不能把它当作 root 身份。
	return current.BatchID == expected.BatchID &&
		deliveryBatchIdentityEqual(current, expected) &&
		current.CreatedAt == expected.CreatedAt
}

func deliveryGroupBatchRootMatchesReceipt(batch k12.DeliveryBatch, receipt k12.DeliveryReceipt) bool {
	return batch.BatchID == receipt.BatchID && batch.AgentName == receipt.AgentName &&
		batch.ObjectKind == receipt.ObjectKind && batch.ObjectID == receipt.ObjectID
}

// validateCreativeDeliveryGroupVia 校验同一物理目标的完整 Markdown+图片信封。
// PDF 不属于该信封，继续保留独立组件回执与独立发送语义。
func validateCreativeDeliveryGroupVia(
	ctx context.Context,
	q dbQueryer,
	agentName string,
	deliveryIDs []string,
	receipts []k12.DeliveryReceipt,
) error {
	if len(receipts) != len(deliveryIDs) || len(receipts) < 2 {
		return fmt.Errorf("%w: creative delivery group is incomplete", ErrDeliveryBatchConflict)
	}
	first := receipts[0]
	if first.AgentName != agentName || first.BatchID == "" || first.ObjectKind != "creative_work" {
		return fmt.Errorf("%w: delivery group is not a frozen creative work batch", ErrDeliveryBatchConflict)
	}
	for index, receipt := range receipts {
		if receipt.DeliveryID != deliveryIDs[index] ||
			!deliveryGroupImmutableIdentityEqual(first, receipt) ||
			!deliveryGroupMutableStateEqual(first, receipt) {
			return fmt.Errorf("%w: creative delivery group identity or state is mixed", ErrDeliveryBatchConflict)
		}
	}

	rows, err := q.QueryContext(ctx, `SELECT `+deliveryReceiptColumns+`
        FROM k12_delivery_receipts
        WHERE agent_name=? AND batch_id=? AND binding_id=?
          AND platform=? AND instance_id=? AND chat_id=?
        ORDER BY part_ordinal,batch_ordinal,delivery_id`,
		agentName, first.BatchID, first.BindingID,
		first.Target.Platform, first.Target.InstanceID, first.Target.ChatID,
	)
	if err != nil {
		return fmt.Errorf("k12storage: list creative delivery group target: %w", err)
	}
	defer rows.Close()
	allTargetParts := make([]k12.DeliveryReceipt, 0, len(deliveryIDs)+1)
	for rows.Next() {
		receipt, scanErr := scanDeliveryReceipt(rows)
		if scanErr != nil {
			return scanErr
		}
		if !deliveryGroupImmutableIdentityEqual(first, receipt) {
			return fmt.Errorf("%w: creative delivery group target identity is mixed", ErrDeliveryBatchConflict)
		}
		allTargetParts = append(allTargetParts, receipt)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	expected := make([]k12.DeliveryReceipt, 0, len(allTargetParts))
	seenPDF := false
	for _, receipt := range allTargetParts {
		switch {
		case receipt.PartKind == messagecontent.PartMarkdown:
			if len(expected) != 0 || receipt.PartOrdinal != 1 || receipt.PartMIME != "" || seenPDF {
				return fmt.Errorf("%w: creative delivery group markdown order is invalid", ErrDeliveryBatchConflict)
			}
			expected = append(expected, receipt)
		case receipt.PartKind == messagecontent.PartArtifact && strings.HasPrefix(receipt.PartMIME, "image/"):
			if seenPDF || receipt.PartOrdinal != len(expected)+1 || receipt.PreparedResourceID == "" {
				return fmt.Errorf("%w: creative delivery group image order or prepared resource is invalid", ErrDeliveryBatchConflict)
			}
			expected = append(expected, receipt)
		case receipt.PartKind == messagecontent.PartArtifact && receipt.PartMIME == "application/pdf":
			seenPDF = true
		default:
			return fmt.Errorf("%w: creative delivery group contains an unsupported part", ErrDeliveryBatchConflict)
		}
	}
	if len(expected) < 2 || len(expected) != len(deliveryIDs) {
		return fmt.Errorf("%w: creative delivery group does not contain the exact markdown and image rows", ErrDeliveryBatchConflict)
	}
	for index := range expected {
		if expected[index].DeliveryID != deliveryIDs[index] {
			return fmt.Errorf("%w: creative delivery group order or membership differs", ErrDeliveryBatchConflict)
		}
	}
	return nil
}

func (s *Store) beginCreativeDeliveryGroupTx(
	ctx context.Context,
	agentName string,
	deliveryIDs []string,
	expectedBatch *k12.DeliveryBatch,
) (*sql.Tx, []k12.DeliveryReceipt, error) {
	var err error
	agentName, deliveryIDs, err = normalizeDeliveryGroupIdentity(agentName, deliveryIDs)
	if err != nil {
		return nil, nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("k12storage: begin creative delivery group transaction: %w", err)
	}
	// 首个数据库动作先取得写锁，避免并发调用在读快照后升级写锁。
	var locked sql.Result
	var batchRoot k12.DeliveryBatch
	if expectedBatch != nil {
		batchID := strings.TrimSpace(expectedBatch.BatchID)
		locked, err = tx.ExecContext(ctx, `UPDATE k12_delivery_batches SET batch_id=batch_id
            WHERE agent_name=? AND batch_id=?`, agentName, batchID)
		if err == nil {
			batchRoot, err = scanDeliveryBatchRoot(tx.QueryRowContext(ctx,
				`SELECT `+deliveryBatchColumns+` FROM k12_delivery_batches
                 WHERE agent_name=? AND batch_id=?`, agentName, batchID,
			))
		}
	} else {
		locked, err = tx.ExecContext(ctx, `UPDATE k12_delivery_receipts SET delivery_id=delivery_id
            WHERE agent_name=? AND delivery_id=?`, agentName, deliveryIDs[0])
	}
	if err != nil {
		tx.Rollback()
		if expectedBatch != nil && errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("%w: creative delivery group batch root is missing", ErrDeliveryBatchConflict)
		}
		return nil, nil, fmt.Errorf("k12storage: lock creative delivery group: %w", err)
	}
	if changed, _ := locked.RowsAffected(); changed != 1 {
		tx.Rollback()
		return nil, nil, fmt.Errorf("%w: creative delivery group anchor is missing", ErrDeliveryBatchConflict)
	}
	if expectedBatch != nil && !deliveryGroupBatchRootSnapshotEqual(batchRoot, *expectedBatch) {
		tx.Rollback()
		return nil, nil, fmt.Errorf("%w: creative delivery group batch root snapshot differs", ErrDeliveryBatchConflict)
	}
	receipts, err := getDeliveryGroupReceiptsVia(ctx, tx, agentName, deliveryIDs)
	if err != nil {
		tx.Rollback()
		return nil, nil, err
	}
	if err := validateCreativeDeliveryGroupVia(ctx, tx, agentName, deliveryIDs, receipts); err != nil {
		tx.Rollback()
		return nil, nil, err
	}
	if expectedBatch != nil && !deliveryGroupBatchRootMatchesReceipt(batchRoot, receipts[0]) {
		tx.Rollback()
		return nil, nil, fmt.Errorf("%w: creative delivery group batch root and children differ", ErrDeliveryBatchConflict)
	}
	return tx, receipts, nil
}

func deliveryGroupPlaceholders(size int) string {
	return strings.TrimSuffix(strings.Repeat("?,", size), ",")
}

func updateCreativeDeliveryGroupTx(
	ctx context.Context,
	tx *sql.Tx,
	agentName string,
	deliveryIDs []string,
	current []k12.DeliveryReceipt,
	setClause string,
	setArgs ...any,
) ([]k12.DeliveryReceipt, error) {
	first := current[0]
	args := make([]any, 0, len(setArgs)+len(deliveryIDs)+6)
	args = append(args, setArgs...)
	args = append(args, agentName)
	for _, deliveryID := range deliveryIDs {
		args = append(args, deliveryID)
	}
	args = append(args, first.Status, first.Attempt, first.ExternalMessageID, first.LastError)
	res, err := tx.ExecContext(ctx, `UPDATE k12_delivery_receipts SET `+setClause+`
        WHERE agent_name=? AND delivery_id IN (`+deliveryGroupPlaceholders(len(deliveryIDs))+`)
          AND status=? AND attempt=? AND external_message_id=? AND last_error=?`, args...)
	if err != nil {
		return nil, fmt.Errorf("k12storage: update creative delivery group: %w", err)
	}
	if changed, _ := res.RowsAffected(); changed != int64(len(deliveryIDs)) {
		return nil, fmt.Errorf("%w: creative delivery group compare-and-swap lost", ErrDeliveryBatchConflict)
	}
	updated, err := getDeliveryGroupReceiptsVia(ctx, tx, agentName, deliveryIDs)
	if err != nil {
		return nil, err
	}
	if err := validateCreativeDeliveryGroupVia(ctx, tx, agentName, deliveryIDs, updated); err != nil {
		return nil, err
	}
	return updated, nil
}

func commitCreativeDeliveryGroupTx(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("k12storage: commit creative delivery group: %w", err)
	}
	return nil
}

// BeginDeliveryGroupAttempt 原子开启同一创作作品 Markdown+图片信封的一次发送。
// expected 是发送前校验通过的有序冻结快照；事务内必须逐字段一致后才能改变可变状态。
func (s *Store) BeginDeliveryGroupAttempt(
	ctx context.Context,
	expectedBatch k12.DeliveryBatch,
	expected []k12.DeliveryReceipt,
) ([]k12.DeliveryReceipt, bool, error) {
	agentName, deliveryIDs, err := deliveryGroupIDsFromSnapshot(expectedBatch.AgentName, expected)
	if err != nil {
		return nil, false, err
	}
	tx, receipts, err := s.beginCreativeDeliveryGroupTx(ctx, agentName, deliveryIDs, &expectedBatch)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if err := validateDeliveryGroupFrozenSnapshot(receipts, expected); err != nil {
		return nil, false, err
	}
	first := receipts[0]
	if first.Status == k12.DeliverySending {
		return receipts, false, nil
	}
	if first.Status != k12.DeliveryPending && first.Status != k12.DeliveryFailed {
		return nil, false, fmt.Errorf("%w: creative delivery group status %s cannot send", ErrDeliveryBatchConflict, first.Status)
	}
	updated, err := updateCreativeDeliveryGroupTx(
		ctx, tx, agentName, deliveryIDs, expected,
		"status='sending',attempt=attempt+1,external_message_id='',last_error='',updated_at=?",
		nowUnix(),
	)
	if err != nil {
		return nil, false, err
	}
	if err := commitCreativeDeliveryGroupTx(tx); err != nil {
		return nil, false, err
	}
	return updated, true, nil
}

// MarkDeliveryGroupAccepted 为组内所有组件记录同一个 provider 外部消息标识。
func (s *Store) MarkDeliveryGroupAccepted(
	ctx context.Context,
	agentName string,
	deliveryIDs []string,
	externalMessageID string,
) ([]k12.DeliveryReceipt, error) {
	externalMessageID = strings.TrimSpace(externalMessageID)
	if externalMessageID == "" {
		return nil, fmt.Errorf("k12storage: accepted delivery group requires external_message_id")
	}
	agentName, deliveryIDs, err := normalizeDeliveryGroupIdentity(agentName, deliveryIDs)
	if err != nil {
		return nil, err
	}
	tx, receipts, err := s.beginCreativeDeliveryGroupTx(ctx, agentName, deliveryIDs, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	first := receipts[0]
	if first.Status != k12.DeliverySending {
		return nil, fmt.Errorf("%w: creative delivery group status %s cannot accept", ErrDeliveryBatchConflict, first.Status)
	}
	if first.ExternalMessageID == externalMessageID {
		return receipts, nil
	}
	if first.ExternalMessageID != "" {
		return nil, fmt.Errorf("%w: creative delivery group external id differs", ErrDeliveryBatchConflict)
	}
	updated, err := updateCreativeDeliveryGroupTx(
		ctx, tx, agentName, deliveryIDs, receipts,
		"external_message_id=?,updated_at=?", externalMessageID, nowUnix(),
	)
	if err != nil {
		return nil, err
	}
	if err := commitCreativeDeliveryGroupTx(tx); err != nil {
		return nil, err
	}
	return updated, nil
}

// MarkDeliveryGroupDelivered 原子收敛组内所有组件为 delivered。
func (s *Store) MarkDeliveryGroupDelivered(
	ctx context.Context,
	agentName string,
	deliveryIDs []string,
) ([]k12.DeliveryReceipt, error) {
	return s.markCreativeDeliveryGroupTerminal(ctx, agentName, deliveryIDs, k12.DeliveryDelivered, "")
}

// MarkDeliveryGroupFailed 原子收敛组内所有组件为 failed。
func (s *Store) MarkDeliveryGroupFailed(
	ctx context.Context,
	agentName string,
	deliveryIDs []string,
	detail string,
) ([]k12.DeliveryReceipt, error) {
	return s.markCreativeDeliveryGroupTerminal(ctx, agentName, deliveryIDs, k12.DeliveryFailed, detail)
}

// MarkDeliveryGroupOutcomeUnknown 原子记录组级发送结果未知，后续只能查询收敛。
func (s *Store) MarkDeliveryGroupOutcomeUnknown(
	ctx context.Context,
	agentName string,
	deliveryIDs []string,
	detail string,
) ([]k12.DeliveryReceipt, error) {
	return s.markCreativeDeliveryGroupTerminal(ctx, agentName, deliveryIDs, k12.DeliveryOutcomeUnknown, detail)
}

func (s *Store) markCreativeDeliveryGroupTerminal(
	ctx context.Context,
	agentName string,
	deliveryIDs []string,
	status k12.DeliveryReceiptStatus,
	detail string,
) ([]k12.DeliveryReceipt, error) {
	detail = strings.TrimSpace(detail)
	if (status == k12.DeliveryFailed || status == k12.DeliveryOutcomeUnknown) && detail == "" {
		return nil, fmt.Errorf("k12storage: %s delivery group requires detail", status)
	}
	agentName, deliveryIDs, err := normalizeDeliveryGroupIdentity(agentName, deliveryIDs)
	if err != nil {
		return nil, err
	}
	tx, receipts, err := s.beginCreativeDeliveryGroupTx(ctx, agentName, deliveryIDs, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	first := receipts[0]
	if first.Status == status {
		if status == k12.DeliveryDelivered || first.LastError == detail {
			return receipts, nil
		}
		return nil, fmt.Errorf("%w: creative delivery group terminal detail differs", ErrDeliveryBatchConflict)
	}
	if first.Status != k12.DeliverySending &&
		!(status == k12.DeliveryDelivered && first.Status == k12.DeliveryOutcomeUnknown) {
		return nil, fmt.Errorf("%w: creative delivery group status %s cannot become %s", ErrDeliveryBatchConflict, first.Status, status)
	}
	if status == k12.DeliveryDelivered && first.ExternalMessageID == "" {
		return nil, fmt.Errorf("%w: delivered creative delivery group requires external_message_id", ErrDeliveryBatchConflict)
	}
	setClause := "status=?,last_error=?,updated_at=?"
	lastError := detail
	if status == k12.DeliveryDelivered {
		lastError = ""
	}
	updated, err := updateCreativeDeliveryGroupTx(
		ctx, tx, agentName, deliveryIDs, receipts,
		setClause, status, lastError, nowUnix(),
	)
	if err != nil {
		return nil, err
	}
	if err := commitCreativeDeliveryGroupTx(tx); err != nil {
		return nil, err
	}
	return updated, nil
}

// ReconcileDeliveryGroup 用一次 provider 查询证据原子收敛整个创作作品信封。
func (s *Store) ReconcileDeliveryGroup(
	ctx context.Context,
	agentName string,
	deliveryIDs []string,
	status k12.DeliveryReceiptStatus,
	externalMessageID string,
	detail string,
) ([]k12.DeliveryReceipt, error) {
	externalMessageID = strings.TrimSpace(externalMessageID)
	detail = strings.TrimSpace(detail)
	if status != k12.DeliveryDelivered && status != k12.DeliveryFailed && status != k12.DeliverySending {
		return nil, fmt.Errorf("k12storage: unsupported delivery group reconciliation status %q", status)
	}
	if status == k12.DeliveryFailed && detail == "" {
		return nil, fmt.Errorf("k12storage: failed delivery group reconciliation requires detail")
	}
	agentName, deliveryIDs, err := normalizeDeliveryGroupIdentity(agentName, deliveryIDs)
	if err != nil {
		return nil, err
	}
	tx, receipts, err := s.beginCreativeDeliveryGroupTx(ctx, agentName, deliveryIDs, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	first := receipts[0]
	if first.Status == status && (status == k12.DeliveryDelivered || status == k12.DeliveryFailed) {
		if externalMessageID == "" || first.ExternalMessageID == "" || first.ExternalMessageID == externalMessageID {
			return receipts, nil
		}
		return nil, fmt.Errorf("%w: creative delivery group reconciliation external id differs", ErrDeliveryBatchConflict)
	}
	if first.Status != k12.DeliveryOutcomeUnknown && first.Status != k12.DeliverySending {
		return nil, fmt.Errorf("%w: creative delivery group status %s cannot reconcile", ErrDeliveryBatchConflict, first.Status)
	}
	if first.ExternalMessageID != "" && externalMessageID != "" &&
		first.ExternalMessageID != externalMessageID {
		return nil, fmt.Errorf("%w: creative delivery group reconciliation external id differs", ErrDeliveryBatchConflict)
	}
	if externalMessageID == "" {
		externalMessageID = first.ExternalMessageID
	}
	if (status == k12.DeliveryDelivered || status == k12.DeliverySending) && externalMessageID == "" {
		return nil, fmt.Errorf("k12storage: %s delivery group reconciliation requires external_message_id", status)
	}
	lastError := detail
	if status == k12.DeliveryDelivered || status == k12.DeliverySending {
		lastError = ""
	}
	updated, err := updateCreativeDeliveryGroupTx(
		ctx, tx, agentName, deliveryIDs, receipts,
		"status=?,external_message_id=?,last_error=?,updated_at=?",
		status, externalMessageID, lastError, nowUnix(),
	)
	if err != nil {
		return nil, err
	}
	if err := commitCreativeDeliveryGroupTx(tx); err != nil {
		return nil, err
	}
	return updated, nil
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
