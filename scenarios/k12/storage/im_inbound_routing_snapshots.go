package k12storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
)

func inboundRoutingSnapshotDigest(candidates []InboundPhotoRoutingCandidate) (string, string, error) {
	if len(candidates) == 0 {
		return strings.Repeat("0", 64), "[]", nil
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.PracticeSetID) == "" ||
			strings.TrimSpace(candidate.PaperNo) == "" || candidate.SentAt <= 0 {
			return "", "", fmt.Errorf("k12storage: routing candidate identity is incomplete")
		}
	}
	raw, err := json.Marshal(candidates)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), string(raw), nil
}

func inboundRoutingRequestDigest(commandDigest, snapshotDigest string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(commandDigest) + "\x00" + strings.TrimSpace(snapshotDigest)))
	return hex.EncodeToString(sum[:])
}

func validateInboundRoutingRequestDigest(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("k12storage: routing request digest is invalid")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("k12storage: routing request digest is invalid: %w", err)
	}
	return nil
}

// validateInboundRoutingRequestDigestBinding 校验已持久化的请求摘要仍绑定原始入站命令
// 与冻结候选摘要；旧版本未写入该字段的记录保留只读兼容，但新记录始终要求精确匹配。
func validateInboundRoutingRequestDigestBinding(bundle InboundPhotoBundle, snapshot InboundPhotoRoutingSnapshot) error {
	requestDigest := strings.TrimSpace(snapshot.RequestDigest)
	if requestDigest == "" {
		return nil
	}
	expected := inboundRoutingRequestDigest(bundle.Receipt.CommandDigest, snapshot.SnapshotDigest)
	if requestDigest != expected {
		return fmt.Errorf("k12storage: routing request digest does not match inbound command")
	}
	return nil
}

func scanInboundPhotoRoutingSnapshot(row interface{ Scan(...any) error }) (InboundPhotoRoutingSnapshot, error) {
	var snapshot InboundPhotoRoutingSnapshot
	var stage, candidatesJSON string
	if err := row.Scan(
		&snapshot.ReceiptID, &stage, &snapshot.SnapshotDigest, &snapshot.RequestDigest, &candidatesJSON,
		&snapshot.SelectedPracticeSetID, &snapshot.SelectionDigest, &snapshot.Version,
		&snapshot.CreatedAt, &snapshot.UpdatedAt,
	); err != nil {
		return InboundPhotoRoutingSnapshot{}, err
	}
	snapshot.Stage = InboundPhotoRoutingStage(stage)
	if !json.Valid([]byte(candidatesJSON)) {
		return InboundPhotoRoutingSnapshot{}, fmt.Errorf("k12storage: invalid routing candidate snapshot")
	}
	if err := json.Unmarshal([]byte(candidatesJSON), &snapshot.Candidates); err != nil {
		return InboundPhotoRoutingSnapshot{}, fmt.Errorf("k12storage: decode routing candidate snapshot: %w", err)
	}
	if err := validateInboundRoutingRequestDigest(snapshot.RequestDigest); err != nil {
		return InboundPhotoRoutingSnapshot{}, err
	}
	if err := validateInboundPhotoRoutingSnapshot(snapshot); err != nil {
		return InboundPhotoRoutingSnapshot{}, err
	}
	return snapshot, nil
}

func validateInboundPhotoRoutingSnapshot(snapshot InboundPhotoRoutingSnapshot) error {
	if snapshot.Stage != InboundPhotoRoutingStageIntent && snapshot.Stage != InboundPhotoRoutingStageCandidate {
		return fmt.Errorf("k12storage: invalid routing snapshot stage")
	}
	if snapshot.Stage == InboundPhotoRoutingStageCandidate && len(snapshot.Candidates) < 2 {
		return fmt.Errorf("k12storage: candidate routing snapshot needs at least two candidates")
	}
	digest, _, err := inboundRoutingSnapshotDigest(snapshot.Candidates)
	if err != nil {
		return err
	}
	if snapshot.SnapshotDigest != "" && snapshot.SnapshotDigest != digest {
		return fmt.Errorf("k12storage: routing snapshot digest mismatch")
	}
	if err := validateInboundRoutingRequestDigest(snapshot.RequestDigest); err != nil {
		return err
	}
	return nil
}

