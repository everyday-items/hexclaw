package builtin

// 批量/目录入库 Skill 测试，验证不变量：
// 逐文件读取真实正文入库，绝不把"文件名列表"当 content。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/knowledge"
)

type recordingIngestor struct {
	calls []ingestCall
}
type ingestCall struct{ title, content, source string }

func (f *recordingIngestor) AddDocument(_ context.Context, title, content, source string) (*knowledge.Document, error) {
	f.calls = append(f.calls, ingestCall{title, content, source})
	return &knowledge.Document{ID: "doc-" + title, Title: title}, nil
}

func writeIngestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIngestPath_EachFileContentIngested(t *testing.T) {
	ws := t.TempDir()
	writeIngestFile(t, ws, "a.md", "alpha body content")
	writeIngestFile(t, ws, "b.md", "bravo body content")
	writeIngestFile(t, ws, "c.md", "charlie body content")

	kb := &recordingIngestor{}
	s := NewKnowledgeIngestPathSkill(kb, ws)
	res, err := s.Execute(context.Background(), map[string]any{"path": "."})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(kb.calls) != 3 {
		t.Fatalf("expected 3 AddDocument calls (one per file), got %d", len(kb.calls))
	}
	want := map[string]string{
		"a.md": "alpha body content",
		"b.md": "bravo body content",
		"c.md": "charlie body content",
	}
	for _, c := range kb.calls {
		exp, ok := want[c.title]
		if !ok {
			t.Fatalf("unexpected title %q (title must be the file name)", c.title)
		}
		// content must be THIS file's real body.
		if c.content != exp {
			t.Fatalf("Bug-1: file %s ingested content=%q, want real body %q", c.title, c.content, exp)
		}
		// content must NOT be a filename listing.
		if strings.Contains(c.content, "a.md") || strings.Contains(c.content, "b.md") || strings.Contains(c.content, "c.md") {
			t.Fatalf("Bug-1 regression: content looks like a filename listing: %q", c.content)
		}
		if !strings.HasSuffix(c.source, c.title) {
			t.Fatalf("default source should be the file path ending in %q, got %q", c.title, c.source)
		}
	}
	if !strings.Contains(res.Content, "Ingested 3/3") {
		t.Fatalf("summary should report 3/3, got %q", res.Content)
	}
}

func TestIngestPath_SymlinkEscape_Rejected(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	writeIngestFile(t, outside, "secret.md", "top secret")
	if err := os.Symlink(outside, filepath.Join(ws, "escape")); err != nil {
		t.Skip("symlink unsupported on host")
	}
	kb := &recordingIngestor{}
	s := NewKnowledgeIngestPathSkill(kb, ws)

	if _, err := s.Execute(context.Background(), map[string]any{"path": "escape"}); err == nil {
		t.Fatalf("symlink escape must be rejected")
	}
	if _, err := s.Execute(context.Background(), map[string]any{"path": "../.."}); err == nil {
		t.Fatalf("'..' traversal must be rejected")
	}
	if len(kb.calls) != 0 {
		t.Fatalf("nothing outside the workspace may be ingested, got %d", len(kb.calls))
	}
}

func TestIngestPath_Glob(t *testing.T) {
	ws := t.TempDir()
	writeIngestFile(t, ws, "keep.md", "markdown body")
	writeIngestFile(t, ws, "skip.txt", "text body")
	kb := &recordingIngestor{}
	s := NewKnowledgeIngestPathSkill(kb, ws)

	if _, err := s.Execute(context.Background(), map[string]any{"path": ".", "glob": "*.md"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(kb.calls) != 1 || kb.calls[0].title != "keep.md" {
		t.Fatalf("glob *.md should ingest only keep.md, got %+v", kb.calls)
	}
}

func TestIngestPath_EmptyMatch_Errors(t *testing.T) {
	ws := t.TempDir()
	writeIngestFile(t, ws, "a.txt", "x")
	s := NewKnowledgeIngestPathSkill(&recordingIngestor{}, ws)
	if _, err := s.Execute(context.Background(), map[string]any{"path": ".", "glob": "*.md"}); err == nil {
		t.Fatalf("no glob match must error loudly, not silently succeed")
	}
}

func TestIngestPath_TooMany_Errors(t *testing.T) {
	ws := t.TempDir()
	for i := 0; i < maxIngestFiles+5; i++ {
		writeIngestFile(t, ws, fmt.Sprintf("f%03d.md", i), "body")
	}
	kb := &recordingIngestor{}
	s := NewKnowledgeIngestPathSkill(kb, ws)
	if _, err := s.Execute(context.Background(), map[string]any{"path": "."}); err == nil {
		t.Fatalf("exceeding maxIngestFiles must error before ingesting")
	}
	if len(kb.calls) != 0 {
		t.Fatalf("must reject the batch atomically, ingested %d", len(kb.calls))
	}
}

func TestIngestPath_NonRecursiveByDefault(t *testing.T) {
	ws := t.TempDir()
	writeIngestFile(t, ws, "top.md", "top body")
	writeIngestFile(t, ws, "sub/nested.md", "nested body")
	kb := &recordingIngestor{}
	s := NewKnowledgeIngestPathSkill(kb, ws)

	if _, err := s.Execute(context.Background(), map[string]any{"path": "."}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(kb.calls) != 1 || kb.calls[0].title != "top.md" {
		t.Fatalf("default must not recurse, got %+v", kb.calls)
	}

	kb2 := &recordingIngestor{}
	s2 := NewKnowledgeIngestPathSkill(kb2, ws)
	if _, err := s2.Execute(context.Background(), map[string]any{"path": ".", "recursive": true}); err != nil {
		t.Fatalf("Execute recursive: %v", err)
	}
	if len(kb2.calls) != 2 {
		t.Fatalf("recursive should ingest both files, got %d", len(kb2.calls))
	}
}
