package k12

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

type GradingFinalArtifactCoverageStatus string

const (
	GradingFinalArtifactStructureVersion = 1

	GradingFinalArtifactCoverageComplete        GradingFinalArtifactCoverageStatus = "complete"
	GradingFinalArtifactCoverageWithSkips       GradingFinalArtifactCoverageStatus = "with_skips"
	GradingFinalArtifactCoverageGeneralGuidance GradingFinalArtifactCoverageStatus = "general_guidance"
)

var ErrGradingFinalArtifactInvariant = errors.New("grading final artifact invariant violated")

// GradingFinalArtifact is the one immutable, canonical grading result exposed
// to print, export and formal delivery. OrderedCurrentDigestsJSON freezes the
// exact current per-problem receipts used to build CanonicalMarkdown.
type GradingFinalArtifact struct {
	ArtifactID                string                             `json:"artifact_id"`
	AgentName                 string                             `json:"agent_name"`
	JobID                     string                             `json:"job_id"`
	StructureVersion          int                                `json:"structure_version"`
	CoverageStatus            GradingFinalArtifactCoverageStatus `json:"coverage_status"`
	TotalCount                int                                `json:"total_count"`
	PublishedCount            int                                `json:"published_count"`
	SkippedCount              int                                `json:"skipped_count"`
	OrderedCurrentDigestsJSON string                             `json:"ordered_current_digests_json"`
	CanonicalMarkdown         string                             `json:"canonical_markdown"`
	ArtifactDigest            string                             `json:"artifact_digest"`
	SummaryInvocationID       string                             `json:"summary_invocation_id"`
	// AnnotatedAssetOwnerScope 只用于内部按已认证 owner 边界读取资产，
	// 不得进入公开 JSON、Markdown 或 IM 消息。
	AnnotatedAssetOwnerScope string `json:"-"`
	AnnotatedAssetID         string `json:"annotated_asset_id,omitempty"`
	AnnotatedMIME            string `json:"annotated_mime,omitempty"`
	AnnotatedDigest          string `json:"annotated_digest,omitempty"`
	OriginalSourceDigest     string `json:"original_source_digest,omitempty"`
	CreatedAt                int64  `json:"created_at"`
	UpdatedAt                int64  `json:"updated_at"`
}

// GradingFinalAnnotatedAsset 是由 final artifact 冻结并在每次读取时重新校验的批注图。
// 资产身份与字节只在应用内部流转，公开投影应只返回受控资源。
type GradingFinalAnnotatedAsset struct {
	OwnerScope           string `json:"-"`
	AssetID              string `json:"-"`
	MIME                 string `json:"-"`
	Digest               string `json:"-"`
	OriginalSourceDigest string `json:"-"`
	Data                 []byte `json:"-"`
}

func validGradingFinalArtifactSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func (a GradingFinalArtifact) hasAnyAnnotatedAssetField() bool {
	return strings.TrimSpace(a.AnnotatedAssetOwnerScope) != "" ||
		strings.TrimSpace(a.AnnotatedAssetID) != "" ||
		strings.TrimSpace(a.AnnotatedMIME) != "" ||
		strings.TrimSpace(a.AnnotatedDigest) != "" ||
		strings.TrimSpace(a.OriginalSourceDigest) != ""
}

// HasAnnotatedAsset 只在五个冻结身份字段完整时返回 true。
func (a GradingFinalArtifact) HasAnnotatedAsset() bool {
	return strings.TrimSpace(a.AnnotatedAssetOwnerScope) != "" &&
		strings.TrimSpace(a.AnnotatedAssetID) != "" &&
		strings.TrimSpace(a.AnnotatedMIME) != "" &&
		strings.TrimSpace(a.AnnotatedDigest) != "" &&
		strings.TrimSpace(a.OriginalSourceDigest) != ""
}

