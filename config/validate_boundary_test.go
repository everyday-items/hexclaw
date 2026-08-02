package config

import (
	"errors"
	"strings"
	"testing"
)

// validBase returns a Config that passes Validate(), so each table case can mutate
// exactly one field and assert the resulting accept/reject decision in isolation.
func validBase() *Config {
	c := &Config{}
	c.Server.Port = 16060
	c.Server.MCPPort = 16070
	c.Server.Host = "127.0.0.1"
	c.Budget.MaxDuration = "30m"
	c.Budget.MaxCost = 5.0
	return c
}

// TestValidate_ServerPortBoundaries exercises the inclusive 1..65535 range for both
// the primary and MCP ports, including the explicit Port=0 rejection.
func TestValidate_ServerPortBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		port    int
		wantErr bool
	}{
		{"port_zero_rejected", 0, true},
		{"port_negative_rejected", -1, true},
		{"port_min_valid", 1, false},
		{"port_typical_valid", 16060, false},
		{"port_max_valid", 65535, false},
		{"port_above_max_rejected", 65536, true},
		{"port_far_above_rejected", 999999, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validBase()
			c.Server.Port = tt.port
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("port %d should be rejected", tt.port)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("port %d should be accepted: %v", tt.port, err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "server.port") {
				t.Errorf("error should reference server.port: %v", err)
			}
		})
	}
}

// TestValidate_MCPPortBoundaries covers the MCP port range independently of the
// main port (a separate field with its own bounds check).
func TestValidate_MCPPortBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		mcpPort int
		wantErr bool
	}{
		{"mcp_zero_rejected", 0, true},
		{"mcp_min_valid", 1, false},
		{"mcp_max_valid", 65535, false},
		{"mcp_above_max_rejected", 70000, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validBase()
			c.Server.MCPPort = tt.mcpPort
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("mcp_port %d should be rejected", tt.mcpPort)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("mcp_port %d should be accepted: %v", tt.mcpPort, err)
			}
		})
	}
}

// TestValidate_HostFormats covers isValidHost: legitimate hosts pass while values
// containing whitespace or injection punctuation are rejected. Empty host is
// allowed because the default is applied elsewhere.
func TestValidate_HostFormats(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		wantErr bool
	}{
		{"empty_allowed", "", false},
		{"loopback_ipv4", "127.0.0.1", false},
		{"localhost", "localhost", false},
		{"all_interfaces", "0.0.0.0", false},
		{"hostname", "hexclaw.local", false},
		{"space_rejected", "127.0.0.1 evil", true},
		{"tab_rejected", "127.0.0.1\tx", true},
		{"semicolon_rejected", "127.0.0.1;rm -rf", true},
		{"double_quote_rejected", "host\"name", true},
		{"single_quote_rejected", "host'name", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validBase()
			c.Server.Host = tt.host
			// This test isolates hostname syntax. Remote listeners independently
			// require authentication under the server trust-boundary contract.
			if tt.host != "" && !isLoopbackListenerHost(tt.host) {
				c.Server.APIToken = "test-capability"
			}
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("host %q should be rejected", tt.host)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("host %q should be accepted: %v", tt.host, err)
			}
		})
	}
}

// TestValidate_BudgetDurationFormats covers Go-duration parsing for budget.max_duration.
func TestValidate_BudgetDurationFormats(t *testing.T) {
	tests := []struct {
		name     string
		duration string
		wantErr  bool
	}{
		{"empty_uses_default", "", false},
		{"minutes", "30m", false},
		{"hours", "1h", false},
		{"compound", "1h30m", false},
		{"seconds", "90s", false},
		{"natural_language_rejected", "30 minutes", true},
		{"chinese_rejected", "三十分钟", true},
		{"bare_number_rejected", "30", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validBase()
			c.Budget.MaxDuration = tt.duration
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("duration %q should be rejected", tt.duration)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("duration %q should be accepted: %v", tt.duration, err)
			}
		})
	}
}

// TestValidate_BudgetMaxCost covers the non-negative constraint, including the
// zero boundary (zero means "use default" and is allowed).
func TestValidate_BudgetMaxCost(t *testing.T) {
	tests := []struct {
		name    string
		cost    float64
		wantErr bool
	}{
		{"zero_allowed", 0, false},
		{"positive_allowed", 5.0, false},
		{"tiny_positive_allowed", 0.0001, false},
		{"negative_rejected", -0.01, true},
		{"large_negative_rejected", -100, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validBase()
			c.Budget.MaxCost = tt.cost
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("cost %v should be rejected", tt.cost)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("cost %v should be accepted: %v", tt.cost, err)
			}
		})
	}
}

