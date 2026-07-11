package router

import "testing"

func auditDispatcherWithMutableConfig(t *testing.T) *Dispatcher {
	t.Helper()
	d := New()
	temperature := 0.25
	if err := d.Register(AgentConfig{
		Name:        "assistant",
		Skills:      []string{"read"},
		Metadata:    map[string]string{"scope": "original"},
		Temperature: &temperature,
	}); err != nil {
		t.Fatal(err)
	}
	if err := d.AddRule(Rule{ID: 7, Platform: "web", AgentName: "assistant"}); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestAudit20260711GetAgentReturnsDetachedSnapshot(t *testing.T) {
	d := auditDispatcherWithMutableConfig(t)
	got, ok := d.GetAgent("assistant")
	if !ok {
		t.Fatal("registered agent missing")
	}
	got.Metadata["scope"] = "mutated"
	got.Skills[0] = "write"
	*got.Temperature = 0.9

	again, _ := d.GetAgent("assistant")
	if again.Metadata["scope"] != "original" || again.Skills[0] != "read" || *again.Temperature != 0.25 {
		t.Fatalf("GetAgent leaked mutable dispatcher state: %+v", again)
	}
}

func TestAudit20260711ListAgentsReturnsDetachedSnapshots(t *testing.T) {
	d := auditDispatcherWithMutableConfig(t)
	list := d.ListAgents()
	list[0].Metadata["scope"] = "mutated"
	list[0].Skills[0] = "write"

	again := d.ListAgents()
	if again[0].Metadata["scope"] != "original" || again[0].Skills[0] != "read" {
		t.Fatalf("ListAgents leaked mutable dispatcher state: %+v", again[0])
	}
}

func TestAudit20260711RouteReturnsDetachedSnapshots(t *testing.T) {
	d := auditDispatcherWithMutableConfig(t)
	result := d.Route(RouteRequest{Platform: "web"})
	if result == nil || result.Rule == nil || result.AgentConfig == nil {
		t.Fatalf("route result incomplete: %+v", result)
	}
	result.Rule.AgentName = "tampered"
	result.AgentConfig.Metadata["scope"] = "mutated"

	again := d.Route(RouteRequest{Platform: "web"})
	if again == nil || again.AgentName != "assistant" || again.Rule == nil || again.Rule.AgentName != "assistant" {
		t.Fatalf("Route leaked its internal rule pointer: %+v", again)
	}
	if again.AgentConfig.Metadata["scope"] != "original" {
		t.Fatalf("Route leaked its internal agent pointer: %+v", again.AgentConfig)
	}
}
