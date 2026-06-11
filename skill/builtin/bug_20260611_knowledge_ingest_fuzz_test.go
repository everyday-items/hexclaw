package builtin

// Property-based invariants for knowledge_ingest (BUG-20260611, test-closure
// method #1). For arbitrary inputs the skill must uphold:
//
//	I1: err == nil  ⟺  trimmed content is non-empty
//	I2: on success exactly one AddDocument call is made
//	I3: the persisted title is never empty and never spans multiple lines
//	I4: a derived title never exceeds ingestTitleMaxRunes runes
//	I5: empty source defaults to "agent"; no input ever panics
import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

func FuzzBug20260611_KnowledgeIngestInvariants(f *testing.F) {
	f.Add("title", "content", "source")
	f.Add("", "", "")
	f.Add("", "content without title", "")
	f.Add("   ", "\n\n  spaced  \n second line", " ")
	f.Add("提纲", "中文内容，含标点。\r\n第二行", "cron:job-1")
	f.Add("q'); DROP TABLE kb_documents;--", "Robert'); DROP TABLE x;--", "url://x")
	f.Add("", strings.Repeat("长内容", 500), "")
	f.Add("", "\t\r\n ", "src")

	f.Fuzz(func(t *testing.T, title, content, source string) {
		ing := &fakeIngestor{}
		s := NewKnowledgeIngestSkill(ing)

		result, err := s.Execute(context.Background(), map[string]any{
			"title":   title,
			"content": content,
			"source":  source,
		})

		trimmedContent := strings.TrimSpace(content)
		if trimmedContent == "" {
			// I1 (empty side) + I2: rejected without touching the store.
			if err == nil {
				t.Fatalf("empty content must be rejected, got result %+v", result)
			}
			if len(ing.calls) != 0 {
				t.Fatalf("rejected input must not reach AddDocument, got %d calls", len(ing.calls))
			}
			return
		}

		// I1 (non-empty side)
		if err != nil {
			t.Fatalf("non-empty content must ingest, got error: %v", err)
		}
		// I2
		if len(ing.calls) != 1 {
			t.Fatalf("want exactly 1 AddDocument call, got %d", len(ing.calls))
		}
		call := ing.calls[0]

		// I3
		if strings.TrimSpace(call.title) == "" {
			t.Fatalf("persisted title must never be empty (title=%q content=%q)", title, content)
		}
		if strings.ContainsAny(call.title, "\r\n") {
			t.Fatalf("persisted title must be single-line, got %q", call.title)
		}
		// I4: only derived titles are length-capped
		if strings.TrimSpace(title) == "" && utf8.RuneCountInString(call.title) > ingestTitleMaxRunes {
			t.Fatalf("derived title exceeds %d runes: %q", ingestTitleMaxRunes, call.title)
		}
		// I5
		if strings.TrimSpace(source) == "" && call.source != "agent" {
			t.Fatalf("empty source must default to \"agent\", got %q", call.source)
		}
		if result == nil || !strings.Contains(result.Content, "doc-test-1") {
			t.Fatalf("result must echo the document ID, got %+v", result)
		}
	})
}
