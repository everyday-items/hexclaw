package k12storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hexagon-codes/toolkit/util/idgen"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type PracticeCandidateOpenInput struct {
	AgentName         string
	SourceMistakeID   string
	IdempotencyKey    string
	Grade             string
	Textbook          string
	SourceSession     string
	RouteSnapshotJSON string
}

type PracticeCandidateCommitInput struct {
	AgentName      string
	SelectionID    string
	Revision       int
	CandidateIDs   []string
	IdempotencyKey string
}

type PracticeCandidateCommitReceipt struct {
	Selection      k12.PracticeCandidateSelection `json:"selection"`
	AddedCount     int                            `json:"added_count"`
	AlreadyPresent []string                       `json:"already_present"`
	Replayed       bool                           `json:"replayed"`
}

type MistakeReviewCommandInput struct {
	AgentName       string
	MistakeRecordID string
	ExpectedVersion int
	IdempotencyKey  string
	CommandType     string
	ISOYear         int
	ISOWeek         int
	PlanID          string
	PlanRevision    int
	WeeklyItemID    string
}

type MistakeReviewCommandResult struct {
	State          string                 `json:"state"`
	MistakeVersion int                    `json:"mistake_version"`
	Replayed       bool                   `json:"replayed"`
	Review         k12.MistakeReviewState `json:"review"`
}

type candidateSelectionQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func stableRequestDigest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (s *Store) findOrCreatePracticeBasket(
	ctx context.Context,
	agentName, sourceSession string,
) (*records.AgentRecord, error) {
	recordsInScope, err := s.ListByScope(ctx, agentName, k12.CollectionPracticeSet, k12.PracticeStatusDraft)
	if err != nil {
		return nil, err
	}
	if len(recordsInScope) > 0 {
		sort.SliceStable(recordsInScope, func(i, j int) bool {
			return recordsInScope[i].CreatedAt < recordsInScope[j].CreatedAt
		})
		return recordsInScope[0], nil
	}
	record, err := k12.NewPracticeSetRecord(agentName, sourceSession, k12.PracticeSetFields{
		SourceKind: "mixed",
		Title:      "练习集",
		Items:      []k12.PracticeItem{},
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.Put(ctx, record); err != nil {
		return nil, err
	}
	return record, nil
}

func (s *Store) OpenPracticeCandidateSelection(
	ctx context.Context,
	in PracticeCandidateOpenInput,
) (k12.PracticeCandidateSelection, bool, error) {
	in.AgentName = strings.TrimSpace(in.AgentName)
	in.SourceMistakeID = strings.TrimSpace(in.SourceMistakeID)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	in.SourceSession = strings.TrimSpace(in.SourceSession)
	if in.AgentName == "" || in.SourceMistakeID == "" || in.IdempotencyKey == "" {
		return k12.PracticeCandidateSelection{}, false, fmt.Errorf(
			"k12storage: agent/source_mistake/idempotency_key required",
		)
	}
	source, err := s.Get(ctx, in.SourceMistakeID)
	if err != nil {
		return k12.PracticeCandidateSelection{}, false, err
	}
	if source.AgentName != in.AgentName || source.Collection != k12.CollectionMistakes {
		return k12.PracticeCandidateSelection{}, false, records.ErrNotFound
	}
	fields, err := k12.ParseMistakeFields(source.Fields)
	if err != nil {
		return k12.PracticeCandidateSelection{}, false, err
	}
	if in.SourceSession == "" {
		in.SourceSession = source.SourceSession
	}
	if strings.TrimSpace(in.RouteSnapshotJSON) == "" {
		in.RouteSnapshotJSON = "{}"
	}
	if !json.Valid([]byte(in.RouteSnapshotJSON)) {
		return k12.PracticeCandidateSelection{}, false, fmt.Errorf(
			"k12storage: invalid route snapshot",
		)
	}
	requestDigest, err := stableRequestDigest(struct {
		SourceMistakeID   string `json:"source_mistake_id"`
		Grade             string `json:"grade"`
		Textbook          string `json:"textbook"`
		SourceSession     string `json:"source_session"`
		RouteSnapshotJSON string `json:"route_snapshot_json"`
	}{
		SourceMistakeID:   in.SourceMistakeID,
		Grade:             strings.TrimSpace(in.Grade),
		Textbook:          strings.TrimSpace(in.Textbook),
		SourceSession:     in.SourceSession,
		RouteSnapshotJSON: in.RouteSnapshotJSON,
	})
	if err != nil {
		return k12.PracticeCandidateSelection{}, false, err
	}

	var existingID, existingDigest string
	err = s.db.QueryRowContext(ctx, `SELECT selection_id,request_digest
		FROM k12_practice_candidate_selections
		WHERE agent_name=? AND idempotency_key=?`,
		in.AgentName, in.IdempotencyKey).Scan(&existingID, &existingDigest)
	if err == nil {
		if existingDigest != requestDigest {
			return k12.PracticeCandidateSelection{}, false, records.ErrVersionConflict
		}
		selection, getErr := s.GetPracticeCandidateSelection(ctx, in.AgentName, existingID)
		return selection, true, getErr
	}
	if err != sql.ErrNoRows {
		return k12.PracticeCandidateSelection{}, false, err
	}

	basket, err := s.findOrCreatePracticeBasket(ctx, in.AgentName, in.SourceSession)
	if err != nil {
		return k12.PracticeCandidateSelection{}, false, err
	}
	problem := k12.NormalizePracticeCandidateProblem(k12.PracticeCandidateProblem{
		Subject:                fields.Subject,
		QuestionMarkdown:       fields.Question,
		ExpectedAnswerMarkdown: fields.CanonicalAnswer,
	})
	contentHash, _, err := k12.StablePracticeProblemHash(problem)
	if err != nil {
		return k12.PracticeCandidateSelection{}, false, err
	}
	problemJSON, err := json.Marshal(problem)
	if err != nil {
		return k12.PracticeCandidateSelection{}, false, err
	}
	now := nowUnix()
	selectionID, candidateID := idgen.NanoID(), idgen.NanoID()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.PracticeCandidateSelection{}, false, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT selection_id,request_digest
		FROM k12_practice_candidate_selections
		WHERE agent_name=? AND idempotency_key=?`,
		in.AgentName, in.IdempotencyKey).Scan(&existingID, &existingDigest); err == nil {
		if existingDigest != requestDigest {
			return k12.PracticeCandidateSelection{}, false, records.ErrVersionConflict
		}
		selection, getErr := getPracticeCandidateSelectionVia(
			ctx, tx, in.AgentName, existingID,
		)
		if getErr != nil {
			return k12.PracticeCandidateSelection{}, false, getErr
		}
		return selection, true, tx.Commit()
	} else if err != sql.ErrNoRows {
		return k12.PracticeCandidateSelection{}, false, err
	}

	candidateState := k12.PracticeCandidateReady
	var already int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_practice_set_items
		WHERE set_record_id=? AND normalized_content_hash=?`,
		basket.RecordID, contentHash).Scan(&already); err != nil {
		return k12.PracticeCandidateSelection{}, false, err
	}
	if already > 0 {
		candidateState = k12.PracticeCandidateAlreadyInSet
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_practice_candidate_selections
		(selection_id,agent_name,source_mistake_id,target_set_record_id,state,
		 next_batch_ordinal,revision,idempotency_key,request_digest,grade,textbook,
		 route_snapshot_json,source_session_id,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		selectionID, in.AgentName, in.SourceMistakeID, basket.RecordID,
		k12.PracticeCandidateSelectionOpen, 1, 1, in.IdempotencyKey, requestDigest,
		strings.TrimSpace(in.Grade), strings.TrimSpace(in.Textbook),
		in.RouteSnapshotJSON, in.SourceSession, now, now); err != nil {
		return k12.PracticeCandidateSelection{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_practice_candidates
		(candidate_id,selection_id,candidate_kind,batch_ordinal,candidate_ordinal,
		 normalized_content_hash,state,problem_json,failure_message,
		 batch_idempotency_key,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		candidateID, selectionID, k12.PracticeCandidateOriginal, 0, 0,
		contentHash, candidateState, string(problemJSON), "", "", now, now); err != nil {
		return k12.PracticeCandidateSelection{}, false, err
	}
	selection, err := getPracticeCandidateSelectionVia(ctx, tx, in.AgentName, selectionID)
	if err != nil {
		return k12.PracticeCandidateSelection{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return k12.PracticeCandidateSelection{}, false, err
	}
	return selection, false, nil
}

func getPracticeCandidateSelectionVia(
	ctx context.Context,
	q candidateSelectionQuerier,
	agentName, selectionID string,
) (k12.PracticeCandidateSelection, error) {
	var selection k12.PracticeCandidateSelection
	err := q.QueryRowContext(ctx, `SELECT selection_id,agent_name,source_mistake_id,
		target_set_record_id,state,next_batch_ordinal,revision,grade,textbook,
		route_snapshot_json,source_session_id
		FROM k12_practice_candidate_selections
		WHERE agent_name=? AND selection_id=?`,
		agentName, selectionID).Scan(
		&selection.SelectionID, &selection.AgentName, &selection.SourceMistakeID,
		&selection.TargetSetRecordID, &selection.State, &selection.NextBatchOrdinal,
		&selection.Revision, &selection.Grade, &selection.Textbook,
		&selection.RouteSnapshotJSON, &selection.SourceSessionID,
	)
	if err == sql.ErrNoRows {
		return k12.PracticeCandidateSelection{}, records.ErrNotFound
	}
	if err != nil {
		return k12.PracticeCandidateSelection{}, err
	}
	rows, err := q.QueryContext(ctx, `SELECT candidate_id,candidate_kind,
		batch_ordinal,candidate_ordinal,normalized_content_hash,state,problem_json,
		failure_message,batch_idempotency_key
		FROM k12_practice_candidates WHERE selection_id=?
		ORDER BY batch_ordinal,candidate_ordinal`, selectionID)
	if err != nil {
		return k12.PracticeCandidateSelection{}, err
	}
	defer rows.Close()
	selection.Candidates = []k12.PracticeCandidate{}
	for rows.Next() {
		var candidate k12.PracticeCandidate
		var problemJSON string
		if err := rows.Scan(
			&candidate.CandidateID, &candidate.CandidateKind,
			&candidate.BatchOrdinal, &candidate.CandidateOrdinal,
			&candidate.NormalizedContentHash, &candidate.State, &problemJSON,
			&candidate.FailureMessage, &candidate.BatchIdempotencyKey,
		); err != nil {
			return k12.PracticeCandidateSelection{}, err
		}
		if err := json.Unmarshal([]byte(problemJSON), &candidate.Problem); err != nil {
			return k12.PracticeCandidateSelection{}, err
		}
		candidate.QuestionMarkdown = candidate.Problem.QuestionMarkdown
		candidate.ExpectedAnswerMarkdown = candidate.Problem.ExpectedAnswerMarkdown
		selection.Candidates = append(selection.Candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return k12.PracticeCandidateSelection{}, err
	}
	return selection, nil
}

func (s *Store) GetPracticeCandidateSelection(
	ctx context.Context,
	agentName, selectionID string,
) (k12.PracticeCandidateSelection, error) {
	return getPracticeCandidateSelectionVia(
		ctx, s.db, strings.TrimSpace(agentName), strings.TrimSpace(selectionID),
	)
}

func (s *Store) GetPracticeCandidateSelectionByIdempotencyKey(
	ctx context.Context,
	agentName, idempotencyKey string,
) (k12.PracticeCandidateSelection, error) {
	var selectionID string
	err := s.db.QueryRowContext(ctx, `SELECT selection_id
		FROM k12_practice_candidate_selections
		WHERE agent_name=? AND idempotency_key=?`,
		strings.TrimSpace(agentName), strings.TrimSpace(idempotencyKey),
	).Scan(&selectionID)
	if err == sql.ErrNoRows {
		return k12.PracticeCandidateSelection{}, records.ErrNotFound
	}
	if err != nil {
		return k12.PracticeCandidateSelection{}, err
	}
	return s.GetPracticeCandidateSelection(ctx, agentName, selectionID)
}

func (s *Store) ReservePracticeCandidateBatch(
	ctx context.Context,
	agentName, selectionID string,
	expectedRevision int,
	idempotencyKey string,
	count int,
) ([]k12.PracticeCandidate, bool, error) {
	agentName = strings.TrimSpace(agentName)
	selectionID = strings.TrimSpace(selectionID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if agentName == "" || selectionID == "" || idempotencyKey == "" ||
		expectedRevision < 1 || count < 1 || count > 3 {
		return nil, false, fmt.Errorf("k12storage: invalid candidate batch")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	replayed, err := candidateBatchVia(ctx, tx, agentName, selectionID, idempotencyKey)
	if err != nil {
		return nil, false, err
	}
	if len(replayed) > 0 {
		return replayed, true, tx.Commit()
	}
	var state string
	var revision, batchOrdinal int
	err = tx.QueryRowContext(ctx, `SELECT state,revision,next_batch_ordinal
		FROM k12_practice_candidate_selections
		WHERE agent_name=? AND selection_id=?`,
		agentName, selectionID).Scan(&state, &revision, &batchOrdinal)
	if err == sql.ErrNoRows {
		return nil, false, records.ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	if state != k12.PracticeCandidateSelectionOpen {
		return nil, false, records.ErrIllegalTransition
	}
	if revision != expectedRevision {
		return nil, false, records.ErrVersionConflict
	}
	now := nowUnix()
	out := make([]k12.PracticeCandidate, 0, count)
	for i := 1; i <= count; i++ {
		candidate := k12.PracticeCandidate{
			CandidateID:         idgen.NanoID(),
			CandidateKind:       k12.PracticeCandidateVariant,
			BatchOrdinal:        batchOrdinal,
			CandidateOrdinal:    i,
			State:               k12.PracticeCandidateGenerating,
			BatchIdempotencyKey: idempotencyKey,
			Problem:             k12.PracticeCandidateProblem{},
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO k12_practice_candidates
			(candidate_id,selection_id,candidate_kind,batch_ordinal,candidate_ordinal,
			 normalized_content_hash,state,problem_json,failure_message,
			 batch_idempotency_key,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			candidate.CandidateID, selectionID, candidate.CandidateKind,
			candidate.BatchOrdinal, candidate.CandidateOrdinal, "",
			candidate.State, "{}", "", idempotencyKey, now, now); err != nil {
			return nil, false, err
		}
		out = append(out, candidate)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_practice_candidate_selections
		SET next_batch_ordinal=next_batch_ordinal+1,revision=revision+1,updated_at=?
		WHERE agent_name=? AND selection_id=? AND revision=?`,
		now, agentName, selectionID, expectedRevision); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return out, false, nil
}

func candidateBatchVia(
	ctx context.Context,
	q candidateSelectionQuerier,
	agentName, selectionID, idempotencyKey string,
) ([]k12.PracticeCandidate, error) {
	rows, err := q.QueryContext(ctx, `SELECT c.candidate_id,c.candidate_kind,
		c.batch_ordinal,c.candidate_ordinal,c.normalized_content_hash,c.state,
		c.problem_json,c.failure_message,c.batch_idempotency_key
		FROM k12_practice_candidates c
		JOIN k12_practice_candidate_selections s ON s.selection_id=c.selection_id
		WHERE s.agent_name=? AND c.selection_id=? AND c.batch_idempotency_key=?
		ORDER BY c.candidate_ordinal`, agentName, selectionID, idempotencyKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []k12.PracticeCandidate{}
	for rows.Next() {
		var candidate k12.PracticeCandidate
		var problemJSON string
		if err := rows.Scan(
			&candidate.CandidateID, &candidate.CandidateKind,
			&candidate.BatchOrdinal, &candidate.CandidateOrdinal,
			&candidate.NormalizedContentHash, &candidate.State, &problemJSON,
			&candidate.FailureMessage, &candidate.BatchIdempotencyKey,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(problemJSON), &candidate.Problem); err != nil {
			return nil, err
		}
		candidate.QuestionMarkdown = candidate.Problem.QuestionMarkdown
		candidate.ExpectedAnswerMarkdown = candidate.Problem.ExpectedAnswerMarkdown
		out = append(out, candidate)
	}
	return out, rows.Err()
}

func (s *Store) CompletePracticeCandidate(
	ctx context.Context,
	agentName, candidateID string,
	problem k12.PracticeCandidateProblem,
	failureMessage string,
) (k12.PracticeCandidate, error) {
	agentName = strings.TrimSpace(agentName)
	candidateID = strings.TrimSpace(candidateID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.PracticeCandidate{}, err
	}
	defer tx.Rollback()
	var candidate k12.PracticeCandidate
	var selectionID, targetSetID, problemJSON string
	err = tx.QueryRowContext(ctx, `SELECT c.selection_id,s.target_set_record_id,
		c.candidate_kind,c.batch_ordinal,c.candidate_ordinal,c.state,c.problem_json,
		c.normalized_content_hash,c.failure_message,c.batch_idempotency_key
		FROM k12_practice_candidates c
		JOIN k12_practice_candidate_selections s ON s.selection_id=c.selection_id
		WHERE s.agent_name=? AND c.candidate_id=?`,
		agentName, candidateID).Scan(
		&selectionID, &targetSetID, &candidate.CandidateKind,
		&candidate.BatchOrdinal, &candidate.CandidateOrdinal, &candidate.State,
		&problemJSON, &candidate.NormalizedContentHash, &candidate.FailureMessage,
		&candidate.BatchIdempotencyKey,
	)
	if err == sql.ErrNoRows {
		return k12.PracticeCandidate{}, records.ErrNotFound
	}
	if err != nil {
		return k12.PracticeCandidate{}, err
	}
	candidate.CandidateID = candidateID
	if candidate.State != k12.PracticeCandidateGenerating {
		if err := json.Unmarshal([]byte(problemJSON), &candidate.Problem); err != nil {
			return k12.PracticeCandidate{}, err
		}
		candidate.QuestionMarkdown = candidate.Problem.QuestionMarkdown
		candidate.ExpectedAnswerMarkdown = candidate.Problem.ExpectedAnswerMarkdown
		return candidate, tx.Commit()
	}
	now := nowUnix()
	candidate.FailureMessage = strings.TrimSpace(failureMessage)
	if candidate.FailureMessage != "" {
		candidate.State = k12.PracticeCandidateFailed
		candidate.Problem = k12.PracticeCandidateProblem{}
		problemJSON = "{}"
		candidate.NormalizedContentHash = ""
	} else {
		candidate.Problem = k12.NormalizePracticeCandidateProblem(problem)
		if candidate.Problem.QuestionMarkdown == "" {
			return k12.PracticeCandidate{}, fmt.Errorf("k12storage: generated question empty")
		}
		hash, _, hashErr := k12.StablePracticeProblemHash(candidate.Problem)
		if hashErr != nil {
			return k12.PracticeCandidate{}, hashErr
		}
		candidate.NormalizedContentHash = hash
		raw, marshalErr := json.Marshal(candidate.Problem)
		if marshalErr != nil {
			return k12.PracticeCandidate{}, marshalErr
		}
		problemJSON = string(raw)
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_practice_set_items
			WHERE set_record_id=? AND normalized_content_hash=?`,
			targetSetID, hash).Scan(&exists); err != nil {
			return k12.PracticeCandidate{}, err
		}
		if exists > 0 {
			candidate.State = k12.PracticeCandidateAlreadyInSet
		} else {
			candidate.State = k12.PracticeCandidateReady
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_practice_candidates SET
		normalized_content_hash=?,state=?,problem_json=?,failure_message=?,updated_at=?
		WHERE candidate_id=? AND state=?`,
		candidate.NormalizedContentHash, candidate.State, problemJSON,
		candidate.FailureMessage, now, candidateID,
		k12.PracticeCandidateGenerating); err != nil {
		return k12.PracticeCandidate{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_practice_candidate_selections
		SET revision=revision+1,updated_at=? WHERE selection_id=?`,
		now, selectionID); err != nil {
		return k12.PracticeCandidate{}, err
	}
	if err := tx.Commit(); err != nil {
		return k12.PracticeCandidate{}, err
	}
	candidate.QuestionMarkdown = candidate.Problem.QuestionMarkdown
	candidate.ExpectedAnswerMarkdown = candidate.Problem.ExpectedAnswerMarkdown
	return candidate, nil
}

type persistedCandidateCommitResult struct {
	AddedCount     int      `json:"added_count"`
	AlreadyPresent []string `json:"already_present"`
}

func (s *Store) CommitPracticeCandidateSelection(
	ctx context.Context,
	in PracticeCandidateCommitInput,
) (PracticeCandidateCommitReceipt, error) {
	in.AgentName = strings.TrimSpace(in.AgentName)
	in.SelectionID = strings.TrimSpace(in.SelectionID)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	candidateIDs := append([]string(nil), in.CandidateIDs...)
	sort.Strings(candidateIDs)
	if in.AgentName == "" || in.SelectionID == "" || in.IdempotencyKey == "" ||
		in.Revision < 1 || len(candidateIDs) == 0 {
		return PracticeCandidateCommitReceipt{}, fmt.Errorf("k12storage: invalid candidate commit")
	}
	for i, value := range candidateIDs {
		candidateIDs[i] = strings.TrimSpace(value)
		if candidateIDs[i] == "" || (i > 0 && candidateIDs[i] == candidateIDs[i-1]) {
			return PracticeCandidateCommitReceipt{}, fmt.Errorf("k12storage: invalid candidate ids")
		}
	}
	requestDigest, err := stableRequestDigest(struct {
		SelectionID  string   `json:"selection_id"`
		Revision     int      `json:"revision"`
		CandidateIDs []string `json:"candidate_ids"`
	}{
		SelectionID: in.SelectionID, Revision: in.Revision, CandidateIDs: candidateIDs,
	})
	if err != nil {
		return PracticeCandidateCommitReceipt{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PracticeCandidateCommitReceipt{}, err
	}
	defer tx.Rollback()
	var storedDigest, resultJSON, storedSelectionID string
	err = tx.QueryRowContext(ctx, `SELECT request_digest,result_json,selection_id
		FROM k12_practice_candidate_commits
		WHERE agent_name=? AND idempotency_key=?`,
		in.AgentName, in.IdempotencyKey).Scan(
		&storedDigest, &resultJSON, &storedSelectionID,
	)
	if err == nil {
		if storedDigest != requestDigest || storedSelectionID != in.SelectionID {
			return PracticeCandidateCommitReceipt{}, records.ErrVersionConflict
		}
		var persisted persistedCandidateCommitResult
		if err := json.Unmarshal([]byte(resultJSON), &persisted); err != nil {
			return PracticeCandidateCommitReceipt{}, err
		}
		selection, err := getPracticeCandidateSelectionVia(
			ctx, tx, in.AgentName, in.SelectionID,
		)
		if err != nil {
			return PracticeCandidateCommitReceipt{}, err
		}
		receipt := PracticeCandidateCommitReceipt{
			Selection: selection, AddedCount: persisted.AddedCount,
			AlreadyPresent: persisted.AlreadyPresent, Replayed: true,
		}
		return receipt, tx.Commit()
	}
	if err != sql.ErrNoRows {
		return PracticeCandidateCommitReceipt{}, err
	}

	var state, targetSetID, sourceMistakeID string
	var revision int
	err = tx.QueryRowContext(ctx, `SELECT state,revision,target_set_record_id,
		source_mistake_id FROM k12_practice_candidate_selections
		WHERE agent_name=? AND selection_id=?`,
		in.AgentName, in.SelectionID).Scan(
		&state, &revision, &targetSetID, &sourceMistakeID,
	)
	if err == sql.ErrNoRows {
		return PracticeCandidateCommitReceipt{}, records.ErrNotFound
	}
	if err != nil {
		return PracticeCandidateCommitReceipt{}, err
	}
	if state != k12.PracticeCandidateSelectionOpen {
		return PracticeCandidateCommitReceipt{}, records.ErrIllegalTransition
	}
	if revision != in.Revision {
		return PracticeCandidateCommitReceipt{}, records.ErrVersionConflict
	}

	type selectedCandidate struct {
		ID      string
		Hash    string
		State   string
		Problem k12.PracticeCandidateProblem
	}
	selected := make([]selectedCandidate, 0, len(candidateIDs))
	hashes := make([]string, 0, len(candidateIDs))
	for _, candidateID := range candidateIDs {
		var value selectedCandidate
		var problemJSON string
		err := tx.QueryRowContext(ctx, `SELECT candidate_id,normalized_content_hash,
			state,problem_json FROM k12_practice_candidates
			WHERE selection_id=? AND candidate_id=?`,
			in.SelectionID, candidateID).Scan(
			&value.ID, &value.Hash, &value.State, &problemJSON,
		)
		if err == sql.ErrNoRows {
			return PracticeCandidateCommitReceipt{}, records.ErrNotFound
		}
		if err != nil {
			return PracticeCandidateCommitReceipt{}, err
		}
		if value.State != k12.PracticeCandidateReady &&
			value.State != k12.PracticeCandidateAlreadyInSet {
			return PracticeCandidateCommitReceipt{}, records.ErrIllegalTransition
		}
		if value.Hash == "" {
			return PracticeCandidateCommitReceipt{}, fmt.Errorf("k12storage: invalid selected candidate")
		}
		if err := json.Unmarshal([]byte(problemJSON), &value.Problem); err != nil {
			return PracticeCandidateCommitReceipt{}, fmt.Errorf(
				"k12storage: decode selected candidate: %w", err,
			)
		}
		selected = append(selected, value)
		hashes = append(hashes, value.Hash)
	}
	hashDigest := k12.StableHashSetDigest(hashes)
	var itemIndex int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(item_index),-1)+1
		FROM k12_practice_set_items WHERE set_record_id=?`,
		targetSetID).Scan(&itemIndex); err != nil {
		return PracticeCandidateCommitReceipt{}, err
	}
	now := nowUnix()
	addedCount := 0
	alreadyPresent := []string{}
	for _, candidate := range selected {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_practice_set_items
			WHERE set_record_id=? AND normalized_content_hash=?`,
			targetSetID, candidate.Hash).Scan(&exists); err != nil {
			return PracticeCandidateCommitReceipt{}, err
		}
		if exists > 0 {
			alreadyPresent = append(alreadyPresent, candidate.ID)
			continue
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO k12_practice_set_items
			(set_record_id,item_index,item_id,source_problem_id,subject,added_via,
			 question_markdown,expected_answer_markdown,verification_status,
			 verification_evidence,generation_status,source_mistake_summary,
			 normalized_content_hash)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			targetSetID, itemIndex, candidate.ID, sourceMistakeID,
			candidate.Problem.Subject, k12.PracticeAddedViaSingleVariant,
			candidate.Problem.QuestionMarkdown,
			candidate.Problem.ExpectedAnswerMarkdown,
			k12.PracticeItemVerified, "candidate-selection:v1",
			k12.PracticeItemGenerationReady,
			candidate.Problem.QuestionMarkdown, candidate.Hash)
		if err != nil {
			return PracticeCandidateCommitReceipt{}, err
		}
		itemIndex++
		addedCount++
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_practice_candidates SET
		state=?,updated_at=? WHERE selection_id=? AND candidate_id IN (`+
		placeholders(len(candidateIDs))+`)`,
		append([]any{
			k12.PracticeCandidateAlreadyInSet, now, in.SelectionID,
		}, stringsToAny(candidateIDs)...)...); err != nil {
		return PracticeCandidateCommitReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_practice_sets
		SET version=version+1,updated_at=? WHERE record_id=? AND agent_name=?`,
		now, targetSetID, in.AgentName); err != nil {
		return PracticeCandidateCommitReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_practice_candidate_selections SET
		state=?,revision=revision+1,updated_at=? WHERE selection_id=?`,
		k12.PracticeCandidateSelectionCommitted, now, in.SelectionID); err != nil {
		return PracticeCandidateCommitReceipt{}, err
	}
	persisted := persistedCandidateCommitResult{
		AddedCount: addedCount, AlreadyPresent: alreadyPresent,
	}
	resultRaw, err := json.Marshal(persisted)
	if err != nil {
		return PracticeCandidateCommitReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_practice_candidate_commits
		(commit_id,agent_name,selection_id,target_set_record_id,
		 selected_hashes_digest,added_count,result_json,request_digest,
		 idempotency_key,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		idgen.NanoID(), in.AgentName, in.SelectionID, targetSetID,
		hashDigest, addedCount, string(resultRaw), requestDigest,
		in.IdempotencyKey, now); err != nil {
		return PracticeCandidateCommitReceipt{}, err
	}
	selection, err := getPracticeCandidateSelectionVia(
		ctx, tx, in.AgentName, in.SelectionID,
	)
	if err != nil {
		return PracticeCandidateCommitReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return PracticeCandidateCommitReceipt{}, err
	}
	return PracticeCandidateCommitReceipt{
		Selection: selection, AddedCount: addedCount,
		AlreadyPresent: alreadyPresent,
	}, nil
}

func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for i := range values {
		out[i] = values[i]
	}
	return out
}

func (s *Store) GetMistakeReviewState(
	ctx context.Context,
	agentName, mistakeRecordID string,
) (k12.MistakeReviewState, error) {
	return getMistakeReviewStateVia(
		ctx, s.db, strings.TrimSpace(agentName), strings.TrimSpace(mistakeRecordID),
	)
}

func getMistakeReviewStateVia(
	ctx context.Context,
	q weeklyPlanQuerier,
	agentName, mistakeRecordID string,
) (k12.MistakeReviewState, error) {
	var state k12.MistakeReviewState
	err := q.QueryRowContext(ctx, `SELECT agent_name,mistake_record_id,state,
		deferred_iso_year,deferred_iso_week,prior_schedule_json,revision,updated_at
		FROM k12_mistake_review_states
		WHERE agent_name=? AND mistake_record_id=?`,
		agentName, mistakeRecordID).Scan(
		&state.AgentName, &state.MistakeRecordID, &state.State,
		&state.DeferredISOYear, &state.DeferredISOWeek,
		&state.PriorScheduleJSON, &state.Revision, &state.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return k12.MistakeReviewState{}, records.ErrNotFound
	}
	return state, err
}

func (s *Store) ListMistakeReviewStates(
	ctx context.Context,
	agentName string,
) (map[string]k12.MistakeReviewState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent_name,mistake_record_id,state,
		deferred_iso_year,deferred_iso_week,prior_schedule_json,revision,updated_at
		FROM k12_mistake_review_states WHERE agent_name=?`,
		strings.TrimSpace(agentName))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]k12.MistakeReviewState{}
	for rows.Next() {
		var state k12.MistakeReviewState
		if err := rows.Scan(
			&state.AgentName, &state.MistakeRecordID, &state.State,
			&state.DeferredISOYear, &state.DeferredISOWeek,
			&state.PriorScheduleJSON, &state.Revision, &state.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out[state.MistakeRecordID] = state
	}
	return out, rows.Err()
}

type priorMistakeSchedule struct {
	State           string `json:"state"`
	DueAt           *int64 `json:"due_at,omitempty"`
	DeferredISOYear int    `json:"deferred_iso_year,omitempty"`
	DeferredISOWeek int    `json:"deferred_iso_week,omitempty"`
}

func (s *Store) ApplyMistakeReviewCommand(
	ctx context.Context,
	in MistakeReviewCommandInput,
) (MistakeReviewCommandResult, error) {
	in.AgentName = strings.TrimSpace(in.AgentName)
	in.MistakeRecordID = strings.TrimSpace(in.MistakeRecordID)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	in.CommandType = strings.TrimSpace(in.CommandType)
	if in.AgentName == "" || in.MistakeRecordID == "" || in.IdempotencyKey == "" ||
		in.ExpectedVersion < 0 {
		return MistakeReviewCommandResult{}, fmt.Errorf("k12storage: invalid review command")
	}
	switch in.CommandType {
	case k12.MistakeReviewCommandDeferThisWeek,
		k12.MistakeReviewCommandSuppress,
		k12.MistakeReviewCommandRestore:
	default:
		return MistakeReviewCommandResult{}, fmt.Errorf("k12storage: unknown review command")
	}
	requestDigest, err := stableRequestDigest(struct {
		MistakeRecordID string `json:"mistake_record_id"`
		ExpectedVersion int    `json:"expected_version"`
		CommandType     string `json:"command_type"`
		ISOYear         int    `json:"iso_year"`
		ISOWeek         int    `json:"iso_week"`
		PlanID          string `json:"plan_id"`
		PlanRevision    int    `json:"plan_revision"`
		WeeklyItemID    string `json:"weekly_item_id"`
	}{
		MistakeRecordID: in.MistakeRecordID,
		ExpectedVersion: in.ExpectedVersion,
		CommandType:     in.CommandType, ISOYear: in.ISOYear, ISOWeek: in.ISOWeek,
		PlanID: strings.TrimSpace(in.PlanID), PlanRevision: in.PlanRevision,
		WeeklyItemID: strings.TrimSpace(in.WeeklyItemID),
	})
	if err != nil {
		return MistakeReviewCommandResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MistakeReviewCommandResult{}, err
	}
	defer tx.Rollback()
	var storedDigest, resultJSON string
	err = tx.QueryRowContext(ctx, `SELECT request_digest,result_json
		FROM k12_mistake_review_commands
		WHERE agent_name=? AND idempotency_key=?`,
		in.AgentName, in.IdempotencyKey).Scan(&storedDigest, &resultJSON)
	if err == nil {
		if storedDigest != requestDigest {
			return MistakeReviewCommandResult{}, records.ErrVersionConflict
		}
		var result MistakeReviewCommandResult
		if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
			return MistakeReviewCommandResult{}, err
		}
		result.Replayed = true
		return result, tx.Commit()
	}
	if err != sql.ErrNoRows {
		return MistakeReviewCommandResult{}, err
	}

	var status string
	var version int
	var dueAt sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT status,version,due_at FROM k12_mistakes
		WHERE agent_name=? AND record_id=?`,
		in.AgentName, in.MistakeRecordID).Scan(&status, &version, &dueAt)
	if err == sql.ErrNoRows {
		return MistakeReviewCommandResult{}, records.ErrNotFound
	}
	if err != nil {
		return MistakeReviewCommandResult{}, err
	}
	if version != in.ExpectedVersion {
		return MistakeReviewCommandResult{}, records.ErrVersionConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO k12_mistake_review_states
		(agent_name,mistake_record_id,state,deferred_iso_year,deferred_iso_week,
		 prior_schedule_json,revision,updated_at)
		VALUES(?,?,?,?,?,?,?,?)`,
		in.AgentName, in.MistakeRecordID,
		initialReviewState(status), 0, 0, "{}", 1, nowUnix()); err != nil {
		return MistakeReviewCommandResult{}, err
	}
	current, err := getMistakeReviewStateVia(
		ctx, tx, in.AgentName, in.MistakeRecordID,
	)
	if err != nil {
		return MistakeReviewCommandResult{}, err
	}
	fromState := current.State
	toState := current.State
	priorJSON := current.PriorScheduleJSON
	deferredYear, deferredWeek := current.DeferredISOYear, current.DeferredISOWeek
	var due *int64
	if dueAt.Valid {
		value := dueAt.Int64
		due = &value
	}
	switch in.CommandType {
	case k12.MistakeReviewCommandDeferThisWeek:
		if current.State != k12.MistakeReviewScheduled {
			return MistakeReviewCommandResult{}, records.ErrIllegalTransition
		}
		if in.ISOYear <= 0 || in.ISOWeek < 1 || in.ISOWeek > 53 {
			return MistakeReviewCommandResult{}, fmt.Errorf("k12storage: valid ISO week required")
		}
		if err := validateDeferWeeklyItemTx(ctx, tx, in); err != nil {
			return MistakeReviewCommandResult{}, err
		}
		snapshot, _ := json.Marshal(priorMistakeSchedule{
			State: current.State, DueAt: due,
		})
		priorJSON = string(snapshot)
		toState = k12.MistakeReviewDeferredThisWeek
		deferredYear, deferredWeek = in.ISOYear, in.ISOWeek
	case k12.MistakeReviewCommandSuppress:
		if current.State != k12.MistakeReviewScheduled &&
			current.State != k12.MistakeReviewDeferredThisWeek {
			return MistakeReviewCommandResult{}, records.ErrIllegalTransition
		}
		snapshot, _ := json.Marshal(priorMistakeSchedule{
			State: current.State, DueAt: due,
			DeferredISOYear: current.DeferredISOYear,
			DeferredISOWeek: current.DeferredISOWeek,
		})
		priorJSON = string(snapshot)
		toState = k12.MistakeReviewSuppressed
		deferredYear, deferredWeek = 0, 0
	case k12.MistakeReviewCommandRestore:
		if current.State != k12.MistakeReviewSuppressed {
			return MistakeReviewCommandResult{}, records.ErrIllegalTransition
		}
		var prior priorMistakeSchedule
		if err := json.Unmarshal([]byte(current.PriorScheduleJSON), &prior); err != nil {
			return MistakeReviewCommandResult{}, err
		}
		if prior.State != k12.MistakeReviewScheduled &&
			prior.State != k12.MistakeReviewDeferredThisWeek {
			prior.State = k12.MistakeReviewScheduled
		}
		toState = prior.State
		deferredYear, deferredWeek = prior.DeferredISOYear, prior.DeferredISOWeek
		priorJSON = "{}"
	}
	now := nowUnix()
	if _, err := tx.ExecContext(ctx, `UPDATE k12_mistake_review_states SET
		state=?,deferred_iso_year=?,deferred_iso_week=?,prior_schedule_json=?,
		revision=revision+1,updated_at=?
		WHERE agent_name=? AND mistake_record_id=?`,
		toState, deferredYear, deferredWeek, priorJSON, now,
		in.AgentName, in.MistakeRecordID); err != nil {
		return MistakeReviewCommandResult{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE k12_mistakes SET
		version=version+1,updated_at=?
		WHERE agent_name=? AND record_id=? AND version=?`,
		now, in.AgentName, in.MistakeRecordID, in.ExpectedVersion)
	if err != nil {
		return MistakeReviewCommandResult{}, err
	}
	if affected, _ := res.RowsAffected(); affected != 1 {
		return MistakeReviewCommandResult{}, records.ErrVersionConflict
	}
	review, err := getMistakeReviewStateVia(
		ctx, tx, in.AgentName, in.MistakeRecordID,
	)
	if err != nil {
		return MistakeReviewCommandResult{}, err
	}
	result := MistakeReviewCommandResult{
		State: toState, MistakeVersion: version + 1, Review: review,
	}
	resultRaw, err := json.Marshal(result)
	if err != nil {
		return MistakeReviewCommandResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_mistake_review_commands
		(agent_name,mistake_record_id,idempotency_key,command_type,from_state,
		 to_state,prior_schedule_json,request_digest,result_json,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		in.AgentName, in.MistakeRecordID, in.IdempotencyKey, in.CommandType,
		fromState, toState, priorJSON, requestDigest, string(resultRaw), now); err != nil {
		return MistakeReviewCommandResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return MistakeReviewCommandResult{}, err
	}
	return result, nil
}

func initialReviewState(status string) string {
	switch status {
	case k12.StatusMastered:
		return k12.MistakeReviewMastered
	case k12.StatusArchived:
		return k12.MistakeReviewSuppressed
	default:
		return k12.MistakeReviewScheduled
	}
}

func validateDeferWeeklyItemTx(
	ctx context.Context,
	tx *sql.Tx,
	in MistakeReviewCommandInput,
) error {
	if strings.TrimSpace(in.PlanID) == "" {
		return nil
	}
	var revision, year, week int
	var planJSON string
	err := tx.QueryRowContext(ctx, `SELECT revision,iso_week_year,iso_week_number,
		plan_json FROM k12_weekly_practice_plans
		WHERE agent_name=? AND plan_id=?`,
		in.AgentName, strings.TrimSpace(in.PlanID)).Scan(
		&revision, &year, &week, &planJSON,
	)
	if err == sql.ErrNoRows {
		return records.ErrNotFound
	}
	if err != nil {
		return err
	}
	if revision != in.PlanRevision || year != in.ISOYear || week != in.ISOWeek {
		return records.ErrVersionConflict
	}
	var plan k12.WeeklyPracticePlan
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return err
	}
	for _, track := range plan.Tracks {
		for _, item := range track.Items {
			if item.ItemID == in.WeeklyItemID &&
				item.SourceRef == in.MistakeRecordID {
				return nil
			}
		}
	}
	return records.ErrNotFound
}
