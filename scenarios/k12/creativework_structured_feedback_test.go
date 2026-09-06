package k12

import (
	"encoding/json"
	"strings"
	"testing"
)

func assertApprovedWorkFeedbackProjection(
	t *testing.T,
	markdown, limitation string,
) {
	t.Helper()
	wantHeadings := []string{
		"## 可见证据",
		"## 先这样肯定",
		"## 家长可以这样问或讲",
		"## 下一次只试一个点",
	}
	gotHeadings := make([]string, 0, len(wantHeadings))
	for _, line := range strings.Split(markdown, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			gotHeadings = append(gotHeadings, line)
		}
	}
	if strings.Join(gotHeadings, "\x00") != strings.Join(wantHeadings, "\x00") {
		t.Fatalf("作品点评 H2 顺序 = %#v，期望 %#v；markdown=%q", gotHeadings, wantHeadings, markdown)
	}
	if strings.Contains(markdown, "## 说明") {
		t.Fatalf("能力限制只能作为四段后的轻量正文，不能形成第五个 H2：%q", markdown)
	}
	if strings.Count(markdown, limitation) != 1 {
		t.Fatalf("能力限制出现次数 = %d，期望 1；markdown=%q", strings.Count(markdown, limitation), markdown)
	}
	lastSection := strings.Index(markdown, "## 下一次只试一个点")
	limitationAt := strings.LastIndex(markdown, limitation)
	if lastSection < 0 || limitationAt <= lastSection {
		t.Fatalf("能力限制必须位于第四段之后：section=%d limitation=%d markdown=%q", lastSection, limitationAt, markdown)
	}
	if !strings.HasSuffix(strings.TrimSpace(markdown), limitation) {
		t.Fatalf("能力限制必须是投影最后一个非空内容块：%q", markdown)
	}
}

func TestWorkFeedbackCanonicalContractAndStrictJSON(t *testing.T) {
	feedback := WorkFeedback{
		FeedbackID:   "feedback-1",
		VersionID:    "v1",
		FeedbackType: WorkTypeWriting,
		EvidenceRefs: []string{"content-ref:sha256:abc#full"},
		Observations: []WorkFeedbackObservation{{Dimension: "expression", Evidence: "使用了可见的比喻句。"}},
		SourceSnapshot: WorkFeedbackSourceSnapshot{
			Source: "ai", MethodRef: "writing-feedback@1.0.0/embedded", Capability: "evidence_based_feedback",
		},
		Limitations:        "只依据已确认原文。",
		Suggestions:        []string{"由孩子补充一个听觉细节。"},
		ParentGuidance:     "### 家长参考稿\n\n" + strings.Repeat("柳枝像绿色的丝带，随风轻轻摆动。\n\n", 40) + "先比较形状，再让孩子解释相似处。",
		ProjectionMarkdown: "### 观察\n\n- 使用了可见的比喻句。",
	}
	if err := feedback.Validate(); err != nil {
		t.Fatalf("valid canonical feedback rejected: %v", err)
	}

	if _, err := ParseWorkFeedbackJSON([]byte(`{
		"feedback_id":"feedback-1","version_id":"v1","feedback_type":"writing",
		"evidence_refs":["content-ref:sha256:abc#full"],
		"observations":[{"dimension":"expression","evidence":"可见证据"}],
		"source_snapshot":{"source":"ai","method_ref":"builtin","capability":"evidence_based_feedback"},
		"limitations":"只依据已确认原文","suggestions":["孩子自己补一个细节"],
		"projection_markdown":"### 观察",
		"score":95
	}`)); err == nil {
		t.Fatal("unknown/prohibited score field must fail closed")
	}
}

func TestWorkFeedbackCanonicalContractRejectsMarkdownScaffoldingInsideAtoms(t *testing.T) {
	base := WorkFeedback{
		FeedbackID:   "feedback-1",
		VersionID:    "v1",
		FeedbackType: WorkTypeWriting,
		EvidenceRefs: []string{"content-ref:sha256:abc#full"},
		Observations: []WorkFeedbackObservation{{
			Dimension: "expression",
			Evidence:  "使用了可见的比喻句。",
		}},
		SourceSnapshot: WorkFeedbackSourceSnapshot{
			Source: "ai", MethodRef: "writing-feedback@1.0.0/embedded", Capability: "evidence_based_feedback",
		},
		Limitations:        "只依据已确认原文。",
		Suggestions:        []string{"由孩子补充一个听觉细节。"},
		ProjectionMarkdown: "### 观察\n\n- 使用了可见的比喻句。",
	}
	cases := []struct {
		name   string
		mutate func(*WorkFeedback)
	}{
		{"heading leaked into evidence", func(f *WorkFeedback) {
			f.Observations[0].Evidence = "### 1. 总体评价"
		}},
		{"whole projection leaked into evidence", func(f *WorkFeedback) {
			f.Observations[0].Evidence = "观察成立。\n### 下一步\n**补一个细节**"
		}},
		{"unpaired emphasis leaked into suggestion", func(f *WorkFeedback) {
			f.Suggestions[0] = "**试试补一个细节"
		}},
		{"pure control token leaked into suggestion", func(f *WorkFeedback) {
			f.Suggestions[0] = "**"
		}},
		{"projection leaked into limitations", func(f *WorkFeedback) {
			f.Limitations = "只依据原文。\n### 下一步"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := base
			got.Observations = append([]WorkFeedbackObservation(nil), base.Observations...)
			got.Suggestions = append([]string(nil), base.Suggestions...)
			tc.mutate(&got)
			if err := got.Validate(); err == nil {
				t.Fatal("Markdown scaffold in canonical atom must fail closed")
			}
		})
	}
}

