package usecase

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

const (
	TutoringTipsSourceTextbook         = "📖 依据课本"
	TutoringTipsSourceAI               = "🤖 AI 归纳·供参考"
	TutoringTipsSourceLearningEvidence = "🧠 学情信号"
)

// TutoringTipsSection is one of the three fixed, ordered sections rendered in
// the confirmed homework flow. It is never a standalone navigation object.
type TutoringTipsSection struct {
	Title       string `json:"title"`
	Content     string `json:"content"`
	SourceLabel string `json:"source_label"`
}

// TutoringTipsProblem is the trusted internal exact-set used to prove that the
// third section covers every answerable durable Problem once. HTTP responses do
// not expose it because the current contract returns only knowledge_points and
// the three sections.
type TutoringTipsProblem struct {
	ProblemID            string
	StemMarkdown         string
	Subject              string
	ConceptIDs           []string
	SourceNumberPath     []string
	DisplayLabel         string
	SourceSectionPath    []string
	SourceSectionLabel   string
	SystemSectionOrdinal int
	SystemDisplayLabel   string
}

// TutoringTips is an ephemeral, read-only projection of one confirmed
// GradingJob. No additional table or mutable current-selection state exists.
type TutoringTips struct {
	GradingJobID              string
	SubmissionID              string
	Grade                     string
	Subject                   string
	KnowledgePoints           []string                   `json:"knowledge_points"`
	Problems                  []TutoringTipsProblem      `json:"-"`
	Sections                  []TutoringTipsSection      `json:"sections"`
	GroundingEvidenceReceipts []GroundingEvidenceReceipt `json:"grounding_evidence_receipts"`
}

var tutoringTipsBuildBudget = 90 * time.Second

// BuildTutoringTips resolves every content fact from an owner-scoped confirmed
// GradingJob. The client cannot supply grade, subject, concepts, or problems.
func (d Deps) BuildTutoringTips(ctx context.Context, agentName, gradingJobID string) (TutoringTips, error) {
	return d.BuildTutoringTipsSubject(ctx, agentName, gradingJobID)
}

// BuildTutoringTipsForOwner carries the authenticated/composition-owned
// Knowledge principal into the textbook grounding lookup without treating the
// tutor agent name as an authorization identity.
func (d Deps) BuildTutoringTipsForOwner(
	ctx context.Context,
	ownerID, agentName, gradingJobID string,
) (TutoringTips, error) {
	d.TextbookOwnerID = strings.TrimSpace(ownerID)
	if d.TextbookOwnerID == "" {
		return TutoringTips{}, fmt.Errorf("%w: textbook owner required", ErrInvalidInput)
	}
	return d.BuildTutoringTipsSubject(ctx, agentName, gradingJobID)
}

// BuildTutoringTipsSubject is the subject-aware canonical builder. Subject is
// derived from the durable Problem exact-set rather than accepted as an input.
func (d Deps) BuildTutoringTipsSubject(ctx context.Context, agentName, gradingJobID string) (TutoringTips, error) {
	return d.buildTutoringTipsSubject(ctx, agentName, gradingJobID, nil)
}

