package main

import (
	"context"
	"testing"

	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

func TestK12IMDelivererResolveDirectBindingsNormalizesDeduplicatesAndSorts(t *testing.T) {
	d, dispatcher, _ := newDelivererFixture(t)
	for _, rule := range []agentrouter.Rule{
		{ID: 9, Platform: " DingTalk ", InstanceID: " bot-b ", ChatID: " mom ", AgentName: "child-a"},
		{ID: 8, Platform: "dingtalk", InstanceID: "bot-a", ChatID: "mom", AgentName: "child-a"},
		{ID: 7, Platform: "DINGTALK", InstanceID: "bot-a", ChatID: "mom", AgentName: "child-a"},
		{ID: 6, Platform: "feishu", InstanceID: "fs-1", ChatID: "parent", AgentName: "child-a"},
		{ID: 5, Platform: "dingtalk", InstanceID: "bot-a", ChatID: "\x00dingtalk-group:g1", AgentName: "child-a"},
		{ID: 4, Platform: "dingtalk", InstanceID: "other", ChatID: "other", AgentName: "child-b"},
	} {
		if err := dispatcher.AddRule(rule); err != nil {
			t.Fatal(err)
		}
	}
	d.MarkReady()

	targets, err := d.ResolveTextTargets(context.Background(), "child-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 {
		t.Fatalf("active direct bindings=%d want 3: %+v", len(targets), targets)
	}
	got := [][3]string{}
	for _, target := range targets {
		got = append(got, [3]string{
			target.Target.Platform, target.Target.InstanceID, target.Target.ChatID,
		})
	}
	want := [][3]string{
		{"dingtalk", "bot-a", "mom"},
		{"dingtalk", "bot-b", "mom"},
		{"feishu", "fs-1", "parent"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("target[%d]=%v want %v (all=%v)", i, got[i], want[i], got)
		}
	}
	if targets[0].BindingID != "agent-rule:7" {
		t.Fatalf("duplicate target must choose stable smallest binding identity, got %q", targets[0].BindingID)
	}
}
