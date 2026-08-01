package k12

// Problem/Attempt 是拍照识题确认的类型化 canonical 事实（架构 §5.4 / DD-010~012）。
// compound_parent 只承载公共题干，不拥有 Attempt；每个可作答题独立 Attempt。
const (
	ProblemKindStandalone     = "standalone"
	ProblemKindCompoundParent = "compound_parent"
	ProblemKindSubproblem     = "subproblem"
)

type Problem struct {
	ProblemID               string   `json:"problem_id"`
	AgentName               string   `json:"agent_name"`
	SubmissionID            string   `json:"submission_id"`
	PageAssetID             string   `json:"page_asset_id"`
	Ordinal                 int      `json:"ordinal"`
	ProblemKind             string   `json:"problem_kind"`
	ParentProblemID         string   `json:"parent_problem_id,omitempty"`
	SubproblemNo            string   `json:"subproblem_no,omitempty"`
	SourceNumberPath        []string `json:"source_number_path,omitempty"`
	DisplayLabel            string   `json:"display_label,omitempty"`
	SourceSectionPath       []string `json:"source_section_path,omitempty"`
	SourceSectionLabel      string   `json:"source_section_label,omitempty"`
	SystemSectionOrdinal    int      `json:"system_section_ordinal,omitempty"`
	SystemDisplayLabel      string   `json:"system_display_label,omitempty"`
	Subject                 string   `json:"subject,omitempty"`
	StemRaw                 string   `json:"stem_raw"`
	StemMarkdown            string   `json:"stem_markdown"`
	ConceptIDs              []string `json:"concept_ids,omitempty"`
	TranscriptionConfidence *float64 `json:"transcription_confidence,omitempty"`
	ConfirmationRequired    bool     `json:"confirmation_required,omitempty"`
	ConfirmationReasons     []string `json:"confirmation_reasons,omitempty"`
	CanonicalVersion        int      `json:"canonical_version"`
	CreatedAt               int64    `json:"created_at"`
	UpdatedAt               int64    `json:"updated_at"`
}

type AttemptBBox struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type Attempt struct {
	AttemptID        string       `json:"attempt_id"`
	AgentName        string       `json:"agent_name"`
	SubmissionID     string       `json:"submission_id"`
	ProblemID        string       `json:"problem_id"`
	AnswerState      string       `json:"answer_state"`
	AnswerRaw        string       `json:"answer_raw"`
	AnswerMarkdown   string       `json:"answer_markdown"`
	ConfirmedVersion int          `json:"confirmed_version"`
	InputDigest      string       `json:"input_digest,omitempty"`
	BBox             *AttemptBBox `json:"bbox,omitempty"`
	CreatedAt        int64        `json:"created_at"`
	UpdatedAt        int64        `json:"updated_at"`
}

type ProblemAttemptSnapshot struct {
	Problems []Problem `json:"problems"`
	Attempts []Attempt `json:"attempts"`
}
