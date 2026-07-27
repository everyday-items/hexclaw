package k12

// WeeklyPracticeAssessmentCommandStatus is the crash-durable ownership and
// recovery state for one physical answer-assessment call.
type WeeklyPracticeAssessmentCommandStatus string

const (
	WeeklyAssessmentPrepared      WeeklyPracticeAssessmentCommandStatus = "prepared"
	WeeklyAssessmentSent          WeeklyPracticeAssessmentCommandStatus = "sent"
	WeeklyAssessmentSucceeded     WeeklyPracticeAssessmentCommandStatus = "succeeded"
	WeeklyAssessmentFailed        WeeklyPracticeAssessmentCommandStatus = "failed"
	WeeklyAssessmentOutcomeUnknown WeeklyPracticeAssessmentCommandStatus = "outcome_unknown"
	WeeklyAssessmentCommitted     WeeklyPracticeAssessmentCommandStatus = "committed"
)

// WeeklyPracticeAssessmentCommand persists both the immutable request binding
// and the provider receipt. A succeeded receipt can be committed locally after
// a restart without issuing another physical provider call.
type WeeklyPracticeAssessmentCommand struct {
	CommandID        string
	AgentName        string
	SnapshotID       string
	ItemID           string
	IdempotencyKey   string
	RequestDigest    string
	Status           WeeklyPracticeAssessmentCommandStatus
	AssessmentJSON   string
	AssessmentDigest string
	FailureKind      string
	AttemptID        string
	CreatedAt        int64
	UpdatedAt        int64
}
