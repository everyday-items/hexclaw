package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
)

// FileOpsSkill provides read/write/edit file operations for the LLM agent.
//
// All paths are restricted to the configured workspace directory.
// Symlink traversal outside workspace is blocked.
type FileOpsSkill struct {
	workspace string // allowed base directory
}

// NewFileOpsSkill creates a file operations skill.
// workspace is the root directory the agent may operate on (e.g. /tmp/hexclaw-workspace).
func NewFileOpsSkill(workspace string) *FileOpsSkill {
	return &FileOpsSkill{workspace: workspace}
}

func (s *FileOpsSkill) Name() string        { return "file_ops" }
func (s *FileOpsSkill) Description() string  { return "Read, write, or edit files in the workspace" }
func (s *FileOpsSkill) Match(_ string) bool  { return false }

func (s *FileOpsSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("file_ops",
		"Read, write, or edit files in the workspace. Actions: read (with optional offset/limit), write (create/overwrite), edit (precise string replacement).",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"action":     {Type: "string", Description: "Action: read, write, edit"},
				"path":       {Type: "string", Description: "File path relative to workspace"},
				"content":    {Type: "string", Description: "File content (for write) or new_string (for edit)"},
				"old_string": {Type: "string", Description: "String to replace (for edit, must match exactly once)"},
				"offset":     {Type: "integer", Description: "Start line (for read, 1-based, default 1)"},
				"limit":      {Type: "integer", Description: "Max lines to read (default 100)"},
			},
			Required: []string{"action", "path"},
		})
}

func (s *FileOpsSkill) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	action, _ := args["action"].(string)
	relPath, _ := args["path"].(string)

	if relPath == "" {
		return nil, fmt.Errorf("path is required")
	}

	absPath, err := s.safePath(relPath)
	if err != nil {
		return nil, err
	}

	switch action {
	case "read":
		return s.doRead(absPath, args)
	case "write":
		content, _ := args["content"].(string)
		return s.doWrite(absPath, content)
	case "edit":
		oldStr, _ := args["old_string"].(string)
		newStr, _ := args["content"].(string)
		return s.doEdit(absPath, oldStr, newStr)
	default:
		return nil, fmt.Errorf("unknown action %q (use read, write, edit)", action)
	}
}

func (s *FileOpsSkill) safePath(relPath string) (string, error) {
	// Block absolute paths and traversal
	if filepath.IsAbs(relPath) || strings.Contains(relPath, "..") {
		return "", fmt.Errorf("path must be relative and cannot contain '..'")
	}
	abs := filepath.Join(s.workspace, relPath)
	resolved, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("failed to resolve path: %w", err)
	}
	if err == nil {
		wsResolved, _ := filepath.EvalSymlinks(s.workspace)
		if !strings.HasPrefix(resolved, wsResolved) {
			return "", fmt.Errorf("path escapes workspace (symlink?)")
		}
	}
	return abs, nil
}

func (s *FileOpsSkill) doRead(absPath string, args map[string]any) (*skill.Result, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("read failed: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	offset := intArg(args, "offset", 1) - 1 // 1-based to 0-based
	limit := intArg(args, "limit", 100)

	if offset < 0 {
		offset = 0
	}
	if offset >= len(lines) {
		return &skill.Result{Content: "(empty: offset beyond file end)"}, nil
	}
	end := offset + limit
	if end > len(lines) {
		end = len(lines)
	}

	var sb strings.Builder
	for i := offset; i < end; i++ {
		sb.WriteString(fmt.Sprintf("%d\t%s\n", i+1, lines[i]))
	}
	return &skill.Result{
		Content: sb.String(),
		Metadata: map[string]string{
			"path":        absPath,
			"total_lines": fmt.Sprintf("%d", len(lines)),
		},
	}, nil
}

func (s *FileOpsSkill) doWrite(absPath, content string) (*skill.Result, error) {
	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create directory failed: %w", err)
	}
	if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
		return nil, fmt.Errorf("write failed: %w", err)
	}
	return &skill.Result{
		Content: fmt.Sprintf("Written %d bytes to %s", len(content), absPath),
	}, nil
}

func (s *FileOpsSkill) doEdit(absPath, oldStr, newStr string) (*skill.Result, error) {
	if oldStr == "" {
		return nil, fmt.Errorf("old_string is required for edit")
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("read failed: %w", err)
	}
	text := string(data)
	count := strings.Count(text, oldStr)
	if count == 0 {
		return nil, fmt.Errorf("old_string not found in file")
	}
	if count > 1 {
		return nil, fmt.Errorf("old_string matches %d times (must match exactly once)", count)
	}
	result := strings.Replace(text, oldStr, newStr, 1)
	if err := os.WriteFile(absPath, []byte(result), 0644); err != nil {
		return nil, fmt.Errorf("write failed: %w", err)
	}
	return &skill.Result{
		Content: fmt.Sprintf("Replaced 1 occurrence in %s", absPath),
	}, nil
}

func intArg(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return def
	}
}
