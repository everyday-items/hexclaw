// Package chain provides Skill Chain engine for sequential multi-skill execution.
//
// A Chain is an ordered list of steps. Each step calls a Skill with arguments
// that can reference previous step outputs via $prev.result or $user_input.
package chain

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/skill"
)

// Step defines a single step in a Skill Chain.
type Step struct {
	Skill string         `yaml:"skill" json:"skill"` // Skill name to call
	Input map[string]any `yaml:"input" json:"input"` // Arguments, supports $prev.result and $user_input
}

// ChainDef defines a Skill Chain.
type ChainDef struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
	Steps       []Step `yaml:"steps" json:"steps"`
}

// StepResult holds the output of a single step.
type StepResult struct {
	StepIndex int    `json:"step"`
	Skill     string `json:"skill"`
	Output    string `json:"output"`
	Error     string `json:"error,omitempty"`
}

// Executor runs Skill Chains by dispatching steps to a skill executor function.
type Executor struct {
	execFn func(ctx context.Context, skillName string, args map[string]any) (*skill.Result, error)
}

// NewExecutor creates a Chain executor.
// execFn is called for each step to execute the target skill.
func NewExecutor(execFn func(ctx context.Context, skillName string, args map[string]any) (*skill.Result, error)) *Executor {
	return &Executor{execFn: execFn}
}

// Run executes a chain sequentially. Returns combined output and per-step results.
func (e *Executor) Run(ctx context.Context, def *ChainDef, userInput string) (string, []StepResult, error) {
	if len(def.Steps) == 0 {
		return "", nil, fmt.Errorf("chain '%s' has no steps", def.Name)
	}

	var results []StepResult
	prevResult := ""

	for i, step := range def.Steps {
		select {
		case <-ctx.Done():
			return buildPartialOutput(def.Name, results, i, len(def.Steps)), results, ctx.Err()
		default:
		}

		// Resolve input variables
		args := resolveArgs(step.Input, userInput, prevResult)

		result, err := e.execFn(ctx, step.Skill, args)

		sr := StepResult{StepIndex: i, Skill: step.Skill}
		if err != nil {
			sr.Error = err.Error()
			results = append(results, sr)
			// Chain breaks on first error
			return buildPartialOutput(def.Name, results, i, len(def.Steps)), results,
				fmt.Errorf("chain '%s' step %d (%s) failed: %w", def.Name, i+1, step.Skill, err)
		}

		sr.Output = result.Content
		results = append(results, sr)
		prevResult = result.Content
	}

	return buildOutput(def.Name, results), results, nil
}

// resolveArgs replaces $user_input and $prev.result placeholders in step arguments.
func resolveArgs(input map[string]any, userInput, prevResult string) map[string]any {
	resolved := make(map[string]any, len(input))
	for k, v := range input {
		switch s := v.(type) {
		case string:
			s = strings.ReplaceAll(s, "$user_input", userInput)
			s = strings.ReplaceAll(s, "$prev.result", prevResult)
			resolved[k] = s
		default:
			resolved[k] = v
		}
	}
	return resolved
}

func buildOutput(chainName string, results []StepResult) string {
	if len(results) == 0 {
		return ""
	}
	// Return the last step's output as the chain result
	last := results[len(results)-1]
	if len(results) == 1 {
		return last.Output
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[Chain '%s' completed %d steps]\n\n", chainName, len(results)))
	sb.WriteString(last.Output)
	return sb.String()
}

func buildPartialOutput(chainName string, results []StepResult, failedAt, total int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[Chain '%s': %d/%d steps completed]\n\n", chainName, len(results), total))
	for _, r := range results {
		if r.Error != "" {
			sb.WriteString(fmt.Sprintf("Step %d (%s): ERROR: %s\n", r.StepIndex+1, r.Skill, r.Error))
		} else {
			sb.WriteString(fmt.Sprintf("Step %d (%s): OK\n", r.StepIndex+1, r.Skill))
		}
	}
	return sb.String()
}
