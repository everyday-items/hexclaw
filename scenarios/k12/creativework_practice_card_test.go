package k12

import (
	"strings"
	"testing"
)

// 观察练习卡提炼契约（§3.10 美术：给少量可执行练习且练习必须有产物；2026-07-18 补：
// 练习卡界面必须携带打印/发送动作）。规格申报：v0.5 最小实现从点评正文提炼「建议段」
// （art-feedback skill 输出信封：建议全部用「试试/比一比」表达）为练习卡文本——
//  1. 优先取标题含「建议」的小节正文；
//  2. 无小节结构时收集含「试试 / 比一比 / 练习」的行；
//  3. 全都提不出时整段点评兜底（宁全勿空——练习必须有产物）；
//  4. 空点评 → 空卡（无点评就没有练习承诺）。
func TestObservationPracticeCard_SectionExtraction(t *testing.T) {
	feedback := "我在画里看到一间红色的房子。\n\n## 亮点\n左下角的快线很有风感。\n\n## 建议\n- 试试把最重要的画得比拳头还大；练习：同一物体画三张比一比。\n- 试试只用 3 支颜色画一张小画。\n\n## 其他\n继续保持。"
	card := ObservationPracticeCard(feedback)
	if !strings.Contains(card, "试试把最重要的画得比拳头还大") || !strings.Contains(card, "3 支颜色") {
		t.Fatalf("应提炼「建议」小节两条练习, got %q", card)
	}
	if strings.Contains(card, "红色的房子") || strings.Contains(card, "继续保持") {
		t.Fatalf("不应混入建议段以外内容, got %q", card)
	}
}

func TestObservationPracticeCard_LineFallback(t *testing.T) {
	feedback := "画面很完整。试试下次把太阳画在左上角，比一比哪张更亮。颜色涂得很满。"
	card := ObservationPracticeCard(feedback)
	if !strings.Contains(card, "试试下次把太阳画在左上角") {
		t.Fatalf("无小节结构时应按「试试/比一比」行提炼, got %q", card)
	}
}

func TestObservationPracticeCard_WholeFeedbackFallback(t *testing.T) {
	feedback := "多观察生活里的光影变化。"
	if card := ObservationPracticeCard(feedback); card != feedback {
		t.Fatalf("提不出建议段时整段兜底（练习必须有产物）, got %q", card)
	}
	if card := ObservationPracticeCard("  "); card != "" {
		t.Fatalf("空点评应空卡, got %q", card)
	}
}

func TestObservationPracticeCardFromStructured_UsesCanonicalSuggestions(t *testing.T) {
	feedback := &WorkFeedback{
		FeedbackType: WorkTypeArt,
		Suggestions: []string{
			"试试让人物的视线和右下角的小猫发生联系。",
			"如果愿意，可以让地面的绿色再分出深浅两层。",
		},
		ProjectionMarkdown: "# 下一步建议\n这段旧投影故意与结构化建议不同。",
	}
	card := ObservationPracticeCardFromStructured(feedback, "旧自由文本也不应成为第二事实源")
	for _, want := range feedback.Suggestions {
		if !strings.Contains(card, want) {
			t.Fatalf("观察卡必须完整保留 canonical suggestion %q, got %q", want, card)
		}
	}
	for _, unwanted := range []string{"旧投影", "旧自由文本"} {
		if strings.Contains(card, unwanted) {
			t.Fatalf("已有结构化点评时不得回退二次解析 %q: %q", unwanted, card)
		}
	}
}

// PracticeCardDoneAt 字段随版本 JSON 往返（打卡记录的持久载体）。
func TestCreativeWorkVersion_PracticeCardDoneRoundtrip(t *testing.T) {
	f := CreativeWorkFields{WorkType: WorkTypeArt, Title: "雨后的校园", Task: "写生",
		Versions: []CreativeWorkVersion{{VersionID: "v1", Feedback: "试试三档明暗。", PracticeCardDoneAt: 1234}}}
	rec, err := NewCreativeWorkRecord("mingming", "s", f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseCreativeWorkFields(rec.Fields)
	if err != nil || got.Versions[0].PracticeCardDoneAt != 1234 {
		t.Fatalf("practice_card_done_at 应随版本持久化, got %+v err=%v", got, err)
	}
	if !strings.Contains(rec.Fields, `"practice_card_done_at"`) {
		t.Fatalf("JSON 键应为 practice_card_done_at（跨端契约）, got %s", rec.Fields)
	}
}
