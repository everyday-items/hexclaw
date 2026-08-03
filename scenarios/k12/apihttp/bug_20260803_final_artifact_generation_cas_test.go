package apihttp_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func loadFinalizationGeneration(t *testing.T, seed problemSourceActionSeed) int64 {
	t.Helper()
	var generation int64
	if err := seed.fixture.db.QueryRow(`
		SELECT finalization_generation
		FROM k12_grading_jobs
		WHERE agent_name='mingming' AND record_id=?`,
		seed.jobID,
	).Scan(&generation); err != nil {
		t.Fatalf("load finalization generation: %v", err)
	}
	return generation
}

func generationCASFinalArtifact(jobID, digest string) k12.GradingFinalArtifact {
	return k12.GradingFinalArtifact{
		ArtifactID:                "artifact-" + digest[:12],
		AgentName:                 "mingming",
		JobID:                     jobID,
		StructureVersion:          k12.GradingFinalArtifactStructureVersion,
		CoverageStatus:            k12.GradingFinalArtifactCoverageComplete,
		TotalCount:                1,
		PublishedCount:            1,
		OrderedCurrentDigestsJSON: `["receipt-generation-cas"]`,
		CanonicalMarkdown:         "# generation-fenced final artifact",
		ArtifactDigest:            digest,
		SummaryInvocationID:       "summary-generation-cas",
		CreatedAt:                 1000,
		UpdatedAt:                 1000,
	}
}

// K12-FINAL-GENERATION-CAS-001: a finalizer may spend minutes in a provider
// call after reading current receipt digests. A source action accepted during
// that interval must advance a durable generation, making the old candidate
// impossible to publish even in another process.
func TestFinalArtifactCommitRejectsGenerationStaleAfterSourceAction(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	readGeneration := loadFinalizationGeneration(t, seed)
	if readGeneration != 0 {
		t.Fatalf("initial finalization generation=%d, want 0", readGeneration)
	}

	rec, body := postProblemSourceAction(
		t,
		seed.fixture.handler,
		seed.dispatchID,
		seed.problemID,
		"generation-cas-source-action",
		validSkipSourceActionBody,
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("commit source action: status=%d body=%#v", rec.Code, body)
	}
	if got := loadFinalizationGeneration(t, seed); got != readGeneration+1 {
		t.Fatalf("source action generation=%d, want %d", got, readGeneration+1)
	}

	artifact := generationCASFinalArtifact(seed.jobID, strings.Repeat("a", 64))
	_, replay, err := seed.fixture.coordinator.Records.CommitGradingFinalArtifact(
		context.Background(),
		artifact,
		readGeneration,
	)
	if !errors.Is(err, k12storage.ErrGradingFinalArtifactConflict) || replay {
		t.Fatalf("stale artifact commit: replay=%v err=%v, want generation conflict", replay, err)
	}
	var artifacts int
	if err := seed.fixture.db.QueryRow(`
		SELECT COUNT(*) FROM k12_grading_final_artifacts
		WHERE agent_name='mingming' AND job_id=?`,
		seed.jobID,
	).Scan(&artifacts); err != nil {
		t.Fatal(err)
	}
	if artifacts != 0 {
		t.Fatalf("stale generation published %d final artifacts, want 0", artifacts)
	}

	// An exact command replay must not consume another aggregate generation.
	replayRec, replayBody := postProblemSourceAction(
		t,
		seed.fixture.handler,
		seed.dispatchID,
		seed.problemID,
		"generation-cas-source-action",
		validSkipSourceActionBody,
	)
	if replayRec.Code != http.StatusOK {
		t.Fatalf("replay source action: status=%d body=%#v", replayRec.Code, replayBody)
	}
	if got := loadFinalizationGeneration(t, seed); got != readGeneration+1 {
		t.Fatalf("source action replay generation=%d, want %d", got, readGeneration+1)
	}
}

// K12-FINAL-GENERATION-CAS-002: generation fencing must retain the original
// exact-replay contract for retries after an ambiguous artifact commit.
func TestFinalArtifactGenerationCASPreservesExactReplay(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	generation := loadFinalizationGeneration(t, seed)
	artifact := generationCASFinalArtifact(seed.jobID, strings.Repeat("b", 64))

	first, replay, err := seed.fixture.coordinator.Records.CommitGradingFinalArtifact(
		context.Background(), artifact, generation,
	)
	if err != nil || replay {
		t.Fatalf("first artifact commit: replay=%v err=%v", replay, err)
	}
	second, replay, err := seed.fixture.coordinator.Records.CommitGradingFinalArtifact(
		context.Background(), artifact, generation,
	)
	if err != nil || !replay {
		t.Fatalf("exact artifact replay: replay=%v err=%v", replay, err)
	}
	if first.ArtifactID != second.ArtifactID || first.ArtifactDigest != second.ArtifactDigest {
		t.Fatalf("artifact replay changed identity: first=%+v second=%+v", first, second)
	}
	if got := loadFinalizationGeneration(t, seed); got != generation {
		t.Fatalf("artifact replay changed generation=%d, want %d", got, generation)
	}
}
