package usecase_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// 积累默写出题装篮（架构设计 §3.9 出口 + 默写出题格式，2026-07-18 定死）RED 契约：
//   - ≤20 字（单词/短句）→ 全文默写题「默写：{内容}」，答案 = 内容；
//   - 古诗默认补空式（逐句留 1～2 个关键字空），整首默写须显式选择；
//   - >100 字长文拒绝：“内容过长，不适合默写练习”；
//   - 语/英字符比对达门 → 直接 verified；item added_via=accumulation，入待打印篮。

func TestDictationToBasket_ShortFullText(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	id, _, err := d.AddAccumulation(ctx, "xiaoming", "s1", k12.AccumFields{
		Subject: "语文", EntryType: "好词好句", Content: "桂花香",
	})
	if err != nil {
		t.Fatal(err)
	}
	setID, added, err := d.GenerateDictationToBasket(ctx, "xiaoming", "s1", id, false)
	if err != nil || !added {
		t.Fatalf("默写出题装篮: added=%v err=%v", added, err)
	}
	v, err := d.GetPracticeSet(ctx, "xiaoming", setID)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Fields.Items) != 1 {
		t.Fatalf("篮内应 1 题，got %d", len(v.Fields.Items))
	}
	it := v.Fields.Items[0]
	if it.QuestionMarkdown != "默写：桂花香" || it.ExpectedAnswerMarkdown != "桂花香" {
		t.Errorf("≤20 字应出全文默写题「默写：{内容}」答案=内容, got q=%q a=%q", it.QuestionMarkdown, it.ExpectedAnswerMarkdown)
	}
	if it.AddedVia != k12.PracticeAddedViaAccumulation {
		t.Errorf("added_via 应为 accumulation, got %q", it.AddedVia)
	}
	if it.VerificationStatus != k12.PracticeItemVerified {
		t.Errorf("语文字符比对达门应直接 verified, got %q", it.VerificationStatus)
	}
	// 幂等：同一条积累重复生成不重复装篮。
	if _, added2, err := d.GenerateDictationToBasket(ctx, "xiaoming", "s1", id, false); err != nil || added2 {
		t.Errorf("重复生成应幂等去重: added=%v err=%v", added2, err)
	}
}

func TestDictationToBasket_PoemBlankFill(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	poem := "床前明月光，疑是地上霜。举头望明月，低头思故乡。"
	id, _, err := d.AddAccumulation(ctx, "xiaoming", "s1", k12.AccumFields{
		Subject: "语文", EntryType: "古诗积累", Content: poem, Source: "李白《静夜思》",
	})
	if err != nil {
		t.Fatal(err)
	}
	setID, added, err := d.GenerateDictationToBasket(ctx, "xiaoming", "s1", id, false)
	if err != nil || !added {
		t.Fatalf("古诗默写装篮: added=%v err=%v", added, err)
	}
	v, _ := d.GetPracticeSet(ctx, "xiaoming", setID)
	it := v.Fields.Items[0]
	if !strings.Contains(it.QuestionMarkdown, "＿") {
		t.Errorf("古诗默认应出补空式（含空格占位），got %q", it.QuestionMarkdown)
	}
	if strings.Contains(it.QuestionMarkdown, "疑是地上霜") {
		t.Errorf("补空题不得整句泄露原文，got %q", it.QuestionMarkdown)
	}
	if it.ExpectedAnswerMarkdown != poem {
		t.Errorf("答案应为全文, got %q", it.ExpectedAnswerMarkdown)
	}

	// 整首默写须家长显式选择（fullDictation=true）→ 全文默写形态。
	id2, _, _ := d.AddAccumulation(ctx, "xiaoming", "s1", k12.AccumFields{
		Subject: "语文", EntryType: "古诗积累", Content: "春眠不觉晓，处处闻啼鸟。",
	})
	setID2, _, err := d.GenerateDictationToBasket(ctx, "xiaoming", "s1", id2, true)
	if err != nil {
		t.Fatal(err)
	}
	v2, _ := d.GetPracticeSet(ctx, "xiaoming", setID2)
	found := false
	for _, item := range v2.Fields.Items {
		if item.ExpectedAnswerMarkdown == "春眠不觉晓，处处闻啼鸟。" && !strings.Contains(item.QuestionMarkdown, "＿") {
			found = true
		}
	}
	if !found {
		t.Error("显式选择整首默写应出全文默写题（无补空）")
	}
}

func TestDictationToBasket_RejectLongContent(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	long := strings.Repeat("好句素材内容很长", 15) // 120 字 > 100
	id, _, err := d.AddAccumulation(ctx, "xiaoming", "s1", k12.AccumFields{
		Subject: "语文", EntryType: "写作素材", Content: long,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.GenerateDictationToBasket(ctx, "xiaoming", "s1", id, false); err == nil {
		t.Fatal(">100 字长文应拒绝生成默写练习（§3.9）")
	} else if !errors.Is(err, usecase.ErrInvalidInput) {
		t.Errorf("长文拒绝应为客户端可修正错误（ErrInvalidInput→400），got %v", err)
	}
	// 拒绝后不得留下半截篮。
	sets, _ := d.ListPracticeSets(ctx, "xiaoming", "")
	for _, s := range sets {
		if len(s.Fields.Items) > 0 {
			t.Errorf("拒绝生成后篮内不应有题: %+v", s.Fields.Items)
		}
	}
}

// TestAccumKeepTypes_NewTaxonomyNotCorrective 学科分化新类型（§3.9 类型按学科分化，2026-07-18 定死）
// 均为积累型：不进复习队列（防「古诗积累」被当纠错型误入错题飞轮）。
func TestAccumKeepTypes_NewTaxonomyNotCorrective(t *testing.T) {
	for _, typ := range []string{"古诗积累", "写作素材", "表达积累", "词汇积累", "好词好句", "古诗", "语法点", "作文"} {
		if k12.AccumIsCorrective(typ) {
			t.Errorf("类型 %q 应为积累/留档型（不进复习队列）", typ)
		}
	}
	for _, typ := range []string{"默写错", "错词", "语法改错"} {
		if !k12.AccumIsCorrective(typ) {
			t.Errorf("类型 %q 应保持纠错型", typ)
		}
	}
}
