package k12storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// GroundingRetrievalInvocationStatus 是一次教材召回的耐久状态；running 与
// outcome_unknown 均必须先恢复/对账，不能直接再次召回。
type GroundingRetrievalInvocationStatus string

const (
	GroundingRetrievalInvocationStatusPrepared       GroundingRetrievalInvocationStatus = "prepared"
	GroundingRetrievalInvocationStatusRunning        GroundingRetrievalInvocationStatus = "running"
	GroundingRetrievalInvocationStatusSucceeded      GroundingRetrievalInvocationStatus = "succeeded"
	GroundingRetrievalInvocationStatusFailed         GroundingRetrievalInvocationStatus = "failed"
	GroundingRetrievalInvocationStatusOutcomeUnknown GroundingRetrievalInvocationStatus = "outcome_unknown"
)

var (
	ErrGroundingRetrievalInvocationOutcomeUnknown    = errors.New("grounding retrieval invocation outcome unknown")
	ErrGroundingRetrievalInvocationLedgerUnavailable = errors.New("grounding retrieval invocation ledger unavailable")
)

type GroundingRetrievalInvocationClaim struct {
	OwnerID                 string
	AgentName               string
	JobID                   string
	ProblemID               string
	Operation               string
	GroundingSnapshotDigest string
	QueryDigest             string
	DocumentID              string
	DocumentGeneration      int64
	RevisionID              string
	ProfileConfigHash       string
	ScopeDigest             string
	Provider                string
	Model                   string
}

type GroundingRetrievalInvocation struct {
	InvocationID            string
	InvocationKey           string
	OwnerID                 string
	AgentName               string
	JobID                   string
	ProblemID               string
	Operation               string
	GroundingSnapshotDigest string
	QueryDigest             string
	DocumentID              string
	DocumentGeneration      int64
	RevisionID              string
	ProfileConfigHash       string
	ScopeDigest             string
	Provider                string
	Model                   string
	Status                  GroundingRetrievalInvocationStatus
	ResultJSON              string
	QueryReceiptDigest      string
	HitSetDigest            string
	CitationSetDigest       string
	CreatedAt               time.Time
	UpdatedAt               time.Time
	Fresh                   bool `json:"-"`
}

type GroundingRetrievalInvocationResult struct {
	ResultJSON         string
	QueryReceiptDigest string
	HitSetDigest       string
	CitationSetDigest  string
	Provider           string
	Model              string
	RevisionID         string
	ProfileConfigHash  string
}

func validateGroundingRetrievalClaim(claim GroundingRetrievalInvocationClaim) error {
	for name, value := range map[string]string{
		"owner_id": claim.OwnerID, "agent_name": claim.AgentName, "job_id": claim.JobID,
		"problem_id": claim.ProblemID, "operation": claim.Operation,
	} {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("invalid grounding retrieval claim: %s", name)
		}
	}
	for name, value := range map[string]string{
		"grounding_snapshot_digest": claim.GroundingSnapshotDigest,
		"query_digest":              claim.QueryDigest,
	} {
		if !validSHA256Digest(value) {
			return fmt.Errorf("invalid grounding retrieval claim: %s", name)
		}
	}
	if claim.DocumentGeneration < 0 {
		return fmt.Errorf("invalid grounding retrieval claim: document_generation")
	}
	for name, value := range map[string]string{
		"document_id": claim.DocumentID, "revision_id": claim.RevisionID,
		"profile_config_hash": claim.ProfileConfigHash, "scope_digest": claim.ScopeDigest,
		"provider": claim.Provider, "model": claim.Model,
	} {
		if value != "" && value != strings.TrimSpace(value) {
			return fmt.Errorf("invalid grounding retrieval claim: %s", name)
		}
	}
	return nil
}

