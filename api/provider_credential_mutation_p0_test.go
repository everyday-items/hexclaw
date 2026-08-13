package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

const credentialTestProviderID = "pvd_v1_00112233445566778899aabbccddeeff"
const credentialTestRef = "llm_provider/" + credentialTestProviderID + "/api_key"

func TestProviderCredentialMutationP0_ResolveModes(t *testing.T) {
	resolver := newInMemoryCredentialResolver()
	if err := resolver.Hydrate(map[string]string{credentialTestRef: "sk-from-native-vault"}); err != nil {
		t.Fatal(err)
	}
	old := config.LLMProviderConfig{
		ProviderInstanceID: credentialTestProviderID,
		CredentialRef:      credentialTestRef,
		APIKey:             "sk-old-runtime",
	}

	tests := []struct {
		name     string
		mutation *APIKeyMutation
		old      config.LLMProviderConfig
		wantKey  string
		wantRef  string
		wantErr  bool
	}{
		{name: "preserve", mutation: &APIKeyMutation{Mode: APIKeyMutationPreserve}, old: old, wantKey: "sk-old-runtime", wantRef: credentialTestRef},
		{name: "replace hydrated ref", mutation: &APIKeyMutation{Mode: APIKeyMutationReplace, CredentialRef: credentialTestRef}, old: old, wantKey: "sk-from-native-vault", wantRef: credentialTestRef},
		{name: "delete", mutation: &APIKeyMutation{Mode: APIKeyMutationDelete}, old: old},
		{name: "replace missing ref", mutation: &APIKeyMutation{Mode: APIKeyMutationReplace, CredentialRef: "llm_provider/pvd_v1_ffeeddccbbaa99887766554433221100/api_key"}, old: old, wantErr: true},
		{name: "invalid mode", mutation: &APIKeyMutation{Mode: "rotate"}, old: old, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, ref, err := resolveProviderAPIKeyMutation(context.Background(), credentialTestProviderID, tt.old, true, tt.mutation, resolver)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, tt.wantErr)
			}
			if err == nil && (key != tt.wantKey || ref != tt.wantRef) {
				t.Fatalf("key/ref=%q/%q want %q/%q", key, ref, tt.wantKey, tt.wantRef)
			}
		})
	}
}

