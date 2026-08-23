package main

import "testing"

// TestShouldPersistRouterConfigSeed 只允许在持久化 Agent 为空时写入配置种子。
func TestShouldPersistRouterConfigSeed(t *testing.T) {
	tests := []struct {
		name            string
		loadedFromStore bool
		effectiveAgents int
		want            bool
	}{
		{name: "empty store with config agents", loadedFromStore: false, effectiveAgents: 1, want: true},
		{name: "existing store agents", loadedFromStore: true, effectiveAgents: 1, want: false},
		{name: "empty store without config agents", loadedFromStore: false, effectiveAgents: 0, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldPersistRouterConfigSeed(tt.loadedFromStore, tt.effectiveAgents); got != tt.want {
				t.Fatalf("shouldPersistRouterConfigSeed(%t, %d) = %t, want %t", tt.loadedFromStore, tt.effectiveAgents, got, tt.want)
			}
		})
	}
}