func TestProjectWorkFeedbackMarkdownIsDeterministicFromCanonicalFields(t *testing.T) {
	feedback := WorkFeedback{
		FeedbackType: WorkTypeWriting,
		Observations: []WorkFeedbackObservation{
			{Dimension: "expression", Evidence: "使用了可见的比喻句。"},
		},
		Limitations: "只依据已确认原文。",
		Suggestions: []string{"由孩子补充一个听觉细节。"},
	}
	got := ProjectWorkFeedbackMarkdown(feedback)
	for _, want := range []string{"使用了可见的比喻句。", "由孩子补充一个听觉细节。", "只依据已确认原文。"} {
		if !strings.Contains(got, want) {
			t.Fatalf("deterministic projection missing %q: %q", want, got)
		}
	}
	assertApprovedWorkFeedbackProjection(t, got, feedback.Limitations)
	if strings.Contains(got, "feedback_id") || strings.Contains(got, "allowed_actions") {
		t.Fatalf("display projection must not leak internal schema: %q", got)
	}
}

func TestProjectWorkFeedbackMarkdownUsesApprovedParentFacingFourPartProjection(t *testing.T) {
	feedback := WorkFeedback{
		FeedbackType: WorkTypeArt,
		Observations: []WorkFeedbackObservation{
			{Dimension: "composition", Evidence: "主体位于画面中央。"},
			{Dimension: "color", Evidence: "使用了紫色上衣和蓝色裙子。"},
		},
		Limitations: "只依据当前原图中可见内容，不猜测创作意图。",
		Suggestions: []string{"保留主体位置，再加强最亮与最暗处的差别。"},
	}
	if err := json.Unmarshal([]byte(`{"affirmation":"主角非常清楚。","parent_guidance":"画下一张之前，可以先观察自己举手时肩膀、手肘和手腕分别朝哪里。","next_step":"5分钟练习：对着镜子做三个不同的挥手动作，只用圆圈和线条画小人骨架。"}`), &feedback); err != nil {
		t.Fatal(err)
	}

	got := ProjectWorkFeedbackMarkdown(feedback)
	for _, want := range []string{
		"## 可见证据",
		"主体位于画面中央。",
		"## 先这样肯定",
		"## 家长可以这样问或讲",
		"## 下一次只试一个点",
		"主角非常清楚。",
		"肩膀、手肘和手腕分别朝哪里",
		"对着镜子做三个不同的挥手动作",
		"只依据当前原图中可见内容，不猜测创作意图。",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("approved parent-facing projection missing %q: %q", want, got)
		}
	}
	assertApprovedWorkFeedbackProjection(t, got, feedback.Limitations)
	if strings.Count(got, "主体位于画面中央。") != 1 || strings.Contains(got, "画面里你最想保留的是哪一处") {
		t.Fatalf("projection duplicated evidence or replaced concrete guidance: %q", got)
	}
	for _, retiredHeading := range []string{"## 观察与依据", "## 下一步建议", "## 能力与证据限制"} {
		if strings.Contains(got, retiredHeading) {
			t.Fatalf("retired projection heading %q must not remain: %q", retiredHeading, got)
		}
	}
}

func TestParseLegacyWorkFeedbackStripsRetiredAllowedActions(t *testing.T) {
	raw := []byte(`{
		"feedback_id":"feedback-legacy","version_id":"v1","feedback_type":"writing",
		"evidence_refs":["content-ref:sha256:abc"],
		"observations":[{"dimension":"expression","evidence":"使用了可见的比喻句。"}],
		"source_snapshot":{"source":"ai","method_ref":"builtin","capability":"evidence_based_feedback"},
		"limitations":"只依据已确认原文。","suggestions":["由孩子补充一个听觉细节。"],
		"allowed_actions":["send","collect","record_language_issue"],
		"projection_markdown":"旧投影"
	}`)
	feedback, err := ParseLegacyWorkFeedbackJSON(raw)
	if err != nil {
		t.Fatalf("historical feedback should remain readable after action retirement: %v", err)
	}
	encoded, err := json.Marshal(feedback)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "allowed_actions") {
		t.Fatalf("retired actions must be stripped at compatibility boundary: %s", encoded)
	}
}