func groundingRetrievalInvocationKey(claim GroundingRetrievalInvocationClaim) string {
	raw, _ := json.Marshal(struct {
		OwnerID                 string `json:"owner_id"`
		AgentName               string `json:"agent_name"`
		JobID                   string `json:"job_id"`
		ProblemID               string `json:"problem_id"`
		Operation               string `json:"operation"`
		GroundingSnapshotDigest string `json:"grounding_snapshot_digest"`
		QueryDigest             string `json:"query_digest"`
		DocumentID              string `json:"document_id"`
		DocumentGeneration      int64  `json:"document_generation"`
		RevisionID              string `json:"revision_id"`
		ProfileConfigHash       string `json:"profile_config_hash"`
		ScopeDigest             string `json:"scope_digest"`
		Provider                string `json:"provider"`
		Model                   string `json:"model"`
	}{claim.OwnerID, claim.AgentName, claim.JobID, claim.ProblemID, claim.Operation,
		claim.GroundingSnapshotDigest, claim.QueryDigest, claim.DocumentID, claim.DocumentGeneration,
		claim.RevisionID, claim.ProfileConfigHash, claim.ScopeDigest, claim.Provider, claim.Model})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func scanGroundingRetrievalInvocation(row interface{ Scan(...any) error }) (GroundingRetrievalInvocation, error) {
	var invocation GroundingRetrievalInvocation
	var status string
	var createdAt, updatedAt int64
	if err := row.Scan(
		&invocation.InvocationID, &invocation.InvocationKey, &invocation.OwnerID,
		&invocation.AgentName, &invocation.JobID, &invocation.ProblemID, &invocation.Operation,
		&invocation.GroundingSnapshotDigest, &invocation.QueryDigest, &invocation.DocumentID,
		&invocation.DocumentGeneration, &invocation.RevisionID, &invocation.ProfileConfigHash,
		&invocation.ScopeDigest, &invocation.Provider, &invocation.Model, &status,
		&invocation.ResultJSON, &invocation.QueryReceiptDigest, &invocation.HitSetDigest,
		&invocation.CitationSetDigest, &createdAt, &updatedAt,
	); err != nil {
		return GroundingRetrievalInvocation{}, err
	}
	invocation.Status = GroundingRetrievalInvocationStatus(status)
	invocation.CreatedAt = time.UnixMilli(createdAt).UTC()
	invocation.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return invocation, nil
}

func groundingRetrievalLedgerError(err error) error {
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such table") {
		return ErrGroundingRetrievalInvocationLedgerUnavailable
	}
	return err
}

const groundingRetrievalInvocationSelect = `SELECT invocation_id,invocation_key,owner_id,agent_name,
job_id,problem_id,operation,grounding_snapshot_digest,query_digest,document_id,
document_generation,revision_id,profile_config_hash,scope_digest,provider,model,status,
result_json,query_receipt_digest,hit_set_digest,citation_set_digest,created_at,updated_at
FROM k12_grounding_retrieval_invocations`

