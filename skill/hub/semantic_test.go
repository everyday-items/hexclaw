package hub

import (
	"testing"
)

func TestSemanticSearch_BasicMatch(t *testing.T) {
	skills := []SkillMeta{
		{Name: "csv-analyzer", Description: "Analyze CSV files and generate statistics", Tags: []string{"data", "csv", "pandas"}},
		{Name: "web-search", Description: "Search the web using DuckDuckGo", Tags: []string{"search", "web"}},
		{Name: "weather", Description: "Get current weather for a location", Tags: []string{"weather", "forecast"}},
		{Name: "translate", Description: "Translate text between languages", Tags: []string{"language", "translate"}},
		{Name: "code-runner", Description: "Execute Python and JavaScript code", Tags: []string{"code", "python", "javascript"}},
	}

	s := NewSemanticSearch()
	s.Index(skills)

	tests := []struct {
		query    string
		expected string // should be in top results
	}{
		{"analyze csv data statistics", "csv-analyzer"},
		{"search the web using DuckDuckGo", "web-search"},
		{"weather forecast location", "weather"},
		{"translate text language", "translate"},
		{"execute python code", "code-runner"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			results := s.Search(tt.query, 3)
			if len(results) == 0 {
				t.Fatalf("no results for %q", tt.query)
			}
			found := false
			for _, r := range results {
				if r == tt.expected {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected %q in results for %q, got %v", tt.expected, tt.query, results)
			}
		})
	}
}

func TestSemanticSearch_EmptyIndex(t *testing.T) {
	s := NewSemanticSearch()
	results := s.Search("test", 5)
	if len(results) != 0 {
		t.Fatalf("expected empty results, got %v", results)
	}
}

func TestSemanticSearch_Ranking(t *testing.T) {
	skills := []SkillMeta{
		{Name: "github", Description: "GitHub API operations: create issues, pull requests, repositories"},
		{Name: "git", Description: "Local git operations: commit, push, pull, diff"},
		{Name: "web-search", Description: "Search the web"},
	}

	s := NewSemanticSearch()
	s.Index(skills)

	results := s.Search("create github issue", 3)
	if len(results) == 0 {
		t.Fatal("no results")
	}
	// github should rank higher than git for "github issue" query
	if results[0] != "github" {
		t.Errorf("expected 'github' first, got %v", results)
	}
}
