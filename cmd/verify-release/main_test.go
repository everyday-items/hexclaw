package main

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/featureflag"
	"github.com/hexagon-codes/hexclaw/release"
)

func TestRequireCompleteReleaseGateRejectsSkippedChecks(t *testing.T) {
	report := &release.Report{
		Results: []release.NamedResult{
			{Name: "tests-pass", Result: release.Result{Status: release.StatusPass}},
			{Name: "signatures-valid", Result: release.Result{Status: release.StatusSkip}},
			{Name: "migration-ready", Result: release.Result{Status: release.StatusSkip}},
		},
		PassCount: 1,
		SkipCount: 2,
	}

	err := requireCompleteReleaseGate(report)
	if err == nil || !strings.Contains(err.Error(), "signatures-valid") || !strings.Contains(err.Error(), "migration-ready") {
		t.Fatalf("release gate must fail closed with stable skipped check names, got %v", err)
	}
}

func TestRequireCompleteReleaseGateAcceptsNoSkippedChecks(t *testing.T) {
	report := &release.Report{}
	for _, check := range release.Default10() {
		report.Results = append(report.Results, release.NamedResult{
			Name: check.Name(), Result: release.Result{Status: release.StatusPass},
		})
		report.PassCount++
	}

	if err := requireCompleteReleaseGate(report); err != nil {
		t.Fatalf("fully implemented gate should pass completeness check: %v", err)
	}
}

func TestRequireCompleteReleaseGateRejectsMissingRequiredCheck(t *testing.T) {
	report := &release.Report{}
	for _, check := range release.Default10() {
		if check.Name() == "signatures-valid" {
			continue
		}
		report.Results = append(report.Results, release.NamedResult{
			Name: check.Name(), Result: release.Result{Status: release.StatusPass},
		})
		report.PassCount++
	}

	err := requireCompleteReleaseGate(report)
	if err == nil || !strings.Contains(err.Error(), "signatures-valid") {
		t.Fatalf("missing required gate must fail closed, got %v", err)
	}
}

func TestReplaceReleaseCheckUsesExactlyOneNamedImplementation(t *testing.T) {
	checks := release.Default10()
	replacement := release.FuncCheck{N: "sbom-fresh", Fn: func(_ context.Context) release.Result {
		return release.Result{Status: release.StatusPass}
	}}

	got, err := replaceReleaseCheck(checks, replacement)
	if err != nil {
		t.Fatalf("replace check: %v", err)
	}
	count := 0
	for _, check := range got {
		if check.Name() == "sbom-fresh" {
			count++
			if result := check.Run(context.Background()); result.Status != release.StatusPass {
				t.Fatalf("replacement was not installed: %+v", result)
			}
		}
	}
	if count != 1 {
		t.Fatalf("want exactly one sbom-fresh check, got %d", count)
	}
}

func TestBuildReleaseChecksWiresDefaultConfigValidation(t *testing.T) {
	checks, err := buildReleaseChecks(t.TempDir(), "0.5.0", []string{"version.txt"}, "sbom.cdx.json", 7)
	if err != nil {
		t.Fatal(err)
	}
	var configCheck release.Check
	for _, check := range checks {
		if check.Name() == "config-validated" {
			configCheck = check
			break
		}
	}
	if configCheck == nil {
		t.Fatal("config-validated check missing")
	}
	if result := configCheck.Run(context.Background()); result.Status != release.StatusPass {
		t.Fatalf("default config dry-run must be a real passing gate: %+v", result)
	}
}

func TestBuildReleaseChecksWiresSQLiteMigrationReadiness(t *testing.T) {
	checks, err := buildReleaseChecks(t.TempDir(), "0.5.0", []string{"version.txt"}, "sbom.cdx.json", 7)
	if err != nil {
		t.Fatal(err)
	}
	var migrationCheck release.Check
	for _, check := range checks {
		if check.Name() == "migration-ready" {
			migrationCheck = check
			break
		}
	}
	if migrationCheck == nil {
		t.Fatal("migration-ready check missing")
	}
	if result := migrationCheck.Run(context.Background()); result.Status != release.StatusPass {
		t.Fatalf("fresh/reopen SQLite migration verification must pass: %+v", result)
	}
}

func TestBuildReleaseChecksWiresPassingFlagStageAudit(t *testing.T) {
	checks, err := buildReleaseChecks(t.TempDir(), "0.5.0", []string{"version.txt"}, "sbom.cdx.json", 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range checks {
		if check.Name() != "flag-stage-audit" {
			continue
		}
		result := check.Run(context.Background())
		if result.Status == release.StatusSkip {
			t.Fatalf("flag-stage-audit must be wired, got SKIP: %+v", result)
		}
		if result.Status != release.StatusPass {
			t.Fatalf("production flag stage audit must pass: %+v", result)
		}
		return
	}
	t.Fatal("flag-stage-audit check missing")
}

func TestVerifyReleaseLinksEveryProductionFlagOwner(t *testing.T) {
	want := []string{
		"agent.factory.real",
		"config.tx.hotload.v1",
		"eval.framework.v1",
		"interactive.render.v1",
		"local.inference.coordinator.v1",
		"mcp.lifecycle.v2",
		"model.gateway.v1",
		"plugin.extension.v1",
		"pricing.layered.v1",
		"rag.pipeline.v1",
		"skill.pipeline.v1",
		"tool.lifecycle.v2",
		"tool.policy.engine",
	}
	registered := featureflag.Registered()
	if len(registered) != len(want) {
		t.Fatalf("verify-release linked flags = %d, want %d: %+v", len(registered), len(want), registered)
	}
	for i, flag := range registered {
		if flag.Name != want[i] {
			t.Fatalf("verify-release linked flag[%d] = %q, want %q", i, flag.Name, want[i])
		}
	}
}
