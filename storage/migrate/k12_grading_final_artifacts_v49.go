package migrate

// K12GradingFinalArtifactsV49 adds the only canonical, immutable final grading
// artifact. Print, export and formal delivery consume this identity instead of
// rebuilding content from mutable page state.
var K12GradingFinalArtifactsV49 = Migration{
	Version:     49,
	Description: "BUG-20260726-031 K12 批改唯一最终 Artifact",
	SQL:         K12GradingFinalArtifactsV49DDL,
}

const K12GradingFinalArtifactsV49DDL = `
CREATE TABLE IF NOT EXISTS k12_grading_final_artifacts (
    artifact_id TEXT PRIMARY KEY,
    agent_name TEXT NOT NULL,
    job_id TEXT NOT NULL,
    structure_version INTEGER NOT NULL CHECK(structure_version >= 1),
    coverage_status TEXT NOT NULL
        CHECK(coverage_status IN ('complete','with_skips')),
    total_count INTEGER NOT NULL CHECK(total_count >= 1),
    published_count INTEGER NOT NULL CHECK(published_count >= 0),
    skipped_count INTEGER NOT NULL CHECK(skipped_count >= 0),
    ordered_current_digests_json TEXT NOT NULL
        CHECK(
            json_valid(ordered_current_digests_json) AND
            json_type(ordered_current_digests_json) = 'array'
        ),
    canonical_markdown TEXT NOT NULL CHECK(length(trim(canonical_markdown)) > 0),
    artifact_digest TEXT NOT NULL CHECK(length(artifact_digest) = 64),
    summary_invocation_id TEXT NOT NULL,
    created_at INTEGER NOT NULL CHECK(created_at > 0),
    updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
    UNIQUE(agent_name,job_id),
    UNIQUE(agent_name,job_id,structure_version,artifact_digest),
    FOREIGN KEY(agent_name,job_id)
        REFERENCES k12_grading_jobs(agent_name,record_id) ON DELETE CASCADE,
    CHECK(published_count + skipped_count = total_count),
    CHECK(
        (coverage_status = 'complete' AND
            published_count = total_count AND skipped_count = 0 AND
            length(trim(summary_invocation_id)) > 0)
        OR
        (coverage_status = 'with_skips' AND skipped_count > 0 AND
            length(trim(summary_invocation_id)) = 0)
    )
);
CREATE INDEX IF NOT EXISTS idx_k12_grading_final_artifacts_digest
    ON k12_grading_final_artifacts(
        agent_name,job_id,structure_version,artifact_digest
    );`