// TestValidate_TTSProviderMatrix covers isValidTTSProvider across hardcoded
// whitelist entries, llm-prefixed "-tts" forms, and the common misconfigurations.
func TestValidate_TTSProviderMatrix(t *testing.T) {
	tests := []struct {
		name      string
		provider  string
		providers map[string]LLMProviderConfig
		wantErr   bool
	}{
		{"edge_tts_no_key_needed", "edge-tts", nil, false},
		{"edge_alias", "edge", nil, false},
		{"openai_tts_hardcoded", "openai-tts", nil, false},
		{"azure_tts_hardcoded", "azure-tts", nil, false},
		{"llm_prefixed_tts_with_provider", "deepseek-tts", map[string]LLMProviderConfig{"deepseek": {}}, false},
		{"llm_prefixed_tts_missing_provider", "qwen-tts", map[string]LLMProviderConfig{"deepseek": {}}, true},
		{"bare_name_no_suffix", "openai", map[string]LLMProviderConfig{"deepseek": {}}, true},
		{"wrong_suffix_stt", "deepseek-stt", map[string]LLMProviderConfig{"deepseek": {}}, true},
		{"typo_tss", "edge-tss", nil, true},
		{"empty_suffix_only", "-tts", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validBase()
			c.Voice.Enabled = true
			c.Voice.TTS.Provider = tt.provider
			c.LLM.Providers = tt.providers
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("provider %q should be rejected", tt.provider)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("provider %q should be accepted: %v", tt.provider, err)
			}
		})
	}
}

// TestValidate_TTSSkippedWhenVoiceDisabled verifies TTS rules are not enforced when
// voice itself is off, even for an obviously bad provider value.
func TestValidate_TTSSkippedWhenVoiceDisabled(t *testing.T) {
	c := validBase()
	c.Voice.Enabled = false
	c.Voice.TTS.Provider = "totally-bogus"
	c.Voice.TTS.Voice = "晓晓"
	if err := c.Validate(); err != nil {
		t.Errorf("voice disabled should skip TTS validation: %v", err)
	}
}

// TestValidate_VoiceIDPlausibility covers isPlausibleVoiceID boundaries: ASCII IDs
// pass, while non-ASCII, over-long, and injection-prone values are rejected.
func TestValidate_VoiceIDPlausibility(t *testing.T) {
	tests := []struct {
		name    string
		voice   string
		wantErr bool
	}{
		{"empty_skipped", "", false},
		{"azure_neural_id", "zh-CN-XiaoxiaoNeural", false},
		{"openai_short_name", "alloy", false},
		{"max_len_128", strings.Repeat("a", 128), false},
		{"over_len_129", strings.Repeat("a", 129), true},
		{"chinese_display_name", "晓晓", true},
		{"double_quote", `voice"x`, true},
		{"backslash", `voice\x`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validBase()
			c.Voice.Enabled = true
			c.Voice.TTS.Provider = "edge-tts"
			c.Voice.TTS.Voice = tt.voice
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("voice %q should be rejected", tt.voice)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("voice %q should be accepted: %v", tt.voice, err)
			}
		})
	}
}

// TestValidationError_ErrorString covers the ValidationError formatter in both the
// with-suggestion and without-suggestion branches.
func TestValidationError_ErrorString(t *testing.T) {
	withSuggest := &ValidationError{Field: "server.port", Value: "0", Rule: "1-65535", Suggest: "use 16060"}
	msg := withSuggest.Error()
	for _, want := range []string{"server.port", "1-65535", "use 16060"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error string missing %q: %s", want, msg)
		}
	}

	noSuggest := &ValidationError{Field: "server.host", Value: "bad host", Rule: "no spaces"}
	msg2 := noSuggest.Error()
	if !strings.Contains(msg2, "server.host") || !strings.Contains(msg2, "no spaces") {
		t.Errorf("no-suggest error string malformed: %s", msg2)
	}
}

// TestValidationErrors_Aggregation verifies the collection formatter reports the
// count and is usable through errors.As, and that an empty collection is empty.
func TestValidationErrors_Aggregation(t *testing.T) {
	var empty ValidationErrors
	if empty.Error() != "" {
		t.Errorf("empty ValidationErrors should stringify to empty, got %q", empty.Error())
	}

	c := &Config{}
	c.Server.Port = 0           // err 1
	c.Server.MCPPort = 70000    // err 2
	c.Budget.MaxCost = -1       // err 3
	c.Budget.MaxDuration = "xx" // err 4
	err := c.Validate()
	if err == nil {
		t.Fatal("expected aggregated errors")
	}
	var ves ValidationErrors
	if !errors.As(err, &ves) {
		t.Fatalf("expected ValidationErrors, got %T", err)
	}
	if len(ves) < 4 {
		t.Errorf("expected >= 4 aggregated errors, got %d", len(ves))
	}
	if !strings.Contains(err.Error(), "校验失败") {
		t.Errorf("aggregate header missing: %s", err.Error())
	}
}

