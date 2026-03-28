package chain

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/skill"
)

func mockExecFn(_ context.Context, skillName string, args map[string]any) (*skill.Result, error) {
	switch skillName {
	case "search":
		query, _ := args["query"].(string)
		return &skill.Result{Content: "Search results for: " + query}, nil
	case "summary":
		text, _ := args["text"].(string)
		return &skill.Result{Content: "Summary: " + text[:min(50, len(text))]}, nil
	case "failing":
		return nil, fmt.Errorf("skill failed")
	default:
		return &skill.Result{Content: "executed " + skillName}, nil
	}
}

func TestChain_BasicExecution(t *testing.T) {
	executor := NewExecutor(mockExecFn)
	def := &ChainDef{
		Name:        "test-chain",
		Description: "test",
		Steps: []Step{
			{Skill: "search", Input: map[string]any{"query": "$user_input"}},
			{Skill: "summary", Input: map[string]any{"text": "$prev.result"}},
		},
	}

	output, results, err := executor.Run(context.Background(), def, "weather in Beijing")
	if err != nil {
		t.Fatalf("chain failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Skill != "search" || results[1].Skill != "summary" {
		t.Fatalf("wrong skill order: %v", results)
	}
	if output == "" {
		t.Fatal("expected output")
	}
	t.Logf("chain output: %s", output)
}

func TestChain_StepFailure(t *testing.T) {
	executor := NewExecutor(mockExecFn)
	def := &ChainDef{
		Name: "fail-chain",
		Steps: []Step{
			{Skill: "search", Input: map[string]any{"query": "test"}},
			{Skill: "failing", Input: map[string]any{}},
			{Skill: "summary", Input: map[string]any{"text": "$prev.result"}},
		},
	}

	_, results, err := executor.Run(context.Background(), def, "test")
	if err == nil {
		t.Fatal("expected error on failing step")
	}
	if len(results) != 2 { // search OK + failing ERROR
		t.Fatalf("expected 2 results (1 ok + 1 error), got %d", len(results))
	}
}

func TestChain_VariableResolution(t *testing.T) {
	executor := NewExecutor(func(_ context.Context, name string, args map[string]any) (*skill.Result, error) {
		query, _ := args["query"].(string)
		return &skill.Result{Content: query}, nil
	})
	def := &ChainDef{
		Name: "var-chain",
		Steps: []Step{
			{Skill: "step1", Input: map[string]any{"query": "user said: $user_input"}},
			{Skill: "step2", Input: map[string]any{"query": "prev was: $prev.result"}},
		},
	}

	_, results, err := executor.Run(context.Background(), def, "hello")
	if err != nil {
		t.Fatalf("chain failed: %v", err)
	}
	if results[0].Output != "user said: hello" {
		t.Fatalf("step1 variable not resolved: %q", results[0].Output)
	}
	if results[1].Output != "prev was: user said: hello" {
		t.Fatalf("step2 variable not resolved: %q", results[1].Output)
	}
}

func TestChainSkill_AsSkill(t *testing.T) {
	executor := NewExecutor(mockExecFn)
	def := &ChainDef{
		Name:        "search-summarize",
		Description: "Search then summarize",
		Steps: []Step{
			{Skill: "search", Input: map[string]any{"query": "$user_input"}},
			{Skill: "summary", Input: map[string]any{"text": "$prev.result"}},
		},
	}

	cs := NewChainSkill(def, executor)

	if cs.Name() != "search-summarize" {
		t.Fatalf("wrong name: %s", cs.Name())
	}

	result, err := cs.Execute(context.Background(), map[string]any{"input": "AI news"})
	if err != nil {
		t.Fatalf("chain skill failed: %v", err)
	}
	if result.Content == "" {
		t.Fatal("expected output")
	}
}

func TestLoader_LoadFile(t *testing.T) {
	dir := t.TempDir()
	content := `name: test-chain
description: A test chain
steps:
  - skill: search
    input:
      query: "$user_input"
  - skill: summary
    input:
      text: "$prev.result"
`
	path := filepath.Join(dir, "test.yaml")
	os.WriteFile(path, []byte(content), 0644)

	def, err := LoadFile(path)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if def.Name != "test-chain" {
		t.Fatalf("wrong name: %s", def.Name)
	}
	if len(def.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(def.Steps))
	}
}

func TestLoader_LoadDir(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{"chain1.yaml", "chain2.yml", "readme.md"} {
		content := fmt.Sprintf("name: %s\nsteps:\n  - skill: search\n    input:\n      query: test\n", name)
		os.WriteFile(filepath.Join(dir, name), []byte(content), 0644)
	}

	chains, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("load dir failed: %v", err)
	}
	if len(chains) != 2 { // only .yaml and .yml, not .md
		t.Fatalf("expected 2 chains, got %d", len(chains))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
