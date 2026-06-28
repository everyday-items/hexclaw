package builtin

// A scheduled (cron) "写入知识库" job was overwriting the previous run: the LLM
// reused the same title, and AddDocument upserts by (source, title). The skill
// now routes cron writes to IngestSnapshot (append + skip-unchanged + bounded
// retention, implemented in the knowledge layer) and uses the job's stable base
// title so the series stays coherent. Interactive and non-cron writes keep the
// AddDocument upsert path.

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/skill"
)

// Cron dispatch → IngestSnapshot (NOT AddDocument), base title = the model's
// title when no stable job title is stamped.
func TestCronIngest_RoutesToSnapshot(t *testing.T) {
	ing := &fakeIngestor{}
	s := NewKnowledgeIngestSkill(ing)

	ctx := skill.WithSystemDispatchSource(context.Background(), "cron")
	if _, err := s.Execute(ctx, map[string]any{
		"title":   "百度热搜 TOP20",
		"content": "1. 甲\n2. 乙\n3. 丙",
		"source":  "Baidu Hot Search",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(ing.calls) != 0 {
		t.Fatalf("cron write must NOT go through AddDocument (upsert overwrites), got %d AddDocument calls", len(ing.calls))
	}
	if len(ing.snapshots) != 1 {
		t.Fatalf("cron write must go through IngestSnapshot, got %d", len(ing.snapshots))
	}
	if ing.snapshots[0].title != "百度热搜 TOP20" {
		t.Fatalf("snapshot base title should be the model's title here, got %q", ing.snapshots[0].title)
	}
}

// #4: a stable base title stamped by the cron dispatcher overrides the title the
// model improvises, keeping the snapshot series coherent across runs.
func TestCronIngest_StableBaseTitleOverridesModel(t *testing.T) {
	ing := &fakeIngestor{}
	s := NewKnowledgeIngestSkill(ing)

	ctx := skill.WithSystemDispatchSource(context.Background(), "cron")
	ctx = skill.WithSnapshotBaseTitle(ctx, "百度热搜采集") // the cron job's stable name

	// Two runs where the model varies its title — both must land under one series.
	for _, modelTitle := range []string{"百度热搜 TOP20", "今日百度热点榜"} {
		if _, err := s.Execute(ctx, map[string]any{
			"title":   modelTitle,
			"content": "内容 " + modelTitle,
			"source":  "Baidu Hot Search",
		}); err != nil {
			t.Fatalf("Execute(%s): %v", modelTitle, err)
		}
	}
	if len(ing.snapshots) != 2 {
		t.Fatalf("expected 2 snapshot writes, got %d", len(ing.snapshots))
	}
	for i, snap := range ing.snapshots {
		if snap.title != "百度热搜采集" {
			t.Fatalf("run %d base title must be the stable job title, got %q", i, snap.title)
		}
	}
}

// Interactive save: no system-dispatch source → AddDocument upsert path, title
// unchanged (re-saving the same note updates in place, never snapshots).
func TestInteractiveIngest_RoutesToAddDocument(t *testing.T) {
	ing := &fakeIngestor{}
	s := NewKnowledgeIngestSkill(ing)

	if _, err := s.Execute(context.Background(), map[string]any{
		"title":   "我的周报",
		"content": "本周完成了 A、B、C 三项工作，长度足够切块。",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(ing.snapshots) != 0 {
		t.Fatalf("interactive save must NOT snapshot, got %d snapshot calls", len(ing.snapshots))
	}
	if len(ing.calls) != 1 || ing.calls[0].title != "我的周报" {
		t.Fatalf("interactive save must AddDocument with the exact title, got %+v", ing.calls)
	}
}

// The snapshot routing is scoped to cron specifically — other system dispatches
// (webhook/heartbeat/spawn) keep the AddDocument upsert path so a one-shot or
// event-driven write is not silently duplicated.
func TestNonCronDispatch_RoutesToAddDocument(t *testing.T) {
	for _, src := range []string{"webhook", "heartbeat", "spawn"} {
		ing := &fakeIngestor{}
		s := NewKnowledgeIngestSkill(ing)
		ctx := skill.WithSystemDispatchSource(context.Background(), src)
		if _, err := s.Execute(ctx, map[string]any{
			"title":   "事件快照",
			"content": "事件触发写入的内容，长度足够切块处理。",
		}); err != nil {
			t.Fatalf("Execute(%s): %v", src, err)
		}
		if len(ing.snapshots) != 0 {
			t.Fatalf("source=%q must not snapshot, got %d", src, len(ing.snapshots))
		}
		if len(ing.calls) != 1 || ing.calls[0].title != "事件快照" {
			t.Fatalf("source=%q must AddDocument with the exact title, got %+v", src, ing.calls)
		}
	}
}
