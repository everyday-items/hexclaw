package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
)

const (
	maxFileAccessReadBytes = 2 * 1024 * 1024
	maxFileAccessEntries   = 1000
)

type FileAccessBroker struct {
	mu          sync.RWMutex
	allowedDirs []string
}

type fileAccessPolicy struct {
	allowedDirs []string
}

func NewFileAccessBroker(paths []string) *FileAccessBroker {
	return newFileAccessBrokerFromPolicy(prepareFileAccessPolicy(paths))
}

// prepareFileAccessPolicy 在事务 Prepare 阶段完成规范化和路径解析。
func prepareFileAccessPolicy(paths []string) fileAccessPolicy {
	return fileAccessPolicy{allowedDirs: normalizeAllowedPaths(paths)}
}

func newFileAccessBrokerFromPolicy(policy fileAccessPolicy) *FileAccessBroker {
	return &FileAccessBroker{allowedDirs: policy.allowedDirs}
}

// commitFileAccessPolicy 只交换已经准备完成的不可变策略，不执行文件系统探测。
func (b *FileAccessBroker) commitFileAccessPolicy(policy fileAccessPolicy) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.allowedDirs = policy.allowedDirs
}

func (b *FileAccessBroker) AllowedDirectories() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]string(nil), b.allowedDirs...)
}

func (b *FileAccessBroker) ListDirectory(path string) (string, error) {
	resolved, err := b.authorizeExisting(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat failed: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory: %s", path)
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		return "", fmt.Errorf("list directory failed: %w", err)
	}
	if len(entries) > maxFileAccessEntries {
		entries = entries[:maxFileAccessEntries]
	}

	var sb strings.Builder
	for _, entry := range entries {
		if entry.IsDir() {
			sb.WriteString("[DIR] ")
		} else {
			sb.WriteString("[FILE] ")
		}
		sb.WriteString(entry.Name())
		sb.WriteByte('\n')
	}
	if len(entries) == maxFileAccessEntries {
		sb.WriteString(fmt.Sprintf("(truncated at %d entries)\n", maxFileAccessEntries))
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

func (b *FileAccessBroker) ReadFile(path string, offset, limit int) (string, error) {
	resolved, err := b.authorizeExisting(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat failed: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("path is a directory: %s", path)
	}
	if info.Size() > maxFileAccessReadBytes {
		return "", fmt.Errorf("file too large: %d bytes exceeds %d bytes", info.Size(), maxFileAccessReadBytes)
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("read file failed: %w", err)
	}
	text := string(data)
	if limit <= 0 {
		return text, nil
	}

	lines := strings.Split(text, "\n")
	if offset < 0 {
		offset = 0
	}
	if offset >= len(lines) {
		return "", nil
	}
	end := offset + limit
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[offset:end], "\n"), nil
}

func (b *FileAccessBroker) authorizeExisting(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path is required")
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}
	target, err = filepath.EvalSymlinks(filepath.Clean(target))
	if err != nil {
		return "", fmt.Errorf("path not accessible: %w", err)
	}

	b.mu.RLock()
	allowed := append([]string(nil), b.allowedDirs...)
	b.mu.RUnlock()
	for _, root := range allowed {
		resolvedRoot := root
		if r, err := filepath.EvalSymlinks(root); err == nil {
			resolvedRoot = r
		}
		if isPathInside(resolvedRoot, target) {
			return target, nil
		}
	}
	return "", fmt.Errorf("access denied - path outside allowed directories: %s", path)
}

func normalizeAllowedPaths(paths []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, path := range paths {
		p := strings.TrimSpace(path)
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		if _, ok := seen[abs]; ok {
			continue
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
	}
	sort.Strings(out)
	return out
}

func isPathInside(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	if root == target {
		return true
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

type ListDirectorySkill struct {
	broker *FileAccessBroker
}

func NewListDirectorySkill(broker *FileAccessBroker) *ListDirectorySkill {
	return &ListDirectorySkill{broker: broker}
}

func (s *ListDirectorySkill) Name() string { return "list_directory" }
func (s *ListDirectorySkill) Description() string {
	return "List files in a HexClaw-authorized local directory"
}
func (s *ListDirectorySkill) Match(_ string) bool { return false }

func (s *ListDirectorySkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("list_directory", "List files in a local directory authorized in HexClaw data connectors. Access is enforced by HexClaw, not by external MCP server permissions.", &llm.Schema{
		Type: "object",
		Properties: map[string]*llm.Schema{
			"path": {Type: "string", Description: "Absolute path of the authorized directory to list"},
		},
		Required: []string{"path"},
	})
}

func (s *ListDirectorySkill) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	path, _ := args["path"].(string)
	content, err := s.broker.ListDirectory(path)
	if err != nil {
		return nil, err
	}
	return &skill.Result{Content: content}, nil
}

type ReadFileSkill struct {
	broker *FileAccessBroker
}

func NewReadFileSkill(broker *FileAccessBroker) *ReadFileSkill {
	return &ReadFileSkill{broker: broker}
}

func (s *ReadFileSkill) Name() string { return "read_file" }
func (s *ReadFileSkill) Description() string {
	return "Read a file from a HexClaw-authorized local directory"
}
func (s *ReadFileSkill) Match(_ string) bool { return false }

func (s *ReadFileSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("read_file", "Read a file under a local directory authorized in HexClaw data connectors. Access is enforced by HexClaw, not by external MCP server permissions.", &llm.Schema{
		Type: "object",
		Properties: map[string]*llm.Schema{
			"path":   {Type: "string", Description: "Absolute path of the authorized file to read"},
			"offset": {Type: "integer", Description: "Optional zero-based line offset"},
			"limit":  {Type: "integer", Description: "Optional max number of lines to return"},
		},
		Required: []string{"path"},
	})
}

func (s *ReadFileSkill) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	path, _ := args["path"].(string)
	content, err := s.broker.ReadFile(path, intArg(args, "offset", 0), intArg(args, "limit", 0))
	if err != nil {
		return nil, err
	}
	return &skill.Result{Content: content}, nil
}

type ListAllowedDirectoriesSkill struct {
	broker *FileAccessBroker
}

func NewListAllowedDirectoriesSkill(broker *FileAccessBroker) *ListAllowedDirectoriesSkill {
	return &ListAllowedDirectoriesSkill{broker: broker}
}

func (s *ListAllowedDirectoriesSkill) Name() string { return "list_allowed_directories" }
func (s *ListAllowedDirectoriesSkill) Description() string {
	return "List local directories authorized in HexClaw"
}
func (s *ListAllowedDirectoriesSkill) Match(_ string) bool { return false }

func (s *ListAllowedDirectoriesSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("list_allowed_directories", "List local directories currently authorized in HexClaw. This is HexClaw's grant store, not external MCP server state.", &llm.Schema{
		Type:       "object",
		Properties: map[string]*llm.Schema{},
	})
}

func (s *ListAllowedDirectoriesSkill) Execute(_ context.Context, _ map[string]any) (*skill.Result, error) {
	dirs := s.broker.AllowedDirectories()
	if len(dirs) == 0 {
		return &skill.Result{Content: "Allowed directories:"}, nil
	}
	return &skill.Result{Content: "Allowed directories:\n" + strings.Join(dirs, "\n")}, nil
}