const inboundPhotoRoutingSnapshotSelect = `SELECT receipt_id,stage,snapshot_digest,
request_digest,candidates_json,selected_practice_set_id,selection_digest,version,created_at,updated_at
FROM k12_im_inbound_routing_snapshots`

// SaveInboundPhotoRoutingSnapshot 在同一事务内冻结候选快照并把入站调度推进为 waiting。
// 快照摘要和 dispatch version 共同组成恢复边界，重放同一快照不会重复递增版本。
func (s *Store) SaveInboundPhotoRoutingSnapshot(
	ctx context.Context,
	agentName, receiptID string,
	expectedVersion int64,
	snapshot InboundPhotoRoutingSnapshot,
) (InboundPhotoDispatch, error) {
	agentName, receiptID = strings.TrimSpace(agentName), strings.TrimSpace(receiptID)
	if agentName == "" || receiptID == "" || expectedVersion < 0 {
		return InboundPhotoDispatch{}, fmt.Errorf("k12storage: routing snapshot identity is incomplete")
	}
	if err := validateInboundPhotoRoutingSnapshot(snapshot); err != nil {
		return InboundPhotoDispatch{}, err
	}
	digest, candidatesJSON, err := inboundRoutingSnapshotDigest(snapshot.Candidates)
	if err != nil {
		return InboundPhotoDispatch{}, err
	}
	now := nowUnix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InboundPhotoDispatch{}, err
	}
	defer tx.Rollback()
	bundle, err := getInboundPhotoByReceipt(ctx, tx, agentName, receiptID)
	if err != nil {
		if err == sql.ErrNoRows {
			return InboundPhotoDispatch{}, records.ErrNotFound
		}
		return InboundPhotoDispatch{}, err
	}
	if bundle.Dispatch.Version != expectedVersion {
		return InboundPhotoDispatch{}, records.ErrVersionConflict
	}
	requestDigest := inboundRoutingRequestDigest(bundle.Receipt.CommandDigest, digest)
	if snapshot.RequestDigest != "" && snapshot.RequestDigest != requestDigest {
		return InboundPhotoDispatch{}, fmt.Errorf("k12storage: routing request digest mismatch")
	}
	snapshot.RequestDigest = requestDigest
	stored, storedErr := scanInboundPhotoRoutingSnapshot(tx.QueryRowContext(ctx, inboundPhotoRoutingSnapshotSelect+`
	WHERE receipt_id=?`, receiptID))
	if storedErr == nil {
		if stored.SnapshotDigest == digest && bundle.Dispatch.RoutingDecision == InboundPhotoRouteAskedUser {
			if stored.RequestDigest != "" && stored.RequestDigest != requestDigest {
				return InboundPhotoDispatch{}, fmt.Errorf("k12storage: stored routing request digest drifted")
			}
			// 第一阶段只冻结意图；收到“重新批改”后可在同一 dispatch
			// version 内把已冻结候选切换为展示阶段，禁止回退覆盖候选快照。
			if stored.Stage != snapshot.Stage {
				if stored.Stage == InboundPhotoRoutingStageIntent &&
					snapshot.Stage == InboundPhotoRoutingStageCandidate {
					if _, err := tx.ExecContext(ctx, `UPDATE k12_im_inbound_routing_snapshots SET
					stage=?,request_digest=?,updated_at=? WHERE receipt_id=? AND version=?`,
						snapshot.Stage, requestDigest, now, receiptID, stored.Version); err != nil {
						return InboundPhotoDispatch{}, err
					}
				} else if stored.Stage == InboundPhotoRoutingStageCandidate &&
					snapshot.Stage == InboundPhotoRoutingStageIntent {
					// 已进入候选阶段后，重复的第一阶段请求保持当前冻结事实。
				} else {
					return InboundPhotoDispatch{}, fmt.Errorf("k12storage: routing snapshot stage cannot regress")
				}
			}
			if stored.RequestDigest == "" {
				if _, err := tx.ExecContext(ctx, `UPDATE k12_im_inbound_routing_snapshots SET
					request_digest=?,updated_at=? WHERE receipt_id=? AND version=?`,
					requestDigest, now, receiptID, stored.Version); err != nil {
					return InboundPhotoDispatch{}, err
				}
			}
			if err := tx.Commit(); err != nil {
				return InboundPhotoDispatch{}, err
			}
			return bundle.Dispatch, nil
		}
		if stored.SnapshotDigest != digest {
			return InboundPhotoDispatch{}, fmt.Errorf("k12storage: routing snapshot is immutable")
		}
	} else if storedErr != sql.ErrNoRows {
		return InboundPhotoDispatch{}, storedErr
	}
	next := bundle.Dispatch.State()
	next.RoutingDecision = InboundPhotoRouteAskedUser
	next.ConfirmationStatus = InboundPhotoConfirmationWaiting
	if err := validateInboundPhotoDispatchTransition(bundle.Dispatch, next); err != nil {
		return InboundPhotoDispatch{}, err
	}
	version := expectedVersion + 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_im_inbound_routing_snapshots
		(receipt_id,stage,snapshot_digest,request_digest,candidates_json,selected_practice_set_id,selection_digest,version,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(receipt_id) DO UPDATE SET stage=excluded.stage,snapshot_digest=excluded.snapshot_digest,
		request_digest=excluded.request_digest,candidates_json=excluded.candidates_json,selected_practice_set_id='',selection_digest='',
		version=excluded.version,updated_at=excluded.updated_at`,
		receiptID, snapshot.Stage, digest, requestDigest, candidatesJSON, "", "", version, now, now); err != nil {
		return InboundPhotoDispatch{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE k12_im_inbound_dispatches SET
		routing_decision=?,confirmation_status=?,version=version+1,updated_at=?
		WHERE receipt_id=? AND version=?`, next.RoutingDecision, next.ConfirmationStatus, now,
		receiptID, expectedVersion)
	if err != nil {
		return InboundPhotoDispatch{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return InboundPhotoDispatch{}, records.ErrVersionConflict
	}
	if err := tx.Commit(); err != nil {
		return InboundPhotoDispatch{}, err
	}
	bundle.Dispatch.InboundPhotoDispatchState = next
	bundle.Dispatch.Version = version
	bundle.Dispatch.UpdatedAt = now
	return bundle.Dispatch, nil
}

