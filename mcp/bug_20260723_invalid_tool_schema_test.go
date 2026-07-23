package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/testing/mock"
)

func toolWithSchema(name string, s *llm.Schema) hexagon.Tool {
	return mock.NewTool(
		name,
		mock.WithToolDescription("schema regression probe"),
		mock.WithToolSchema(s),
	)
}

func TestListToolDefinitions_PreservesGitHubAnyOfWithoutEmptyType(t *testing.T) {
	githubReview := toolWithSchema("create_pull_request_review", &llm.Schema{
		Type: "object",
		Properties: map[string]*llm.Schema{
			"comments": {
				Type: "array",
				Items: &llm.Schema{
					AnyOf: []*llm.Schema{
						{
							Type:                 "object",
							Required:             []string{"path", "position", "body"},
							AdditionalProperties: false,
						},
						{
							Type:                 "object",
							Required:             []string{"path", "line", "body"},
							AdditionalProperties: false,
						},
					},
				},
			},
		},
	})

	m := NewManager()
	m.mu.Lock()
	m.servers["github"] = &connectedServer{
		name:      "github",
		connected: true,
		tools:     []hexagon.Tool{githubReview},
	}
	m.mu.Unlock()

	defs := m.ListToolDefinitions()
	if len(defs) != 1 {
		t.Fatalf("LLM definitions = %d, want GitHub review tool", len(defs))
	}
	wire, err := json.Marshal(defs[0])
	if err != nil {
		t.Fatalf("Marshal tool definition: %v", err)
	}
	if strings.Contains(string(wire), `"type":""`) {
		t.Fatalf("provider payload contains an empty schema type: %s", wire)
	}
	if !strings.Contains(string(wire), `"anyOf":[`) {
		t.Fatalf("provider payload lost comments.items.anyOf: %s", wire)
	}
}

func TestListToolDefinitions_QuarantinesMalformedSchemaWithoutDroppingHealthyTools(t *testing.T) {
	healthy := toolWithSchema("healthy_tool", &llm.Schema{
		Type: "object",
		Properties: map[string]*llm.Schema{
			"path": {Type: "string"},
		},
	})
	malformed := toolWithSchema("create_pull_request_review", &llm.Schema{
		Type: "object",
		Properties: map[string]*llm.Schema{
			"comments": {
				Type:  "array",
				Items: &llm.Schema{Type: "definitely-not-a-json-schema-type"},
			},
		},
	})

	m := NewManager()
	m.mu.Lock()
	m.servers["github"] = &connectedServer{
		name:      "github",
		connected: true,
		tools:     []hexagon.Tool{healthy, malformed},
	}
	m.mu.Unlock()

	defs := m.ListToolDefinitions()
	if len(defs) != 1 {
		t.Fatalf("LLM definitions = %d, want only the healthy tool", len(defs))
	}
	if defs[0].Function.Name != "healthy_tool" {
		t.Fatalf("remaining tool = %q, want healthy_tool", defs[0].Function.Name)
	}
}
