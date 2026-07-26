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
) (stored k12.GradingFinalArtifact, replay bool, err error) {
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
	result, err := tx.ExecContext(ctx, `
		INSERT INTO k12_grading_final_artifacts (`+gradingFinalArtifactColumns+`)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
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
