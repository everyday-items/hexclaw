package usecase

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// PhotoMode 是整页图片的单一分流结果：有任一真实作答就是批改卷；整页无作答才是空白解题卷。
type PhotoMode string

const (
	PhotoModeGrade PhotoMode = "grade"
	PhotoModeSolve PhotoMode = "solve"
)

// PhotoTaskIntent is the domain meaning of a page-level photo task. PhotoMode
// remains the legacy processing switch; this additive field gives result
// consumers stable product semantics without changing existing grade/solve
// clients.
type PhotoTaskIntent string

const (
	PhotoTaskCompletedHomework PhotoTaskIntent = "completed_homework"
	PhotoTaskBlankWorksheet    PhotoTaskIntent = "blank_worksheet"
)

// PhotoResultSurface identifies the approved result presentation selected by a
// photo task. It is data semantics only; clients retain control of rendering.
type PhotoResultSurface string

const (
	PhotoSurfaceAnnotatedHomework   PhotoResultSurface = "annotated_homework"
	PhotoSurfaceParentTeachingGuide PhotoResultSurface = "parent_teaching_guide"
)

// PhotoItemResultKind makes item-level routing explicit, including the
// unanswered items preserved on a completed-homework page.
type PhotoItemResultKind string

const (
	PhotoItemAssessment          PhotoItemResultKind = "assessment"
	PhotoItemParentTeachingGuide PhotoItemResultKind = "parent_teaching_guide"
	PhotoItemUnanswered          PhotoItemResultKind = "unanswered"
	PhotoItemNeedsReview         PhotoItemResultKind = "needs_review"
	PhotoItemOutOfScope          PhotoItemResultKind = "out_of_scope"
	PhotoItemFailed              PhotoItemResultKind = "failed"
)

// PhotoItemStatus 避免用一个 correct=false 混淆“答错、超纲、待核对、处理失败”。
type PhotoItemStatus string

const (
	PhotoCorrect                 PhotoItemStatus = "correct"
	PhotoCorrectWithProcessIssue PhotoItemStatus = "correct_with_process_issue"
	PhotoWrong                   PhotoItemStatus = "wrong"
	PhotoUnanswered              PhotoItemStatus = "unanswered"
	PhotoAnswerUnclear           PhotoItemStatus = "answer_unclear"
	PhotoBlankSolved             PhotoItemStatus = "blank_solved"
	PhotoOutOfScope              PhotoItemStatus = "out_of_scope"
	PhotoUntrusted               PhotoItemStatus = "untrusted"
	PhotoFailed                  PhotoItemStatus = "failed"
)

type PhotoGradeRequest struct {
	AgentName     string
	Subject       string
	Grade         string
	SourceSession string
	Image         []byte
	// TaskIntent is frozen by ImageTaskDispatch. Empty preserves the legacy
	// direct-photo path which infers intent from recognition evidence.
	TaskIntent PhotoTaskIntent
}

type PhotoGradeItem struct {
	Recognized  RecognizedQuestion
	Status      PhotoItemStatus
	ResultKind  PhotoItemResultKind
	Grade       GradeResult
	Solve       SolveHomeworkResult
	ParentGuide *ParentTeachingGuide
	Warning     string
}

type PhotoGradeResult struct {
	Mode           PhotoMode
	TaskIntent     PhotoTaskIntent
	ResultSurface  PhotoResultSurface
	Items          []PhotoGradeItem
	AnnotatedImage *RenderedPhoto
	ImageWarning   string
	Markdown       string
}

// GradeHomeworkPhoto 编排一整张作业图：识题 → 页级分流 → 并发上限 2 的逐题批改/解题 →
// 强证据 bbox 标记 → Markdown 摘要。普通单题失败只降级该题，其他题仍须完成；
// 结果未知类错误必须向聚合层传播，避免 GradingJob 把未收敛的部分结果标成 completed。
func (d Deps) GradeHomeworkPhoto(ctx context.Context, req PhotoGradeRequest) (PhotoGradeResult, error) {
	return d.gradeHomeworkPhotoWithAssessor(ctx, req, 2, d.assessPhotoItem)
}