// ClaimGroundingRetrievalInvocation 在一次 pinned 检索前冻结身份；同一身份重放只读回
// 原记录，避免 worker 重启后再次召回并产生不同引用集合。
func (s *Store) ClaimGroundingRetrievalInvocation(
	ctx context.Context,
	claim GroundingRetrievalInvocationClaim,
) (GroundingRetrievalInvocation, error) {
	if err := validateGroundingRetrievalClaim(claim); err != nil {
		return GroundingRetrievalInvocation{}, err
	}
	key := groundingRetrievalInvocationKey(claim)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GroundingRetrievalInvocation{}, err
	}
	defer tx.Rollback()
	invocation, err := scanGroundingRetrievalInvocation(tx.QueryRowContext(ctx,
		groundingRetrievalInvocationSelect+` WHERE invocation_key=?`, key))
	if err == nil {
		if invocation.OwnerID != claim.OwnerID || invocation.AgentName != claim.AgentName ||
			invocation.JobID != claim.JobID || invocation.ProblemID != claim.ProblemID ||
			invocation.GroundingSnapshotDigest != claim.GroundingSnapshotDigest ||
			invocation.QueryDigest != claim.QueryDigest || invocation.DocumentID != claim.DocumentID ||
			invocation.DocumentGeneration != claim.DocumentGeneration || invocation.RevisionID != claim.RevisionID ||
			invocation.ProfileConfigHash != claim.ProfileConfigHash || invocation.ScopeDigest != claim.ScopeDigest ||
			invocation.Provider != claim.Provider || invocation.Model != claim.Model {
			return GroundingRetrievalInvocation{}, fmt.Errorf("grounding retrieval invocation identity drifted")
		}
		if err := tx.Commit(); err != nil {
			return GroundingRetrievalInvocation{}, err
		}
		return invocation, nil
	}
	if unavailable := groundingRetrievalLedgerError(err); unavailable != err {
		return GroundingRetrievalInvocation{}, unavailable
	}
	if err != sql.ErrNoRows {
		return GroundingRetrievalInvocation{}, err
	}
	invocationID := "grounding_retrieval_" + key[:32]
	now := time.Now().UTC()
	nowMillis := now.UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_grounding_retrieval_invocations
		(invocation_id,invocation_key,owner_id,agent_name,job_id,problem_id,operation,
		 grounding_snapshot_digest,query_digest,document_id,document_generation,revision_id,
		 profile_config_hash,scope_digest,provider,model,status,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?, 'running',?,?)`,
		invocationID, key, claim.OwnerID, claim.AgentName, claim.JobID, claim.ProblemID,
		claim.Operation, claim.GroundingSnapshotDigest, claim.QueryDigest, claim.DocumentID,
		claim.DocumentGeneration, claim.RevisionID, claim.ProfileConfigHash, claim.ScopeDigest,
		claim.Provider, claim.Model, nowMillis, nowMillis); err != nil {
		return GroundingRetrievalInvocation{}, fmt.Errorf("claim grounding retrieval invocation: %w", err)
	}
	invocation = GroundingRetrievalInvocation{
		InvocationID: invocationID, InvocationKey: key, OwnerID: claim.OwnerID, AgentName: claim.AgentName,
		JobID: claim.JobID, ProblemID: claim.ProblemID, Operation: claim.Operation,
		GroundingSnapshotDigest: claim.GroundingSnapshotDigest, QueryDigest: claim.QueryDigest,
		DocumentID: claim.DocumentID, DocumentGeneration: claim.DocumentGeneration,
		RevisionID: claim.RevisionID, ProfileConfigHash: claim.ProfileConfigHash,
		ScopeDigest: claim.ScopeDigest, Provider: claim.Provider, Model: claim.Model,
		Status: GroundingRetrievalInvocationStatusRunning, CreatedAt: now, UpdatedAt: now, Fresh: true,
	}
	if err := tx.Commit(); err != nil {
		return GroundingRetrievalInvocation{}, err
	}
	return invocation, nil
}

// SaveGroundingRetrievalInvocation 保存同一次 pinned 检索的结果与命中摘要；重复保存
// 相同结果幂等，任何不同结果均拒绝覆盖已冻结事实。
func (s *Store) SaveGroundingRetrievalInvocation(
	ctx context.Context,
	invocation GroundingRetrievalInvocation,
	result GroundingRetrievalInvocationResult,
) error {
	if strings.TrimSpace(invocation.InvocationID) == "" || strings.TrimSpace(invocation.InvocationKey) == "" ||
		!json.Valid([]byte(result.ResultJSON)) || strings.TrimSpace(result.ResultJSON) == "" {
		return fmt.Errorf("invalid grounding retrieval invocation result")
	}
	for name, value := range map[string]string{
		"query_receipt_digest": result.QueryReceiptDigest, "hit_set_digest": result.HitSetDigest,
		"citation_set_digest": result.CitationSetDigest,
	} {
		if !validSHA256Digest(value) {
			return fmt.Errorf("invalid grounding retrieval result: %s", name)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stored, err := scanGroundingRetrievalInvocation(tx.QueryRowContext(ctx,
		groundingRetrievalInvocationSelect+` WHERE invocation_id=?`, invocation.InvocationID))
	if err != nil {
		if unavailable := groundingRetrievalLedgerError(err); unavailable != err {
			return unavailable
		}
		if err == sql.ErrNoRows {
			return fmt.Errorf("grounding retrieval invocation is not claimed")
		}
		return err
	}
	if stored.InvocationKey != invocation.InvocationKey || stored.Status == GroundingRetrievalInvocationStatusFailed {
		return fmt.Errorf("grounding retrieval invocation identity drifted")
	}
	if (result.Provider != "" && result.Provider != stored.Provider) ||
		(result.Model != "" && result.Model != stored.Model) ||
		(result.RevisionID != "" && result.RevisionID != stored.RevisionID) ||
		(result.ProfileConfigHash != "" && result.ProfileConfigHash != stored.ProfileConfigHash) {
		return fmt.Errorf("grounding retrieval invocation route identity drifted")
	}
	if stored.Status == GroundingRetrievalInvocationStatusSucceeded {
		if stored.ResultJSON != result.ResultJSON || stored.QueryReceiptDigest != result.QueryReceiptDigest ||
			stored.HitSetDigest != result.HitSetDigest || stored.CitationSetDigest != result.CitationSetDigest {
			return fmt.Errorf("grounding retrieval invocation result conflicts with stored result")
		}
		return tx.Commit()
	}
	nowMillis := time.Now().UTC().UnixMilli()
	updated, err := tx.ExecContext(ctx, `UPDATE k12_grounding_retrieval_invocations SET status='succeeded',
		result_json=?,query_receipt_digest=?,hit_set_digest=?,citation_set_digest=?,updated_at=?
		WHERE invocation_id=? AND status IN ('prepared','running')`,
		result.ResultJSON, result.QueryReceiptDigest, result.HitSetDigest, result.CitationSetDigest,
		nowMillis,
		invocation.InvocationID)
	if err != nil {
		return err
	}
	if changed, _ := updated.RowsAffected(); changed != 1 {
		return ErrGroundingRetrievalInvocationOutcomeUnknown
	}
	return tx.Commit()
}

// MarkGroundingRetrievalInvocationOutcomeUnknown 把没有可验证返回的召回停在恢复态。
func (s *Store) MarkGroundingRetrievalInvocationOutcomeUnknown(
	ctx context.Context,
	invocation GroundingRetrievalInvocation,
	_ string,
) error {
	if strings.TrimSpace(invocation.InvocationID) == "" {
		return fmt.Errorf("invalid grounding retrieval invocation identity")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	updated, err := tx.ExecContext(ctx, `UPDATE k12_grounding_retrieval_invocations SET
		status='outcome_unknown',updated_at=? WHERE invocation_id=? AND status IN ('prepared','running')`,
		time.Now().UTC().UnixMilli(), invocation.InvocationID)
	if err != nil {
		return err
	}
	if changed, _ := updated.RowsAffected(); changed == 0 {
		return tx.Commit()
	}
	return tx.Commit()
}
