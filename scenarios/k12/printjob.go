package k12

// PracticePrintJob is the durable DD-023 bridge between a frozen practice-paper
// source and one native Desktop print attempt. The PracticeSet remains draft until
// the native adapter reports a definitive printed receipt.
type PracticePrintJob struct {
	PrintJobID         string `json:"print_job_id"`
	AgentName          string `json:"agent_name"`
	IdempotencyKey     string `json:"idempotency_key"`
	RequestDigest      string `json:"request_digest"`
	PracticeSetID      string `json:"practice_set_id"`
	BaseSetVersion     int    `json:"base_set_version"`
	ArtifactKind       string `json:"artifact_kind"`
	ArtifactID         string `json:"artifact_id"`
	QuestionArtifactID string `json:"question_artifact_id"`
	AnswerArtifactID   string `json:"answer_artifact_id"`
	PaperNo            string `json:"paper_no"`
	SourceDigest       string `json:"source_digest"`
	Status             string `json:"status"`
	AttemptCount       int    `json:"attempt_count"`
	NativeJobID        string `json:"native_job_id,omitempty"`
	NativeReceiptID    string `json:"native_receipt_id,omitempty"`
	PrinterSnapshot    string `json:"-"`
	FailureKind        string `json:"failure_kind,omitempty"`
	FailureDetail      string `json:"failure_detail,omitempty"`
	PreparedFieldsJSON string `json:"-"`
	PreparedAt         int64  `json:"prepared_at"`
	PrintedAt          int64  `json:"printed_at,omitempty"`
	CreatedAt          int64  `json:"created_at"`
	UpdatedAt          int64  `json:"updated_at"`
	Version            int    `json:"version"`
}

const (
	PrintJobPreparing  = "preparing"
	PrintJobDialogOpen = "dialog_open"
	PrintJobSubmitted  = "submitted"
	PrintJobPrinted    = "printed"
	PrintJobCancelled  = "cancelled"
	PrintJobFailed     = "failed"
	// PrintJobOutcomeUnknown is deliberately not success and does not expose
	// ordinary retry. A definitive native reconciliation receipt may still settle it.
	PrintJobOutcomeUnknown = "outcome_unknown"
)

func PracticePrintArtifactKindAllowed(kind string) bool {
	return kind == PaperKindQuestion || kind == PaperKindAnswer
}