func TestProviderCredentialMutationP0_TypedReplacePersistsOwnerOnlyYAMLKey(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("HOME", configHome)
	enabled := true
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "custom"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"custom": {
			ProviderInstanceID: credentialTestProviderID,
			APIKey:             "sk-legacy-old",
			BaseURL:            "https://api.example.test/v1",
			Model:              "chat",
			Models:             []string{"chat"},
			Enabled:            &enabled,
		},
	}
	engine := &mockEngine{activeLLM: cfg.LLM}
	srv := NewServer(cfg, engine, nil, nil)
	const persistedKey = "sk-native-yaml-persisted"
	if err := srv.credentialResolver.Hydrate(map[string]string{credentialTestRef: persistedKey}); err != nil {
		t.Fatal(err)
	}
	body := `{"default":"custom","providers":{"custom":{` +
		`"provider_instance_id":"` + credentialTestProviderID + `",` +
		`"api_key_mutation":{"mode":"replace","credential_ref":"` + credentialTestRef + `"},` +
		`"base_url":"https://api.example.test/v1","model":"chat","models":["chat"],"enabled":true}}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", "credential-replace-typed-1")
	rec := httptest.NewRecorder()

	srv.handleUpdateLLMConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("typed replace status=%d body=%s", rec.Code, rec.Body.String())
	}
	provider := srv.cfg.LLM.Providers["custom"]
	if provider.APIKey != persistedKey || provider.CredentialRef != credentialTestRef {
		t.Fatalf("runtime provider key/ref=%q/%q", provider.APIKey, provider.CredentialRef)
	}
	configDirectory := filepath.Join(configHome, ".hexclaw")
	configPath := filepath.Join(configDirectory, "hexclaw.yaml")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), persistedKey) || !strings.Contains(string(raw), credentialTestRef) {
		t.Fatal("persisted config did not retain the provider key and stable reference")
	}
	fileInfo, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("config file permission=%#o, want 0600", fileInfo.Mode().Perm())
	}
	directoryInfo, err := os.Stat(configDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("config directory permission=%#o, want 0700", directoryInfo.Mode().Perm())
	}
}

func TestProviderCredentialMutationP0_UnhydratedRefDoesNotClearPersistedYAMLKey(t *testing.T) {
	enabled := true
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "custom"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"custom": {
			ProviderInstanceID: credentialTestProviderID,
			CredentialRef:      credentialTestRef,
			APIKey:             "sk-persisted-yaml-key",
			BaseURL:            "https://api.example.test/v1",
			Model:              "chat",
			Models:             []string{"chat"},
			Enabled:            &enabled,
		},
	}
	engine := &mockEngine{activeLLM: cfg.LLM}
	srv := NewServer(cfg, engine, nil, nil)

	if err := srv.applyHydratedCredentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := srv.cfg.LLM.Providers["custom"].APIKey; got != "sk-persisted-yaml-key" {
		t.Fatal("an unresolved credential_ref cleared the persisted provider key")
	}
	if engine.reloadCalls != 0 {
		t.Fatalf("unresolved credential_ref unexpectedly reloaded runtime %d times", engine.reloadCalls)
	}
}

func TestProviderCredentialMutationP0_TypedMutationRequiresIdempotencyKey(t *testing.T) {
	enabled := true
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "custom"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"custom": {
			ProviderInstanceID: credentialTestProviderID,
			APIKey:             "sk-old",
			BaseURL:            "https://api.example.test/v1",
			Model:              "chat",
			Models:             []string{"chat"},
			Enabled:            &enabled,
		},
	}
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	if err := srv.credentialResolver.Hydrate(map[string]string{credentialTestRef: "sk-new"}); err != nil {
		t.Fatal(err)
	}
	body := `{"default":"custom","providers":{"custom":{` +
		`"provider_instance_id":"` + credentialTestProviderID + `",` +
		`"api_key_mutation":{"mode":"replace","credential_ref":"` + credentialTestRef + `"},` +
		`"base_url":"https://api.example.test/v1","model":"chat","models":["chat"],"enabled":true}}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(body))
	rec := httptest.NewRecorder()

	srv.handleUpdateLLMConfig(rec, req)

	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("typed mutation without idempotency key status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestProviderCredentialMutationP0_NotHydratedHasStableExclusiveMachineCode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	enabled := true
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "custom"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"custom": {
			ProviderInstanceID: credentialTestProviderID,
			APIKey:             "sk-old-runtime",
			BaseURL:            "https://api.example.test/v1",
			Model:              "chat",
			Models:             []string{"chat"},
			Enabled:            &enabled,
		},
	}
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	request := func(mode string) *httptest.ResponseRecorder {
		body := `{"default":"custom","providers":{"custom":{` +
			`"provider_instance_id":"` + credentialTestProviderID + `",` +
			`"api_key_mutation":{"mode":"` + mode + `","credential_ref":"` + credentialTestRef + `"},` +
			`"base_url":"https://api.example.test/v1","model":"chat","models":["chat"],"enabled":true}}}`
		req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", "credential-machine-code-"+mode)
		rec := httptest.NewRecorder()
		srv.handleUpdateLLMConfig(rec, req)
		return rec
	}

	missing := request(APIKeyMutationReplace)
	var missingPayload map[string]any
	if err := json.Unmarshal(missing.Body.Bytes(), &missingPayload); err != nil {
		t.Fatalf("decode missing hydration response: %v", err)
	}
	if missing.Code != http.StatusUnprocessableEntity || missingPayload["code"] != "credential_ref_not_hydrated" {
		t.Fatalf("missing hydration response status=%d payload=%v", missing.Code, missingPayload)
	}

	invalid := request("rotate")
	var invalidPayload map[string]any
	if err := json.Unmarshal(invalid.Body.Bytes(), &invalidPayload); err != nil {
		t.Fatalf("decode invalid mutation response: %v", err)
	}
	if invalid.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid mutation status=%d payload=%v", invalid.Code, invalidPayload)
	}
	if _, exists := invalidPayload["code"]; exists {
		t.Fatalf("unrelated 422 reused hydration machine code: %v", invalidPayload)
	}
}

