package k12

import (
	"encoding/json"
	"errors"
	"strings"
)

type GradingFinalArtifactCoverageStatus string

const (
	GradingFinalArtifactStructureVersion = 1

	GradingFinalArtifactCoverageComplete  GradingFinalArtifactCoverageStatus = "complete"
	GradingFinalArtifactCoverageWithSkips GradingFinalArtifactCoverageStatus = "with_skips"
)

var ErrGradingFinalArtifactInvariant = errors.New("grading final artifact invariant violated")

// GradingFinalArtifact is the one immutable, canonical grading result exposed
// to print, export and formal delivery. OrderedCurrentDigestsJSON freezes the
// exact current per-problem receipts used to build CanonicalMarkdown.
type GradingFinalArtifact struct {
	ArtifactID                   string                                     `json:"artifact_id"`
	AgentName                    string                                     `json:"agent_name"`
	JobID                        string                                     `json:"job_id"`
	StructureVersion             int                                        `json:"structure_version"`
	CoverageStatus               GradingFinalArtifactCoverageStatus         `json:"coverage_status"`
	TotalCount                   int                                        `json:"total_count"`
	PublishedCount               int                                        `json:"published_count"`
	SkippedCount                 int                                        `json:"skipped_count"`
	OrderedCurrentDigestsJSON    string                                     `json:"ordered_current_digests_json"`
	CanonicalMarkdown            string                                     `json:"canonical_markdown"`
	ArtifactDigest               string                                     `json:"artifact_digest"`
	SummaryInvocationID          string                                     `json:"summary_invocation_id"`
	CreatedAt                    int64                                      `json:"created_at"`
	UpdatedAt                    int64                                      `json:"updated_at"`
}

func (a GradingFinalArtifact) Validate() error {
	if strings.TrimSpace(a.ArtifactID) == "" ||
		strings.TrimSpace(a.AgentName) == "" ||
		strings.TrimSpace(a.JobID) == "" ||
		a.StructureVersion < 1 ||
		a.TotalCount < 1 ||
		a.PublishedCount < 0 ||
		a.SkippedCount < 0 ||
		a.PublishedCount+a.SkippedCount != a.TotalCount ||
		strings.TrimSpace(a.CanonicalMarkdown) == "" ||
		len(a.ArtifactDigest) != 64 ||
		a.CreatedAt <= 0 ||
		a.UpdatedAt < a.CreatedAt {
		return ErrGradingFinalArtifactInvariant
	}
	switch a.CoverageStatus {
	case GradingFinalArtifactCoverageComplete:
		if a.PublishedCount != a.TotalCount || a.SkippedCount != 0 ||
			strings.TrimSpace(a.SummaryInvocationID) == "" {
			return ErrGradingFinalArtifactInvariant
		}
	case GradingFinalArtifactCoverageWithSkips:
		if a.SkippedCount == 0 || strings.TrimSpace(a.SummaryInvocationID) != "" {
			return ErrGradingFinalArtifactInvariant
		}
	default:
		return ErrGradingFinalArtifactInvariant
	}
	var orderedDigests []string
	if json.Unmarshal([]byte(a.OrderedCurrentDigestsJSON), &orderedDigests) != nil ||
		len(orderedDigests) != a.TotalCount {
		return ErrGradingFinalArtifactInvariant
	}
	for _, digest := range orderedDigests {
		if strings.TrimSpace(digest) == "" {
			return ErrGradingFinalArtifactInvariant
		}
	}
	return nil
}

// ProblemSkipReceipt is immutable evidence that a parent explicitly skipped
// one input revision. Only the current receipt participates in final coverage.
type ProblemSkipReceipt struct {
	SkipReceiptID      string `json:"skip_receipt_id"`
	AgentName          string `json:"agent_name"`
	JobID              string `json:"job_id"`
	ProblemID          string `json:"problem_id"`
	StructureVersion   int    `json:"structure_version"`
	InputRevision      int    `json:"input_revision"`
	ResultDigest       string `json:"result_digest"`
	CurrentDisposition string `json:"current_disposition"`
	PublishedRevision  int    `json:"published_revision"`
	SupersededAt       int64  `json:"superseded_at"`
	CreatedAt          int64  `json:"created_at"`
	UpdatedAt          int64  `json:"updated_at"`
}
