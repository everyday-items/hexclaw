package main

import (
	"context"
	"testing"
)

func TestK12IMBinder_PublishesPersistedRuleID(t *testing.T) {
	binder, dispatcher, store, _ := newIMBinderFixture(t)
	if err := binder.Bind(context.Background(), "dingtalk", "bot-1", "family", "child-a"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	memoryRules := dispatcher.ListRules()
	persistedRules, err := store.LoadRules(context.Background())
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	if len(memoryRules) != 1 || len(persistedRules) != 1 {
		t.Fatalf("rules memory=%+v persisted=%+v", memoryRules, persistedRules)
	}
	if memoryRules[0].ID <= 0 || memoryRules[0].ID != persistedRules[0].ID {
		t.Fatalf("published rule lost its persisted ID: memory=%+v persisted=%+v", memoryRules[0], persistedRules[0])
	}
}