func TestProviderCredentialMutationP0_CommitReceiptReplaysAcrossRestartAndConflicts(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("HOME", configHome)
	enabled := true
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "custom"
	cfg.LLM.ReasoningProvider = "custom"
	cfg.LLM.ReasoningModel = "chat"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"custom": {
			ProviderInstanceID: credentialTestProviderID,
			APIKey:             "sk-old",
			BaseURL:            "https://api.example.test/v1",
			Model:              "chat",
			Models:             []string{"chat"},
			Enabled:            &enabled,
		},
	}
	body := `{"default":"custom","reasoning_provider":"custom","reasoning_model":"chat","providers":{"custom":{` +
		`"provider_instance_id":"` + credentialTestProviderID + `",` +
		`"api_key_mutation":{"mode":"replace","credential_ref":"` + credentialTestRef + `"},` +
		`"base_url":"https://api.example.test/v1","model":"chat","models":["chat"],"enabled":true}}}`
	requestID := "credential-replace-ambiguous-1"

	first := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	if err := first.credentialResolver.Hydrate(map[string]string{credentialTestRef: "sk-new-runtime-only"}); err != nil {
		t.Fatal(err)
	}
	firstReq := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(body))
	firstReq.Header.Set("Idempotency-Key", requestID)
	firstRec := httptest.NewRecorder()
	first.handleUpdateLLMConfig(firstRec, firstReq)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first commit status=%d body=%s", firstRec.Code, firstRec.Body.String())
	}
	var committed struct {
		Status         string `json:"status"`
		RequestID      string `json:"request_id"`
		ConfigRevision uint64 `json:"config_revision"`
		ConfigDigest   string `json:"config_digest"`
		Replayed       bool   `json:"replayed"`
	}
	if err := json.Unmarshal(firstRec.Body.Bytes(), &committed); err != nil {
		t.Fatal(err)
	}
	if committed.Status != "ok" || committed.RequestID != requestID || committed.ConfigRevision == 0 || committed.ConfigDigest == "" || committed.Replayed {
		t.Fatalf("incomplete commit receipt: %+v body=%s", committed, firstRec.Body.String())
	}

	persistedPath := filepath.Join(configHome, ".hexclaw", "hexclaw.yaml")
	persisted, err := os.ReadFile(persistedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(persisted), "sk-new-runtime-only") || !strings.Contains(string(persisted), requestID) {
		t.Fatal("durable config did not retain the provider key and commit receipt")
	}
	restartedCfg, err := config.Load(persistedPath)
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewServer(restartedCfg, &mockEngine{activeLLM: restartedCfg.LLM}, nil, nil)
	// Deliberately do not hydrate the resolver: an exact replay must return the
	// durable outcome before trying to apply the secret transition again.
	replayReq := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(body))
	replayReq.Header.Set("Idempotency-Key", requestID)
	replayRec := httptest.NewRecorder()
	restarted.handleUpdateLLMConfig(replayRec, replayReq)
	if replayRec.Code != http.StatusOK {
		t.Fatalf("cross-restart replay status=%d body=%s", replayRec.Code, replayRec.Body.String())
	}
	var replayed struct {
		RequestID      string `json:"request_id"`
		ConfigRevision uint64 `json:"config_revision"`
		ConfigDigest   string `json:"config_digest"`
		Replayed       bool   `json:"replayed"`
	}
	if err := json.Unmarshal(replayRec.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.RequestID != committed.RequestID || replayed.ConfigRevision != committed.ConfigRevision || replayed.ConfigDigest != committed.ConfigDigest {
		t.Fatalf("replay receipt mismatch: committed=%+v replayed=%+v", committed, replayed)
	}

	conflictBody := strings.Replace(body, `"model":"chat"`, `"model":"chat-v2"`, 1)
	conflictReq := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(conflictBody))
	conflictReq.Header.Set("Idempotency-Key", requestID)
	conflictRec := httptest.NewRecorder()
	restarted.handleUpdateLLMConfig(conflictRec, conflictReq)
	if conflictRec.Code != http.StatusConflict {
		t.Fatalf("same key with different request status=%d body=%s", conflictRec.Code, conflictRec.Body.String())
	}
}

