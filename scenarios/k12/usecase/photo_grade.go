package usecase

import (
	"context"
	"fmt"
	"math"
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
	PhotoCorrect     PhotoItemStatus = "correct"
	PhotoWrong       PhotoItemStatus = "wrong"
	PhotoUnanswered  PhotoItemStatus = "unanswered"
	PhotoBlankSolved PhotoItemStatus = "blank_solved"
	PhotoOutOfScope  PhotoItemStatus = "out_of_scope"
	PhotoUntrusted   PhotoItemStatus = "untrusted"
	PhotoFailed      PhotoItemStatus = "failed"
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
	if len(questions) == 0 {
		return PhotoGradeResult{}, fmt.Errorf("%w: 未识别到可处理的题目", ErrInvalidInput)
	}
	if req.Grade == "" && d.Profiles != nil {
		if p, profileErr := d.GetProfile(ctx, req.AgentName); profileErr == nil {
			req.Grade = p.GradeTerm
		}
	}

	mode := PhotoModeSolve
	for _, q := range questions {
		if strings.TrimSpace(q.StudentAnswer) != "" {
			mode = PhotoModeGrade
			break
		}
	}

	result := PhotoGradeResult{Mode: mode, Items: make([]PhotoGradeItem, len(questions))}
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
					if strings.TrimSpace(q.StudentAnswer) == "" {
						item.Status = PhotoUnanswered
						result.Items[i] = item
						continue
					}
					graded, gradeErr := d.GradeHomeworkProblem(ctx, GradeRequest{
						AgentName: req.AgentName, Subject: firstNonEmpty(q.Subject, req.Subject), Grade: req.Grade,
						SourceSession: req.SourceSession, Problem: q.Question, StudentAnswer: q.StudentAnswer,
						KnowledgePoints: q.KnowledgePoints,
					})
					item.Grade = graded
					switch {
					case gradeErr != nil:
						item.Status, item.Warning = PhotoFailed, gradeErr.Error()
					case graded.OutOfScope:
						item.Status = PhotoOutOfScope
					case !photoEvidenceTrusted(graded.Evidence):
						item.Status, item.Warning = PhotoUntrusted, "验算证据不足，暂不在图片上判对错"
					case graded.Outcome.Correct:
						item.Status = PhotoCorrect
					default:
						item.Status = PhotoWrong
					}
					result.Items[i] = item
					continue
				}

				solved, solveErr := d.SolveHomeworkProblem(ctx, GradeRequest{
					AgentName: req.AgentName, Subject: firstNonEmpty(q.Subject, req.Subject), Grade: req.Grade,
					SourceSession: req.SourceSession, Problem: q.Question, KnowledgePoints: q.KnowledgePoints,
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
		marks := photoAnnotations(result.Items)
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
	conflicted := make([]bool, len(marks))
	for i := 0; i < len(marks); i++ {
		if !photoAnnotationHasTrustedBBox(marks[i]) {
			continue
		}
		for j := i + 1; j < len(marks); j++ {
			if !photoAnnotationHasTrustedBBox(marks[j]) {
				continue
			}
			a, b := marks[i].BBox, marks[j].BBox
			aY, bY := a.Y+a.H*0.75, b.Y+b.H*0.75
			if math.Abs(a.X-b.X) < 0.06 && math.Abs(aY-bY) < 0.04 {
				conflicted[i], conflicted[j] = true, true
			}
		}
	}
	for i := range marks {
		if conflicted[i] {
			marks[i].BBox = BBox{}
		}
	}
	return marks
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

	correct, wrong, unanswered, pending := 0, 0, 0, 0
	for _, item := range result.Items {
		switch item.Status {
		case PhotoCorrect:
			correct++
		case PhotoWrong:
			wrong++
		case PhotoUnanswered:
			unanswered++
		default:
			pending++
		}
	}
	b.WriteString("## 📊 作业批改完成\n\n")
	fmt.Fprintf(&b, "- 共识别 **%d** 题\n- 正确 **%d** 题，需订正 **%d** 题", len(result.Items), correct, wrong)
	if unanswered > 0 {
		fmt.Fprintf(&b, "，未作答 **%d** 题", unanswered)
	}
	if pending > 0 {
		fmt.Fprintf(&b, "，待核对 **%d** 题", pending)
	}
	b.WriteString("\n\n")
	determined := correct + wrong
	annotated := 0
	if result.AnnotatedImage != nil && len(result.AnnotatedImage.Data) > 0 {
		annotated = len(trustedPhotoMarks(result.Items))
	}
	if result.AnnotatedImage != nil && len(result.AnnotatedImage.Data) > 0 && annotated < determined {
		fmt.Fprintf(&b, "> ℹ️ 本次 %d 题已判定，其中 %d 题在原作答位置标注，其余 %d 题已在批改图右侧按题号标注，避免猜测坐标造成错位。\n\n",
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
