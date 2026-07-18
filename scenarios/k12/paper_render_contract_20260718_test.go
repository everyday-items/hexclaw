package k12_test

import (
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 2026-07-18 呈现物真实渲染契约（架构设计 §4.13 卷面版面）：
//   ① 题目卷只题面 + 留白，不含答案；答案卷题面 + 答案，两卷绝不混排；
//   ② 入卷题按学科分组顺序（数学→语文→英语→科学→信息科技）连续题号 paper_seq 出现在卷面；
//   ③ 阻断（非 verified）题绝不出现在任一卷面（INV-010 新表述）；
//   ④ 页眉含卷面号；单页超过 6 题分页，每页页脚「第 x/y 页 · 卷面号」（防拍照只拍到第二页）；
//   ⑤ 卷名超过 18 字截断加「…」（§4.13 标题截断规则在呈现物层的应用）。

func renderFixtureFields() k12.PracticeSetFields {
	f := k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceMixed,
		Title:      "本周复习卷 · 07/18",
		PaperNo:    "P-2629-01",
		Items: []k12.PracticeItem{
			{ItemID: "e1", Subject: "英语", QuestionMarkdown: "默写：believe", ExpectedAnswerMarkdown: "believe",
				VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "字符比对"},
			{ItemID: "s1", Subject: "科学", QuestionMarkdown: "选择能点亮小灯泡的闭合电路图",
				VerificationStatus: k12.PracticeItemPending, BlockedReason: "暂不支持自动验证"},
			{ItemID: "m1", Subject: "数学", QuestionMarkdown: "解方程：2x + 19 = 51", ExpectedAnswerMarkdown: "x = 16",
				VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算"},
			{ItemID: "c1", Subject: "语文", QuestionMarkdown: "默写：梅须逊雪三分白", ExpectedAnswerMarkdown: "梅须逊雪三分白",
				VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "原句字符比对"},
		},
	}
	k12.AssignPaperSeqs(f.Items)
	return f
}

// ①③ 题目卷：只题面 + 留白；不含答案，不含阻断题；页眉含卷面号。
func TestRenderQuestionPaperOmitsAnswersAndBlocked(t *testing.T) {
	f := renderFixtureFields()
	md := k12.RenderPaperMarkdown(f, k12.PaperKindQuestion, k12.PaperMeta{Date: time.Date(2026, 7, 18, 0, 0, 0, 0, time.Local)})
	if !strings.Contains(md, "P-2629-01") {
		t.Fatalf("题目卷页眉应含卷面号，got:\n%s", md)
	}
	for _, q := range []string{"解方程：2x + 19 = 51", "默写：梅须逊雪三分白", "默写：believe"} {
		if !strings.Contains(md, q) {
			t.Fatalf("题目卷应含入卷题面 %q，got:\n%s", q, md)
		}
	}
	if strings.Contains(md, "x = 16") {
		t.Fatalf("题目卷不得泄露答案，got:\n%s", md)
	}
	if strings.Contains(md, "闭合电路") {
		t.Fatalf("阻断题不得出现在题目卷，got:\n%s", md)
	}
	// 留白：作答区（≥4 行作答空间）。
	if !strings.Contains(md, "**答：**") {
		t.Fatalf("题目卷每题应带作答留白，got:\n%s", md)
	}
}

// ② 卷面题号连续且按学科分组：数学(1) → 语文(2) → 英语(3)。
func TestRenderPaperSeqSubjectOrderOnPaper(t *testing.T) {
	f := renderFixtureFields()
	md := k12.RenderPaperMarkdown(f, k12.PaperKindQuestion, k12.PaperMeta{Date: time.Now()})
	iMath := strings.Index(md, "1. 解方程：2x + 19 = 51")
	iCn := strings.Index(md, "2. 默写：梅须逊雪三分白")
	iEn := strings.Index(md, "3. 默写：believe")
	if iMath < 0 || iCn < 0 || iEn < 0 {
		t.Fatalf("卷面应按学科分组连续编号（数学1→语文2→英语3），got:\n%s", md)
	}
	if !(iMath < iCn && iCn < iEn) {
		t.Fatalf("卷面题序应为 数学→语文→英语，got 位置 %d/%d/%d", iMath, iCn, iEn)
	}
}

// ①③ 答案卷：题面 + 答案 + 校验依据；不含阻断题。
func TestRenderAnswerPaperContainsAnswers(t *testing.T) {
	f := renderFixtureFields()
	md := k12.RenderPaperMarkdown(f, k12.PaperKindAnswer, k12.PaperMeta{Date: time.Now()})
	for _, a := range []string{"x = 16", "梅须逊雪三分白", "believe"} {
		if !strings.Contains(md, a) {
			t.Fatalf("答案卷应含答案 %q，got:\n%s", a, md)
		}
	}
	if !strings.Contains(md, "独立验算") {
		t.Fatalf("答案卷应含校验依据（家长核对口径），got:\n%s", md)
	}
	if strings.Contains(md, "闭合电路") {
		t.Fatalf("阻断题不得出现在答案卷，got:\n%s", md)
	}
	if !strings.Contains(md, "P-2629-01") {
		t.Fatalf("答案卷页眉应含卷面号，got:\n%s", md)
	}
}

// ④ 分页：7 题 → 2 页，每页页脚「第 x/y 页 · 卷面号」。
func TestRenderPaperPaginationFooterEveryPage(t *testing.T) {
	f := k12.PracticeSetFields{SourceKind: k12.PracticeSourceWeekly, Title: "分页卷", PaperNo: "P-2629-03"}
	for i := 0; i < 7; i++ {
		f.Items = append(f.Items, k12.PracticeItem{
			ItemID: string(rune('a' + i)), Subject: "数学",
			QuestionMarkdown: "第" + string(rune('a'+i)) + "题", ExpectedAnswerMarkdown: "答",
			VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算",
		})
	}
	k12.AssignPaperSeqs(f.Items)
	md := k12.RenderPaperMarkdown(f, k12.PaperKindQuestion, k12.PaperMeta{Date: time.Now()})
	if !strings.Contains(md, "第 1/2 页 · P-2629-03") || !strings.Contains(md, "第 2/2 页 · P-2629-03") {
		t.Fatalf("7 题应分 2 页且每页页脚带卷面号，got:\n%s", md)
	}
}

// ⑤ 卷名截断：超过 18 字加「…」。
func TestRenderPaperTitleTruncated(t *testing.T) {
	if got := k12.TruncateDisplayTitle("一二三四五六七八九十一二三四五六七八九十"); got != "一二三四五六七八九十一二三四五六七八…" {
		t.Fatalf("标题应截断为前 18 字 + …，got %q", got)
	}
	if got := k12.TruncateDisplayTitle("本周复习卷 · 07/18"); got != "本周复习卷 · 07/18" {
		t.Fatalf("18 字内标题不截断，got %q", got)
	}
	f := renderFixtureFields()
	f.Title = "一二三四五六七八九十一二三四五六七八九十"
	md := k12.RenderPaperMarkdown(f, k12.PaperKindQuestion, k12.PaperMeta{Date: time.Now()})
	if !strings.Contains(md, "一二三四五六七八九十一二三四五六七八…") {
		t.Fatalf("卷面标题应截断，got:\n%s", md)
	}
	if strings.Contains(md, "一二三四五六七八九十一二三四五六七八九十") {
		t.Fatalf("卷面不得出现未截断长标题，got:\n%s", md)
	}
}