func TestProviderCredentialMutationP1_OlderReceiptReplaysAfterNewerCommitWithoutRollback(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("HOME", configHome)
	enabled := true
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "custom"
	cfg.LLM.ReasoningProvider = "custom"
	cfg.LLM.ReasoningModel = "chat-a"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"custom": {
			ProviderInstanceID: credentialTestProviderID,
			APIKey:             "sk-old",
			BaseURL:            "https://api.example.test/v1",
			Model:              "chat-a",
			Models:             []string{"chat-a", "chat-b"},
			Enabled:            &enabled,
		},
	}

	bodyForModel := func(model string) string {
		return `{"default":"custom","reasoning_provider":"custom","reasoning_model":"` + model + `","providers":{"custom":{` +
			`"provider_instance_id":"` + credentialTestProviderID + `",` +
			`"api_key_mutation":{"mode":"replace","credential_ref":"` + credentialTestRef + `"},` +
			`"base_url":"https://api.example.test/v1","model":"` + model + `","models":["chat-a","chat-b"],"enabled":true}}}`
	}
	type receipt struct {
		RequestID      string `json:"request_id"`
		ConfigRevision uint64 `json:"config_revision"`
		ConfigDigest   string `json:"config_digest"`
		Replayed       bool   `json:"replayed"`
	}
	commit := func(t *testing.T, srv *Server, requestID, body string) receipt {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(body))
		req.Header.Set("Idempotency-Key", requestID)
		rec := httptest.NewRecorder()
		srv.handleUpdateLLMConfig(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("mutation %s status=%d body=%s", requestID, rec.Code, rec.Body.String())
		}
		var got receipt
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	if err := srv.credentialResolver.Hydrate(map[string]string{credentialTestRef: "sk-runtime-only"}); err != nil {
		t.Fatal(err)
	}
	first := commit(t, srv, "credential-ledger-a", bodyForModel("chat-a"))
	second := commit(t, srv, "credential-ledger-b", bodyForModel("chat-b"))
	if first.ConfigRevision == 0 || second.ConfigRevision != first.ConfigRevision+1 {
		t.Fatalf("non-monotonic commits: first=%+v second=%+v", first, second)
	}

	persistedPath := filepath.Join(configHome, ".hexclaw", "hexclaw.yaml")
	restartedCfg, err := config.Load(persistedPath)
	if err != nil {
		t.Fatal(err)
	}
	if restartedCfg.LLM.Providers["custom"].Model != "chat-b" || restartedCfg.LLM.ConfigRevision != second.ConfigRevision {
		t.Fatalf("newer commit was not durable before replay: %+v", restartedCfg.LLM)
	}
	if len(restartedCfg.LLM.MutationReceipts) != 2 {
		t.Fatalf("durable ledger lost an older receipt: %+v", restartedCfg.LLM.MutationReceipts)
	}
	restarted := NewServer(restartedCfg, &mockEngine{activeLLM: restartedCfg.LLM}, nil, nil)
	// No credential hydration: a durable replay must resolve before executing
	// the old replacement intent again.
	replayed := commit(t, restarted, "credential-ledger-a", bodyForModel("chat-a"))
	if !replayed.Replayed || replayed.RequestID != first.RequestID || replayed.ConfigRevision != first.ConfigRevision || replayed.ConfigDigest != first.ConfigDigest {
		t.Fatalf("older durable receipt was not replayed exactly: first=%+v replay=%+v", first, replayed)
	}
	if restarted.cfg.LLM.Providers["custom"].Model != "chat-b" || restarted.cfg.LLM.ConfigRevision != second.ConfigRevision {
		t.Fatalf("replaying A rolled back newer commit B: %+v", restarted.cfg.LLM)
	}

	conflictReq := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(bodyForModel("chat-b")))
	conflictReq.Header.Set("Idempotency-Key", "credential-ledger-a")
	conflictRec := httptest.NewRecorder()
	restarted.handleUpdateLLMConfig(conflictRec, conflictReq)
	if conflictRec.Code != http.StatusConflict {
		t.Fatalf("older key reused with a different request status=%d body=%s", conflictRec.Code, conflictRec.Body.String())
	}
	if restarted.cfg.LLM.Providers["custom"].Model != "chat-b" || restarted.cfg.LLM.ConfigRevision != second.ConfigRevision {
		t.Fatalf("conflicting replay mutated newer config: %+v", restarted.cfg.LLM)
	}
}

