package k12storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

var ErrGradingFinalArtifactConflict = errors.New("grading final artifact conflict")

const gradingFinalArtifactColumns = `artifact_id,agent_name,job_id,structure_version,
	coverage_status,total_count,published_count,skipped_count,
	ordered_current_digests_json,canonical_markdown,artifact_digest,
	summary_invocation_id,created_at,updated_at`

func scanGradingFinalArtifact(row rowScanner) (k12.GradingFinalArtifact, error) {
	var artifact k12.GradingFinalArtifact
	var coverageStatus string
	err := row.Scan(
		&artifact.ArtifactID,
		&artifact.AgentName,
		&artifact.JobID,
		&artifact.StructureVersion,
		&coverageStatus,
		&artifact.TotalCount,
		&artifact.PublishedCount,
		&artifact.SkippedCount,
		&artifact.OrderedCurrentDigestsJSON,
		&artifact.CanonicalMarkdown,
		&artifact.ArtifactDigest,
		&artifact.SummaryInvocationID,
		&artifact.CreatedAt,
		&artifact.UpdatedAt,
	)
	artifact.CoverageStatus = k12.GradingFinalArtifactCoverageStatus(coverageStatus)
	return artifact, err
}

func getGradingFinalArtifactVia(
	ctx context.Context,
	q dbQueryer,
	agentName string,
	artifactID string,
) (k12.GradingFinalArtifact, error) {
	artifact, err := scanGradingFinalArtifact(q.QueryRowContext(ctx, `
		SELECT `+gradingFinalArtifactColumns+`
		FROM k12_grading_final_artifacts
		WHERE agent_name=? AND artifact_id=?`,
		agentName,
		artifactID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.GradingFinalArtifact{}, records.ErrNotFound
	}
	if err != nil {
		return k12.GradingFinalArtifact{}, fmt.Errorf(
			"k12storage: get grading final artifact: %w",
			err,
		)
	}
	return artifact, nil
}

func getGradingFinalArtifactByJobVia(
	ctx context.Context,
	q dbQueryer,
	agentName string,
	jobID string,
) (k12.GradingFinalArtifact, error) {
	artifact, err := scanGradingFinalArtifact(q.QueryRowContext(ctx, `
		SELECT `+gradingFinalArtifactColumns+`
		FROM k12_grading_final_artifacts
		WHERE agent_name=? AND job_id=?`,
		agentName,
		jobID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.GradingFinalArtifact{}, records.ErrNotFound
	}
	if err != nil {
		return k12.GradingFinalArtifact{}, fmt.Errorf(
			"k12storage: get grading final artifact by job: %w",
			err,
		)
	}
	return artifact, nil
}

func (s *Store) GetGradingFinalArtifact(
	ctx context.Context,
	agentName string,
	artifactID string,
) (k12.GradingFinalArtifact, error) {
	agentName = strings.TrimSpace(agentName)
	artifactID = strings.TrimSpace(artifactID)
	if agentName == "" || artifactID == "" {
		return k12.GradingFinalArtifact{}, records.ErrNotFound
	}
	return getGradingFinalArtifactVia(ctx, s.db, agentName, artifactID)
}

func (s *Store) GetGradingFinalArtifactByJob(
	ctx context.Context,
	agentName string,
	jobID string,
) (k12.GradingFinalArtifact, error) {
	agentName = strings.TrimSpace(agentName)
	jobID = strings.TrimSpace(jobID)
	if agentName == "" || jobID == "" {
		return k12.GradingFinalArtifact{}, records.ErrNotFound
	}
	return getGradingFinalArtifactByJobVia(ctx, s.db, agentName, jobID)
}

// GetCurrentGradingFinalArtifactByJob returns the immutable artifact only when
// it is fenced to the Job's current aggregate generation. Missing and stale
// artifacts are intentionally indistinguishable to callers: neither is safe
// evidence that a completed source-work replay may converge successfully.
func (s *Store) GetCurrentGradingFinalArtifactByJob(
	ctx context.Context,
	agentName string,
	jobID string,
) (k12.GradingFinalArtifact, error) {
	agentName = strings.TrimSpace(agentName)
	jobID = strings.TrimSpace(jobID)
	if agentName == "" || jobID == "" {
		return k12.GradingFinalArtifact{}, records.ErrNotFound
	}
	artifact, err := scanGradingFinalArtifact(s.db.QueryRowContext(ctx, `
		SELECT `+gradingFinalArtifactColumns+`
		FROM k12_grading_final_artifacts
		WHERE agent_name=? AND job_id=?
		  AND finalization_generation=(
			SELECT finalization_generation
			FROM k12_grading_jobs
			WHERE agent_name=? AND record_id=?
		  )`,
		agentName,
		jobID,
		agentName,
		jobID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return k12.GradingFinalArtifact{}, records.ErrNotFound
	}
	if err != nil {
		return k12.GradingFinalArtifact{}, fmt.Errorf(
			"k12storage: get current grading final artifact by job: %w",
			err,
		)
	}
	return artifact, nil
}

// GetGradingFinalizationGeneration snapshots the durable aggregate generation
// that a finalizer must present when it commits the immutable artifact. Source
// actions advance this value in their own state-transition transaction.
func (s *Store) GetGradingFinalizationGeneration(
	ctx context.Context,
	agentName string,
	jobID string,
) (int64, error) {
	agentName = strings.TrimSpace(agentName)
	jobID = strings.TrimSpace(jobID)
	if agentName == "" || jobID == "" {
		return 0, records.ErrNotFound
	}
	var generation int64
	err := s.db.QueryRowContext(ctx, `
		SELECT finalization_generation
		FROM k12_grading_jobs
		WHERE agent_name=? AND record_id=?`,
		agentName,
		jobID,
	).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, records.ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("k12storage: get grading finalization generation: %w", err)
	}
	return generation, nil
}

func normalizeGradingFinalArtifact(
	artifact k12.GradingFinalArtifact,
) k12.GradingFinalArtifact {
	artifact.AgentName = strings.TrimSpace(artifact.AgentName)
	artifact.JobID = strings.TrimSpace(artifact.JobID)
	artifact.ArtifactID = strings.TrimSpace(artifact.ArtifactID)
	artifact.ArtifactDigest = strings.TrimSpace(artifact.ArtifactDigest)
	artifact.SummaryInvocationID = strings.TrimSpace(artifact.SummaryInvocationID)
	if artifact.StructureVersion == 0 {
		artifact.StructureVersion = k12.GradingFinalArtifactStructureVersion
	}
	if artifact.ArtifactID == "" {
		sum := sha256.Sum256([]byte(
			artifact.AgentName + "\x00" +
				artifact.JobID + "\x00" +
				strconv.Itoa(artifact.StructureVersion) + "\x00" +
				artifact.ArtifactDigest,
		))
		artifact.ArtifactID = "grading-final-" + hex.EncodeToString(sum[:])
	}
	if artifact.CreatedAt <= 0 {
		artifact.CreatedAt = nowUnix()
	}
	if artifact.UpdatedAt <= 0 {
		artifact.UpdatedAt = artifact.CreatedAt
	}
	return artifact
}

func sameGradingFinalArtifactContent(
	stored k12.GradingFinalArtifact,
	requested k12.GradingFinalArtifact,
) bool {
	return stored.AgentName == requested.AgentName &&
		stored.JobID == requested.JobID &&
		stored.StructureVersion == requested.StructureVersion &&
		stored.CoverageStatus == requested.CoverageStatus &&
		stored.TotalCount == requested.TotalCount &&
		stored.PublishedCount == requested.PublishedCount &&
		stored.SkippedCount == requested.SkippedCount &&
		stored.OrderedCurrentDigestsJSON == requested.OrderedCurrentDigestsJSON &&
		stored.CanonicalMarkdown == requested.CanonicalMarkdown &&
		stored.ArtifactDigest == requested.ArtifactDigest &&
		stored.SummaryInvocationID == requested.SummaryInvocationID
}

// CommitGradingFinalArtifact freezes the only final grading artifact for a
// job. Exact replays converge on the first stored identity; a different
// structure, digest, coverage or payload cannot replace it.
func (s *Store) CommitGradingFinalArtifact(
	ctx context.Context,
	artifact k12.GradingFinalArtifact,
	expectedGeneration int64,
) (stored k12.GradingFinalArtifact, replay bool, err error) {
	if expectedGeneration < 0 {
		return k12.GradingFinalArtifact{}, false, fmt.Errorf(
			"%w: invalid expected finalization generation %d",
			ErrGradingFinalArtifactConflict,
			expectedGeneration,
		)
	}
	artifact = normalizeGradingFinalArtifact(artifact)
	if err := artifact.Validate(); err != nil {
		return k12.GradingFinalArtifact{}, false, fmt.Errorf(
			"k12storage: invalid grading final artifact: %w",
			err,
		)
	}
	if err := ensureAgentRegistered(ctx, s.db, artifact.AgentName); err != nil {
		return k12.GradingFinalArtifact{}, false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.GradingFinalArtifact{}, false, fmt.Errorf(
			"k12storage: begin grading final artifact commit: %w",
			err,
		)
	}
	defer tx.Rollback()
	// This no-op CAS is intentionally the first statement in the artifact write
	// transaction. It acquires SQLite's writer lock before checking the
	// generation, serializing with CommitProblemSourceAction. Whichever writer
	// loses observes either the advanced generation or the immutable artifact.
	fence, err := tx.ExecContext(ctx, `
		UPDATE k12_grading_jobs
		SET finalization_generation=finalization_generation
		WHERE agent_name=? AND record_id=? AND finalization_generation=?`,
		artifact.AgentName,
		artifact.JobID,
		expectedGeneration,
	)
	if err != nil {
		return k12.GradingFinalArtifact{}, false, fmt.Errorf(
			"k12storage: fence grading final artifact generation: %w",
			err,
		)
	}
	fenced, err := fence.RowsAffected()
	if err != nil {
		return k12.GradingFinalArtifact{}, false, err
	}
	if fenced != 1 {
		return k12.GradingFinalArtifact{}, false, fmt.Errorf(
			"%w: job %s finalization generation advanced from %d",
			ErrGradingFinalArtifactConflict,
			artifact.JobID,
			expectedGeneration,
		)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO k12_grading_final_artifacts (`+gradingFinalArtifactColumns+`,
			finalization_generation)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT DO NOTHING`,
		artifact.ArtifactID,
		artifact.AgentName,
		artifact.JobID,
		artifact.StructureVersion,
		artifact.CoverageStatus,
		artifact.TotalCount,
		artifact.PublishedCount,
		artifact.SkippedCount,
		artifact.OrderedCurrentDigestsJSON,
		artifact.CanonicalMarkdown,
		artifact.ArtifactDigest,
		artifact.SummaryInvocationID,
		artifact.CreatedAt,
		artifact.UpdatedAt,
		expectedGeneration,
	)
	if err != nil {
		return k12.GradingFinalArtifact{}, false, fmt.Errorf(
			"k12storage: commit grading final artifact: %w",
			err,
		)
	}
	stored, err = getGradingFinalArtifactByJobVia(
		ctx,
		tx,
		artifact.AgentName,
		artifact.JobID,
	)
	if err != nil {
		return k12.GradingFinalArtifact{}, false, fmt.Errorf(
			"%w: final artifact identity is already bound outside the job",
			ErrGradingFinalArtifactConflict,
		)
	}
	if !sameGradingFinalArtifactContent(stored, artifact) {
		return k12.GradingFinalArtifact{}, false, fmt.Errorf(
			"%w: job %s already has a different canonical artifact",
			ErrGradingFinalArtifactConflict,
			artifact.JobID,
		)
	}
	var storedGeneration int64
	if err := tx.QueryRowContext(ctx, `
		SELECT finalization_generation
		FROM k12_grading_final_artifacts
		WHERE agent_name=? AND job_id=?`,
		artifact.AgentName,
		artifact.JobID,
	).Scan(&storedGeneration); err != nil {
		return k12.GradingFinalArtifact{}, false, fmt.Errorf(
			"k12storage: get grading final artifact generation: %w",
			err,
		)
	}
	if storedGeneration != expectedGeneration {
		return k12.GradingFinalArtifact{}, false, fmt.Errorf(
			"%w: job %s artifact generation=%d expected=%d",
			ErrGradingFinalArtifactConflict,
			artifact.JobID,
			storedGeneration,
			expectedGeneration,
		)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return k12.GradingFinalArtifact{}, false, err
	}
	replay = affected == 0
	if err := tx.Commit(); err != nil {
		return k12.GradingFinalArtifact{}, false, fmt.Errorf(
			"k12storage: commit grading final artifact transaction: %w",
			err,
		)
	}
	return stored, replay, nil
}
