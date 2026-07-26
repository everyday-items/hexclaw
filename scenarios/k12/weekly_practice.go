package k12

const (
	WeeklySectionDueReview             = "due_review"
	WeeklySectionTextbookConsolidation = "textbook_consolidation"
	WeeklySectionArithmeticWarmup      = "arithmetic_warmup"

	WeeklyTrackReady    = "ready"
	WeeklyTrackDisabled = "disabled"
	WeeklyTrackFailed   = "failed"

	WeeklyPlanDraft         = "draft"
	WeeklyPlanFrozen        = "frozen"
	WeeklyPlanArchived      = "archived"
	WeeklyPlanExpiredUnused = "expired_unused"

	WeeklyVerificationVerified = "verified"
	WeeklyVerificationFailed   = "failed"

	WeeklyAttemptCorrect     = "correct"
	WeeklyAttemptWrong       = "wrong"
	WeeklyAttemptNeedsReview = "needs_review"

	WeeklyGenerationMethodOriginal      = "original"
	WeeklyGenerationMethodAIVariant     = "ai_variant"
	WeeklyGenerationMethodAIGenerated   = "ai_generated"
	WeeklyGenerationMethodRuleGenerated = "rule_generated"
)

func WeeklySupplementGenerationMethodAllowed(value string) bool {
	switch value {
	case WeeklyGenerationMethodAIVariant,
		WeeklyGenerationMethodAIGenerated,
		WeeklyGenerationMethodRuleGenerated:
		return true
	default:
		return false
	}
}

type CurriculumCatalogLesson struct {
	LessonID string `json:"lesson_id"`
	Title    string `json:"title"`
	PageFrom int    `json:"page_from"`
	PageTo   int    `json:"page_to"`
}

type CurriculumCatalogUnit struct {
	UnitID   string                    `json:"unit_id"`
	Title    string                    `json:"title"`
	PageFrom int                       `json:"page_from"`
	PageTo   int                       `json:"page_to"`
	Lessons  []CurriculumCatalogLesson `json:"lessons"`
}

type CurriculumCatalog struct {
	AgentName        string                  `json:"agent"`
	Subject          string                  `json:"subject"`
	TextbookBindingID string                 `json:"textbook_binding_id"`
	TextbookEdition  string                  `json:"textbook_edition"`
	TextbookVersion  string                  `json:"textbook_version"`
	Title            string                  `json:"title"`
	Volume           string                  `json:"volume"`
	PageMin          int                     `json:"page_min"`
	PageMax          int                     `json:"page_max"`
	Units            []CurriculumCatalogUnit `json:"units"`
}

type CurriculumProgress struct {
	ProgressID            string   `json:"progress_id"`
	AgentName              string   `json:"agent"`
	Subject                string   `json:"subject"`
	Revision               int      `json:"revision"`
	TextbookBindingID      string   `json:"textbook_binding_id"`
	TextbookEdition        string   `json:"textbook_edition"`
	TextbookVersion        string   `json:"textbook_version"`
	Title                  string   `json:"title"`
	Volume                 string   `json:"volume"`
	UnitID                 string   `json:"unit_id"`
	UnitTitle              string   `json:"unit_title"`
	LessonID               string   `json:"lesson_id,omitempty"`
	LessonTitle            string   `json:"lesson_title,omitempty"`
	RequestedPageFrom      *int     `json:"requested_page_from,omitempty"`
	RequestedPageTo        *int     `json:"requested_page_to,omitempty"`
	VerifiedPageFrom       *int     `json:"verified_page_from,omitempty"`
	VerifiedPageTo         *int     `json:"verified_page_to,omitempty"`
	PageVerificationStatus string   `json:"page_verification_status"`
	SegmentRefs            []string `json:"segment_refs"`
	EvidenceSource         string   `json:"evidence_source"`
	ConfirmedAt            int64    `json:"confirmed_at"`
	CreatedAt              int64    `json:"created_at"`
	UpdatedAt              int64    `json:"updated_at"`
}

type WeeklyPracticeSettings struct {
	AgentName                     string `json:"agent"`
	Revision                      int    `json:"revision"`
	Timezone                      string `json:"timezone"`
	DueReviewEnabled              bool   `json:"due_review_enabled"`
	TextbookConsolidationEnabled  bool   `json:"textbook_consolidation_enabled"`
	ArithmeticWarmupEnabled       bool   `json:"arithmetic_warmup_enabled"`
	ArithmeticMinutes             int    `json:"arithmetic_minutes"`
	CreatedAt                     int64  `json:"created_at"`
	UpdatedAt                     int64  `json:"updated_at"`
}

func DefaultWeeklyPracticeSettings(agentName string) WeeklyPracticeSettings {
	return WeeklyPracticeSettings{
		AgentName: agentName, Timezone: "Asia/Shanghai", DueReviewEnabled: true,
		TextbookConsolidationEnabled: false, ArithmeticWarmupEnabled: false,
		ArithmeticMinutes: 2,
	}
}

type WeeklyProfile struct {
	ChildName        string           `json:"child_name"`
	GradeTerm        string           `json:"grade_term"`
	SubjectTextbooks SubjectTextbooks `json:"subject_textbooks"`
	TextbookEdition  string           `json:"textbook_edition"`
	Revision         int              `json:"revision"`
}

