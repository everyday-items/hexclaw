package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/hexagon-codes/hexclaw/config"
)

const (
	APIKeyMutationPreserve = "preserve"
	APIKeyMutationReplace  = "replace"
	APIKeyMutationDelete   = "delete"

	maxHydratedCredentials = 64
	maxCredentialBytes     = 64 * 1024
)

var errCredentialRefNotHydrated = errors.New("credential_ref has not been hydrated by the native coordinator")

// APIKeyMutation is an explicit secret-state transition. Plaintext is never a
// field of this contract: native code hydrates the referenced secret through
// the protected internal endpoint before applying replace.
type APIKeyMutation struct {
	Mode          string `json:"mode"`
	CredentialRef string `json:"credential_ref,omitempty"`
}

type credentialResolver interface {
	Resolve(ref string) (string, bool)
}

type inMemoryCredentialResolver struct {
	mu      sync.RWMutex
	entries map[string]string
}

type credentialSnapshotValue struct {
	secret  string
	present bool
}

func newInMemoryCredentialResolver() *inMemoryCredentialResolver {
	return &inMemoryCredentialResolver{entries: make(map[string]string)}
}

func (r *inMemoryCredentialResolver) Resolve(ref string) (string, bool) {
	if r == nil {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	secret, ok := r.entries[strings.TrimSpace(ref)]
	return secret, ok && secret != ""
}

// Hydrate atomically installs validated process-local secret material. The
// caller is the native coordinator; secrets are never returned by any API.
func (r *inMemoryCredentialResolver) Hydrate(entries map[string]string) error {
	if r == nil {
		return errors.New("credential resolver unavailable")
	}
	if len(entries) == 0 || len(entries) > maxHydratedCredentials {
		return fmt.Errorf("credential hydrate entries must contain 1..%d items", maxHydratedCredentials)
	}
	validated := make(map[string]string, len(entries))
	for ref, secret := range entries {
		ref = strings.TrimSpace(ref)
		if _, err := providerIDFromCredentialRef(ref); err != nil {
			return err
		}
		if secret == "" || len(secret) > maxCredentialBytes {
			return fmt.Errorf("credential secret is invalid")
		}
		validated[ref] = secret
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for ref, secret := range validated {
		r.entries[ref] = secret
	}
	return nil
}

func (r *inMemoryCredentialResolver) Delete(ref string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.entries, strings.TrimSpace(ref))
	r.mu.Unlock()
}

func (r *inMemoryCredentialResolver) snapshot(refs []string) map[string]credentialSnapshotValue {
	snapshot := make(map[string]credentialSnapshotValue, len(refs))
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, ref := range refs {
		secret, ok := r.entries[ref]
		snapshot[ref] = credentialSnapshotValue{secret: secret, present: ok}
	}
	return snapshot
}

func (r *inMemoryCredentialResolver) restore(snapshot map[string]credentialSnapshotValue) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ref, value := range snapshot {
		if value.present {
			r.entries[ref] = value.secret
		} else {
			delete(r.entries, ref)
		}
	}
}

func providerIDFromCredentialRef(ref string) (string, error) {
	const prefix = "llm_provider/"
	const suffix = "/api_key"
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, prefix) || !strings.HasSuffix(ref, suffix) {
		return "", fmt.Errorf("credential_ref is not an LLM provider api_key identity")
	}
	providerID := strings.TrimSuffix(strings.TrimPrefix(ref, prefix), suffix)
	if err := config.ValidateProviderAPIKeyCredentialRef(ref, providerID); err != nil {
		return "", err
	}
	return providerID, nil
}

func resolveProviderAPIKeyMutation(
	_ context.Context,
	providerInstanceID string,
	old config.LLMProviderConfig,
	oldExists bool,
	mutation *APIKeyMutation,
	resolver credentialResolver,
) (apiKey, credentialRef string, err error) {
	if mutation == nil {
		return "", "", errors.New("api_key_mutation is required")
	}
	mode := strings.ToLower(strings.TrimSpace(mutation.Mode))
	ref := strings.TrimSpace(mutation.CredentialRef)
	switch mode {
	case APIKeyMutationPreserve:
		if ref != "" {
			return "", "", errors.New("preserve must not include credential_ref")
		}
		if !oldExists {
			return "", "", errors.New("preserve requires an existing provider")
		}
		return old.APIKey, old.CredentialRef, nil
	case APIKeyMutationReplace:
		if err := config.ValidateProviderAPIKeyCredentialRef(ref, providerInstanceID); err != nil {
			return "", "", err
		}
		if resolver == nil {
			return "", "", errors.New("credential resolver unavailable")
		}
		secret, ok := resolver.Resolve(ref)
		if !ok {
			return "", "", errCredentialRefNotHydrated
		}
		return secret, ref, nil
	case APIKeyMutationDelete:
		if ref != "" {
			return "", "", errors.New("delete must not include credential_ref")
		}
		return "", "", nil
	default:
		return "", "", fmt.Errorf("api_key_mutation.mode must be preserve, replace, or delete")
	}
}

