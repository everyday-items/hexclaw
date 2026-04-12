package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
)

// GlobSkill provides file pattern matching in directories.
//
// All paths are restricted to the configured workspace directory.
// Designed to mirror Claude Code's Glob tool: find files by name pattern,
// returning sorted file paths.
type GlobSkill struct {
	workspace string
}

func NewGlobSkill(workspace string) *GlobSkill {
	return &GlobSkill{workspace: workspace}
}

func (s *GlobSkill) Name() string        { return "glob" }
func (s *GlobSkill) Description() string { return "Find files by name pattern" }
func (s *GlobSkill) Match(_ string) bool { return false }

func (s *GlobSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("glob",
		"Find files by glob pattern. Supports patterns like \"*.go\", \"**/*.ts\", \"src/**/*.tsx\". "+
			"Returns matching file paths sorted alphabetically. "+
			"Paths are relative to workspace or absolute within workspace.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"pattern":     {Type: "string", Description: "Glob pattern, e.g. \"**/*.go\", \"src/*.ts\", \"*.json\""},
				"path":        {Type: "string", Description: "Base directory to search in (relative to workspace, default: workspace root)"},
				"max_results": {Type: "integer", Description: "Maximum number of files to return (default: 200)"},
			},
			Required: []string{"pattern"},
		})
}

func (s *GlobSkill) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}

	rawPath, _ := args["path"].(string)
	basePath, err := resolveSafePath(s.workspace, rawPath)
	if err != nil {
		return nil, fmt.Errorf("path rejected: %w", err)
	}

	info, err := os.Stat(basePath)
	if err != nil {
		return nil, fmt.Errorf("path %q not accessible: %w", basePath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("path %q is not a directory", basePath)
	}

	maxResults := intArg(args, "max_results", 200)

	// Check if pattern uses ** (doublestar)
	useDoublestar := strings.Contains(pattern, "**")

	var matches []string

	if useDoublestar {
		// Walk directory and match each relative path
		filepath.WalkDir(basePath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if len(matches) >= maxResults {
				return filepath.SkipAll
			}

			rel, err := filepath.Rel(basePath, path)
			if err != nil {
				return nil
			}

			if doublestarMatch(pattern, rel) {
				matches = append(matches, path)
			}
			return nil
		})
	} else {
		// Simple glob — use filepath.Glob with basePath prefix
		fullPattern := filepath.Join(basePath, pattern)
		globMatches, err := filepath.Glob(fullPattern)
		if err != nil {
			return nil, fmt.Errorf("invalid glob pattern %q: %w", pattern, err)
		}

		// Filter out directories
		for _, m := range globMatches {
			if len(matches) >= maxResults {
				break
			}
			fi, err := os.Stat(m)
			if err != nil || fi.IsDir() {
				continue
			}
			matches = append(matches, m)
		}
	}

	sort.Strings(matches)

	if len(matches) == 0 {
		return &skill.Result{
			Content: fmt.Sprintf("No files found matching %q in %s", pattern, basePath),
		}, nil
	}

	var sb strings.Builder
	for _, m := range matches {
		sb.WriteString(m)
		sb.WriteByte('\n')
	}

	suffix := ""
	if len(matches) >= maxResults {
		suffix = fmt.Sprintf("\n(truncated at %d results)", maxResults)
	}

	return &skill.Result{
		Content: fmt.Sprintf("Found %d files matching %q:\n\n%s%s", len(matches), pattern, sb.String(), suffix),
	}, nil
}

// doublestarMatch implements ** glob matching.
// Pattern segments are matched against path segments.
// ** matches zero or more path segments.
func doublestarMatch(pattern, path string) bool {
	patParts := strings.Split(filepath.ToSlash(pattern), "/")
	pathParts := strings.Split(filepath.ToSlash(path), "/")
	return matchParts(patParts, pathParts)
}

func matchParts(pat, path []string) bool {
	for len(pat) > 0 && len(path) > 0 {
		if pat[0] == "**" {
			// ** matches zero or more path segments
			pat = pat[1:]
			if len(pat) == 0 {
				return true // trailing ** matches everything
			}
			// Try matching remaining pattern at every position
			for i := 0; i <= len(path); i++ {
				if matchParts(pat, path[i:]) {
					return true
				}
			}
			return false
		}

		matched, _ := filepath.Match(pat[0], path[0])
		if !matched {
			return false
		}
		pat = pat[1:]
		path = path[1:]
	}

	// Handle trailing **
	for len(pat) > 0 && pat[0] == "**" {
		pat = pat[1:]
	}

	return len(pat) == 0 && len(path) == 0
}
