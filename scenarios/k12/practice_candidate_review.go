package k12

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

const (
	PracticeCandidateOriginal = "original"
	PracticeCandidateVariant  = "variant"

	PracticeCandidateGenerating   = "generating"
	PracticeCandidateReady        = "ready"
	PracticeCandidateFailed       = "failed"
	PracticeCandidateAlreadyInSet = "already_in_set"

	PracticeCandidateSelectionOpen      = "open"
	PracticeCandidateSelectionCommitted = "committed"

	MistakeReviewScheduled        = "scheduled"
	MistakeReviewDeferredThisWeek = "deferred_this_week"
	MistakeReviewSuppressed       = "suppressed"
	MistakeReviewMastered         = "mastered"

	MistakeReviewCommandDeferThisWeek = "defer_this_week"
	MistakeReviewCommandSuppress      = "suppress"
	MistakeReviewCommandRestore       = "restore"
)

type PracticeCandidateProblem struct {
	Subject                string   `json:"subject"`
	QuestionMarkdown       string   `json:"question_markdown"`
	Options                []string `json:"options,omitempty"`
	ResourceDigests        []string `json:"resource_digests,omitempty"`
	ExpectedAnswerMarkdown string   `json:"expected_answer_markdown,omitempty"`
}

type canonicalPracticeProblem struct {
	Subject          string   `json:"subject"`
	QuestionMarkdown string   `json:"question_markdown"`
	Options          []string `json:"options"`
	ResourceDigests  []string `json:"resource_digests"`
}

func normalizeProblemText(value string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(value, "\r\n", "\n")), " ")
}

func NormalizePracticeCandidateProblem(problem PracticeCandidateProblem) PracticeCandidateProblem {
	problem.Subject = strings.TrimSpace(problem.Subject)
	problem.QuestionMarkdown = normalizeProblemText(problem.QuestionMarkdown)
	problem.ExpectedAnswerMarkdown = strings.TrimSpace(problem.ExpectedAnswerMarkdown)
	for i := range problem.Options {
		problem.Options[i] = normalizeProblemText(problem.Options[i])
	}
	for i := range problem.ResourceDigests {
		problem.ResourceDigests[i] = strings.TrimSpace(problem.ResourceDigests[i])
	}
	return problem
}

func StablePracticeProblemHash(problem PracticeCandidateProblem) (hash, canonicalJSON string, err error) {
	problem = NormalizePracticeCandidateProblem(problem)
	canonical := canonicalPracticeProblem{
		Subject: problem.Subject, QuestionMarkdown: problem.QuestionMarkdown,
		Options:         append([]string(nil), problem.Options...),
		ResourceDigests: append([]string(nil), problem.ResourceDigests...),
	}
	if canonical.Options == nil {
		canonical.Options = []string{}
	}
	if canonical.ResourceDigests == nil {
		canonical.ResourceDigests = []string{}
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(append([]byte("k12-problem-hash:v1\n"), raw...))
	return hex.EncodeToString(sum[:]), string(raw), nil
}

func StableHashSetDigest(hashes []string) string {
	values := append([]string(nil), hashes...)
	sort.Strings(values)
	sum := sha256.Sum256([]byte(strings.Join(values, "\n")))
	return hex.EncodeToString(sum[:])
}

type PracticeCandidate struct {
	CandidateID            string                   `json:"candidate_id"`
	CandidateKind          string                   `json:"candidate_kind"`
	BatchOrdinal           int                      `json:"batch_ordinal"`
	CandidateOrdinal       int                      `json:"candidate_ordinal"`
	NormalizedContentHash  string                   `json:"normalized_content_hash"`
	State                  string                   `json:"state"`
	Problem                PracticeCandidateProblem `json:"-"`
	QuestionMarkdown       string                   `json:"question_markdown"`
	ExpectedAnswerMarkdown string                   `json:"expected_answer_markdown,omitempty"`
	FailureMessage         string                   `json:"failure_message,omitempty"`
	BatchIdempotencyKey    string                   `json:"-"`
}

type PracticeCandidateSelection struct {
	SelectionID       string              `json:"selection_id"`
	AgentName         string              `json:"-"`
	SourceMistakeID   string              `json:"source_mistake_id"`
	TargetSetRecordID string              `json:"target_set_record_id"`
	State             string              `json:"state"`
	NextBatchOrdinal  int                 `json:"next_batch_ordinal"`
	Revision          int                 `json:"revision"`
	Grade             string              `json:"-"`
	Textbook          string              `json:"-"`
	RouteSnapshotJSON string              `json:"-"`
	SourceSessionID   string              `json:"-"`
	Candidates        []PracticeCandidate `json:"candidates"`
}

type MistakeReviewState struct {
	AgentName         string `json:"agent"`
	MistakeRecordID   string `json:"mistake_record_id"`
	State             string `json:"state"`
	DeferredISOYear   int    `json:"deferred_iso_year,omitempty"`
	DeferredISOWeek   int    `json:"deferred_iso_week,omitempty"`
	PriorScheduleJSON string `json:"-"`
	Revision          int    `json:"revision"`
	UpdatedAt         int64  `json:"updated_at"`
}