type credentialHydrateRequest struct {
	Entries []struct {
		CredentialRef string `json:"credential_ref"`
		Secret        string `json:"secret"`
	} `json:"entries"`
}

// handleReserveProviderCredentialIdentity returns a stateless, server-created
// provider identity and its only valid API-key vault reference. Reservation
// does not mutate config; uniqueness is revalidated by the eventual config PUT.
func (s *Server) handleReserveProviderCredentialIdentity(w http.ResponseWriter, _ *http.Request) {
	providerID, err := config.NewProviderInstanceID()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "provider identity generation failed"})
		return
	}
	credentialRef, err := config.ProviderAPIKeyCredentialRef(providerID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "credential identity generation failed"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"provider_instance_id": providerID,
		"credential_ref":       credentialRef,
	})
}

func (s *Server) handleHydrateDesktopCredentials(w http.ResponseWriter, r *http.Request) {
	var req credentialHydrateRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid credential hydrate request"})
		return
	}
	if len(req.Entries) == 0 || len(req.Entries) > maxHydratedCredentials {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid credential hydrate entry count"})
		return
	}
	entries := make(map[string]string, len(req.Entries))
	refs := make([]string, 0, len(req.Entries))
	for _, entry := range req.Entries {
		ref := strings.TrimSpace(entry.CredentialRef)
		if _, duplicate := entries[ref]; duplicate {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "duplicate credential_ref"})
			return
		}
		entries[ref] = entry.Secret
		refs = append(refs, ref)
	}
	previous := s.credentialResolver.snapshot(refs)
	if err := s.credentialResolver.Hydrate(entries); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.applyHydratedCredentials(r.Context()); err != nil {
		s.credentialResolver.restore(previous)
		_ = s.applyHydratedCredentials(context.Background())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "credential runtime activation failed"})
		return
	}
	sort.Strings(refs)
	writeJSON(w, http.StatusOK, map[string]any{"hydrated_count": len(refs), "credential_refs": refs})
}

type credentialDehydrateRequest struct {
	CredentialRefs []string `json:"credential_refs"`
}

func (s *Server) handleDehydrateDesktopCredentials(w http.ResponseWriter, r *http.Request) {
	var req credentialDehydrateRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || len(req.CredentialRefs) == 0 || len(req.CredentialRefs) > maxHydratedCredentials {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid credential dehydrate request"})
		return
	}
	refs := make([]string, 0, len(req.CredentialRefs))
	seen := make(map[string]struct{}, len(req.CredentialRefs))
	for _, raw := range req.CredentialRefs {
		ref := strings.TrimSpace(raw)
		if _, err := providerIDFromCredentialRef(ref); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if _, duplicate := seen[ref]; duplicate {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "duplicate credential_ref"})
			return
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	previous := s.credentialResolver.snapshot(refs)
	for _, ref := range refs {
		s.credentialResolver.Delete(ref)
	}
	if err := s.applyHydratedCredentials(r.Context()); err != nil {
		s.credentialResolver.restore(previous)
		_ = s.applyHydratedCredentials(context.Background())
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "credential runtime deactivation failed"})
		return
	}
	sort.Strings(refs)
	writeJSON(w, http.StatusOK, map[string]any{"dehydrated_count": len(refs), "credential_refs": refs})
}

func (s *Server) applyHydratedCredentials(ctx context.Context) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	oldLLM := s.cfg.LLM
	nextLLM := oldLLM
	nextLLM.Providers = make(map[string]config.LLMProviderConfig, len(oldLLM.Providers))
	changed := false
	for name, provider := range oldLLM.Providers {
		if ref := strings.TrimSpace(provider.CredentialRef); ref != "" {
			// credential_ref 是原生请求链路使用的稳定标识，而不是与 YAML 竞争的数据源。
			// 重启后 resolver 未命中属于预期情况，绝不能清空 owner 持久化在 YAML 中的 API key。
			if secret, ok := s.credentialResolver.Resolve(ref); ok && provider.APIKey != secret {
				provider.APIKey = secret
				changed = true
			}
		}
		nextLLM.Providers[name] = provider
	}
	if !changed {
		return nil
	}
	if runtime, ok := s.engine.(llmConfigRuntime); ok {
		if err := runtime.ReloadLLMConfig(ctx, nextLLM); err != nil {
			return err
		}
	}
	if err := s.drainSemanticRuntime(ctx, nextLLM); err != nil {
		if runtime, ok := s.engine.(llmConfigRuntime); ok {
			_ = runtime.ReloadLLMConfig(context.Background(), oldLLM)
		}
		return err
	}
	s.cfg.LLM = nextLLM
	if s.reloadGenServices != nil {
		s.reloadGenServices()
	}
	return nil
}