type photoItemAssessor func(context.Context, PhotoGradeRequest, PhotoMode, RecognizedQuestion) (PhotoGradeItem, error)

// gradeHomeworkPhotoWithAssessor keeps page normalization, answer-mode routing,
// bounded fan-out, annotation and rendering shared between the historical
// direct path and the durable GradingJob item executor.
func (d Deps) gradeHomeworkPhotoWithAssessor(
	ctx context.Context,
	req PhotoGradeRequest,
	itemConcurrency int,
	assess photoItemAssessor,
) (PhotoGradeResult, error) {
	return d.gradeHomeworkPhotoWithAssessorInput(ctx, req, itemConcurrency, assess, false)
}

// gradeFrozenHomeworkPhotoWithAssessor consumes the already-normalized,
// parent-confirmed Problem/Attempt identity restored from a GradingJob
// checkpoint. Re-normalizing that value would mint/reset identity at the
// assessment boundary and invalidate the durable per-item authorization.
func (d Deps) gradeFrozenHomeworkPhotoWithAssessor(
	ctx context.Context,
	req PhotoGradeRequest,
	itemConcurrency int,
	assess photoItemAssessor,
) (PhotoGradeResult, error) {
	return d.gradeHomeworkPhotoWithAssessorInput(ctx, req, itemConcurrency, assess, true)
}