// GetInboundPhotoRoutingSnapshot 只按 agent+receipt 读取已冻结候选，不重新读取练习集列表。
func (s *Store) GetInboundPhotoRoutingSnapshot(
	ctx context.Context, agentName, receiptID string,
) (InboundPhotoRoutingSnapshot, error) {
	agentName, receiptID = strings.TrimSpace(agentName), strings.TrimSpace(receiptID)
	if agentName == "" || receiptID == "" {
		return InboundPhotoRoutingSnapshot{}, fmt.Errorf("k12storage: routing snapshot identity is incomplete")
	}
	bundle, err := s.GetInboundPhoto(ctx, agentName, receiptID)
	if err != nil {
		return InboundPhotoRoutingSnapshot{}, err
	}
	snapshot, err := scanInboundPhotoRoutingSnapshot(s.db.QueryRowContext(ctx,
		inboundPhotoRoutingSnapshotSelect+` WHERE receipt_id=?`, receiptID))
	if err == sql.ErrNoRows {
		return InboundPhotoRoutingSnapshot{}, records.ErrNotFound
	}
	if err != nil {
		return InboundPhotoRoutingSnapshot{}, err
	}
	if err := validateInboundRoutingRequestDigestBinding(bundle, snapshot); err != nil {
		return InboundPhotoRoutingSnapshot{}, err
	}
	return snapshot, nil
}

