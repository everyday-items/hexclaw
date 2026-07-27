package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

func TestBUG20260726034A01GenericK12AgentPUTRejectsOwnedFields(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "display_name", body: `{"display_name":"新名称"}`},
		{name: "description", body: `{"description":"新描述"}`},
		{name: "system_prompt", body: `{"system_prompt":"新人设"}`},
		{name: "provider", body: `{"provider":"new-provider"}`},
		{name: "model", body: `{"model":"new-model"}`},
		{
			name: "profile_owned_metadata",
			body: `{"metadata":{"scenario":"k12-tutor","k12.child_name":"另一个孩子",
				"k12.grade_term":"五年级下","custom.note":"keep"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dispatcher := agentrouter.New()
			original := agentrouter.AgentConfig{
				Name: "mingming", DisplayName: "明明的辅导助手",
				Description: "旧描述", SystemPrompt: "旧人设",
				Provider: "old-provider", Model: "old-model",
				Skills: []string{"solve"}, MaxTokens: 2048,
				Metadata: map[string]string{
					"scenario":       "k12-tutor",
					"k12.child_name": "明明",
					"k12.grade_term": "五年级下",
					"custom.note":    "keep",
				},
			}
			if err := dispatcher.Register(original); err != nil {
				t.Fatal(err)
			}
			srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
			srv.SetAgentRouter(dispatcher)

			req := httptest.NewRequest(http.MethodPut,
				"/api/v1/agents/mingming", strings.NewReader(tt.body))
			req.SetPathValue("name", "mingming")
			rec := httptest.NewRecorder()
			srv.handleUpdateAgent(rec, req)

			if rec.Code != http.StatusConflict {
				t.Errorf("status=%d want 409; body=%s", rec.Code, rec.Body.String())
			}
			if got := strings.TrimSpace(rec.Body.String()); got !=
				`{"error":"K12 profile fields require /api/k12/profile-bundle"}` {
				t.Errorf("body=%s", got)
			}
			stored, ok := dispatcher.GetAgent("mingming")
			if !ok {
				t.Fatal("K12 agent disappeared")
			}
			if stored.DisplayName != original.DisplayName ||
				stored.Description != original.Description ||
				stored.SystemPrompt != original.SystemPrompt ||
				stored.Provider != original.Provider ||
				stored.Model != original.Model ||
				!reflect.DeepEqual(stored.Metadata, original.Metadata) {
				t.Errorf("rejected generic PUT mutated K12-owned state: %+v", stored)
			}
		})
	}
}

func TestBUG20260726034A01GenericK12AgentPUTKeepsGenericFieldsPatchable(t *testing.T) {
	dispatcher := agentrouter.New()
	original := agentrouter.AgentConfig{
		Name: "mingming", DisplayName: "明明的辅导助手",
		Metadata: map[string]string{
			"scenario":       "k12-tutor",
			"k12.child_name": "明明",
			"k12.grade_term": "五年级下",
			"custom.note":    "old",
		},
	}
	if err := dispatcher.Register(original); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetAgentRouter(dispatcher)
	body := `{"max_tokens":3072,
		"metadata":{"scenario":"k12-tutor","k12.child_name":"明明",
		"k12.grade_term":"五年级下","custom.note":"new"}}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/agents/mingming", strings.NewReader(body))
	req.SetPathValue("name", "mingming")
	rec := httptest.NewRecorder()
	srv.handleUpdateAgent(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("generic-only patch status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, _ := dispatcher.GetAgent("mingming")
	if stored.MaxTokens != 3072 || len(stored.Skills) != 0 ||
		stored.Metadata["custom.note"] != "new" {
		t.Fatalf("generic-only fields were not patched: %+v", stored)
	}
}
