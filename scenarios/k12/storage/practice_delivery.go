package k12storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// PracticeDeliveryBatchFactory freezes the exact question-paper payload for
// the transaction-reserved paper number. Implementations must be
// side-effect-free: provider calls are only legal after this transaction
// commits.
type PracticeDeliveryBatchFactory func(
	context.Context,
	k12.PracticeSetFields,
) (k12.DeliveryBatch, error)

// FinalizePracticeSetWithDeliveryBatch makes the PracticeSet finalization,
// paper-number reservation, DeliveryBatch root and every child receipt one
// atomic durability boundary. A concurrent replay returns the already-linked
// frozen batch and never invokes build.
func (s *Store) FinalizePracticeSetWithDeliveryBatch(
	ctx context.Context,
	agentName, setID string,
	expectedVersion int,
	at int64,
	base k12.PracticeSetFields,
	build PracticeDeliveryBatchFactory,
) (k12.DeliveryBatch, bool, error) {
	agentName = strings.TrimSpace(agentName)
	setID = strings.TrimSpace(setID)
	if agentName == "" || setID == "" || expectedVersion < 0 || at <= 0 || build == nil {
		return k12.DeliveryBatch{}, false, fmt.Errorf(
			"k12storage: atomic PracticeSet delivery requires owner/set/version/time/factory",
		)
	}
	if err := ensureAgentRegistered(ctx, s.db, agentName); err != nil {
		return k12.DeliveryBatch{}, false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.DeliveryBatch{}, false, fmt.Errorf(
			"k12storage: begin atomic PracticeSet delivery: %w", err,
		)
	}
	defer tx.Rollback()

	// SQLite transactions are deferred. Make the first transactional statement
	// a scoped write so concurrent finalizers serialize before either reads the
	// draft or reserves a learner-local paper number.
	locked, err := tx.ExecContext(ctx, `UPDATE k12_practice_sets SET version=version
        WHERE record_id=? AND agent_name=?`, setID, agentName)
	if err != nil {
		return k12.DeliveryBatch{}, false, fmt.Errorf(
			"k12storage: lock PracticeSet delivery source: %w", err,
		)
	}
	if n, _ := locked.RowsAffected(); n == 0 {
		return k12.DeliveryBatch{}, false, records.ErrNotFound
	}

	cur, err := s.getVia(ctx, tx, setID)
	if err != nil {
		return k12.DeliveryBatch{}, false, err
	}
	if cur.AgentName != agentName || cur.Collection != k12.CollectionPracticeSet {
		return k12.DeliveryBatch{}, false, records.ErrNotFound
	}
	currentFields, err := k12.ParsePracticeSetFields(cur.Fields)
	if err != nil {
		return k12.DeliveryBatch{}, false, fmt.Errorf(
			"k12storage: parse PracticeSet delivery source: %w", err,
		)
	}
	if cur.Status == k12.PracticeStatusAssigned &&
		currentFields.FinalizedVia == "send" &&
		currentFields.DeliveryBatchID != "" {
		batch, getErr := getDeliveryBatchVia(
			ctx, tx, agentName, currentFields.DeliveryBatchID,
		)
		return batch, true, getErr
	}
	if cur.Status != k12.PracticeStatusDraft || cur.Version != expectedVersion {
		return k12.DeliveryBatch{}, false, records.ErrVersionConflict
	}

	paperNo, err := reservePracticePaperNoTx(ctx, tx, agentName, at)
	if err != nil {
		return k12.DeliveryBatch{}, false, err
	}
	base.PaperNo = paperNo
	base.FinalizedAt = at
	base.FinalizedVia = "send"
	base.DeliveryStatus = string(k12.DeliveryPending)
	base.DeliveryBatchID = ""
	base.DeliveryTarget = ""

	batch, err := build(ctx, base)
	if err != nil {
		return k12.DeliveryBatch{}, false, err
	}
	batch, err = normalizeDeliveryBatch(batch)
	if err != nil {
		return k12.DeliveryBatch{}, false, err
	}
	if batch.AgentName != agentName ||
		batch.ObjectKind != "practice_set_question" ||
		batch.ObjectID != setID {
		return k12.DeliveryBatch{}, false, fmt.Errorf(
			"k12storage: PracticeSet delivery batch owner/object mismatch",
		)
	}
	base.DeliveryBatchID = batch.BatchID

	fieldsJSON, err := json.Marshal(base)
	if err != nil {
		return k12.DeliveryBatch{}, false, fmt.Errorf(
			"k12storage: marshal finalized PracticeSet: %w", err,
		)
	}
	schema, err := s.registry.Get(k12.CollectionPracticeSet)
	if err != nil {
		return k12.DeliveryBatch{}, false, err
	}
	if schema.ValidateFields != nil {
		if err := schema.ValidateFields(string(fieldsJSON)); err != nil {
			return k12.DeliveryBatch{}, false, fmt.Errorf(
				"%w: PracticeSet atomic delivery: %v", records.ErrInvalidFields, err,
			)
		}
	}
	mp, err := s.mapperFor(k12.CollectionPracticeSet)
	if err != nil {
		return k12.DeliveryBatch{}, false, err
	}
	domainVals, err := mp.encode(string(fieldsJSON))
	if err != nil {
		return k12.DeliveryBatch{}, false, err
	}

	if created, err := insertDeliveryBatchRoot(ctx, tx, batch, false); err != nil {
		return k12.DeliveryBatch{}, false, err
	} else if !created {
		return k12.DeliveryBatch{}, false, fmt.Errorf(
			"%w: PracticeSet delivery batch root was not inserted", ErrDeliveryBatchConflict,
		)
	}

	assigns := make([]string, 0, len(mp.domainCols()))
	for _, col := range mp.domainCols() {
		assigns = append(assigns, col+"=?")
	}
	dedupeSet, dedupeArgs := dedupeReleaseAssign(
		schema, cur, k12.PracticeStatusConfirmed,
	)
	updateConfirmed := fmt.Sprintf(`UPDATE %s SET status=?%s, %s,
        version=version+1, updated_at=?
        WHERE record_id=? AND agent_name=? AND status=? AND version=?`,
		mp.table(), dedupeSet, strings.Join(assigns, ", "))
	args := []any{k12.PracticeStatusConfirmed}
	args = append(args, dedupeArgs...)
	args = append(args, domainVals...)
	args = append(args, at, setID, agentName, k12.PracticeStatusDraft, expectedVersion)
	updated, err := tx.ExecContext(ctx, updateConfirmed, args...)
	if err != nil {
		return k12.DeliveryBatch{}, false, fmt.Errorf(
			"k12storage: finalize PracticeSet delivery: %w", err,
		)
	}
	if n, _ := updated.RowsAffected(); n == 0 {
		return k12.DeliveryBatch{}, false, records.ErrVersionConflict
	}
	if err := mp.syncChildren(ctx, tx, setID, string(fieldsJSON)); err != nil {
		return k12.DeliveryBatch{}, false, err
	}

	updated, err = tx.ExecContext(ctx, `UPDATE k12_practice_sets SET
        status=?,version=version+1,updated_at=?
        WHERE record_id=? AND agent_name=? AND status=? AND version=?`,
		k12.PracticeStatusAssigned, at, setID, agentName,
		k12.PracticeStatusConfirmed, expectedVersion+1,
	)
	if err != nil {
		return k12.DeliveryBatch{}, false, fmt.Errorf(
			"k12storage: assign PracticeSet delivery: %w", err,
		)
	}
	if n, _ := updated.RowsAffected(); n == 0 {
		return k12.DeliveryBatch{}, false, records.ErrVersionConflict
	}

	if err := insertDeliveryBatchChildren(ctx, tx, batch); err != nil {
		return k12.DeliveryBatch{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return k12.DeliveryBatch{}, false, fmt.Errorf(
			"k12storage: commit atomic PracticeSet delivery: %w", err,
		)
	}
	batch.Status = k12.DeliveryBatchStatusOf(batch.Receipts)
	return batch, false, nil
}
