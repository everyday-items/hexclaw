package builtin

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
)

// GrepSkill provides recursive text/regex search in directories.
//
// All paths are restricted to the configured workspace directory.
// Designed to mirror Claude Code's Grep tool: search file contents
// by pattern, returning matching lines with file paths and line numbers.
type GrepSkill struct {
	workspace string
}

func NewGrepSkill(workspace string) *GrepSkill {
	return &GrepSkill{workspace: workspace}
}

func (s *GrepSkill) Name() string        { return "grep" }
func (s *GrepSkill) Description() string { return "Search file contents by text or regex pattern" }
func (s *GrepSkill) Match(_ string) bool { return false }

func (s *GrepSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("grep",
		"Search file contents recursively by text or regex pattern. "+
			"Returns matching lines with file paths and line numbers. "+
			"Paths are relative to workspace or absolute within workspace.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"pattern":     {Type: "string", Description: "Text or regex pattern to search for"},
				"path":        {Type: "string", Description: "Directory or file to search in (relative to workspace, default: workspace root)"},
				"glob":        {Type: "string", Description: "File pattern filter, e.g. \"*.go\", \"*.ts\" (default: all files)"},
				"max_results": {Type: "integer", Description: "Maximum number of matching lines to return (default: 100)"},
			},
			Required: []string{"pattern"},
		})
}

// skipDirs are directories to skip during recursive search.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	".next":        true,
	"dist":         true,
	"build":        true,
	"__pycache__":  true,
	".idea":        true,
	".vscode":      true,
	"target":       true,
}

func (s *GrepSkill) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}

	rawPath, _ := args["path"].(string)
	searchPath, err := resolveSafePath(s.workspace, rawPath)
	if err != nil {
		return nil, fmt.Errorf("path rejected: %w", err)
	}

	fileGlob, _ := args["glob"].(string)
	maxResults := intArg(args, "max_results", 100)

	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex pattern %q: %w", pattern, err)
	}

	info, err := os.Stat(searchPath)
	if err != nil {
		return nil, fmt.Errorf("path %q not accessible: %w", searchPath, err)
	}

	var matches []string
	matchCount := 0

	if !info.IsDir() {
		// Single file search
		matches, matchCount = grepFile(searchPath, re, maxResults)
	} else {
		// Recursive directory search
		filepath.WalkDir(searchPath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // skip inaccessible entries
			}
			if d.IsDir() {
				if skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if matchCount >= maxResults {
				return filepath.SkipAll
			}

			// Apply glob filter
			if fileGlob != "" {
				matched, _ := filepath.Match(fileGlob, d.Name())
				if !matched {
					return nil
				}
			}

			// Skip binary/large files
			if isBinaryFilename(d.Name()) {
				return nil
			}
			fi, err := d.Info()
			if err != nil || fi.Size() > 2*1024*1024 { // skip files > 2MB
				return nil
			}

			fileMatches, count := grepFile(path, re, maxResults-matchCount)
			matches = append(matches, fileMatches...)
			matchCount += count
			return nil
		})
	}

	if len(matches) == 0 {
		return &skill.Result{
			Content: fmt.Sprintf("No matches found for pattern %q in %s", pattern, searchPath),
		}, nil
	}

	var sb strings.Builder
	for _, m := range matches {
		sb.WriteString(m)
		sb.WriteByte('\n')
	}

	suffix := ""
	if matchCount >= maxResults {
		suffix = fmt.Sprintf("\n(truncated at %d results, use max_results to increase)", maxResults)
	}

	return &skill.Result{
		Content: fmt.Sprintf("Found %d matches for %q:\n\n%s%s", matchCount, pattern, sb.String(), suffix),
	}, nil
}

// grepFile searches a single file and returns formatted match lines.
func grepFile(path string, re *regexp.Regexp, limit int) ([]string, int) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0
	}
	defer f.Close()

	var matches []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024) // handle long lines
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if re.MatchString(line) {
			matches = append(matches, fmt.Sprintf("%s:%d:%s", path, lineNum, line))
			if len(matches) >= limit {
				break
			}
		}
	}
	return matches, len(matches)
}

// isBinaryFilename checks common binary file extensions.
func isBinaryFilename(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".exe", ".dll", ".so", ".dylib", ".bin", ".o", ".a",
		".png", ".jpg", ".jpeg", ".gif", ".bmp", ".ico", ".webp",
		".mp3", ".mp4", ".avi", ".mov", ".wav", ".flac",
		".zip", ".tar", ".gz", ".bz2", ".xz", ".7z", ".rar", ".zst",
		".pdf", ".doc", ".docx", ".xls", ".xlsx",
		".wasm", ".pyc", ".class",
		".ttf", ".otf", ".woff", ".woff2",
		".sqlite", ".db":
		return true
	}
	return false
}
