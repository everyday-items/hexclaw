package k12

import (
	"fmt"
	"strings"
)

// Work feedback generation lifecycle. A failed initial generation is retried
// in place; later commands append a new generation.
const (
	WorkFeedbackQueued    = "queued"
	WorkFeedbackRunning   = "running"
	WorkFeedbackSucceeded = "succeeded"
	WorkFeedbackFailed    = "failed"
)

// CreativeWorkSourceSnapshot is the immutable evidence input frozen when the
// work and its initial feedback generation are created. It deliberately has no
// public version identity: every current upload/save is an independent work.
type CreativeWorkSourceSnapshot struct {
	WorkType           string `json:"work_type"`
	DisplayName        string `json:"display_name"`
	WorkTitle          string `json:"work_title,omitempty"`
	ContentMarkdown    string `json:"content_markdown,omitempty"`
	SourceAssetID      string `json:"source_asset_id,omitempty"`
	OCRRaw             string `json:"ocr_raw,omitempty"`
	OCRVersion         int    `json:"ocr_version,omitempty"`
	OCRDigest          string `json:"ocr_confirmed_digest,omitempty"`
	ContentConfirmedAt int64  `json:"content_confirmed_at,omitempty"`
}

// WorkFeedbackGeneration is the durable internal generation record. Feedback
// uses the existing closed canonical fact; version_id remains an internal
// legacy-adapter detail and is never exposed by the current HTTP DTO.
type WorkFeedbackGeneration struct {
	GenerationID  string                     `json:"generation_id"`
	WorkID        string                     `json:"work_id"`
	AgentName     string                     `json:"-"`
	GenerationNo  int                        `json:"-"`
	CommandKey    string                     `json:"-"`
	RequestDigest string                     `json:"-"`
	Status        string                     `json:"status"`
	FeedbackType  string                     `json:"-"`
	Source        CreativeWorkSourceSnapshot `json:"-"`
	Feedback      *WorkFeedback              `json:"feedback,omitempty"`
	FailureReason string                     `json:"failure_message,omitempty"`
	Attempt       int                        `json:"-"`
	CreatedAt     int64                      `json:"-"`
	UpdatedAt     int64                      `json:"-"`
}

type CreativeWorkGenerationState struct {
	Initial    *WorkFeedbackGeneration
	Latest     *WorkFeedbackGeneration
	RowVersion int
}

const (
	DictationQueued     = "queued"
	DictationGenerating = "generating"
	DictationValidating = "validating"
	DictationCommitted  = "committed"
	DictationFailed     = "failed"
	DictationReAdd      = "re_add"
)

// AccumulationDictationGeneration is persisted before any question or
// PracticeSetItem is produced. PracticeItemID is populated only at commit.
type AccumulationDictationGeneration struct {
	GenerationID   string `json:"generation_id"`
	AccumulationID string `json:"-"`
	AgentName      string `json:"-"`
	CommandKey     string `json:"-"`
	RequestDigest  string `json:"-"`
	Status         string `json:"status"`
	SourceSnapshot string `json:"-"`
	PracticeItemID string `json:"practice_item_id,omitempty"`
	FailureReason  string `json:"failure_reason,omitempty"`
	Attempt        int    `json:"attempt,omitempty"`
	CreatedAt      int64  `json:"-"`
	UpdatedAt      int64  `json:"updated_at,omitempty"`
}

// DerivationProvenance records why a server-derived accumulation fact exists.
// Client requests cannot supply this value.
type DerivationProvenance struct {
	Method      string   `json:"method"`
	Policy      string   `json:"policy"`
	Version     string   `json:"version"`
	Confidence  *float64 `json:"confidence,omitempty"`
	EvidenceRef string   `json:"evidence_ref,omitempty"`
}

func (p DerivationProvenance) Validate() error {
	if p.Method != "rule" && p.Method != "model" {
		return fmt.Errorf("派生 method 只允许 rule/model，got %q", p.Method)
	}
	if strings.TrimSpace(p.Policy) == "" || strings.TrimSpace(p.Version) == "" {
		return fmt.Errorf("派生 provenance 缺少 policy/version")
	}
	if p.Confidence != nil && (*p.Confidence < 0 || *p.Confidence > 1) {
		return fmt.Errorf("派生 confidence 必须在 0..1")
	}
	return nil
}

// AccumulationDerivedMetadata is the closed result of the trusted server-side
// derivation port. Source may be empty only when no reliable evidence exists.
type AccumulationDerivedMetadata struct {
	Subject             string                `json:"subject"`
	EntryType           string                `json:"entry_type"`
	Source              string                `json:"source,omitempty"`
	SubjectProvenance   DerivationProvenance  `json:"subject_provenance"`
	EntryTypeProvenance DerivationProvenance  `json:"entry_type_provenance"`
	SourceProvenance    *DerivationProvenance `json:"source_provenance,omitempty"`
}

func (m AccumulationDerivedMetadata) Validate() error {
	allowed := map[string]map[string]bool{
		"语文": {
			"好词好句": true, "古诗积累": true, "写作素材": true,
		},
		"英语": {
			"表达积累": true, "词汇积累": true,
		},
	}
	if !allowed[m.Subject][m.EntryType] {
		return fmt.Errorf("积累派生分类非法: subject=%q entry_type=%q", m.Subject, m.EntryType)
	}
	if err := m.SubjectProvenance.Validate(); err != nil {
		return fmt.Errorf("subject provenance: %w", err)
	}
	if err := m.EntryTypeProvenance.Validate(); err != nil {
		return fmt.Errorf("entry_type provenance: %w", err)
	}
	if strings.TrimSpace(m.Source) == "" {
		if m.SourceProvenance != nil {
			return fmt.Errorf("空 source 不得携带 source provenance")
		}
		return nil
	}
	if len([]rune(strings.TrimSpace(m.Source))) > 50 {
		return fmt.Errorf("积累来源超过 50 字")
	}
	if m.SourceProvenance == nil {
		return fmt.Errorf("非空 source 缺少 provenance")
	}
	if err := m.SourceProvenance.Validate(); err != nil {
		return fmt.Errorf("source provenance: %w", err)
	}
	return nil
}

// CurrentDeleteReceipt is stored verbatim on the aggregate root so a replay
// with the same command key returns byte-for-byte stable facts.
type CurrentDeleteReceipt struct {
	ObjectKind string `json:"object_kind"`
	ObjectID   string `json:"object_id"`
	Deleted    bool   `json:"deleted"`
	RowVersion int    `json:"row_version"`
	DeletedAt  int64  `json:"deleted_at"`
}

type CurrentCreateReceipt struct {
	ObjectKind    string `json:"object_kind"`
	ObjectID      string `json:"object_id"`
	CommandKey    string `json:"-"`
	RequestDigest string `json:"-"`
	Created       bool   `json:"created"`
	CreatedAt     int64  `json:"created_at"`
}
