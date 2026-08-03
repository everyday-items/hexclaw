package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/skill"
)

// syncChildToolPolicyCaptureProvider records the actual tool definitions sent by
// the synchronous Process path. It returns text immediately so no tool is run.
type syncChildToolPolicyCaptureProvider struct {
	mu       sync.Mutex
	requests []hexagon.CompletionRequest
}

func (*syncChildToolPolicyCaptureProvider) Name() string { return "sync-child-tool-policy" }

func (p *syncChildToolPolicyCaptureProvider) Complete(_ context.Context, req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	return &hexagon.CompletionResponse{Content: "ok"}, nil
}

func (*syncChildToolPolicyCaptureProvider) Stream(context.Context, hexagon.CompletionRequest) (*llm.Stream, error) {
	return nil, nil
}

func (*syncChildToolPolicyCaptureProvider) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{ID: "sync-child-tool-policy"}}
}

func (*syncChildToolPolicyCaptureProvider) CountTokens([]hexagon.Message) (int, error) { return 0, nil }

func (p *syncChildToolPolicyCaptureProvider) toolNamesAt(t *testing.T, index int) []string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if index >= len(p.requests) {
		t.Fatalf("provider requests=%d, want request at index %d", len(p.requests), index)
	}
	names := make([]string, 0, len(p.requests[index].Tools))
	for _, tool := range p.requests[index].Tools {
		names = append(names, tool.Function.Name)
	}
	return names
}

type syncChildToolPolicySkill struct{ name string }

func (s *syncChildToolPolicySkill) Name() string        { return s.name }
func (s *syncChildToolPolicySkill) Description() string { return "test tool " + s.name }
func (*syncChildToolPolicySkill) Match(string) bool     { return false }
func (*syncChildToolPolicySkill) Execute(context.Context, map[string]any) (*skill.Result, error) {
	return &skill.Result{Content: "unused"}, nil
}
func (s *syncChildToolPolicySkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition(s.name, s.Description(), &llm.Schema{Type: "object"})
}

func TestBUG20260803K12SyncChildToolPolicy(t *testing.T) {
	provider := &syncChildToolPolicyCaptureProvider{}
	eng := newEngineWithProviders(t,
		map[string]hexagon.Provider{"sync-child-tool-policy": provider},
		map[string]config.LLMProviderConfig{"sync-child-tool-policy": {Model: "test-model"}},
		"sync-child-tool-policy",
	)
	eng.cfg.LLM.Tools.Enabled = "on"

	registry := skill.NewRegistry()
	for _, name := range []string{codeExecToolName, "solve", "spawn_agent", "orchestrate", "transfer_to_agent", "ordinary_tool"} {
		if err := registry.Register(&syncChildToolPolicySkill{name: name}); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	eng.SetToolCollector(NewToolCollector(registry, nil, 40))

	// A normal root request has no inherited child policy and must retain the
	// ordinary collector behaviour.
	if _, err := eng.Process(context.Background(), &adapter.Message{
		ID:       "sync-policy-root",
		Platform: adapter.PlatformAPI,
		UserID:   "test-user",
		ChatID:   "test-chat",
		Content:  "Return a concise arithmetic answer.",
	}); err != nil {
		t.Fatalf("root Process: %v", err)
	}
	rootTools := syncToolNames(provider.toolNamesAt(t, 0))
	for _, required := range []string{codeExecToolName, "solve", "spawn_agent", "ordinary_tool"} {
		if !rootTools[required] {
			t.Fatalf("ordinary root unexpectedly lost %q: %v", required, rootTools)
		}
	}

	// These are the actual specs constructed for the K12 solver, verifier and
	// grader children before cmd/hexclaw sends them through synchronous
	// agentExecFn -> Process -> completeWithTools.  Checking all three guards
	// against a future role-local ToolAllow drift, not just the common helper.
	k12Specs := []struct {
		name string
		spec SubAgentSpec
	}{
		{name: solverAgentName, spec: solverSpec("2 + 2 = ?", "math", "", "", "")},
		{name: verifierAgentName, spec: verifierSpec("2 + 2 = ?", "4", "")},
		{name: graderAgentName, spec: graderSpec("2 + 2 = ?", "4", "4", "4")},
	}
	for index, tc := range k12Specs {
		t.Run(tc.name, func(t *testing.T) {
			child := &adapter.Message{
				ID:       "sync-policy-k12-" + tc.name,
				Platform: adapter.PlatformAPI,
				UserID:   "test-user",
				ChatID:   "test-chat",
				Content:  tc.spec.Task,
			}
			ApplySpecToMessage(child, tc.spec)
			if _, err := eng.Process(context.Background(), child); err != nil {
				t.Fatalf("K12 %s child Process: %v", tc.name, err)
			}
			childTools := provider.toolNamesAt(t, 1+index)
			if len(childTools) != 1 || childTools[0] != codeExecToolName {
				t.Fatalf("K12 synchronous %s tools=%v, want exactly [%s]", tc.name, childTools, codeExecToolName)
			}
		})
	}

	// Leaf recursion stripping is a separate shared invariant: a child with no
	// explicit allowlist still cannot receive orchestration tools at max depth.
	leaf := &adapter.Message{
		ID:       "sync-policy-leaf",
		Platform: adapter.PlatformAPI,
		UserID:   "test-user",
		ChatID:   "test-chat",
		Content:  "Return a concise arithmetic answer.",
	}
	ApplySpecToMessage(leaf, SubAgentSpec{
		RunID:  "sync-policy-leaf-run",
		Agent:  "leaf",
		Task:   leaf.Content,
		Mode:   "run",
		Depth:  maxSpawnDepth,
		Source: spawnDispatchSource,
	})
	if _, err := eng.Process(context.Background(), leaf); err != nil {
		t.Fatalf("leaf Process: %v", err)
	}
	leafTools := syncToolNames(provider.toolNamesAt(t, 1+len(k12Specs)))
	for _, forbidden := range multiAgentRecursiveTools {
		if leafTools[forbidden] {
			t.Fatalf("synchronous leaf received recursive tool %q: %v", forbidden, leafTools)
		}
	}
}

func syncToolNames(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}
