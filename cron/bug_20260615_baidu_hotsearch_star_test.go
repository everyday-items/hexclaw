package cron

// BUG-20260615 (P1 architecture): the Baidu collector migrated from a python
// script (needs python3) to a Starlark script (pure-Go engine, zero runtime
// dependency, works on Windows). This locks that the archived .star stays valid
// against the real StarlarkEngine and keeps its defining properties.

import (
	"os"
	"strings"
	"testing"
)

const baiduHotsearchStarPath = "scripts/baidu_hotsearch.star"

func loadBaiduStar(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(baiduHotsearchStarPath)
	if err != nil {
		t.Fatalf("read archived starlark script: %v", err)
	}
	return string(b)
}

func TestBug20260615_BaiduStar_PassesEngineValidation(t *testing.T) {
	if err := NewStarlarkEngine().Validate(loadBaiduStar(t)); err != nil {
		t.Fatalf("archived collector must pass StarlarkEngine.Validate, got: %v", err)
	}
}

func TestBug20260615_BaiduStar_Invariants(t *testing.T) {
	s := loadBaiduStar(t)
	if strings.Contains(s, "load(") {
		t.Error("must not use load() — no module loading in the sandbox")
	}
	if !strings.Contains(s, "s-data") {
		t.Error("must parse the embedded s-data JSON block (stable extraction path)")
	}
	if !strings.Contains(s, "non-2xx") {
		t.Error("must verify HTTP response status (non-2xx => error)")
	}
	if !strings.Contains(s, "emit(") {
		t.Error("must call emit({...}) to return its result")
	}
}
