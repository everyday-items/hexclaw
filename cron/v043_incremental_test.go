package cron

import (
	"context"
	"strings"
	"testing"
)

func TestStateStore_RoundtripAndBounds(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := NewScheduler(db, nil, nil)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	st := s.state
	if st == nil {
		t.Fatal("scheduler should have a state store when db is set")
	}
	if err := st.Set("j", "k", "v1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if v, ok := st.Get("j", "k"); !ok || v != "v1" {
		t.Errorf("roundtrip got (%q,%v), want (v1,true)", v, ok)
	}
	if err := st.Set("j", "k", "v2"); err != nil { // update existing
		t.Fatalf("update: %v", err)
	}
	if v, _ := st.Get("j", "k"); v != "v2" {
		t.Errorf("update got %q, want v2", v)
	}
	if _, ok := st.Get("j", "missing"); ok {
		t.Error("missing key should report ok=false")
	}
	// 容量界：超大 value 必须报错，不静默截断/吞。
	if err := st.Set("j", "big", strings.Repeat("x", maxStateValueBytes+1)); err == nil {
		t.Error("oversized value should be rejected")
	}
}

func TestStarlarkStateBuiltins_PersistAcrossRuns(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := NewScheduler(db, nil, nil)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	eng := s.engines[RuntimeStarlark].(*StarlarkEngine)
	ctx := withStateJobID(context.Background(), "job1")

	// run 1: state_get returns default, then state_set persists.
	r1, err := eng.Execute(ctx, &JobSpec{Runtime: RuntimeStarlark, TimeoutSec: 10, Script: `
prev = state_get("seen", "none")
state_set("seen", "yes")
emit({"status": "success", "data": prev})
`})
	if err != nil || r1.Status != "success" {
		t.Fatalf("run1 err=%v status=%s", err, r1.Status)
	}
	if v, ok := s.state.Get("job1", "seen"); !ok || v != "yes" {
		t.Errorf("state_set not persisted: got (%q,%v)", v, ok)
	}

	// run 2: state_get must read the value persisted by run 1.
	if _, err := eng.Execute(ctx, &JobSpec{Runtime: RuntimeStarlark, TimeoutSec: 10, Script: `
state_set("echo", state_get("seen", "none"))
emit({"status": "success"})
`}); err != nil {
		t.Fatalf("run2: %v", err)
	}
	if v, _ := s.state.Get("job1", "echo"); v != "yes" {
		t.Errorf("state_get did not read prior run's value: echo=%q, want yes", v)
	}
}

func TestStateSet_NoStoreErrors(t *testing.T) {
	eng := NewStarlarkEngine() // no store wired
	r, err := eng.Execute(context.Background(), &JobSpec{
		Runtime: RuntimeStarlark, TimeoutSec: 10, Script: `state_set("k", "v")` + "\n" + `emit({"status":"success"})`,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// state_set with no store must error loudly (not silently drop), surfacing as a run error.
	if r.Status != "error" {
		t.Errorf("state_set without a store should error; got status=%q", r.Status)
	}
}

func TestOnlyIfChanged_SuppressesUnchanged(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	s := NewScheduler(db, nil, nil)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	r := &RunResult{Status: "success", Stdout: "same content"}
	if s.outputUnchanged("j", r) {
		t.Error("first run should be treated as changed (deliver)")
	}
	if !s.outputUnchanged("j", r) {
		t.Error("identical second run should be unchanged (suppress delivery)")
	}
	if s.outputUnchanged("j", &RunResult{Status: "success", Stdout: "different content"}) {
		t.Error("changed content should be treated as changed (deliver)")
	}
}
