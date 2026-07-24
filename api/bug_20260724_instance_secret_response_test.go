package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/instances"
)

const (
	instanceAppSecret    = "instance-app-secret-1234"
	instanceToken        = "instance-token-5678"
	instancePassword     = "instance-password-9012"
	instanceAPIKey       = "instance-api-key-3456"
	instanceNestedToken  = "nested-access-token-7890"
	instanceNestedSecret = "nested-client-secret-1111"
	instanceCamelAPIKey  = "camel-api-key-2222"
	instanceCamelToken   = "camel-access-token-3333"
	instanceCamelSecret  = "camel-client-secret-4444"
)

func secretRichInstanceConfig(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"app_key":    "public-app-key",
		"app_secret": instanceAppSecret,
		"token":      instanceToken,
		"password":   instancePassword,
		"api_key":    instanceAPIKey,
		"apiKey":     instanceCamelAPIKey,
		"nested": map[string]any{
			"access_token":  instanceNestedToken,
			"client_secret": instanceNestedSecret,
			"accessToken":   instanceCamelToken,
			"clientSecret":  instanceCamelSecret,
			"label":         "public-label",
		},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return raw
}

func assertNoRawInstanceSecrets(t *testing.T, body string) {
	t.Helper()
	for _, raw := range []string{
		instanceAppSecret,
		instanceToken,
		instancePassword,
		instanceAPIKey,
		instanceNestedToken,
		instanceNestedSecret,
		instanceCamelAPIKey,
		instanceCamelToken,
		instanceCamelSecret,
	} {
		if strings.Contains(body, raw) {
			t.Fatalf("instance response leaked raw credential %q: %s", raw, body)
		}
	}
}

func decodeInstanceConfig(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode instance config: %v (raw=%s)", err, raw)
	}
	return got
}

// GET /platforms/instances is consumed by the editable IM-channel cards. It may
// retain non-sensitive fields, but every credential value (including nested
// config) must use the repository's established ****/****last4 mask contract.
func TestBug20260724_ListInstancesMasksEveryCredential(t *testing.T) {
	mgr, cleanup := newTestInstanceManager(t)
	defer cleanup()
	if err := mgr.Upsert(context.Background(), &instances.Instance{
		Provider: "dingtalk",
		Name:     "secure-instance",
		Enabled:  false,
		Config:   secretRichInstanceConfig(t),
	}); err != nil {
		t.Fatalf("seed instance: %v", err)
	}

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetInstanceManager(mgr)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/platforms/instances", nil)
	w := httptest.NewRecorder()
	srv.handleListInstances(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	assertNoRawInstanceSecrets(t, w.Body.String())

	var response struct {
		Instances []instanceResponse `json:"instances"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(response.Instances) != 1 {
		t.Fatalf("instances=%d, want 1", len(response.Instances))
	}
	got := decodeInstanceConfig(t, response.Instances[0].Config)
	if got["app_key"] != "public-app-key" {
		t.Fatalf("non-secret app_key changed: %#v", got["app_key"])
	}
	for key, raw := range map[string]string{
		"app_secret": instanceAppSecret,
		"token":      instanceToken,
		"password":   instancePassword,
		"api_key":    instanceAPIKey,
		"apiKey":     instanceCamelAPIKey,
	} {
		if got[key] != config.MaskAPIKey(raw) {
			t.Fatalf("%s=%#v, want mask %q", key, got[key], config.MaskAPIKey(raw))
		}
	}
	nested, ok := got["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested config missing: %#v", got["nested"])
	}
	if nested["access_token"] != config.MaskAPIKey(instanceNestedToken) ||
		nested["client_secret"] != config.MaskAPIKey(instanceNestedSecret) ||
		nested["accessToken"] != config.MaskAPIKey(instanceCamelToken) ||
		nested["clientSecret"] != config.MaskAPIKey(instanceCamelSecret) {
		t.Fatalf("nested credentials not masked: %#v", nested)
	}
	if nested["label"] != "public-label" {
		t.Fatalf("nested non-secret field changed: %#v", nested)
	}
}

// POST/PUT responses share instanceToResponse and must never become a second
// credential-exfiltration path after the list endpoint is fixed.
func TestBug20260724_InstanceMutationResponsesMaskCredentials(t *testing.T) {
	mgr, cleanup := newTestInstanceManager(t)
	defer cleanup()
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetInstanceManager(mgr)

	body, err := json.Marshal(UpsertInstanceRequest{
		Provider: "dingtalk",
		Name:     "secure-instance",
		Enabled:  false,
		Config:   secretRichInstanceConfig(t),
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/platforms/instances", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	srv.handleUpsertInstance(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	assertNoRawInstanceSecrets(t, w.Body.String())

	var response instanceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode mutation response: %v", err)
	}
	got := decodeInstanceConfig(t, response.Config)
	if got["app_secret"] != config.MaskAPIKey(instanceAppSecret) {
		t.Fatalf("app_secret=%#v, want %q", got["app_secret"], config.MaskAPIKey(instanceAppSecret))
	}
}

// Editing an instance starts from the masked response above. Sending an
// unchanged placeholder back must retain the stored secret, while ordinary
// fields remain editable.
func TestBug20260724_MaskedInstanceUpdateRetainsStoredCredential(t *testing.T) {
	mgr, cleanup := newTestInstanceManager(t)
	defer cleanup()
	original := &instances.Instance{
		Provider: "dingtalk",
		Name:     "secure-instance",
		Enabled:  false,
		Config: json.RawMessage(`{
			"app_key":"public-app-key",
			"app_secret":"` + instanceAppSecret + `",
			"robot_code":"old-robot"
		}`),
	}
	if err := mgr.Upsert(context.Background(), original); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	stored, err := mgr.Get(context.Background(), original.Name)
	if err != nil {
		t.Fatalf("read seeded instance: %v", err)
	}

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetInstanceManager(mgr)
	body := `{
		"provider":"dingtalk",
		"name":"secure-instance",
		"enabled":false,
		"config":{
			"app_key":"public-app-key",
			"app_secret":"` + config.MaskAPIKey(instanceAppSecret) + `",
			"robot_code":"new-robot"
		}
	}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/platforms/instances/by-id/"+stored.ID, strings.NewReader(body))
	req.SetPathValue("id", stored.ID)
	w := httptest.NewRecorder()
	srv.handleUpdateInstanceByID(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	assertNoRawInstanceSecrets(t, w.Body.String())

	after, err := mgr.GetByID(context.Background(), stored.ID)
	if err != nil {
		t.Fatalf("read updated instance: %v", err)
	}
	got := decodeInstanceConfig(t, after.Config)
	if got["app_secret"] != instanceAppSecret {
		t.Fatalf("masked placeholder overwrote stored credential: %#v", got["app_secret"])
	}
	if got["robot_code"] != "new-robot" {
		t.Fatalf("ordinary edit was not persisted: %#v", got["robot_code"])
	}
}
