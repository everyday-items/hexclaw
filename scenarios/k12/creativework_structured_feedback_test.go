package k12

import (
	"encoding/json"
	"strings"
	"testing"
)

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
	for _, want := range []string{"## 可见证据", "使用了可见的比喻句。", "## 先这样肯定", "## 家长可以这样问或讲", "## 下一次只试一个点", "由孩子补充一个听觉细节。", "## 说明", "只依据已确认原文。"} {
		if !strings.Contains(got, want) {
			t.Fatalf("deterministic projection missing %q: %q", want, got)
		}
	}
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

	got := ProjectWorkFeedbackMarkdown(feedback)
	for _, want := range []string{
		"## 可见证据",
		"主体位于画面中央。",
		"## 先这样肯定",
		"## 家长可以这样问或讲",
		"## 下一次只试一个点",
		"保留主体位置，再加强最亮与最暗处的差别。",
		"## 说明",
		"只依据当前原图中可见内容，不猜测创作意图。",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("approved parent-facing projection missing %q: %q", want, got)
		}
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