func (d Deps) gradeHomeworkPhotoWithAssessorInput(
	ctx context.Context,
	req PhotoGradeRequest,
	itemConcurrency int,
	assess photoItemAssessor,
	frozenIdentity bool,
) (PhotoGradeResult, error) {
	if strings.TrimSpace(req.AgentName) == "" || len(req.Image) == 0 {
		return PhotoGradeResult{}, fmt.Errorf("%w: AgentName / Image 不可空", ErrInvalidInput)
	}
	if itemConcurrency <= 0 || assess == nil {
		return PhotoGradeResult{}, fmt.Errorf("%w: item concurrency / assessor 非法", ErrInvalidInput)
	}
	if err := validateGradeInput(req.Grade); err != nil {
		return PhotoGradeResult{}, err
	}
	questions, err := d.RecognizeHomework(ctx, req.Image)
	if err != nil {
		return PhotoGradeResult{}, err
	}
	if frozenIdentity {
		questions = cloneRecognizedQuestions(questions)
		if err := validateNormalizedRecognizedProblems(questions); err != nil {
			return PhotoGradeResult{}, err
		}
	} else {
		questions, err = NormalizeRecognizedProblems("photo-"+shortSHA1(req.Image), questions)
		if err != nil {
			return PhotoGradeResult{}, err
		}
	}
	if len(questions) == 0 {
		return PhotoGradeResult{}, fmt.Errorf("%w: 未识别到可处理的题目", ErrInvalidInput)
	}
	if req.Grade == "" && d.Profiles != nil {
		if p, profileErr := d.GetProfile(ctx, req.AgentName); profileErr == nil {
			req.Grade = p.GradeTerm
		}
	}

	imageWarning := ""
	hasPresent, hasUnclear := photoAnswerCandidates(questions)
	anchorVerified := false
	if (hasPresent || hasUnclear) && d.AnswerAnchorer != nil {
		anchored, anchorErr := d.AnchorHomeworkAnswers(ctx, req.Image, questions)
		if anchorErr == nil {
			questions = anchored
			anchorVerified = true
		} else {
			if hasPresent {
				imageWarning = "未能可靠定位作答位置，本次仅提供文字批改"
			} else {
				imageWarning = "未能独立核验疑似学生笔迹，为避免泄露答案，本次按批改卷处理"
			}
		}
	}
	// 锚点阶段保留父题与所有子题的同序结构；只有在几何回位后，才丢弃不产生
	// Attempt/Assessment 的公共父题并把公共题干组合到各子题评估副本。
	questions = RecognizedQuestionsForAssessment(questions)
	if len(questions) == 0 {
		return PhotoGradeResult{}, fmt.Errorf("%w: 未识别到可作答的独立题目", ErrInvalidInput)
	}
	mode := classifyPhotoMode(questions)
	if mode == PhotoModeSolve && hasUnclear && !anchorVerified {
		// An unavailable evidence adapter must fail closed: a genuinely answered but unreadable page
		// must never receive generated answers merely because the independent verifier did not run.
		mode = PhotoModeGrade
		if imageWarning == "" {
			imageWarning = "未配置疑似笔迹核验，为避免泄露答案，本次按批改卷处理"
		}
	}
	if req.TaskIntent != "" {
		expectedMode := PhotoModeGrade
		switch req.TaskIntent {
		case PhotoTaskCompletedHomework:
			expectedMode = PhotoModeGrade
		case PhotoTaskBlankWorksheet:
			expectedMode = PhotoModeSolve
		default:
			return PhotoGradeResult{}, fmt.Errorf("%w: frozen photo task intent 非法: %q", ErrInvalidInput, req.TaskIntent)
		}
		if mode != expectedMode {
			return PhotoGradeResult{}, fmt.Errorf(
				"%w: frozen photo task intent %q 与识别到的作答证据冲突",
				ErrInvalidInput, req.TaskIntent,
			)
		}
		mode = expectedMode
	}

	taskIntent, resultSurface := photoTaskSemantics(mode)
	result := PhotoGradeResult{
		Mode: mode, TaskIntent: taskIntent, ResultSurface: resultSurface,
		Items: make([]PhotoGradeItem, len(questions)), ImageWarning: imageWarning,
	}
	var wg sync.WaitGroup
	var dispatchMu sync.Mutex
	nextQuestion := 0
	dispatchStopped := false
	var unknownErr error
	var unknownErrOnce sync.Once
	var firstItemErr error
	var firstItemErrOnce sync.Once
	claimQuestion := func() (int, bool) {
		dispatchMu.Lock()
		defer dispatchMu.Unlock()
		if dispatchStopped || nextQuestion >= len(questions) {
			return 0, false
		}
		i := nextQuestion
		nextQuestion++
		return i, true
	}
	stopDispatch := func() {
		dispatchMu.Lock()
		dispatchStopped = true
		dispatchMu.Unlock()
	}
	workerCount := itemConcurrency
	if len(questions) < workerCount {
		workerCount = len(questions)
	}
	for worker := 0; worker < workerCount; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i, ok := claimQuestion()
				if !ok {
					return
				}
				item, itemErr := assess(ctx, req, mode, questions[i])
				if itemErr != nil {
					item.Status, item.Warning = PhotoFailed, itemErr.Error()
					firstItemErrOnce.Do(func() { firstItemErr = itemErr })
					if invocationOutcomeUnknown(itemErr) {
						unknownErrOnce.Do(func() {
							unknownErr = itemErr
							stopDispatch()
						})
					}
				}
				result.Items[i] = item
			}
		}()
	}
	wg.Wait()
	for i := range result.Items {
		if result.Items[i].ResultKind == "" {
			result.Items[i].ResultKind = photoItemResultKind(result.Items[i].Status)
		}
	}
	if unknownErr != nil {
		result.Markdown = photoGradeMarkdown(result)
		return result, unknownErr
	}
	if firstItemErr != nil {
		result.Markdown = photoGradeMarkdown(result)
		return result, firstItemErr
	}

	if mode == PhotoModeGrade {
		// The renderer must never receive zero/invalid coordinates. Otherwise a verified verdict with
		// no safely located answer can be encoded as an unchanged copy of the worksheet and falsely
		// presented to the IM channel as a correction image.
		marks := trustedPhotoMarks(result.Items)
		if len(marks) > 0 && d.PhotoAnnotator != nil {
			rendered, renderErr := d.PhotoAnnotator.Annotate(ctx, req.Image, marks)
			if renderErr == nil && len(rendered.Data) > 0 {
				result.AnnotatedImage = &rendered
			} else if renderErr != nil {
				// Rendering is a page-level projection. Per-item results may
				// already be committed as immutable assessment receipts, so a
				// compositor failure must not rewrite their canonical facts.
				result.ImageWarning = "批改结论已完成，但批改图生成失败"
			}
		}
	}
	result.Markdown = photoGradeMarkdown(result)
	return result, nil
}