type ProfileBundleProfile struct {
	ChildName        string           `json:"child_name"`
	GradeTerm        string           `json:"grade_term"`
	SubjectTextbooks SubjectTextbooks `json:"subject_textbooks"`
	TextbookEdition  string           `json:"textbook_edition"`
}

type ProfileBundleResult struct {
	Profile                ProfileBundleProfile   `json:"profile"`
	CurriculumProgress     CurriculumProgress     `json:"curriculum_progress"`
	WeeklyPracticeSettings WeeklyPracticeSettings `json:"weekly_practice_settings"`
	Replayed               bool                   `json:"replayed"`
}

type WeeklyPracticeVerification struct {
	Status                  string   `json:"status"`
	EvidenceRefs            []string `json:"evidence_refs"`
	TextbookBindingID       string   `json:"textbook_binding_id,omitempty"`
	UnitID                  string   `json:"unit_id,omitempty"`
	LessonID                string   `json:"lesson_id,omitempty"`
	VerifiedPageFrom        *int     `json:"verified_page_from,omitempty"`
	VerifiedPageTo          *int     `json:"verified_page_to,omitempty"`
}

type WeeklyPracticeItem struct {
	ItemID           string                     `json:"item_id"`
	Position         int                        `json:"position"`
	PlanSection      string                     `json:"plan_section"`
	SourceKind       string                     `json:"source_kind"`
	GenerationMethod string                     `json:"generation_method"`
	SourceRef        string                     `json:"source_ref"`
	Verification    WeeklyPracticeVerification `json:"verification"`
	PromptMarkdown  string                     `json:"prompt_markdown"`
}

type WeeklyPracticeTrack struct {
	PlanSection    string               `json:"plan_section"`
	Status         string               `json:"status"`
	FailureMessage string               `json:"failure_message,omitempty"`
	Items          []WeeklyPracticeItem `json:"items"`
}

type WeeklyPracticePlan struct {
	PlanID                     string                `json:"plan_id"`
	AgentName                  string                `json:"agent"`
	Revision                   int                   `json:"revision"`
	ISOWeekYear                int                   `json:"iso_week_year"`
	ISOWeekNumber              int                   `json:"iso_week_number"`
	Timezone                   string                `json:"timezone"`
	WeekStart                  int64                 `json:"week_start"`
	WeekEnd                    int64                 `json:"week_end"`
	LocalStartDate             string                `json:"local_start_date"`
	LocalEndDate               string                `json:"local_end_date"`
	Status                     string                `json:"status"`
	SettingsRevision           int                   `json:"settings_revision"`
	CurriculumProgressRevision *int                  `json:"curriculum_progress_revision,omitempty"`
	Tracks                     []WeeklyPracticeTrack `json:"tracks"`
	CreatedAt                  int64                 `json:"created_at"`
	UpdatedAt                  int64                 `json:"updated_at"`
	SourceDigest               string                `json:"-"`
	AnswerKeys                 map[string]string     `json:"-"`
}

type WeeklyPracticeSnapshot struct {
	SnapshotID                 string                `json:"snapshot_id"`
	PlanID                     string                `json:"plan_id"`
	PlanRevision               int                   `json:"plan_revision"`
	AgentName                  string                `json:"agent"`
	ISOWeekYear                int                   `json:"iso_week_year"`
	ISOWeekNumber              int                   `json:"iso_week_number"`
	Timezone                   string                `json:"timezone"`
	WeekStart                  int64                 `json:"week_start"`
	WeekEnd                    int64                 `json:"week_end"`
	LocalStartDate             string                `json:"local_start_date"`
	LocalEndDate               string                `json:"local_end_date"`
	SettingsRevision           int                   `json:"settings_revision"`
	CurriculumProgressRevision *int                  `json:"curriculum_progress_revision,omitempty"`
	Tracks                     []WeeklyPracticeTrack `json:"tracks"`
	RenderVersion              string                `json:"render_version"`
	SnapshotDigest             string                `json:"snapshot_digest"`
	CreatedAt                  int64                 `json:"created_at"`
	AnswerKeys                 map[string]string     `json:"-"`
}

type WeeklyPracticeHistorySummary struct {
	SnapshotID     string `json:"snapshot_id"`
	PlanID         string `json:"plan_id"`
	ISOWeekYear    int    `json:"iso_week_year"`
	ISOWeekNumber  int    `json:"iso_week_number"`
	Timezone       string `json:"timezone"`
	LocalStartDate string `json:"local_start_date"`
	LocalEndDate   string `json:"local_end_date"`
	ItemCount      int    `json:"item_count"`
	ArchivedAt     int64  `json:"archived_at"`
}

type WeeklyPracticeAttempt struct {
	AttemptID           string `json:"attempt_id"`
	SnapshotID          string `json:"snapshot_id"`
	ItemID              string `json:"item_id"`
	AssessmentID        string `json:"assessment_id"`
	Result              string `json:"result"`
	VerificationEvidence string `json:"verification_evidence"`
	MistakeRecordID      string `json:"mistake_record_id,omitempty"`
	ReviewScheduled      bool   `json:"review_scheduled"`
	CreatedAt            int64  `json:"created_at"`
}

type WeeklyPracticeSaveReceipt struct {
	SaveReceiptID string `json:"save_receipt_id"`
	PlanID        string `json:"plan_id"`
	PlanRevision  int    `json:"plan_revision"`
	SnapshotID    string `json:"snapshot_id"`
	PracticeSetID string `json:"practice_set_id"`
	CreatedAt     int64  `json:"created_at"`
}
