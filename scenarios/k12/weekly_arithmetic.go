package k12

const (
	WeeklyArithmeticPreparing      = "preparing"
	WeeklyArithmeticReady          = "ready"
	WeeklyArithmeticInProgress     = "in_progress"
	WeeklyArithmeticCompleted      = "completed"
	WeeklyArithmeticFailedRetryable = "failed_retryable"
	WeeklyArithmeticFailedTerminal = "failed_terminal"
)

// WeeklyArithmeticBatch is the public projection of one append-only internal
// batch. Internal ownership, ordinal, checkpoint and frozen content are omitted
// from JSON so the public DTO remains exact.
type WeeklyArithmeticBatch struct {
	BatchID       string `json:"batch_id"`
	State         string `json:"state"`
	ItemCount     int    `json:"item_count"`
	ContentDigest string `json:"content_digest"`
	Retryable     bool   `json:"retryable"`
	FailureMessage string `json:"failure_message"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
	CompletedAt   *int64 `json:"completed_at,omitempty"`

	AgentName           string                  `json:"-"`
	PlanID              string                  `json:"-"`
	Ordinal             int                     `json:"-"`
	GenerationCheckpoint string                  `json:"-"`
	Items                []WeeklyPracticeItem    `json:"-"`
	AnswerKeys           map[string]string       `json:"-"`
}

type WeeklyArithmeticAttempt struct {
	AttemptID           string `json:"attempt_id"`
	BatchID             string `json:"batch_id"`
	ItemID              string `json:"item_id"`
	AssessmentID        string `json:"assessment_id"`
	Result              string `json:"result"`
	VerificationEvidence string `json:"verification_evidence"`
	MistakeRecordID     string `json:"mistake_record_id,omitempty"`
	ReviewScheduled     bool   `json:"review_scheduled"`
	CreatedAt           int64  `json:"created_at"`
}