// ConfirmInboundPhotoRoutingSelection 按冻结快照中的内部 ID 完成二阶段选择，重复相同选择
// 返回原 dispatch；不同选择或已过期快照 fail-closed。
func (s *Store) ConfirmInboundPhotoRoutingSelection(
	ctx context.Context,
	agentName, receiptID string,
	expectedVersion int64,
	decision InboundPhotoRoutingDecision,
	practiceSetID string,
) (InboundPhotoDispatch, error) {
	agentName, receiptID, practiceSetID = strings.TrimSpace(agentName), strings.TrimSpace(receiptID), strings.TrimSpace(practiceSetID)
	if agentName == "" || receiptID == "" || expectedVersion < 0 || practiceSetID == "" ||
		(decision != InboundPhotoRouteRegrade && decision != InboundPhotoRouteNewSubmission) {
		return InboundPhotoDispatch{}, fmt.Errorf("k12storage: routing selection is incomplete")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InboundPhotoDispatch{}, err
	}
	defer tx.Rollback()
	bundle, err := getInboundPhotoByReceipt(ctx, tx, agentName, receiptID)
	if err != nil {
		if err == sql.ErrNoRows {
			return InboundPhotoDispatch{}, records.ErrNotFound
		}
		return InboundPhotoDispatch{}, err
	}
	if bundle.Dispatch.Version != expectedVersion {
		return InboundPhotoDispatch{}, records.ErrVersionConflict
	}
	snapshot, err := scanInboundPhotoRoutingSnapshot(tx.QueryRowContext(ctx, inboundPhotoRoutingSnapshotSelect+`
	WHERE receipt_id=?`, receiptID))
	if err == sql.ErrNoRows {
		return InboundPhotoDispatch{}, fmt.Errorf("%w: routing snapshot is missing", records.ErrIllegalTransition)
	}
	if err != nil {
		return InboundPhotoDispatch{}, err
	}
	if err := validateInboundRoutingRequestDigestBinding(bundle, snapshot); err != nil {
		return InboundPhotoDispatch{}, err
	}
	if snapshot.Stage != InboundPhotoRoutingStageCandidate {
		return InboundPhotoDispatch{}, fmt.Errorf("%w: candidate selection is not pending", records.ErrIllegalTransition)
	}
	if decision != InboundPhotoRouteRegrade {
		return InboundPhotoDispatch{}, fmt.Errorf("%w: candidate selection only supports regrade", records.ErrIllegalTransition)
	}
	found := false
	for _, candidate := range snapshot.Candidates {
		if candidate.PracticeSetID == practiceSetID {
			found = true
			break
		}
	}
	if !found {
		return InboundPhotoDispatch{}, fmt.Errorf("%w: routing candidate is not in frozen snapshot", records.ErrIllegalTransition)
	}
	if bundle.Dispatch.RoutingDecision != InboundPhotoRouteAskedUser ||
		bundle.Dispatch.ConfirmationStatus != InboundPhotoConfirmationWaiting {
		if snapshot.SelectedPracticeSetID == practiceSetID && bundle.Dispatch.RoutingDecision == decision &&
			bundle.Dispatch.ConfirmationStatus == InboundPhotoConfirmationConfirmed {
			if err := tx.Commit(); err != nil {
				return InboundPhotoDispatch{}, err
			}
			return bundle.Dispatch, nil
		}
		return InboundPhotoDispatch{}, fmt.Errorf("%w: inbound photo route is not waiting", records.ErrIllegalTransition)
	}
	next := bundle.Dispatch.State()
	next.RoutingDecision = decision
	next.ConfirmationStatus = InboundPhotoConfirmationConfirmed
	if err := validateInboundPhotoDispatchTransition(bundle.Dispatch, next); err != nil {
		return InboundPhotoDispatch{}, err
	}
	selectionSum := sha256.Sum256([]byte(string(decision) + "\x00" + practiceSetID + "\x00" + snapshot.SnapshotDigest))
	selectionDigest := hex.EncodeToString(selectionSum[:])
	now := nowUnix()
	version := expectedVersion + 1
	result, err := tx.ExecContext(ctx, `UPDATE k12_im_inbound_dispatches SET
		routing_decision=?,confirmation_status=?,version=version+1,updated_at=?
		WHERE receipt_id=? AND version=?`, next.RoutingDecision, next.ConfirmationStatus, now,
		receiptID, expectedVersion)
	if err != nil {
		return InboundPhotoDispatch{}, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return InboundPhotoDispatch{}, records.ErrVersionConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_im_inbound_routing_snapshots SET
		selected_practice_set_id=?,selection_digest=?,version=?,updated_at=? WHERE receipt_id=?`,
		practiceSetID, selectionDigest, version, now, receiptID); err != nil {
		return InboundPhotoDispatch{}, err
	}
	if err := tx.Commit(); err != nil {
		return InboundPhotoDispatch{}, err
	}
	bundle.Dispatch.InboundPhotoDispatchState = next
	bundle.Dispatch.Version = version
	bundle.Dispatch.UpdatedAt = now
	return bundle.Dispatch, nil
}
