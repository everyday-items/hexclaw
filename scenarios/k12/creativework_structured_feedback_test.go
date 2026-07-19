package k12

import "testing"

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
