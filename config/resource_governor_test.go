package config

import (
	"strings"
	"testing"
)

func TestDefaultResourceGovernorConfigIsBounded(t *testing.T) {
	cfg := DefaultConfig().ResourceGovernor
	if cfg.VLMConcurrency != 2 || cfg.AcceleratorConcurrency != 1 ||
		cfg.CPUHeavyConcurrency != 2 || cfg.SQLiteWriteConcurrency != 1 {
		t.Fatalf("unexpected process resource defaults: %+v", cfg)
	}
	if cfg.BackgroundAging != "5s" || cfg.MaxInteractiveBurst != 8 {
		t.Fatalf("unexpected fairness defaults: %+v", cfg)
	}
}

func TestValidateRejectsUnboundedResourceGovernorConfig(t *testing.T) {
	tests := []struct {
		name  string
		apply func(*Config)
		field string
	}{
		{"vlm zero", func(c *Config) { c.ResourceGovernor.VLMConcurrency = 0 }, "resource_governor.vlm_concurrency"},
		{"accelerator zero", func(c *Config) { c.ResourceGovernor.AcceleratorConcurrency = 0 }, "resource_governor.accelerator_concurrency"},
		{"cpu zero", func(c *Config) { c.ResourceGovernor.CPUHeavyConcurrency = 0 }, "resource_governor.cpu_heavy_concurrency"},
		{"sqlite zero", func(c *Config) { c.ResourceGovernor.SQLiteWriteConcurrency = 0 }, "resource_governor.sqlite_write_concurrency"},
		{"aging invalid", func(c *Config) { c.ResourceGovernor.BackgroundAging = "soon" }, "resource_governor.background_aging"},
		{"burst zero", func(c *Config) { c.ResourceGovernor.MaxInteractiveBurst = 0 }, "resource_governor.max_interactive_burst"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			test.apply(cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("Validate error=%v, want field %s", err, test.field)
			}
		})
	}
}
