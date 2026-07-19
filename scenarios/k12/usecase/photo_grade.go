package usecase

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// PhotoMode 是整页图片的单一分流结果：有任一真实作答就是批改卷；整页无作答才是空白解题卷。
type PhotoMode string

const (
	PhotoModeGrade PhotoMode = "grade"
	PhotoModeSolve PhotoMode = "solve"
)

// PhotoItemStatus 避免用一个 correct=false 混淆“答错、超纲、待核对、处理失败”。
type PhotoItemStatus string

const (
	PhotoCorrect       PhotoItemStatus = "correct"
	PhotoWrong         PhotoItemStatus = "wrong"
	PhotoUnanswered    PhotoItemStatus = "unanswered"
	PhotoAnswerUnclear PhotoItemStatus = "answer_unclear"
	PhotoBlankSolved   PhotoItemStatus = "blank_solved"
	PhotoOutOfScope    PhotoItemStatus = "out_of_scope"
	PhotoUntrusted     PhotoItemStatus = "untrusted"
	PhotoFailed        PhotoItemStatus = "failed"
)

type PhotoGradeRequest struct {
	AgentName     string
	Subject       string
	Grade         string
	SourceSession string
	Image         []byte
}

type PhotoGradeItem struct {
	Recognized RecognizedQuestion
	Status     PhotoItemStatus
	Grade      GradeResult
	Solve      SolveHomeworkResult
	Warning    string
}

type PhotoGradeResult struct {
	Mode           PhotoMode
	Items          []PhotoGradeItem
	AnnotatedImage *RenderedPhoto
	ImageWarning   string
	Markdown       string
}

// GradeHomeworkPhoto 编排一整张作业图：识题 → 页级分流 → 并发上限 2 的逐题批改/解题 →
// 强证据 bbox 标记 → Markdown 摘要。单题失败只降级该题，其他题仍须完成。
func (d Deps) GradeHomeworkPhoto(ctx context.Context, req PhotoGradeRequest) (PhotoGradeResult, error) {
	if strings.TrimSpace(req.AgentName) == "" || len(req.Image) == 0 {
		return PhotoGradeResult{}, fmt.Errorf("%w: AgentName / Image 不可空", ErrInvalidInput)
	}
	if err := validateGradeInput(req.Grade); err != nil {
		return PhotoGradeResult{}, err
	}
	questions, err := d.RecognizeHomework(ctx, req.Image)
	if err != nil {
		return PhotoGradeResult{}, err
	}
	questions, err = NormalizeRecognizedProblems("photo-"+shortSHA1(req.Image), questions)
	if err != nil {
		return PhotoGradeResult{}, err
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

	result := PhotoGradeResult{Mode: mode, Items: make([]PhotoGradeItem, len(questions)), ImageWarning: imageWarning}
	jobs := make(chan int)
	var wg sync.WaitGroup
	workerCount := 2
	if len(questions) < workerCount {
		workerCount = len(questions)
	}
	for worker := 0; worker < workerCount; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				q := questions[i]
				item := PhotoGradeItem{Recognized: q}
				if mode == PhotoModeGrade {
					switch q.AnswerState {
					case AnswerStateBlank:
						item.Status = PhotoUnanswered
						result.Items[i] = item
						continue
					case AnswerStateUnclear:
						item.Status = PhotoAnswerUnclear
						item.Warning = "检测到学生笔迹，但未能可靠读出；请家长补录后再批改"
						result.Items[i] = item
						continue
					}
					graded, gradeErr := d.GradeHomeworkProblem(ctx, GradeRequest{
						AgentName: req.AgentName, Subject: firstNonEmpty(q.Subject, req.Subject), Grade: req.Grade,
						SourceSession: req.SourceSession, Problem: q.Question, StudentAnswer: q.StudentAnswer,
						KnowledgePoints: photoGradeKnowledgePoints(q),
					})
					item.Grade = graded
					switch {
					case gradeErr != nil:
						item.Status, item.Warning = PhotoFailed, gradeErr.Error()
					case graded.OutOfScope:
						item.Status = PhotoOutOfScope
					case !photoEvidenceTrusted(graded.Evidence):
						item.Status, item.Warning = PhotoUntrusted, "验算证据不足，暂不在图片上判对错"
					case graded.Outcome.Verdict == VerdictAgree:
						item.Status = PhotoCorrect
					case graded.Outcome.Verdict == VerdictDisagree:
						item.Status = PhotoWrong
					default:
						// 判定统一 Verdict 五值（§4.5）：非二元结论不伪装成对/错，按证据不足降级。
						item.Status, item.Warning = PhotoUntrusted, "批改判定无二元结论，暂不在图片上判对错"
					}
					result.Items[i] = item
					continue
				}

				solved, solveErr := d.SolveHomeworkProblem(ctx, GradeRequest{
					AgentName: req.AgentName, Subject: firstNonEmpty(q.Subject, req.Subject), Grade: req.Grade,
					SourceSession: req.SourceSession, Problem: q.Question, KnowledgePoints: photoGradeKnowledgePoints(q),
				})
				item.Solve = solved
				switch {
				case solveErr != nil:
					item.Status, item.Warning = PhotoFailed, solveErr.Error()
				case solved.OutOfScope:
					item.Status = PhotoOutOfScope
				default:
					item.Status = PhotoBlankSolved
					if !photoEvidenceTrusted(solved.Evidence) {
						item.Warning = "答案未通过程序级验算，请家长核对"
					}
				}
				result.Items[i] = item
			}
		}()
	}
	for i := range questions {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

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
				for i := range result.Items {
					if result.Items[i].Warning == "" {
						result.Items[i].Warning = "批改结论已完成，但批改图生成失败"
						break
					}
				}
			}
		}
	}
	result.Markdown = photoGradeMarkdown(result)
	return result, nil
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
		if item.Status != PhotoCorrect && item.Status != PhotoWrong {
			continue
		}
		mark := PhotoAnnotation{QuestionNumber: i + 1, Correct: item.Status == PhotoCorrect}
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