func TestProviderCredentialMutationP0_RemovingProviderForgetsProcessCredentialAfterCommit(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("HOME", configHome)
	enabled := true
	cfg := config.DefaultConfig()
	cfg.LLM.Default = "custom"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"custom": {
			ProviderInstanceID: credentialTestProviderID,
			CredentialRef:      credentialTestRef,
			APIKey:             "sk-runtime-only",
			BaseURL:            "https://api.example.test/v1",
			Model:              "chat",
			Models:             []string{"chat"},
			Enabled:            &enabled,
		},
	}
	srv := NewServer(cfg, &mockEngine{activeLLM: cfg.LLM}, nil, nil)
	if err := srv.credentialResolver.Hydrate(map[string]string{credentialTestRef: "sk-runtime-only"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{"providers":{}}`))
	rec := httptest.NewRecorder()

	srv.handleUpdateLLMConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("remove provider status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := srv.credentialResolver.Resolve(credentialTestRef); ok {
		t.Fatal("removed provider retained process-local credential")
	}
}

func TestProviderCredentialMutationP0_InternalHydrateRequiresNativeSidecarCapability(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.APIToken = "api-token"
	srv := NewServer(cfg, nil, nil, nil)
	srv.SetSidecarCapabilityToken("native-sidecar-token-012345678901")
	h := srv.routes()
	body := `{"entries":[{"credential_ref":"` + credentialTestRef + `","secret":"sk-native"}]}`

	apiReq := httptest.NewRequest(http.MethodPost, "/api/internal/desktop/credentials/hydrate", strings.NewReader(body))
	apiReq.RemoteAddr = "127.0.0.1:41001"
	apiReq.Header.Set("Authorization", "Bearer api-token")
	apiRec := httptest.NewRecorder()
	h.ServeHTTP(apiRec, apiReq)
	if apiRec.Code != http.StatusUnauthorized {
		t.Fatalf("API token reached native credential hydrate: status=%d body=%s", apiRec.Code, apiRec.Body.String())
	}

	nativeReq := httptest.NewRequest(http.MethodPost, "/api/internal/desktop/credentials/hydrate", strings.NewReader(body))
	nativeReq.RemoteAddr = "127.0.0.1:41002"
	nativeReq.Header.Set("Authorization", "Bearer native-sidecar-token-012345678901")
	nativeRec := httptest.NewRecorder()
	h.ServeHTTP(nativeRec, nativeReq)
	if nativeRec.Code != http.StatusOK {
		t.Fatalf("native hydrate status=%d body=%s", nativeRec.Code, nativeRec.Body.String())
	}
	if secret, ok := srv.credentialResolver.Resolve(credentialTestRef); !ok || secret != "sk-native" {
		t.Fatal("native hydrate did not populate process-local resolver")
	}

	replaceBody := `{"entries":[{"credential_ref":"` + credentialTestRef + `","secret":"sk-native-replaced"}]}`
	replaceReq := httptest.NewRequest(http.MethodPost, "/api/internal/desktop/credentials/hydrate", strings.NewReader(replaceBody))
	replaceReq.RemoteAddr = "127.0.0.1:41003"
	replaceReq.Header.Set("Authorization", "Bearer native-sidecar-token-012345678901")
	replaceRec := httptest.NewRecorder()
	h.ServeHTTP(replaceRec, replaceReq)
	if replaceRec.Code != http.StatusOK {
		t.Fatalf("native rehydrate status=%d body=%s", replaceRec.Code, replaceRec.Body.String())
	}
	if secret, ok := srv.credentialResolver.Resolve(credentialTestRef); !ok || secret != "sk-native-replaced" {
		t.Fatal("rehydrate did not replace process-local value")
	}

	dehydrateReq := httptest.NewRequest(http.MethodPost, "/api/internal/desktop/credentials/dehydrate",
		strings.NewReader(`{"credential_refs":["`+credentialTestRef+`"]}`))
	dehydrateReq.RemoteAddr = "127.0.0.1:41004"
	dehydrateReq.Header.Set("Authorization", "Bearer native-sidecar-token-012345678901")
	dehydrateRec := httptest.NewRecorder()
	h.ServeHTTP(dehydrateRec, dehydrateReq)
	if dehydrateRec.Code != http.StatusOK {
		t.Fatalf("native dehydrate status=%d body=%s", dehydrateRec.Code, dehydrateRec.Body.String())
	}
	if _, ok := srv.credentialResolver.Resolve(credentialTestRef); ok {
		t.Fatal("dehydrate retained process-local credential")
	}
}

func TestProviderCredentialMutationP0_NativeReserveKeepsProviderIdentityServerOwned(t *testing.T) {
	cfg := config.DefaultConfig()
	srv := NewServer(cfg, nil, nil, nil)
	srv.SetSidecarCapabilityToken("native-sidecar-token-012345678901")
	h := srv.routes()
	seen := make(map[string]struct{})
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/internal/desktop/provider-credentials/reserve", nil)
		req.RemoteAddr = "127.0.0.1:41100"
		req.Header.Set("Authorization", "Bearer native-sidecar-token-012345678901")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("reserve status=%d body=%s", rec.Code, rec.Body.String())
		}
		var response struct {
			ProviderInstanceID string `json:"provider_instance_id"`
			CredentialRef      string `json:"credential_ref"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if err := config.ValidateProviderInstanceID(response.ProviderInstanceID); err != nil {
			t.Fatalf("server reserve returned invalid provider identity: %v", err)
		}
		if err := config.ValidateProviderAPIKeyCredentialRef(response.CredentialRef, response.ProviderInstanceID); err != nil {
			t.Fatalf("server reserve returned mismatched credential ref: %v", err)
		}
		if _, duplicate := seen[response.ProviderInstanceID]; duplicate {
			t.Fatalf("reserve reused provider_instance_id %q", response.ProviderInstanceID)
		}
		seen[response.ProviderInstanceID] = struct{}{}
	}
}