// TestValidate_RouterDefaultAgentMatrix covers the router.default_agent membership
// rule and the runtime-injection exemption (agents may be empty).
func TestValidate_RouterDefaultAgentMatrix(t *testing.T) {
	tests := []struct {
		name         string
		agents       []AgentStaticConfig
		defaultAgent string
		wantErr      bool
	}{
		{"no_agents_no_default", nil, "", false},
		{"no_agents_with_default_runtime_inject", nil, "anything", false},
		{"agents_default_matches", []AgentStaticConfig{{Name: "a"}, {Name: "b"}}, "b", false},
		{"agents_default_absent_rejected", []AgentStaticConfig{{Name: "a"}}, "missing", true},
		{"agents_empty_default_ok", []AgentStaticConfig{{Name: "a"}}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validBase()
			c.Router.Agents = tt.agents
			c.Router.DefaultAgent = tt.defaultAgent
			err := c.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("default_agent %q should be rejected", tt.defaultAgent)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("default_agent %q should be accepted: %v", tt.defaultAgent, err)
			}
		})
	}
}

// TestDefaultConfig_RemainingFields asserts default fields not covered by the
// existing TestDefaultConfig: budget, MCP port, cost, rate limit, and the
// nil-safe code-exec policy defaults.
func TestDefaultConfig_RemainingFields(t *testing.T) {
	c := DefaultConfig()

	if c.Server.MCPPort != 16070 {
		t.Errorf("MCPPort = %d, want 16070", c.Server.MCPPort)
	}
	if c.Server.Mode != "production" {
		t.Errorf("Mode = %q, want production", c.Server.Mode)
	}
	if c.Budget.MaxTokens != 500000 {
		t.Errorf("Budget.MaxTokens = %d, want 500000", c.Budget.MaxTokens)
	}
	if c.Budget.MaxDuration != "30m" {
		t.Errorf("Budget.MaxDuration = %q, want 30m", c.Budget.MaxDuration)
	}
	if c.Budget.MaxCost != 5.0 {
		t.Errorf("Budget.MaxCost = %v, want 5.0", c.Budget.MaxCost)
	}
	if c.Security.Cost.BudgetPerUser != 10.0 {
		t.Errorf("Cost.BudgetPerUser = %v, want 10.0", c.Security.Cost.BudgetPerUser)
	}
	if c.Security.Cost.BudgetGlobal != 1000.0 {
		t.Errorf("Cost.BudgetGlobal = %v, want 1000.0", c.Security.Cost.BudgetGlobal)
	}
	if c.Security.RateLimit.RequestsPerMinute != 20 {
		t.Errorf("RateLimit.RequestsPerMinute = %d, want 20", c.Security.RateLimit.RequestsPerMinute)
	}
	if c.LLM.Cache.Similarity != 0.92 {
		t.Errorf("LLM.Cache.Similarity = %v, want 0.92", c.LLM.Cache.Similarity)
	}
	if c.Storage.Driver != "sqlite" {
		t.Errorf("Storage.Driver = %q, want sqlite", c.Storage.Driver)
	}
	// Approval retains its legacy policy semantics. Network access is
	// deny-by-default and requires an explicit grant.
	if c.Skill.Builtin.CodeExecPolicy.CodeExecRequiresApproval() {
		t.Error("default code_exec policy should not require approval")
	}
	if c.Skill.Builtin.CodeExecPolicy.CodeExecNetworkAllowed() {
		t.Error("default code_exec policy should deny network")
	}
}

// TestDefaultConfig_PassesValidate is a guardrail: shipping defaults must not be
// rejected by the startup validator.
func TestDefaultConfig_PassesValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("DefaultConfig() must pass Validate(): %v", err)
	}
}

func TestValidate_AutonomyProfileEnum(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Security.Autonomy.Profile = "function-fisrt"
	if err := cfg.Validate(); err == nil {
		t.Fatal("invalid security.autonomy.profile should be rejected")
	}

	cfg.Security.Autonomy.Profile = "full_access"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("full_access should be accepted: %v", err)
	}
}
