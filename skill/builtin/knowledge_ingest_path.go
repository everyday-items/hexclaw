package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
)

// maxIngestFiles caps how many files a single path-ingest reads — a guard against
// a huge directory exploding token / IO cost.
const maxIngestFiles = 200

// maxIngestFileBytes skips individual files larger than this (binary blobs / huge logs).
const maxIngestFileBytes = 2 << 20 // 2 MiB

// KnowledgeIngestPathSkill reads EACH file under a path/glob and ingests its real
// CONTENT into the knowledge base — one AddDocument per file.
//
// The model supplies only a PATH; the code reads each file's body itself, so the
// KB archives document contents rather than a filename listing.
type KnowledgeIngestPathSkill struct {
	kb        KnowledgeIngestor // AddDocument(ctx,title,content,source) — knowledge.go
	workspace string            // resolveSafePath boundary (.. / symlink escape rejected)
}

func NewKnowledgeIngestPathSkill(kb KnowledgeIngestor, workspace string) *KnowledgeIngestPathSkill {
	return &KnowledgeIngestPathSkill{kb: kb, workspace: workspace}
}

func (s *KnowledgeIngestPathSkill) Name() string { return "knowledge_ingest_path" }

func (s *KnowledgeIngestPathSkill) Description() string {
	return "Read files under a path (directory or glob) and ingest each file's content into the knowledge base"
}

// Match always defers to the LLM tool-calling path; no keyword fast path.
func (s *KnowledgeIngestPathSkill) Match(string) bool { return false }

func (s *KnowledgeIngestPathSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("knowledge_ingest_path",
		"Read files under a path (directory or glob) and ingest EACH file's CONTENT into the "+
			"knowledge base. To archive local documents pass the PATH here — do NOT paste a file "+
			"listing as content (that ingests filenames, not the documents).",
		&llm.Schema{Type: "object", Properties: map[string]*llm.Schema{
			"path":      {Type: "string", Description: "directory or file path (required)"},
			"glob":      {Type: "string", Description: `e.g. "*.md" (optional; matched on each file's name)`},
			"recursive": {Type: "boolean", Description: "recurse into subdirectories"},
			"source":    {Type: "string", Description: "source label, default = each file's path"},
		}, Required: []string{"path"}})
}

func (s *KnowledgeIngestPathSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	if s.kb == nil {
		return nil, fmt.Errorf("knowledge base is not enabled")
	}
	root, err := resolveSafePath(s.workspace, firstStringArg(args, "path"))
	if err != nil {
		return nil, err // .. / symlink escape rejected
	}

	files, err := s.collectFiles(root, firstStringArg(args, "glob"), argBool(args, "recursive"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no files matched under %s", root)
	}
	if len(files) > maxIngestFiles {
		return nil, fmt.Errorf("too many files (%d > %d); narrow the path or glob", len(files), maxIngestFiles)
	}

	source := firstStringArg(args, "source")
	ok, failed := 0, 0
	for _, f := range files {
		content, rerr := readFileAsText(f)
		if rerr != nil || strings.TrimSpace(content) == "" {
			failed++
			continue
		}
		src := source
		if src == "" {
			src = f
		}
		// title = file name, content = THIS file's real body (never a filename
		// listing). One AddDocument per file.
		if _, aerr := s.kb.AddDocument(ctx, filepath.Base(f), content, src); aerr != nil {
			failed++
			continue
		}
		ok++
	}
	return &skill.Result{
		Content: fmt.Sprintf("Ingested %d/%d files from %s (%d failed)", ok, len(files), root, failed),
		Metadata: map[string]string{
			"ingested": strconv.Itoa(ok),
			"failed":   strconv.Itoa(failed),
		},
	}, nil
}

// collectFiles enumerates regular files under root (a file or directory),
// honoring an optional name glob and the recursion flag. Every candidate is
// re-checked through resolveSafePath so a symlinked entry can't escape.
func (s *KnowledgeIngestPathSkill) collectFiles(root, glob string, recursive bool) ([]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("path not accessible: %w", err)
	}
	if !info.IsDir() {
		if matchGlob(glob, filepath.Base(root)) {
			return []string{root}, nil
		}
		return nil, nil
	}

	var files []string
	walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if p != root && !recursive {
				return filepath.SkipDir
			}
			return nil
		}
		if !matchGlob(glob, d.Name()) {
			return nil
		}
		if _, serr := resolveSafePath(s.workspace, p); serr != nil {
			return nil // symlink/.. escape — skip
		}
		files = append(files, p)
		if len(files) > maxIngestFiles {
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return files, nil
}

// matchGlob reports whether name matches glob ("" matches everything).
func matchGlob(glob, name string) bool {
	if glob == "" {
		return true
	}
	ok, err := filepath.Match(glob, name)
	return err == nil && ok
}

// readFileAsText reads a file as UTF-8 text, rejecting oversized or non-text files.
func readFileAsText(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() > maxIngestFileBytes {
		return "", fmt.Errorf("file too large: %d bytes", info.Size())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("not valid UTF-8 text")
	}
	return string(b), nil
}

// argBool coerces a tool arg to bool (handles JSON bool and "true"/"1" strings).
func argBool(args map[string]any, key string) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		b, _ := strconv.ParseBool(strings.TrimSpace(v))
		return b
	}
	return false
}