// ComputeGradingFinalArtifactDigest 保持无批注图历史产物的旧摘要算法；
// 一旦存在批注图，owner、身份、MIME、字节摘要与原图摘要全部参与计算。
func ComputeGradingFinalArtifactDigest(artifact GradingFinalArtifact) string {
	var raw []byte
	if artifact.hasAnyAnnotatedAssetField() {
		raw, _ = json.Marshal(struct {
			StructureVersion          int
			CoverageStatus            GradingFinalArtifactCoverageStatus
			TotalCount                int
			PublishedCount            int
			SkippedCount              int
			OrderedCurrentDigestsJSON string
			CanonicalMarkdown         string
			SummaryInvocationID       string
			AnnotatedAssetOwnerScope  string
			AnnotatedAssetID          string
			AnnotatedMIME             string
			AnnotatedDigest           string
			OriginalSourceDigest      string
		}{
			artifact.StructureVersion,
			artifact.CoverageStatus,
			artifact.TotalCount,
			artifact.PublishedCount,
			artifact.SkippedCount,
			artifact.OrderedCurrentDigestsJSON,
			artifact.CanonicalMarkdown,
			artifact.SummaryInvocationID,
			artifact.AnnotatedAssetOwnerScope,
			artifact.AnnotatedAssetID,
			artifact.AnnotatedMIME,
			artifact.AnnotatedDigest,
			artifact.OriginalSourceDigest,
		})
	} else {
		raw, _ = json.Marshal(struct {
			StructureVersion          int
			CoverageStatus            GradingFinalArtifactCoverageStatus
			TotalCount                int
			PublishedCount            int
			SkippedCount              int
			OrderedCurrentDigestsJSON string
			CanonicalMarkdown         string
			SummaryInvocationID       string
		}{
			artifact.StructureVersion,
			artifact.CoverageStatus,
			artifact.TotalCount,
			artifact.PublishedCount,
			artifact.SkippedCount,
			artifact.OrderedCurrentDigestsJSON,
			artifact.CanonicalMarkdown,
			artifact.SummaryInvocationID,
		})
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
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
	if a.hasAnyAnnotatedAssetField() {
		if !a.HasAnnotatedAsset() ||
			a.AnnotatedAssetOwnerScope != strings.TrimSpace(a.AnnotatedAssetOwnerScope) ||
			a.AnnotatedAssetID != strings.TrimSpace(a.AnnotatedAssetID) ||
			a.AnnotatedMIME != strings.TrimSpace(a.AnnotatedMIME) ||
			a.AnnotatedDigest != strings.TrimSpace(a.AnnotatedDigest) ||
			a.OriginalSourceDigest != strings.TrimSpace(a.OriginalSourceDigest) ||
			!validGradingFinalArtifactSHA256(a.AnnotatedDigest) ||
			!validGradingFinalArtifactSHA256(a.OriginalSourceDigest) ||
			a.ArtifactDigest != ComputeGradingFinalArtifactDigest(a) {
			return ErrGradingFinalArtifactInvariant
		}
		owner, file, err := assetstore.Parse(a.AnnotatedAssetID)
		if err != nil || owner != a.AgentName {
			return ErrGradingFinalArtifactInvariant
		}
		extension := ""
		switch a.AnnotatedMIME {
		case "image/png":
			extension = ".png"
		case "image/jpeg":
			extension = ".jpg"
		case "image/gif":
			extension = ".gif"
		case "image/webp":
			extension = ".webp"
		default:
			return ErrGradingFinalArtifactInvariant
		}
		if file != a.AnnotatedDigest+extension {
			return ErrGradingFinalArtifactInvariant
		}
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
	case GradingFinalArtifactCoverageGeneralGuidance:
		if a.PublishedCount != a.TotalCount || a.SkippedCount != 0 ||
			strings.TrimSpace(a.SummaryInvocationID) != "" ||
			!strings.Contains(a.CanonicalMarkdown, "No verified textbook grounding is available.") {
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