func (d Deps) assessPhotoItem(
	ctx context.Context,
	req PhotoGradeRequest,
	mode PhotoMode,
	q RecognizedQuestion,
) (PhotoGradeItem, error) {
	item := PhotoGradeItem{Recognized: q}
	gradeReq := GradeRequest{
		AgentName: req.AgentName, Subject: firstNonEmpty(q.Subject, req.Subject), Grade: req.Grade,
		SourceSession: req.SourceSession, Problem: q.Question, StudentAnswer: q.StudentAnswer,
		KnowledgePoints: photoGradeKnowledgePoints(q),
	}
	if mode == PhotoModeGrade {
		switch q.AnswerState {
		case AnswerStateBlank:
			item.Status = PhotoUnanswered
			return item, nil
		case AnswerStateUnclear:
			item.Status = PhotoAnswerUnclear
			item.Warning = "检测到学生笔迹，但未能可靠读出；请家长补录后再批改"
			return item, nil
		}
		graded, err := d.GradeHomeworkProblem(ctx, gradeReq)
		item.Grade = graded
		if err != nil {
			return item, err
		}
		item.Status, item.Warning = photoAssessmentStatus(graded)
		return item, nil
	}

	gradeReq.StudentAnswer = ""
	blankResult, err := d.SolveBlankWorksheetProblem(ctx, gradeReq)
	item.Solve = blankResult.Solved
	if err != nil {
		return item, err
	}
	if blankResult.Solved.OutOfScope {
		item.Status = PhotoOutOfScope
		return item, nil
	}
	item.ParentGuide = &blankResult.Guide
	item.Status = PhotoBlankSolved
	if !photoEvidenceTrusted(blankResult.Solved.Evidence) {
		item.Warning = "答案未通过程序级验算，请家长核对"
	}
	return item, nil
}

func classifyPhotoMode(questions []RecognizedQuestion) PhotoMode {
	for _, question := range questions {
		normalized := NormalizeRecognizedQuestion(question)
		if normalized.AnswerState == AnswerStatePresent {
			return PhotoModeGrade
		}
		if normalized.AnswerState == AnswerStateUnclear && normalized.BBox != nil &&
			photoAnnotationHasTrustedBBox(PhotoAnnotation{BBox: *normalized.BBox}) {
			return PhotoModeGrade
		}
	}
	return PhotoModeSolve
}

func photoTaskSemantics(mode PhotoMode) (PhotoTaskIntent, PhotoResultSurface) {
	if mode == PhotoModeSolve {
		return PhotoTaskBlankWorksheet, PhotoSurfaceParentTeachingGuide
	}
	return PhotoTaskCompletedHomework, PhotoSurfaceAnnotatedHomework
}

func photoItemResultKind(status PhotoItemStatus) PhotoItemResultKind {
	switch status {
	case PhotoCorrect, PhotoCorrectWithProcessIssue, PhotoWrong:
		return PhotoItemAssessment
	case PhotoBlankSolved:
		return PhotoItemParentTeachingGuide
	case PhotoUnanswered:
		return PhotoItemUnanswered
	case PhotoAnswerUnclear, PhotoUntrusted:
		return PhotoItemNeedsReview
	case PhotoOutOfScope:
		return PhotoItemOutOfScope
	default:
		return PhotoItemFailed
	}
}

func photoAssessmentStatus(graded GradeResult) (PhotoItemStatus, string) {
	switch {
	case graded.OutOfScope:
		return PhotoOutOfScope, ""
	case !photoEvidenceTrusted(graded.Evidence):
		return PhotoUntrusted, "Verification evidence is insufficient; no correct/incorrect mark is shown on the image"
	}
	switch graded.Outcome.AssessmentStatus() {
	case k12.GradingAssessmentCorrect:
		return PhotoCorrect, ""
	case k12.GradingAssessmentProcessIssue:
		return PhotoCorrectWithProcessIssue, ""
	case k12.GradingAssessmentWrong:
		return PhotoWrong, ""
	case k12.GradingAssessmentOutOfScope:
		return PhotoOutOfScope, ""
	default:
		return PhotoUntrusted, "Grading evidence is insufficient or conflicting, so no correct/incorrect mark is shown on the image"
	}
}

