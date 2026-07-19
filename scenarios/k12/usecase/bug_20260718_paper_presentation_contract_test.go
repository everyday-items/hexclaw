package usecase_test

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 2026-07-18 呈现物裁决契约（架构设计 §3.8/§4.13）：
//   ① 卷面号双 ID——内部主键保留 NanoID，固化时另生成 OCR 友好的卷面号 paper_no
//      （P-{YY}{ISO周}-{2位序}，纯大写字母 P + 数字，去混淆字符），同 Learner 内唯一；
//   ② 题级对齐锚点——固化时给入卷（verified）题按学科分组编连续题号 paper_seq，阻断题无题号；
//   ③ finalized_at/finalized_via——固化时间与方式持久化，历史排序/回传提醒/启发窗口的统一依据；
//   ④ 部分回传——SubmitReturn 按题记录回传（幂等追加，补传合法），全部入卷题回传后才可 graded；
//   ⑤ subject 取值权威——练习项学科只允许六学科中的可组卷五科中文名或空，美术与非法值拒绝。

func mustAddVerified(t *testing.T, d interface {
	AddToBasket(ctx context.Context, agentName, sourceSession string, item k12.PracticeItem) (string, bool, error)
}, ctx context.Context, agent, subject, q, a, via string) string {
	t.Helper()
	id, _, err := d.AddToBasket(ctx, agent, "s", k12.PracticeItem{
		Subject: subject, QuestionMarkdown: q, ExpectedAnswerMarkdown: a, AddedVia: via,
		VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算/字符比对",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestPaperNoAssignedAtFinalize ①：卷面号在固化时分配，格式 P-YYWW-NN，草稿无卷面号，同 Learner 递增唯一。
func TestPaperNoAssignedAtFinalize(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	id := mustAddVerified(t, d, ctx, "xiaoming", "数学", "2x+19=51", "16", k12.PracticeAddedViaWeekly)

	v, _ := d.GetPracticeSet(ctx, "xiaoming", id)
	if v.Fields.PaperNo != "" {
		t.Fatalf("草稿（待打印篮）不应有卷面号，got %q", v.Fields.PaperNo)
	}
	v, _, err := d.FinalizeBasket(ctx, "xiaoming", id, "print", "")
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`^P-\d{4}-\d{2}$`)
	if !pattern.MatchString(v.Fields.PaperNo) {
		t.Fatalf("卷面号格式应为 P-YYWW-NN（OCR 友好、去混淆字符），got %q", v.Fields.PaperNo)
	}
	// Now=1000 → 1970 ISO 第 1 周 → P-7001-01。
	if v.Fields.PaperNo != "P-7001-01" {
		t.Fatalf("首张卷卷面号应为 P-7001-01，got %q", v.Fields.PaperNo)
	}

	// 第二张卷序号递增，不与第一张重复。
	id2 := mustAddVerified(t, d, ctx, "xiaoming", "数学", "3.8×3=?", "11.4", k12.PracticeAddedViaSingleVariant)
	v2, _, err := d.FinalizeBasket(ctx, "xiaoming", id2, "print", "")
	if err != nil {
		t.Fatal(err)
	}
	if v2.Fields.PaperNo != "P-7001-02" {
		t.Fatalf("第二张卷卷面号应递增为 P-7001-02，got %q", v2.Fields.PaperNo)
	}
}

// TestPaperSeqSubjectGrouped ②：题号只给入卷题、连续、按学科分组（数学→语文→英语→科学→信息科技），
// 组内保持装篮顺序；阻断题无题号。
func TestPaperSeqSubjectGrouped(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	f := k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceCustom, Title: "排版卷",
		Items: []k12.PracticeItem{
			{ItemID: "e1", Subject: "英语", QuestionMarkdown: "默写：believe", ExpectedAnswerMarkdown: "believe",
				VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "字符比对"},
			{ItemID: "s1", Subject: "科学", QuestionMarkdown: "闭合电路", VerificationStatus: k12.PracticeItemPending},
			{ItemID: "m1", Subject: "数学", QuestionMarkdown: "2x=10", ExpectedAnswerMarkdown: "5",
				VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算"},
			{ItemID: "m2", Subject: "数学", QuestionMarkdown: "3x=12", ExpectedAnswerMarkdown: "4",
				VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算"},
		},
	}
	id, _, err := d.CreatePracticeSet(ctx, "xiaoming", "s", f)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := d.FinalizeBasket(ctx, "xiaoming", id, "print", "")
	if err != nil {
		t.Fatal(err)
	}
	seq := map[string]int{}
	for _, it := range v.Fields.Items {
		seq[it.ItemID] = it.PaperSeq
	}
	// 数学在前（m1=1、m2=2 保持装篮顺序），英语随后（e1=3）；阻断题 s1 无题号。
	if seq["m1"] != 1 || seq["m2"] != 2 || seq["e1"] != 3 {
		t.Fatalf("题号应按学科分组连续编号：m1=1 m2=2 e1=3，got %v", seq)
	}
	if seq["s1"] != 0 {
		t.Fatalf("阻断题不应有题号，got %d", seq["s1"])
	}
}

// TestPracticeProblemMintedAtFinalize（外部评审 #4c，2026-07-18）：固化时为每个入卷题
// 铸造独立 practice_problem_id（复批 Attempt 的归属对象——变式题面≠原题，不得挂原错题 Problem），
// 并保留 derived_from（来源错题 Problem）；阻断题不铸造。
func TestPracticeProblemMintedAtFinalize(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	f := k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceWeekly, Title: "铸造卷",
		Items: []k12.PracticeItem{
			{ItemID: "v1", Subject: "数学", SourceProblemID: "prob-apple", QuestionMarkdown: "苹果变式题",
				ExpectedAnswerMarkdown: "12.6", AddedVia: k12.PracticeAddedViaWeekly,
				VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算"},
			{ItemID: "b1", Subject: "科学", QuestionMarkdown: "阻断题", VerificationStatus: k12.PracticeItemPending},
		},
	}
	id, _, _ := d.CreatePracticeSet(ctx, "xiaoming", "s", f)
	v, _, err := d.FinalizeBasket(ctx, "xiaoming", id, "print", "")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]k12.PracticeItem{}
	for _, it := range v.Fields.Items {
		byID[it.ItemID] = it
	}
	minted := byID["v1"]
	if minted.PracticeProblemID == "" {
		t.Fatal("入卷题固化时应铸造 practice_problem_id（复批 Attempt 的归属对象）")
	}
	if minted.PracticeProblemID == minted.SourceProblemID {
		t.Fatal("铸造的 practice_problem_id 不得复用来源错题 Problem（题面已不同）")
	}
	if minted.SourceProblemID != "prob-apple" {
		t.Fatalf("derived_from 来源链应保留，got %q", minted.SourceProblemID)
	}
	if byID["b1"].PracticeProblemID != "" {
		t.Fatal("阻断题未入卷，不得铸造 practice_problem_id")
	}
}

// TestFinalizedAtRecorded ③：固化时间与方式持久化。
func TestFinalizedAtRecorded(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	id := mustAddVerified(t, d, ctx, "xiaoming", "数学", "1+1=?", "2", k12.PracticeAddedViaWeekly)
	v, _, err := d.FinalizeBasket(ctx, "xiaoming", id, "send", "钉钉私聊")
	if err != nil {
		t.Fatal(err)
	}
	if v.Fields.FinalizedAt != 1000 {
		t.Fatalf("finalized_at 应记录固化时间（Now=1000），got %d", v.Fields.FinalizedAt)
	}
	if v.Fields.FinalizedVia != "send" {
		t.Fatalf("finalized_via 应记录固化方式，got %q", v.Fields.FinalizedVia)
	}
}

// TestPartialReturnAndResubmit ④：部分回传合法、补传幂等、全部入卷题回传后才可 graded。
func TestPartialReturnAndResubmit(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	f := k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceWeekly, Title: "部分回传卷",
		Items: []k12.PracticeItem{
			{ItemID: "q1", Subject: "数学", QuestionMarkdown: "题1", ExpectedAnswerMarkdown: "答1",
				VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算"},
			{ItemID: "q2", Subject: "数学", QuestionMarkdown: "题2", ExpectedAnswerMarkdown: "答2",
				VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算"},
		},
	}
	id, _, _ := d.CreatePracticeSet(ctx, "xiaoming", "s", f)
	if _, _, err := d.FinalizeBasket(ctx, "xiaoming", id, "print", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	assetID := saveReturnAsset(t, "xiaoming")

	// 部分回传：只回传 q1 → submitted；q2 保持未回传。
	if _, err := d.SubmitReturn(ctx, "xiaoming", id, "return-partial-1", assetID, []string{"q1"}); err != nil {
		t.Fatalf("部分回传应合法: %v", err)
	}
	v, _ := d.GetPracticeSet(ctx, "xiaoming", id)
	if v.Record.Status != k12.PracticeStatusSubmitted {
		t.Fatalf("首次回传后应为 submitted，got %s", v.Record.Status)
	}
	// 未全部回传不得 graded——“全部题有结论后卷才转已批改”。
	if err := d.GradePracticeSet(ctx, "xiaoming", id); err == nil {
		t.Fatal("尚有未回传题却允许整卷 graded")
	} else if !strings.Contains(err.Error(), "未回传") {
		t.Fatalf("错误应提示未回传可补传，got %v", err)
	}
	// 补传是同一卷的合法追加；重复回传同一题幂等不报错。
	if _, err := d.SubmitReturn(ctx, "xiaoming", id, "return-partial-2", assetID, []string{"q1", "q2"}); err != nil {
		t.Fatalf("补传应合法且幂等: %v", err)
	}
	// 回传不存在的题拒绝。
	if _, err := d.SubmitReturn(ctx, "xiaoming", id, "return-ghost", assetID, []string{"ghost"}); err == nil {
		t.Fatal("回传不存在的题应报错")
	}
	if err := d.GradePracticeSet(ctx, "xiaoming", id); err != nil {
		t.Fatalf("全部回传后应可 graded: %v", err)
	}
	v, _ = d.GetPracticeSet(ctx, "xiaoming", id)
	if v.Record.Status != k12.PracticeStatusGraded {
		t.Fatalf("应为 graded，got %s", v.Record.Status)
	}
}

// TestSubjectEnumAuthority ⑤：subject 取值权威——可组卷五科中文名或空；美术与非法值在创建与装篮双入口拒绝。
func TestSubjectEnumAuthority(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	for _, bad := range []string{"美术", "math", "物理"} {
		f := k12.PracticeSetFields{SourceKind: k12.PracticeSourceManual, Title: "非法学科卷",
			Items: []k12.PracticeItem{{ItemID: "x1", Subject: bad, QuestionMarkdown: "题"}}}
		if _, _, err := d.CreatePracticeSet(ctx, "xiaoming", "s", f); err == nil {
			t.Fatalf("学科 %q 不可组卷，创建入口应拒绝", bad)
		}
		if _, _, err := d.AddToBasket(ctx, "xiaoming", "s", k12.PracticeItem{
			Subject: bad, QuestionMarkdown: "题" + bad, AddedVia: k12.PracticeAddedViaManual,
		}); err == nil {
			t.Fatalf("学科 %q 不可组卷，装篮入口应拒绝", bad)
		}
	}
	// 五科与空学科（未分科手工字词题）合法。
	for _, ok := range []string{"", "数学", "语文", "英语", "科学", "信息科技"} {
		if _, _, err := d.AddToBasket(ctx, "xiaoming", "s", k12.PracticeItem{
			Subject: ok, QuestionMarkdown: "合法题 " + ok, AddedVia: k12.PracticeAddedViaManual,
		}); err != nil {
			t.Fatalf("学科 %q 应合法: %v", ok, err)
		}
	}
}
