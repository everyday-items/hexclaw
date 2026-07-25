package k12storage_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func putCreativeWorkWithFeedback(
	t *testing.T,
	store *k12storage.Store,
	title string,
) *records.AgentRecord {
	t.Helper()
	feedback := k12.WorkFeedback{
		FeedbackID:   "feedback-" + title,
		VersionID:    "v1",
		FeedbackType: k12.WorkTypeArt,
		EvidenceRefs: []string{"asset-ref:sha256:test"},
		Observations: []k12.WorkFeedbackObservation{{
			Dimension: "composition",
			Evidence:  "主体位于画面中央。",
		}},
		SourceSnapshot: k12.WorkFeedbackSourceSnapshot{
			Source:     k12.FeedbackSourceAI,
			MethodRef:  "art-feedback@1.0.0/disk",
			Capability: "evidence_based_feedback",
		},
		Limitations: "只依据当前图片中可见的画面。",
		Suggestions: []string{"试试让主体和小猫产生视线联系。"},
	}
	feedback.ProjectionMarkdown = k12.ProjectWorkFeedbackMarkdown(feedback)
	rec, err := k12.NewCreativeWorkRecord("mingming", "session", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeArt,
		Title:    title,
		Task:     "观察构图",
		Versions: []k12.CreativeWorkVersion{{
			VersionID:          "v1",
			SourceAssetID:      "asset://mingming/" + title + ".png",
			Feedback:           feedback.ProjectionMarkdown,
			FeedbackSource:     k12.FeedbackSourceAI,
			FeedbackSkill:      "art-feedback@1.0.0/disk",
			StructuredFeedback: &feedback,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestTyped_CreativeWorkLegacyMarkdownAtomsDoNotPoisonList(t *testing.T) {
	store, db := setup(t)
	healthy := putCreativeWorkWithFeedback(t, store, "健康作品")
	legacy := putCreativeWorkWithFeedback(t, store, "旧版作品")

	var raw string
	if err := db.QueryRow(
		`SELECT structured_feedback_json FROM k12_work_feedback
		 WHERE work_record_id = ? AND version_index = 0`,
		legacy.RecordID,
	).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var polluted k12.WorkFeedback
	if err := json.Unmarshal([]byte(raw), &polluted); err != nil {
		t.Fatal(err)
	}
	polluted.Observations[0].Evidence = "- **构图**：主体位于画面中央。"
	polluted.Suggestions[0] = "1. **试试**让主体和小猫产生视线联系。"
	rawBytes, err := json.Marshal(polluted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`UPDATE k12_work_feedback SET structured_feedback_json = ?
		 WHERE work_record_id = ? AND version_index = 0`,
		string(rawBytes), legacy.RecordID,
	); err != nil {
		t.Fatal(err)
	}

	works, err := store.ListByScope(context.Background(), "mingming", k12.CollectionCreativeWork, "")
	if err != nil {
		t.Fatalf("一条可兼容的旧点评不得让作品列表整体失败: %v", err)
	}
	if len(works) != 2 {
		t.Fatalf("作品列表应保留健康与旧版两条记录，got %d", len(works))
	}
	var foundHealthy, foundLegacy bool
	for _, work := range works {
		fields, parseErr := k12.ParseCreativeWorkFields(work.Fields)
		if parseErr != nil || len(fields.Versions) != 1 {
			t.Fatalf("作品字段应可解析: fields=%q err=%v", work.Fields, parseErr)
		}
		switch work.RecordID {
		case healthy.RecordID:
			foundHealthy = fields.Versions[0].StructuredFeedback != nil
		case legacy.RecordID:
			foundLegacy = true
			repaired := fields.Versions[0].StructuredFeedback
			if repaired == nil {
				t.Fatal("仅混入 Markdown 结构符的旧点评应兼容恢复")
			}
			if err := repaired.Validate(); err != nil {
				t.Fatalf("兼容恢复后的点评必须重新满足严格 schema: %v", err)
			}
			if strings.Contains(repaired.Observations[0].Evidence, "**") ||
				strings.HasPrefix(repaired.Observations[0].Evidence, "- ") ||
				strings.Contains(repaired.Suggestions[0], "**") {
				t.Fatalf("兼容恢复后 canonical atoms 仍含 Markdown: %+v", repaired)
			}
			if repaired.ProjectionMarkdown != k12.ProjectWorkFeedbackMarkdown(*repaired) {
				t.Fatal("旧点评恢复后必须从 canonical atoms 重建唯一 Markdown 投影")
			}
		}
	}
	if !foundHealthy || !foundLegacy {
		t.Fatalf("健康/旧版作品均应保留: healthy=%v legacy=%v", foundHealthy, foundLegacy)
	}
}

func TestTyped_CreativeWorkUnrecoverableFeedbackIsolatedFromList(t *testing.T) {
	store, db := setup(t)
	healthy := putCreativeWorkWithFeedback(t, store, "健康作品")
	broken := putCreativeWorkWithFeedback(t, store, "损坏作品")
	if _, err := db.Exec(
		`UPDATE k12_work_feedback SET structured_feedback_json = ?
		 WHERE work_record_id = ? AND version_index = 0`,
		`{"feedback_id":"bad","score":100}`,
		broken.RecordID,
	); err != nil {
		t.Fatal(err)
	}

	works, err := store.ListByScope(context.Background(), "mingming", k12.CollectionCreativeWork, "")
	if err != nil {
		t.Fatalf("一条不可恢复的旧点评不得让作品列表整体失败: %v", err)
	}
	if len(works) != 2 {
		t.Fatalf("损坏点评只能隔离其结构化事实，不能吞掉作品，got %d", len(works))
	}
	for _, work := range works {
		fields, parseErr := k12.ParseCreativeWorkFields(work.Fields)
		if parseErr != nil || len(fields.Versions) != 1 {
			t.Fatalf("作品字段应可解析: fields=%q err=%v", work.Fields, parseErr)
		}
		if work.RecordID == healthy.RecordID && fields.Versions[0].StructuredFeedback == nil {
			t.Fatal("隔离坏点评不得影响健康作品")
		}
		if work.RecordID == broken.RecordID {
			if fields.Versions[0].StructuredFeedback != nil {
				t.Fatal("含未知 score 字段的坏点评不得进入 canonical structured facts")
			}
			if strings.TrimSpace(fields.Versions[0].Feedback) == "" {
				t.Fatal("坏 structured facts 被隔离时应保留旧 Markdown 只读展示")
			}
		}
	}
}

func TestTyped_CreativeWorkWriteRejectsMarkdownInCanonicalAtoms(t *testing.T) {
	store, _ := setup(t)
	feedback := k12.WorkFeedback{
		FeedbackID:   "feedback-invalid-write",
		VersionID:    "v1",
		FeedbackType: k12.WorkTypeArt,
		EvidenceRefs: []string{"asset-ref:sha256:test"},
		Observations: []k12.WorkFeedbackObservation{{
			Dimension: "composition",
			Evidence:  "- **构图**：主体位于画面中央。",
		}},
		SourceSnapshot: k12.WorkFeedbackSourceSnapshot{
			Source:     k12.FeedbackSourceAI,
			MethodRef:  "art-feedback@1.0.0/disk",
			Capability: "evidence_based_feedback",
		},
		Limitations:        "只依据当前图片中可见的画面。",
		Suggestions:        []string{"试试让主体和小猫产生视线联系。"},
		ProjectionMarkdown: "## 观察与依据",
	}
	rec, err := k12.NewCreativeWorkRecord("mingming", "session", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeArt,
		Title:    "非法结构化点评",
		Task:     "观察构图",
		Versions: []k12.CreativeWorkVersion{{
			VersionID:          "v1",
			SourceAssetID:      "asset://mingming/invalid.png",
			Feedback:           "旧展示文本",
			StructuredFeedback: &feedback,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), rec); err == nil ||
		!strings.Contains(err.Error(), "观察证据混入 Markdown 结构符") {
		t.Fatalf("新写入必须在持久化边界 fail-closed，got %v", err)
	}
}