// EffectiveTaskIntent preserves compatibility with historical/restored values
// which predate the explicit domain fields.
func (r PhotoGradeResult) EffectiveTaskIntent() PhotoTaskIntent {
	if r.TaskIntent != "" {
		return r.TaskIntent
	}
	intent, _ := photoTaskSemantics(r.Mode)
	return intent
}

// EffectiveResultSurface preserves compatibility with historical/restored
// values which predate the explicit domain fields.
func (r PhotoGradeResult) EffectiveResultSurface() PhotoResultSurface {
	if r.ResultSurface != "" {
		return r.ResultSurface
	}
	_, surface := photoTaskSemantics(r.Mode)
	return surface
}

// EffectiveResultKind preserves compatibility with historical item receipts.
func (i PhotoGradeItem) EffectiveResultKind() PhotoItemResultKind {
	if i.ResultKind != "" {
		return i.ResultKind
	}
	return photoItemResultKind(i.Status)
}

func photoAnswerCandidates(questions []RecognizedQuestion) (present, unclear bool) {
	for _, question := range questions {
		switch NormalizeRecognizedQuestion(question).AnswerState {
		case AnswerStatePresent:
			present = true
		case AnswerStateUnclear:
			unclear = true
		}
	}
	return present, unclear
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func photoEvidenceTrusted(e SolveEvidence) bool {
	return (e.Verdict == VerdictAgree || e.Verdict == VerdictVerbatim) && e.StrongTrust()
}

func trustedPhotoMarks(items []PhotoGradeItem) []PhotoAnnotation {
	annotations := photoAnnotations(items)
	marks := make([]PhotoAnnotation, 0, len(annotations))
	for _, annotation := range annotations {
		if photoAnnotationHasTrustedBBox(annotation) {
			marks = append(marks, annotation)
		}
	}
	return marks
}

func photoAnnotations(items []PhotoGradeItem) []PhotoAnnotation {
	marks := make([]PhotoAnnotation, 0, len(items))
	for i, item := range items {
		if item.Status != PhotoCorrect && item.Status != PhotoCorrectWithProcessIssue && item.Status != PhotoWrong {
			continue
		}
		mark := PhotoAnnotation{
			QuestionNumber: i + 1,
			Status:         item.Status,
			Correct:        item.Status == PhotoCorrect,
		}
		if anchor := item.Recognized.BBox; anchor != nil {
			mark.BBox = *anchor
		}
		marks = append(marks, mark)
	}
	return marks
}

var photoFractionToken = regexp.MustCompile(`\d+\s*/\s*\d+`)
var photoChineseFractionToken = regexp.MustCompile(`[零〇一二两三四五六七八九十百]+\s*分之\s*[零〇一二两三四五六七八九十百]+`)

// photoGradeKnowledgePoints corrects a coarse but common photo-recognition label without changing
// the canonical curriculum. Problems such as “8 的 1/4 的 4/5” are an application of the meaning
// of fractions in fifth grade; treating every such expression as the formal sixth-grade
// “分数乘法” unit creates a false out-of-scope verdict.
func photoGradeKnowledgePoints(question RecognizedQuestion) []string {
	points := append([]string(nil), question.KnowledgePoints...)
	problem := strings.TrimSpace(question.Question)
	// Natural-language “a number's m/n” applications belong to the fifth-grade meaning of
	// fractions even though their solution can be rewritten with multiplication/division. Only
	// normalize that wording; explicit fraction ×/÷ expressions remain the formal sixth-grade unit.
	if !strings.Contains(problem, "的") ||
		(len(photoFractionToken.FindAllString(problem, -1)) == 0 && !photoChineseFractionToken.MatchString(problem)) ||
		strings.ContainsAny(problem, "×÷") || strings.Contains(problem, "乘以") || strings.Contains(problem, "除以") {
		return points
	}
	for i, point := range points {
		if normalized := strings.TrimSpace(point); normalized == "分数乘法" || normalized == "分数除法" {
			points[i] = "分数的意义和性质"
		}
	}
	return points
}

func photoAnnotationHasTrustedBBox(mark PhotoAnnotation) bool {
	b := mark.BBox
	values := []float64{b.X, b.Y, b.W, b.H}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return b.X >= 0 && b.Y >= 0 && b.W > 0 && b.H > 0 && b.X+b.W <= 1.005 && b.Y+b.H <= 1.005
}

func photoQuestionSourceLabel(question RecognizedQuestion) string {
	return RecognizedQuestionSourceDisplayLabel(question)
}

func photoQuestionStem(question RecognizedQuestion) string {
	stem := strings.TrimSpace(question.Question)
	printedLabel := strings.TrimSpace(question.DisplayLabel)
	if printedLabel == "" || stem == "" {
		return stem
	}
	candidates := []string{printedLabel}
	if count := len(question.SourceNumberPath); count > 0 {
		candidates = append(candidates, strings.TrimSpace(question.SourceNumberPath[count-1]))
	}
	for _, candidate := range candidates {
		if candidate == "" || !strings.HasPrefix(stem, candidate) {
			continue
		}
		rest := strings.TrimPrefix(stem, candidate)
		if rest == "" {
			return ""
		}
		if !strings.ContainsRune(" .．、)）:：\t", []rune(rest)[0]) {
			continue
		}
		return strings.TrimSpace(strings.TrimLeft(rest, " .．、)）:：\t"))
	}
	return stem
}

func photoQuestionHeading(question RecognizedQuestion, max int) string {
	label := photoQuestionSourceLabel(question)
	stem := photoClip(photoQuestionStem(question), max)
	switch {
	case label == "":
		return stem
	case stem == "":
		return label
	default:
		return label + " " + stem
	}
}

func photoQuestionReference(question RecognizedQuestion) string {
	if label := photoQuestionSourceLabel(question); label != "" {
		return label
	}
	return photoClip(photoQuestionStem(question), 120)
}

func photoGradeMarkdown(result PhotoGradeResult) string {
	var b strings.Builder
	if result.Mode == PhotoModeSolve {
		b.WriteString("## 家长辅导指南\n\n")
		b.WriteString(fmt.Sprintf("共识别 **%d** 道空白题，下面按题号给出家长辅导步骤。\n\n", len(result.Items)))
		for _, item := range result.Items {
			fmt.Fprintf(&b, "### %s\n\n", photoQuestionHeading(item.Recognized, 240))
			switch item.Status {
			case PhotoBlankSolved:
				if item.ParentGuide != nil {
					writeParentTeachingGuideMarkdown(&b, *item.ParentGuide)
				} else {
					b.WriteString(photoClip(item.Solve.Solution, 1200))
				}
				if item.Warning != "" {
					fmt.Fprintf(&b, "\n\n> ⚠️ %s", item.Warning)
				}
			case PhotoOutOfScope:
				fmt.Fprintf(&b, "> ⛔ 超出当前年级范围：%s", item.Solve.OutOfScopeKP)
			default:
				fmt.Fprintf(&b, "> ⚠️ 本题暂未完成：%s", photoClip(item.Warning, 240))
			}
			b.WriteString("\n\n")
		}
		return strings.TrimSpace(b.String())
	}

	correct, processIssue, wrong, unanswered, unclear, pending := 0, 0, 0, 0, 0, 0
	for _, item := range result.Items {
		switch item.Status {
		case PhotoCorrect:
			correct++
		case PhotoCorrectWithProcessIssue:
			processIssue++
		case PhotoWrong:
			wrong++
		case PhotoUnanswered:
			unanswered++
		case PhotoAnswerUnclear:
			unclear++
		default:
			pending++
		}
	}
	b.WriteString("## 📊 作业批改完成\n\n")
	fmt.Fprintf(&b, "- **%d** questions recognized\n- **%d** correct", len(result.Items), correct)
	if processIssue > 0 {
		fmt.Fprintf(&b, ", **%d** with process issues", processIssue)
	}
	fmt.Fprintf(&b, ", **%d** requiring correction", wrong)
	if unanswered > 0 {
		fmt.Fprintf(&b, "，未作答 **%d** 题", unanswered)
	}
	if unclear > 0 {
		fmt.Fprintf(&b, "，作答待补录 **%d** 题", unclear)
	}
	if pending > 0 {
		fmt.Fprintf(&b, "，待核对 **%d** 题", pending)
	}
	b.WriteString("\n\n")
	if result.ImageWarning != "" {
		fmt.Fprintf(&b, "> ℹ️ %s。\n\n", result.ImageWarning)
	}
	determined := correct + processIssue + wrong
	annotated := 0
	if result.AnnotatedImage != nil && len(result.AnnotatedImage.Data) > 0 {
		annotated = len(trustedPhotoMarks(result.Items))
	}
	if result.AnnotatedImage != nil && len(result.AnnotatedImage.Data) > 0 && annotated < determined {
		fmt.Fprintf(&b, "> ℹ️ 本次 %d 题已判定，其中 %d 题在原作答位置标注；其余 %d 题仅作文字汇总，未在图上猜测位置。\n\n",
			determined, annotated, determined-annotated)
	} else if annotated < determined {
		if annotated == 0 {
			fmt.Fprintf(&b, "> ℹ️ 本次 %d 题已判定，0 题已在图上标注；本次未生成批改图，判定结果仅作文字汇总，以避免标记错位。\n\n", determined)
		} else {
			fmt.Fprintf(&b, "> ℹ️ 本次 %d 题已判定，其中 %d 题找到作答位置并已标注；其余仅作文字汇总，以避免标记错位。\n\n", determined, annotated)
		}
	}
	if correct > 0 {
		fmt.Fprintf(&b, "### ✅ 答对的题（%d）\n\n", correct)
		for _, item := range result.Items {
			if item.Status != PhotoCorrect {
				continue
			}
			fmt.Fprintf(&b, "- **%s** → **%s**\n",
				photoQuestionHeading(item.Recognized, 180), photoInline(item.Recognized.StudentAnswer, 180))
		}
		b.WriteString("\n")
	}
	if processIssue > 0 {
		fmt.Fprintf(&b, "### ⚠️ Process issues (%d)\n\n", processIssue)
		b.WriteString("> The final answer is correct. Only process issues supported by clear evidence are shown below, and they are not recorded as wrong.\n\n")
		for _, item := range result.Items {
			if item.Status != PhotoCorrectWithProcessIssue {
				continue
			}
			fmt.Fprintf(&b, "#### %s\n\n", photoQuestionReference(item.Recognized))
			fmt.Fprintf(&b, "- **Question:** %s\n- **Your answer:** %s",
				photoInline(photoQuestionStem(item.Recognized), 240),
				photoInline(item.Recognized.StudentAnswer, 300))
			if item.Grade.Outcome.WrongStep != "" {
				fmt.Fprintf(&b, "\n- **Process note:** %s", photoInline(item.Grade.Outcome.WrongStep, 300))
			}
			if item.Grade.Outcome.ErrorCause != "" {
				fmt.Fprintf(&b, "\n- **Cause:** %s", photoInline(item.Grade.Outcome.ErrorCause, 300))
			}
			if item.ParentGuide != nil {
				b.WriteString("\n\n##### How the parent can explain it\n\n")
				writeParentTeachingGuideMarkdown(&b, *item.ParentGuide)
			}
			b.WriteString("\n\n")
		}
	}
	if wrong > 0 {
		fmt.Fprintf(&b, "### ❌ 需要订正（%d）\n\n", wrong)
	}
	for _, item := range result.Items {
		if item.Status != PhotoWrong {
			continue
		}
		fmt.Fprintf(&b, "#### %s\n\n", photoQuestionReference(item.Recognized))
		fmt.Fprintf(&b, "- **题目：** %s\n- **你的作答：** %s\n- **订正参考：**\n\n%s",
			photoInline(photoQuestionStem(item.Recognized), 240), photoInline(item.Recognized.StudentAnswer, 300), photoMarkdownQuote(item.Grade.Solution, 1000))
		if item.Grade.Outcome.WrongStep != "" {
			fmt.Fprintf(&b, "\n- **第一个错步：** %s", photoInline(item.Grade.Outcome.WrongStep, 300))
		}
		if item.Grade.Outcome.ErrorCause != "" {
			fmt.Fprintf(&b, "\n- **错因：** %s", photoInline(item.Grade.Outcome.ErrorCause, 300))
		}
		if item.ParentGuide != nil {
			b.WriteString("\n\n##### 家长讲法\n\n")
			writeParentTeachingGuideMarkdown(&b, *item.ParentGuide)
		}
		b.WriteString("\n\n")
	}
	if unanswered > 0 {
		fmt.Fprintf(&b, "### ⏸ 未作答（%d）\n\n", unanswered)
		for _, item := range result.Items {
			if item.Status == PhotoUnanswered {
				fmt.Fprintf(&b, "- %s\n", photoQuestionHeading(item.Recognized, 240))
			}
		}
		b.WriteString("\n> 本次已答卷批改不会直接泄露未作答题的答案。\n\n")
	}
	if pending > 0 {
		fmt.Fprintf(&b, "### ⚠️ 待核对（%d）\n\n", pending)
		for _, item := range result.Items {
			switch item.Status {
			case PhotoOutOfScope:
				fmt.Fprintf(&b, "- %s 超出当前年级范围：%s\n",
					photoQuestionReference(item.Recognized), photoInline(item.Grade.OutOfScopeKP, 120))
			case PhotoUntrusted:
				fmt.Fprintf(&b, "- %s 证据不足：%s\n",
					photoQuestionReference(item.Recognized), photoInline(item.Warning, 240))
			case PhotoFailed:
				fmt.Fprintf(&b, "- %s 处理失败：%s\n",
					photoQuestionReference(item.Recognized), photoInline(item.Warning, 240))
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func writeParentTeachingGuideMarkdown(b *strings.Builder, guide ParentTeachingGuide) {
	fmt.Fprintf(b, "**答案：** %s\n\n", photoInline(guide.Answer, 600))
	writeParentGuideListMarkdown(b, "**必要步骤：**", guide.FullSolutionSteps)
	fmt.Fprintf(b, "**本年级方法：** %s\n\n", photoInline(guide.GradeLevelMethod, 1200))
	writeParentGuideListMarkdown(b, "**易错点：**", guide.LikelyMistakes)
	writeParentGuideListMarkdown(b, "**家长怎么讲：**", guide.ParentTeachingSequence)
	writeParentGuideListMarkdown(b, "**可以追问：**", guide.FollowUpQuestions)
	fmt.Fprintf(b, "**怎么检查：** %s", photoInline(guide.CheckingMethod, 1200))
}

func writeParentGuideListMarkdown(b *strings.Builder, label string, values []string) {
	b.WriteString(label)
	b.WriteString("\n\n")
	for i, value := range values {
		fmt.Fprintf(b, "%d. %s\n", i+1, photoInline(value, 600))
	}
	b.WriteString("\n")
}

func photoInline(s string, max int) string {
	return strings.Join(strings.Fields(photoClip(s, max)), " ")
}

func photoMarkdownQuote(s string, max int) string {
	lines := strings.Split(photoClip(s, max), "\n")
	quoted := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			line = strings.TrimSpace(strings.TrimLeft(line, "#"))
		}
		if line != "" {
			quoted = append(quoted, "> "+line)
		}
	}
	if len(quoted) == 0 {
		return "> 暂无可靠订正参考"
	}
	return strings.Join(quoted, "  \n")
}

func photoClip(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}