func photoGradeMarkdown(result PhotoGradeResult) string {
	var b strings.Builder
	if result.Mode == PhotoModeSolve {
		b.WriteString("## 作业解题\n\n")
		b.WriteString(fmt.Sprintf("共识别 **%d** 道空白题，下面按题号给出解答。\n\n", len(result.Items)))
		for i, item := range result.Items {
			fmt.Fprintf(&b, "### %d. %s\n\n", i+1, photoClip(item.Recognized.Question, 240))
			switch item.Status {
			case PhotoBlankSolved:
				b.WriteString(photoClip(item.Solve.Solution, 1200))
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

	correct, wrong, unanswered, unclear, pending := 0, 0, 0, 0, 0
	for _, item := range result.Items {
		switch item.Status {
		case PhotoCorrect:
			correct++
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
	fmt.Fprintf(&b, "- 共识别 **%d** 题\n- 正确 **%d** 题，需订正 **%d** 题", len(result.Items), correct, wrong)
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
	determined := correct + wrong
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
		for i, item := range result.Items {
			if item.Status != PhotoCorrect {
				continue
			}
			fmt.Fprintf(&b, "%d. **%s** → **%s**\n", i+1,
				photoInline(item.Recognized.Question, 180), photoInline(item.Recognized.StudentAnswer, 180))
		}
		b.WriteString("\n")
	}
	if wrong > 0 {
		fmt.Fprintf(&b, "### ❌ 需要订正（%d）\n\n", wrong)
	}
	for i, item := range result.Items {
		if item.Status != PhotoWrong {
			continue
		}
		fmt.Fprintf(&b, "#### 第 %d 题\n\n", i+1)
		fmt.Fprintf(&b, "- **题目：** %s\n- **你的作答：** %s\n- **订正参考：**\n\n%s",
			photoInline(item.Recognized.Question, 240), photoInline(item.Recognized.StudentAnswer, 300), photoMarkdownQuote(item.Grade.Solution, 1000))
		if item.Grade.Outcome.ErrorCause != "" {
			fmt.Fprintf(&b, "\n- **错因：** %s", photoInline(item.Grade.Outcome.ErrorCause, 300))
		}
		b.WriteString("\n\n")
	}
	if unanswered > 0 {
		fmt.Fprintf(&b, "### ⏸ 未作答（%d）\n\n", unanswered)
		for i, item := range result.Items {
			if item.Status == PhotoUnanswered {
				fmt.Fprintf(&b, "- 第 %d 题：%s\n", i+1, photoInline(item.Recognized.Question, 240))
			}
		}
		b.WriteString("\n> 本次已答卷批改不会直接泄露未作答题的答案。\n\n")
	}
	if pending > 0 {
		fmt.Fprintf(&b, "### ⚠️ 待核对（%d）\n\n", pending)
		for i, item := range result.Items {
			switch item.Status {
			case PhotoOutOfScope:
				fmt.Fprintf(&b, "- 第 %d 题超出当前年级范围：%s\n", i+1, photoInline(item.Grade.OutOfScopeKP, 120))
			case PhotoUntrusted:
				fmt.Fprintf(&b, "- 第 %d 题证据不足：%s\n", i+1, photoInline(item.Warning, 240))
			case PhotoFailed:
				fmt.Fprintf(&b, "- 第 %d 题处理失败：%s\n", i+1, photoInline(item.Warning, 240))
			}
		}
	}
	return strings.TrimSpace(b.String())
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
