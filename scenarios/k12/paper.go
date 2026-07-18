package k12

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// 试卷种类（§4.13 呈现物）：题目卷（只题面+留白）与答案卷（题面+答案），两卷绝不混排。
const (
	PaperKindQuestion = "question"
	PaperKindAnswer   = "answer"
)

// paperItemsPerPage 单页题量上限（§4.13 卷面版面）：超过即分页，每页页脚都印卷面号。
const paperItemsPerPage = 6

// paperTitleMaxRunes 卷面展示标题上限（§4.13 标题截断规则在呈现物层的应用）。
const paperTitleMaxRunes = 18

// TruncateDisplayTitle §4.13 标题截断：展示标题超过 18 字截断加「…」。
// 同一规则用于本周复习、全部错题、练习集与卷面页眉，保证同题同名。
func TruncateDisplayTitle(s string) string {
	r := []rune(s)
	if len(r) <= paperTitleMaxRunes {
		return s
	}
	return string(r[:paperTitleMaxRunes]) + "…"
}

// PaperMeta 渲染入参（卷外信息）。
type PaperMeta struct {
	Term    string    // 学期（档案 grade_term，如「五年级上」）；可空
	Date    time.Time // 卷面日期：正卷 = 固化日；预览 = 今天
	Preview bool      // 固化前预览：无卷面号（固化才分配），版面与正卷同口径
}

// RenderPaperMarkdown 渲染练习卷 Markdown（§4.13 卷面版面，2026-07-18 呈现物真实渲染批）：
//   - 页眉：卷名（超 18 字截断）+ 卷面号（预览时明示“固化后分配”）+ 学期 + 日期；
//   - 只渲染入卷（verified）题，按 paper_seq 升序（学科分组连续编号）；阻断题绝不出现（INV-010）；
//   - 题目卷：题面 + ≥4 行作答留白；答案卷：题面 + 答案 + 校验依据，独立成卷；
//   - 单页超过 6 题分页，每页页脚「第 x/y 页 · 卷面号」（拍照裁切也能看到卷面号）。
//
// 预览与正卷共用本渲染器——预览口径 = 固化产物口径（诚实预览，不做第二套排版）。
// 题号缺失（draft 未固化）时按固化同款规则临时编号，不落库。
func RenderPaperMarkdown(f PracticeSetFields, kind string, meta PaperMeta) string {
	items := publishableSorted(f)
	title := TruncateDisplayTitle(f.Title)
	if kind == PaperKindAnswer {
		title += " · 答案卷"
	}
	footerNo := f.PaperNo
	if meta.Preview || footerNo == "" {
		footerNo = "预览"
	}

	var b strings.Builder
	b.WriteString("# " + title + "\n\n")
	headParts := []string{}
	if meta.Preview || f.PaperNo == "" {
		headParts = append(headParts, "预览 · 打印或发送后分配卷面号")
	} else {
		headParts = append(headParts, "卷面号 "+f.PaperNo)
	}
	if meta.Term != "" {
		headParts = append(headParts, meta.Term)
	}
	headParts = append(headParts, meta.Date.Format("2006/01/02"))
	b.WriteString(strings.Join(headParts, " · ") + "\n\n")
	if kind == PaperKindAnswer {
		b.WriteString("先别给孩子看：答案卷和题目卷分开保存，等孩子做完再核对。\n\n")
	} else {
		b.WriteString("姓名：________　日期：________\n\n")
	}

	totalPages := (len(items) + paperItemsPerPage - 1) / paperItemsPerPage
	if totalPages == 0 {
		totalPages = 1
	}
	for i, it := range items {
		b.WriteString(fmt.Sprintf("%d. %s\n\n", it.PaperSeq, it.QuestionMarkdown))
		if kind == PaperKindAnswer {
			line := "**答案：** " + strings.TrimSpace(it.ExpectedAnswerMarkdown)
			if it.VerificationEvidence != "" {
				line += " · " + it.VerificationEvidence
			}
			b.WriteString(line + "\n\n")
		} else {
			// 作答留白：≥4 行作答空间（§4.13；横线在打印/导出渲染为作答线）。
			b.WriteString("**答：**\n\n---\n\n---\n\n---\n\n---\n\n")
		}
		if (i+1)%paperItemsPerPage == 0 || i == len(items)-1 {
			page := i/paperItemsPerPage + 1
			b.WriteString(fmt.Sprintf("第 %d/%d 页 · %s\n\n", page, totalPages, footerNo))
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// publishableSorted 取入卷（verified）题并按 paper_seq 升序；题号缺失（draft 预览）时
// 先按固化同款规则（AssignPaperSeqs）在副本上临时编号，保证预览与正卷同口径。
func publishableSorted(f PracticeSetFields) []PracticeItem {
	items := make([]PracticeItem, len(f.Items))
	copy(items, f.Items)
	needSeq := false
	for _, it := range items {
		if PracticeItemPublishable(it) && it.PaperSeq == 0 {
			needSeq = true
			break
		}
	}
	if needSeq {
		AssignPaperSeqs(items)
	}
	pub := items[:0]
	for _, it := range items {
		if PracticeItemPublishable(it) {
			pub = append(pub, it)
		}
	}
	sort.SliceStable(pub, func(i, j int) bool { return pub[i].PaperSeq < pub[j].PaperSeq })
	return pub
}
