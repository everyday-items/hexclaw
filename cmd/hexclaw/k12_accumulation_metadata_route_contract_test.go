package main

import (
	"os"
	"strings"
	"testing"
)

func TestProductionAccumulationMetadataUsesTutorAgentRouteWithoutDefaultFallback(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	start := strings.Index(text, "accumulationMetadataGenFn :=")
	end := strings.Index(text, "// 单题练习生成只消费 usecase")
	if start < 0 || end <= start {
		t.Fatal("production accumulation metadata composition block not found")
	}
	block := text[start:end]
	if strings.Contains(block, "router.Route(cctx)") ||
		strings.Contains(block, "router.ProviderModel(providerName)") {
		t.Fatal("accumulation metadata still resolves the mutable default route")
	}
	for _, required := range []string{
		"skill.RoutedAgentName(cctx)",
		"agentRouter.GetAgent(agentName)",
		"agentConfig.Provider",
		"agentConfig.Model",
		"resolveK12PracticeModelSnapshotWithCapabilityReceipt(",
		"router.Get(snapshot.Provider)",
		"Model: snapshot.Model",
	} {
		if !strings.Contains(block, required) {
			t.Fatalf("production accumulation metadata route is missing %q", required)
		}
	}
	for expression, want := range map[string]int{
		"resolveK12PracticeModelSnapshotWithCapabilityReceipt(": 1,
		"router.Get(snapshot.Provider)":                         1,
		"provider.Complete(":                                    1,
		"Model: snapshot.Model":                                 1,
	} {
		if got := strings.Count(block, expression); got != want {
			t.Fatalf("production accumulation metadata route %q count=%d, want %d", expression, got, want)
		}
	}
}