func (d Deps) buildTutoringTipsSubject(
	ctx context.Context,
	agentName, gradingJobID string,
	frozenGrounding *GroundingSnapshot,
) (TutoringTips, error) {
	agentName = strings.TrimSpace(agentName)
	gradingJobID = strings.TrimSpace(gradingJobID)
	if agentName == "" || gradingJobID == "" {
		return TutoringTips{}, fmt.Errorf("%w: agent / grading_job_id required", ErrInvalidInput)
	}
	if d.Records == nil {
		return TutoringTips{}, fmt.Errorf("usecase: canonical K12 store unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, tutoringTipsBuildBudget)
	defer cancel()

	job, err := d.GetGradingJob(ctx, agentName, gradingJobID)
	if err != nil {
		return TutoringTips{}, err
	}
	if job.Fields.ConfirmationState != k12.GradingConfirmationConfirmed {
		return TutoringTips{}, fmt.Errorf("%w: GradingJob confirmation_state must be confirmed", records.ErrIllegalTransition)
	}
	if !tutoringTipsStageAllowed(job.Record.Status) {
		return TutoringTips{}, fmt.Errorf("%w: GradingJob stage %s cannot generate tutoring tips", records.ErrIllegalTransition, job.Record.Status)
	}

	profile, err := d.GetProfile(ctx, agentName)
	if err != nil {
		return TutoringTips{}, fmt.Errorf("usecase: derive tutoring tips profile: %w", err)
	}
	grade := strings.TrimSpace(profile.GradeTerm)
	if err := validateGradeInput(grade); err != nil {
		return TutoringTips{}, fmt.Errorf("%w: durable profile grade required", ErrInvalidInput)
	}
	childName := strings.TrimSpace(profile.ChildName)
	if childName == "" {
		return TutoringTips{}, fmt.Errorf("%w: durable profile child_name required", ErrInvalidInput)
	}

	questions, err := d.loadCurrentConfirmedQuestions(
		ctx, agentName, job.Fields.SubmissionID,
	)
	if err != nil {
		return TutoringTips{}, fmt.Errorf("usecase: derive tutoring tips facts: %w", err)
	}
	problems, subject, knowledgePoints, err := validateTutoringTipsFacts(questions, grade)
	if err != nil {
		return TutoringTips{}, err
	}
	grounding, err := d.resolveTutoringGrounding(
		ctx, agentName, subject, grade, profile, frozenGrounding,
	)
	if err != nil {
		return TutoringTips{}, err
	}
	grounding.receipts = new([]GroundingEvidenceReceipt)
	history, err := d.mistakesFor(ctx, agentName, knowledgePoints)
	if err != nil {
		return TutoringTips{}, err
	}
	tips := TutoringTips{
		GradingJobID: gradingJobID, SubmissionID: job.Fields.SubmissionID,
		Grade: grade, Subject: subject, KnowledgePoints: knowledgePoints, Problems: problems,
		GroundingEvidenceReceipts: []GroundingEvidenceReceipt{},
	}
	tips.Sections = []TutoringTipsSection{
		d.tutoringTipsOverviewWithGrounding(ctx, grounding, grade, subject, knowledgePoints),
		tutoringTipsLearningEvidence(childName, history),
		tutoringTipsPerProblem(problems),
	}
	tips.GroundingEvidenceReceipts = cloneGroundingEvidenceReceipts(*grounding.receipts)
	return tips, nil
}

func tutoringTipsStageAllowed(stage string) bool {
	switch stage {
	case k12.GradingStageAssessing, k12.GradingStageRendering,
		k12.GradingStageProjecting, k12.GradingStageCompleted:
		return true
	default:
		return false
	}
}

func validateTutoringTipsFacts(questions []RecognizedQuestion, _ string) ([]TutoringTipsProblem, string, []string, error) {
	if len(questions) == 0 {
		return nil, "", nil, fmt.Errorf("%w: durable Problem exact-set is empty", ErrInvalidInput)
	}
	parents := make(map[string]string)
	for _, question := range questions {
		if question.ProblemKind == ProblemKindCompoundParent {
			parents[question.ProblemID] = strings.TrimSpace(question.CanonicalMarkdown)
		}
	}
	seenProblem := make(map[string]struct{})
	seenConcept := make(map[string]struct{})
	knowledgePoints := make([]string, 0)
	problems := make([]TutoringTipsProblem, 0)
	subject := ""
	for _, question := range questions {
		if question.ProblemKind == ProblemKindCompoundParent {
			continue
		}
		problemID := strings.TrimSpace(question.ProblemID)
		if problemID == "" {
			return nil, "", nil, fmt.Errorf("%w: answerable Problem missing problem_id", ErrInvalidInput)
		}
		if _, duplicate := seenProblem[problemID]; duplicate {
			return nil, "", nil, fmt.Errorf("%w: duplicate answerable problem_id %s", ErrInvalidInput, problemID)
		}
		seenProblem[problemID] = struct{}{}
		if strings.TrimSpace(question.AttemptID) == "" || question.ConfirmedVersion < 1 || strings.TrimSpace(question.InputDigest) == "" {
			return nil, "", nil, fmt.Errorf("%w: Problem %s has no confirmed Attempt", ErrInvalidInput, problemID)
		}
		problemSubject, err := normalizeSubject(question.Subject)
		if err != nil || problemSubject == "" {
			return nil, "", nil, fmt.Errorf("%w: Problem %s has no valid durable subject", ErrInvalidInput, problemID)
		}
		if subject == "" {
			subject = problemSubject
		} else if subject != problemSubject {
			return nil, "", nil, fmt.Errorf("%w: Problem exact-set spans multiple subjects", ErrInvalidInput)
		}
		stem := strings.TrimSpace(question.CanonicalMarkdown)
		if question.ProblemKind == ProblemKindSubproblem {
			parentStem := strings.TrimSpace(parents[question.ParentProblemID])
			if parentStem == "" {
				return nil, "", nil, fmt.Errorf("%w: subproblem %s missing canonical parent", ErrInvalidInput, problemID)
			}
			stem = parentStem + "\n\n" + stem
		}
		concepts := make([]string, 0, len(question.KnowledgePoints))
		for _, raw := range question.KnowledgePoints {
			concept := strings.TrimSpace(raw)
			if concept == "" {
				continue
			}
			concepts = append(concepts, concept)
			if _, exists := seenConcept[concept]; !exists {
				seenConcept[concept] = struct{}{}
				knowledgePoints = append(knowledgePoints, concept)
			}
		}
		problems = append(problems, TutoringTipsProblem{
			ProblemID:            problemID,
			StemMarkdown:         stem,
			Subject:              subject,
			ConceptIDs:           concepts,
			SourceNumberPath:     append([]string(nil), question.SourceNumberPath...),
			DisplayLabel:         strings.TrimSpace(question.DisplayLabel),
			SourceSectionPath:    append([]string(nil), question.SourceSectionPath...),
			SourceSectionLabel:   strings.TrimSpace(question.SourceSectionLabel),
			SystemSectionOrdinal: question.SystemSectionOrdinal,
			SystemDisplayLabel:   strings.TrimSpace(question.SystemDisplayLabel),
		})
	}
	if len(problems) == 0 || len(knowledgePoints) == 0 {
		return nil, "", nil, fmt.Errorf("%w: durable Problem exact-set has no answerable concept facts", ErrInvalidInput)
	}
	return problems, subject, knowledgePoints, nil
}

type tutoringGroundingContext struct {
	snapshot        GroundingSnapshot
	snapshotter     SnapshotGrounding
	legacyPermitted bool
	required        bool
	receipts        *[]GroundingEvidenceReceipt
}

func (d Deps) resolveTutoringGrounding(
	ctx context.Context,
	agentName, subject, grade string,
	profile k12.ChildProfile,
	frozenGrounding *GroundingSnapshot,
) (tutoringGroundingContext, error) {
	if frozenGrounding != nil {
		snapshot := cloneGradingGroundingSnapshot(*frozenGrounding)
		if err := validateGradingGroundingSnapshot(snapshot); err != nil {
			return tutoringGroundingContext{}, err
		}
		if snapshot.AgentName != agentName || snapshot.LearnerID != agentName ||
			snapshot.Subject != subject {
			return tutoringGroundingContext{}, fmt.Errorf(
				"%w: frozen textbook snapshot does not match the page summary",
				ErrGradingGroundingUnavailable,
			)
		}
		snapshotter, ok := d.Grounding.(SnapshotGrounding)
		if !ok {
			return tutoringGroundingContext{}, fmt.Errorf(
				"%w: pinned grounding lookup is unavailable",
				ErrGradingGroundingUnavailable,
			)
		}
		if _, ok := snapshotter.(SnapshotGroundingEvidence); !ok {
			return tutoringGroundingContext{}, fmt.Errorf(
				"%w: pinned grounding evidence lookup is unavailable",
				ErrGradingGroundingUnavailable,
			)
		}
		return tutoringGroundingContext{
			snapshot: snapshot, snapshotter: snapshotter, required: true,
		}, nil
	}

	requested := GroundingSnapshot{
		AgentName: agentName,
		// 当前耐久档案以 agent 为键。这是明确的过渡期 owner 作用域，不能据此虚构
		// 独立 learner ID。
		LearnerID: agentName,
		Subject:   subject,
		Edition:   strings.TrimSpace(profile.TextbookEdition),
		Volume:    textbookVolumeFromGradeTerm(grade),
	}
	required := false
	if d.Records != nil && subject == "数学" && strings.TrimSpace(d.TextbookOwnerID) != "" {
		textbookScope := k12storage.TextbookScope{
			OwnerID: strings.TrimSpace(d.TextbookOwnerID), AgentName: agentName, Subject: "math",
		}
		scope, found, resolveErr := d.Records.GetActiveTextbookGroundingScope(ctx, textbookScope)
		switch {
		case resolveErr != nil:
			required = true
		case found:
			required = true
			requested.TextbookBindingID = scope.TextbookBindingID
			requested.TextbookManifestID = scope.TextbookManifestID
			requested.DocumentID = scope.DocumentID
			requested.DocumentGeneration = scope.DocumentGeneration
			requested.SourceDigest = scope.SourceDigest
			requested.Edition = scope.Edition
			requested.Volume = scope.Volume
			requested.SegmentRefs = append([]string(nil), scope.SegmentRefs...)
			requested.PageRefs = append([]k12.TextbookGroundingPageRef(nil), scope.PageRefs...)
		default:
			_, handled, catalogErr := d.Records.GetActiveTextbookCatalog(ctx, textbookScope)
			required = catalogErr != nil || handled
		}
	}
	grounding := d.freezeTutoringGrounding(ctx, requested)
	grounding.required = required
	return grounding, nil
}

func (d Deps) freezeTutoringGrounding(ctx context.Context, requested GroundingSnapshot) tutoringGroundingContext {
	scoped, supported := d.Grounding.(SnapshotGrounding)
	if !supported {
		return tutoringGroundingContext{snapshot: requested, legacyPermitted: d.Grounding != nil}
	}
	frozen, err := scoped.FreezeGroundingSnapshot(ctx, requested)
	if err != nil {
		// A snapshot-aware implementation must not silently fall back to its
		// mutable legacy interface after a control-plane failure.
		return tutoringGroundingContext{snapshot: requested}
	}
	return tutoringGroundingContext{snapshot: frozen, snapshotter: scoped}
}

func textbookVolumeFromGradeTerm(grade string) string {
	grade = strings.TrimSpace(grade)
	switch {
	case strings.HasSuffix(grade, "上"):
		return "上册"
	case strings.HasSuffix(grade, "下"):
		return "下册"
	default:
		return ""
	}
}

func (d Deps) tutoringTipsOverview(ctx context.Context, agentName, grade, subject string, concepts []string) TutoringTipsSection {
	return d.tutoringTipsOverviewWithGrounding(ctx, tutoringGroundingContext{
		snapshot: GroundingSnapshot{
			AgentName: agentName,
			LearnerID: agentName,
			Subject:   subject,
			Volume:    textbookVolumeFromGradeTerm(grade),
		},
		legacyPermitted: d.Grounding != nil,
	}, grade, subject, concepts)
}

func (d Deps) tutoringTipsOverviewWithGrounding(ctx context.Context, grounding tutoringGroundingContext, grade, subject string, concepts []string) TutoringTipsSection {
	var content strings.Builder
	verifiedGroundedCount := 0
	for _, concept := range concepts {
		if d.Grounding != nil {
			if result, err := d.groundTutoringConcept(ctx, grounding, subject, concept, grade); err == nil && result.found {
				verified := len(result.receipts) > 0
				if grounding.required && !verified {
					fmt.Fprintf(&content, "### %s\n\n暂未找到可核验的课本讲解，请以当前教材为准。\n\n", concept)
					continue
				}
				fmt.Fprintf(&content, "### %s\n\n%s\n\n", concept, strings.TrimSpace(result.text))
				if verified || (grounding.snapshotter == nil && grounding.receipts == nil) {
					verifiedGroundedCount++
				}
				if verified && grounding.receipts != nil {
					*grounding.receipts = appendUniqueGroundingEvidenceReceipts(
						*grounding.receipts, result.receipts,
					)
				}
				continue
			}
		}
		if ctx.Err() != nil {
			fmt.Fprintf(&content, "### %s\n\n暂未找到可核验的课本讲解，请以当前教材为准。\n\n", concept)
			continue
		}
		if grounding.required {
			fmt.Fprintf(&content, "### %s\n\n暂未找到可核验的课本讲解，请以当前教材为准。\n\n", concept)
			continue
		}
		fmt.Fprintf(&content, "### %s\n\n结合课本例题回顾概念、计算步骤和验算方法。\n\n", concept)
	}
	label := TutoringTipsSourceTextbook
	if verifiedGroundedCount != len(concepts) {
		label = TutoringTipsSourceAI
	}
	return TutoringTipsSection{Title: "这页在练什么", Content: strings.TrimSpace(content.String()), SourceLabel: label}
}

type tutoringGroundingResult struct {
	text     string
	found    bool
	receipts []GroundingEvidenceReceipt
}

func appendUniqueGroundingEvidenceReceipts(
	current, incoming []GroundingEvidenceReceipt,
) []GroundingEvidenceReceipt {
	seen := make(map[GroundingEvidenceReceipt]struct{}, len(current)+len(incoming))
	for _, receipt := range current {
		seen[receipt] = struct{}{}
	}
	for _, receipt := range incoming {
		if _, duplicate := seen[receipt]; duplicate {
			continue
		}
		seen[receipt] = struct{}{}
		current = append(current, receipt)
	}
	return current
}

type tutoringTipsCallResult[T any] struct {
	value T
	err   error
}

const tutoringTipsCallCapacity = 8

var tutoringTipsCallGate = make(chan struct{}, tutoringTipsCallCapacity)

// awaitTutoringTipsCall 确保即使端口实现不响应 ctx.Done()，页面摘要预算仍具权威性。
// 全局门禁使过期调用最多占用固定容量，避免不返回的端口持续遗留后台 work。
func awaitTutoringTipsCall[T any](ctx context.Context, call func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	select {
	case tutoringTipsCallGate <- struct{}{}:
	case <-ctx.Done():
		return zero, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		<-tutoringTipsCallGate
		return zero, err
	}
	result := make(chan tutoringTipsCallResult[T], 1)
	go func() {
		defer func() { <-tutoringTipsCallGate }()
		value, err := call()
		result <- tutoringTipsCallResult[T]{value: value, err: err}
	}()
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case completed := <-result:
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		return completed.value, completed.err
	}
}

func groundedTutoringTipsMarkdown(ctx context.Context, generator TutoringTipsReviewGenerator,
	subject, concept, grade, evidence string,
) string {
	evidence = strings.TrimSpace(evidence)
	if grounded, ok := generator.(GroundedTutoringTipsReviewGenerator); ok {
		if text, err := awaitTutoringTipsCall(ctx, func() (string, error) {
			return grounded.GenerateGroundedTutoringTipsReview(ctx, subject, concept, grade, evidence)
		}); err == nil && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return evidence
}

func (d Deps) groundForSubject(ctx context.Context, agentName, subject, concept, grade string) (string, bool, error) {
	if scoped, ok := d.Grounding.(SubjectGrounding); ok {
		return scoped.GroundSubject(ctx, agentName, subject, concept, grade)
	}
	return d.Grounding.Ground(ctx, agentName, concept, grade)
}

func (d Deps) groundTutoringConcept(
	ctx context.Context,
	grounding tutoringGroundingContext,
	subject, concept, grade string,
) (tutoringGroundingResult, error) {
	if grounding.snapshotter == nil && !grounding.legacyPermitted {
		return tutoringGroundingResult{receipts: []GroundingEvidenceReceipt{}}, nil
	}
	type result struct {
		grounding tutoringGroundingResult
	}
	grounded, err := awaitTutoringTipsCall(ctx, func() (result, error) {
		if grounding.snapshotter != nil {
			if evidenced, ok := grounding.snapshotter.(SnapshotGroundingEvidence); ok {
				value, groundErr := evidenced.GroundSnapshotWithEvidence(
					ctx, grounding.snapshot, concept, grade,
				)
				if groundErr != nil {
					return result{}, groundErr
				}
				if validateErr := value.validate(grounding.snapshot); validateErr != nil {
					return result{}, validateErr
				}
				return result{grounding: tutoringGroundingResult{
					text: value.Text, found: value.Found,
					receipts: cloneGroundingEvidenceReceipts(value.Receipts),
				}}, nil
			}
			evidence, found, groundErr := grounding.snapshotter.GroundSnapshot(ctx, grounding.snapshot, concept, grade)
			return result{grounding: tutoringGroundingResult{
				text: evidence, found: found,
				receipts: []GroundingEvidenceReceipt{},
			}}, groundErr
		}
		evidence, found, groundErr := d.groundForSubject(ctx, grounding.snapshot.AgentName, subject, concept, grade)
		return result{grounding: tutoringGroundingResult{
			text: evidence, found: found,
			receipts: []GroundingEvidenceReceipt{},
		}}, groundErr
	})
	if err != nil {
		return tutoringGroundingResult{}, err
	}
	return grounded.grounding, nil
}

func tutoringTipsLearningEvidence(childName string, history []ReviewItem) TutoringTipsSection {
	if len(history) == 0 {
		return TutoringTipsSection{
			Title: childName + "要留意", Content: "暂无历史证据。先观察孩子如何读题、选择方法和检查结果。",
			SourceLabel: TutoringTipsSourceLearningEvidence,
		}
	}
	sort.SliceStable(history, func(i, j int) bool {
		return history[i].Record.RecordID < history[j].Record.RecordID
	})
	var content strings.Builder
	for _, item := range history {
		concept := strings.TrimSpace(item.Fields.KnowledgePoint)
		cause := strings.TrimSpace(item.Fields.ErrorCause)
		if cause == "" {
			cause = "错因尚未归纳"
		}
		fmt.Fprintf(&content, "- %s：%s\n", concept, cause)
	}
	return TutoringTipsSection{
		Title: childName + "要留意", Content: strings.TrimSpace(content.String()),
		SourceLabel: TutoringTipsSourceLearningEvidence,
	}
}

func tutoringTipsPerProblem(problems []TutoringTipsProblem) TutoringTipsSection {
	var content strings.Builder
	for index, problem := range problems {
		label := RecognizedQuestionSourceDisplayLabel(RecognizedQuestion{
			SourceSectionLabel: problem.SourceSectionLabel,
			DisplayLabel:       problem.DisplayLabel,
			SystemDisplayLabel: problem.SystemDisplayLabel,
		})
		if label == "" {
			label = fmt.Sprintf("第 %d 题", index+1)
		}
		fmt.Fprintf(&content, "### %s\n\n", label)
		fmt.Fprintf(&content, "%s\n\n", problem.StemMarkdown)
		if len(problem.ConceptIDs) > 0 {
			fmt.Fprintf(&content, "先让孩子说清题意，再引导他判断要用到的知识点（%s）并说明理由；完成后自己检查步骤、符号和单位。\n\n", strings.Join(problem.ConceptIDs, "、"))
		} else {
			content.WriteString("先让孩子说清题意，再引导他选择已经学过的方法并说明理由；完成后自己检查步骤、符号和单位。\n\n")
		}
	}
	return TutoringTipsSection{
		Title: "每道题怎么带（不直接给答案）", Content: strings.TrimSpace(content.String()),
		SourceLabel: TutoringTipsSourceAI,
	}
}

// mistakesFor aggregates only durable records matching the current exact-set's
// concepts. A missing history is a valid, explicitly rendered fact.
func (d Deps) mistakesFor(ctx context.Context, agentName string, concepts []string) ([]ReviewItem, error) {
	all, err := d.Records.ListByScope(ctx, agentName, k12.CollectionMistakes, "")
	if err != nil {
		if errors.Is(err, records.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("usecase: aggregate learning evidence: %w", err)
	}
	wanted := make(map[string]struct{}, len(concepts))
	for _, concept := range concepts {
		wanted[concept] = struct{}{}
	}
	out := make([]ReviewItem, 0)
	for _, record := range all {
		fields, parseErr := k12.ParseMistakeFields(record.Fields)
		if parseErr != nil {
			return nil, fmt.Errorf("usecase: parse learning evidence: %w", parseErr)
		}
		if _, matched := wanted[fields.KnowledgePoint]; matched {
			out = append(out, ReviewItem{Record: record, Fields: fields})
		}
	}
	return out, nil
}
