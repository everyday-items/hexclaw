package builtin

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
)

// FileEditSkill provides precise text replacement in files.
//
// All paths are restricted to the configured workspace directory.
// Designed to mirror Claude Code's Edit tool: read the file first,
// then use file_edit to replace an exact string occurrence.
// old_string must match exactly once to prevent ambiguous edits.
type FileEditSkill struct {
	workspace string
}

func NewFileEditSkill(workspace string) *FileEditSkill {
	return &FileEditSkill{workspace: workspace}
}

func (s *FileEditSkill) Name() string        { return "file_edit" }
func (s *FileEditSkill) Description() string { return "Replace exact text in a file in the workspace" }
func (s *FileEditSkill) Match(_ string) bool { return false }

func (s *FileEditSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("file_edit",
		"Replace exact text in a file. Read the file first to see current content, "+
			"then use this tool to make precise edits. old_string must match exactly once. "+
			"Paths are relative to workspace or absolute within workspace.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"file_path":  {Type: "string", Description: "Path to the file (relative to workspace, or absolute within workspace)"},
				"old_string": {Type: "string", Description: "Exact text to find and replace (must appear exactly once)"},
				"new_string": {Type: "string", Description: "Replacement text"},
			},
			Required: []string{"file_path", "old_string", "new_string"},
		})
}

func (s *FileEditSkill) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	rawPath, _ := args["file_path"].(string)
	oldStr, _ := args["old_string"].(string)
	newStr, _ := args["new_string"].(string)

	if rawPath == "" {
		return nil, fmt.Errorf("file_path is required")
	}
	if oldStr == "" {
		return nil, fmt.Errorf("old_string is required")
	}
	if oldStr == newStr {
		return nil, fmt.Errorf("old_string and new_string are identical, no change needed")
	}

	filePath, err := resolveSafePath(s.workspace, rawPath)
	if err != nil {
		return nil, fmt.Errorf("path rejected: %w", err)
	}

	// 检查文件大小，防止 OOM
	fi, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("cannot stat %s: %w", filePath, err)
	}
	const maxFileSize = 10 * 1024 * 1024 // 10MB
	if fi.Size() > maxFileSize {
		return nil, fmt.Errorf("file %s is too large (%d bytes, max %d). Use a different approach for large files", filePath, fi.Size(), maxFileSize)
	}
	origPerm := fi.Mode().Perm()

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", filePath, err)
	}

	text := string(data)
	count := strings.Count(text, oldStr)
	switch count {
	case 0:
		lines := strings.Split(text, "\n")
		hint := fmt.Sprintf("old_string not found in %s (%d lines). ", filePath, len(lines))
		if len(oldStr) > 200 {
			hint += "The old_string is very long — try a shorter, unique snippet."
		} else {
			hint += "Check for whitespace, indentation, or encoding differences."
		}
		return nil, fmt.Errorf("%s", hint)
	case 1:
		// Exactly one match — proceed
	default:
		return nil, fmt.Errorf("old_string matches %d times in %s (must match exactly once). Use a larger context to make it unique", count, filePath)
	}

	result := strings.Replace(text, oldStr, newStr, 1)
	if err := os.WriteFile(filePath, []byte(result), origPerm); err != nil {
		return nil, fmt.Errorf("write failed: %w", err)
	}

	return &skill.Result{
		Content: fmt.Sprintf("Replaced 1 occurrence in %s", filePath),
	}, nil
}
