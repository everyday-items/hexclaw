package k12

const (
	PrintSourceTutoringTips        = "tutoring_tips"
	PrintSourceCreativeObservation = "creative_observation_card"
	PrintSourcePracticeQuestion    = "practice_question"
	PrintSourcePracticeAnswer      = "practice_answer"
	MaxPrintAttempts               = 3
)

func GenericPrintSourceKindAllowed(kind string) bool {
	switch kind {
	case PrintSourceTutoringTips, PrintSourceCreativeObservation,
		PrintSourcePracticeQuestion, PrintSourcePracticeAnswer:
		return true
	default:
		return false
	}
}

// PrintArtifact is the immutable canonical payload passed to a native print
// adapter. Its digest is stable across retries and process restarts.
type PrintArtifact struct {
	ArtifactID        string `json:"artifact_id"`
	AgentName         string `json:"agent_name"`
	SourceKind        string `json:"source_kind"`
	SourceRef         string `json:"source_ref"`
	Title             string `json:"title"`
	CanonicalMarkdown string `json:"canonical_markdown"`
	SourceDigest      string `json:"source_digest"`
	CreatedAt         int64  `json:"created_at"`
}

// GenericPrintJob records native-print state without changing the source
// CreativeWork, TutoringTips or already-finalized PracticeSet.
type GenericPrintJob struct {
	PrintJobID      string `json:"print_job_id"`
	AgentName       string `json:"agent_name"`
	IdempotencyKey  string `json:"idempotency_key"`
	RequestDigest   string `json:"request_digest"`
	ArtifactID      string `json:"artifact_id"`
	Status          string `json:"status"`
	AttemptCount    int    `json:"attempt_count"`
	NativeJobID     string `json:"native_job_id,omitempty"`
	NativeReceiptID string `json:"native_receipt_id,omitempty"`
	PrinterSnapshot string `json:"-"`
	FailureKind     string `json:"failure_kind,omitempty"`
	FailureDetail   string `json:"failure_detail,omitempty"`
	PreparedAt      int64  `json:"prepared_at"`
	PrintedAt       int64  `json:"printed_at,omitempty"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
	Version         int    `json:"version"`
}
