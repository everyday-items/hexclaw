package k12

import (
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
		AllowedActions:     []string{"send", "collect", "record_language_issue"},
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
		"allowed_actions":["send"],"projection_markdown":"### 观察",
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
		AllowedActions:     []string{"send", "collect"},
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
	for _, want := range []string{"## 观察与依据", "使用了可见的比喻句。", "## 下一步建议", "由孩子补充一个听觉细节。", "## 能力与证据限制", "只依据已确认原文。"} {
		if !strings.Contains(got, want) {
			t.Fatalf("deterministic projection missing %q: %q", want, got)
		}
	}
	if strings.Contains(got, "feedback_id") || strings.Contains(got, "allowed_actions") {
		t.Fatalf("display projection must not leak internal schema: %q", got)
	}
}
