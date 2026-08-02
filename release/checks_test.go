package release

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

func TestCheckVersionBump_Pass(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"version":"0.4.0"}`), 0644)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`version = 0.4.0 here`), 0644)

	c := CheckVersionBump(dir, "0.4.0", "package.json", "config.json")
	res := c.Run(context.Background())
	if res.Status != StatusPass {
		t.Errorf("应 PASS；got %s: %s", res.Status, res.Message)
	}
}

func TestCheckVersionBump_FailMissing(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), []byte(`old version 0.3.5`), 0644)
	os.WriteFile(filepath.Join(dir, "b"), []byte(`v0.4.0 here`), 0644)

	c := CheckVersionBump(dir, "0.4.0", "a", "b")
	res := c.Run(context.Background())
	if res.Status != StatusFail {
		t.Errorf("应 FAIL（a 没含）；got %s", res.Status)
	}
	if !strings.Contains(res.Detail, "a") {
		t.Errorf("Detail 应列出 a；got %s", res.Detail)
	}
}

func TestCheckVersionBump_SkipEmptyVersion(t *testing.T) {
	c := CheckVersionBump(t.TempDir(), "")
	res := c.Run(context.Background())
	if res.Status != StatusSkip {
		t.Errorf("空 version 应 SKIP；got %s", res.Status)
	}
}

func TestCheckChangelogCurrent_Pass(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte("# Changelog\n## [0.4.0] - 2026-04-28\n\nfeat: x"), 0644)
	c := CheckChangelogCurrent(dir, "0.4.0", "CHANGELOG.md")
	res := c.Run(context.Background())
	if res.Status != StatusPass {
		t.Errorf("应 PASS；got %s: %s", res.Status, res.Message)
	}
}

func TestCheckChangelogCurrent_PassVPrefix(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte("## v0.4.0\n\nrelease"), 0644)
	c := CheckChangelogCurrent(dir, "0.4.0", "CHANGELOG.md")
	res := c.Run(context.Background())
	if res.Status != StatusPass {
		t.Errorf("v0.4.0 形式应 PASS；got %s", res.Status)
	}
}

func TestCheckChangelogCurrent_FailMissing(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte("## v0.3.0\n\nold release"), 0644)
	c := CheckChangelogCurrent(dir, "0.4.0", "CHANGELOG.md")
	res := c.Run(context.Background())
	if res.Status != StatusFail {
		t.Errorf("应 FAIL（无 0.4.0 章节）；got %s", res.Status)
	}
}

func TestCheckChangelogCurrent_FailMissingFile(t *testing.T) {
	c := CheckChangelogCurrent(t.TempDir(), "0.4.0", "CHANGELOG.md")
	res := c.Run(context.Background())
	if res.Status != StatusFail {
		t.Errorf("文件缺失应 FAIL；got %s", res.Status)
	}
}

func TestCheckTestsCoverageUsesStatementCoverage(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/releasecoverage\n\ngo 1.25\n",
		"calc.go": `package calc

func Covered() int { return 1 }
func Uncovered() int { return 2 }
`,
		"calc_test.go": `package calc

import "testing"

func TestCovered(t *testing.T) {
	if Covered() != 1 { t.Fatal("unexpected result") }
}
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if res := CheckTestsCoverage(dir, 40).Run(context.Background()); res.Status != StatusPass {
		t.Fatalf("coverage above threshold should pass: %+v", res)
	}
	if res := CheckTestsCoverage(dir, 90).Run(context.Background()); res.Status != StatusFail ||
		!strings.Contains(res.Detail, "threshold=90.00%") {
		t.Fatalf("coverage below threshold should fail with evidence: %+v", res)
	}
}

func TestCheckTestsCoverageRejectsInvalidThreshold(t *testing.T) {
	for _, threshold := range []float64{-0.1, 100.1} {
		res := CheckTestsCoverage(t.TempDir(), threshold).Run(context.Background())
		if res.Status != StatusFail || !strings.Contains(res.Detail, "between 0 and 100") {
			t.Fatalf("threshold %.2f must fail closed: %+v", threshold, res)
		}
	}
}

func TestBuildDefault10WithReal_ReplacesFour(t *testing.T) {
	dir := t.TempDir()
	checks := BuildDefault10WithReal(dir, "0.4.0", []string{"x"})
	if len(checks) != 10 {
		t.Errorf("应仍是 10 项；got %d", len(checks))
	}
	expected := map[string]string{
		"tests-pass":   "tests-pass",
		"lint-clean":   "lint-clean",
		"version-bump": "version-bump",
		"docs-current": "docs-current",
	}
	found := 0
	for _, c := range checks {
		if _, ok := expected[c.Name()]; ok {
			// FuncCheck 会有 N 字段，这里通过 description 含具体描述判断已被替换
			if !strings.Contains(c.Description(), "skip") {
				found++
			}
		}
	}
	if found < 4 {
		t.Errorf("应至少替换了 4 项；got %d", found)
	}
}

func TestBuildDefault10WithRealWiresCoverageCheck(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":          "module example.com/releasecoverage\n\ngo 1.25\n",
		"covered.go":      "package covered\n\nfunc Value() int { return 1 }\n",
		"covered_test.go": "package covered\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value() != 1 { t.Fatal(\"unexpected\") } }\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var coverage Check
	for _, check := range BuildDefault10WithReal(dir, "0.5.0", []string{"covered.go"}) {
		if check.Name() == "tests-coverage" {
			coverage = check
			break
		}
	}
	if coverage == nil {
		t.Fatal("tests-coverage check missing")
	}
	if res := coverage.Run(context.Background()); res.Status == StatusSkip {
		t.Fatalf("release coverage check must be implemented, got %+v", res)
	}
}

func TestCheckFlagStageAuditPassesExactReviewedAlphaSet(t *testing.T) {
	flags := []featureflag.Flag{
		{Name: "alpha.flag", Description: "alpha", Stage: featureflag.StageAlpha, SinceVersion: "0.5.0"},
		{Name: "beta.flag", Description: "beta", Stage: featureflag.StageBeta, SinceVersion: "0.5.0"},
		{Name: "ga.flag", Description: "ga", Stage: featureflag.StageGA, SinceVersion: "0.4.0"},
		{Name: "deprecated.flag", Description: "deprecated", Stage: featureflag.StageDeprecated, SinceVersion: "0.3.0"},
	}

	result := CheckFlagStageAudit(flags, []string{"alpha.flag"}).Run(context.Background())
	if result.Status != StatusPass {
		t.Fatalf("exact alpha review set must pass: %+v", result)
	}
}

func TestCheckFlagStageAuditFailsClosedOnAllowlistDrift(t *testing.T) {
	flags := []featureflag.Flag{
		{Name: "alpha.one", Description: "alpha one", Stage: featureflag.StageAlpha, SinceVersion: "0.5.0"},
		{Name: "ga.one", Description: "ga one", Stage: featureflag.StageGA, SinceVersion: "0.4.0"},
	}
	tests := []struct {
		name       string
		reviewed   []string
		wantDetail string
	}{
		{name: "missing alpha", reviewed: nil, wantDetail: "alpha.one"},
		{name: "unknown extra", reviewed: []string{"alpha.one", "unknown.flag"}, wantDetail: "unknown.flag"},
		{name: "registered non-alpha", reviewed: []string{"alpha.one", "ga.one"}, wantDetail: "ga.one"},
		{name: "duplicate review", reviewed: []string{"alpha.one", "alpha.one"}, wantDetail: "alpha.one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CheckFlagStageAudit(flags, tt.reviewed).Run(context.Background())
			if result.Status != StatusFail || !strings.Contains(result.Detail, tt.wantDetail) {
				t.Fatalf("allowlist drift must fail closed: %+v", result)
			}
		})
	}
}

func TestCheckFlagStageAuditFailsClosedOnInvalidFlagMetadata(t *testing.T) {
	valid := featureflag.Flag{
		Name: "valid.flag", Description: "valid description", Stage: featureflag.StageGA, SinceVersion: "0.5.0",
	}
	tests := []struct {
		name   string
		flags  []featureflag.Flag
		review []string
	}{
		{name: "empty name", flags: []featureflag.Flag{{Description: "x", Stage: featureflag.StageGA, SinceVersion: "0.5.0"}}},
		{name: "whitespace description", flags: []featureflag.Flag{{Name: "bad.description", Description: " ", Stage: featureflag.StageGA, SinceVersion: "0.5.0"}}},
		{name: "missing since version", flags: []featureflag.Flag{{Name: "bad.since", Description: "x", Stage: featureflag.StageGA}}},
		{name: "malformed since version", flags: []featureflag.Flag{{Name: "bad.version", Description: "x", Stage: featureflag.StageGA, SinceVersion: "next"}}},
		{name: "invalid stage", flags: []featureflag.Flag{{Name: "bad.stage", Description: "x", Stage: featureflag.Stage(99), SinceVersion: "0.5.0"}}},
		{name: "duplicate flag name", flags: []featureflag.Flag{valid, valid}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CheckFlagStageAudit(tt.flags, tt.review).Run(context.Background())
			if result.Status != StatusFail {
				t.Fatalf("invalid flag metadata must fail closed: %+v", result)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	s := "abcdefghij"
	if got := truncate(s, 100); got != s {
		t.Errorf("max>=len 应返回原文；got %s", got)
	}
	if got := truncate(s, 4); !strings.HasPrefix(got, "abcd") || !strings.Contains(got, "truncated") {
		t.Errorf("应截断 + 标记；got %s", got)
	}
}

func TestCheckSBOMFreshAcceptsFreshCycloneDXDocument(t *testing.T) {
	dir := t.TempDir()
	generatedAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	doc := fmt.Sprintf(`{
  "bomFormat": "CycloneDX",
  "specVersion": "1.5",
  "serialNumber": "urn:uuid:3e3dd57d-d388-4f5a-acfd-5788f7720031",
  "version": 1,
  "metadata": {"timestamp": %q},
  "components": [{"type":"application","name":"hexclaw","version":"0.5.0"}]
}`, generatedAt)
	if err := os.WriteFile(filepath.Join(dir, "sbom.cdx.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	res := CheckSBOMFresh(dir, "sbom.cdx.json", 7).Run(context.Background())
	if res.Status != StatusPass {
		t.Fatalf("fresh CycloneDX SBOM should pass: %+v", res)
	}
}

func TestCheckSBOMFreshAcceptsFreshSPDXDocument(t *testing.T) {
	dir := t.TempDir()
	generatedAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	doc := fmt.Sprintf(`{
  "spdxVersion": "SPDX-2.3",
  "SPDXID": "SPDXRef-DOCUMENT",
  "dataLicense": "CC0-1.0",
  "name": "hexclaw",
  "documentNamespace": "https://example.invalid/spdx/hexclaw/1",
  "creationInfo": {"created": %q, "creators": ["Tool: test"]},
  "packages": [{
    "name":"hexclaw",
    "SPDXID":"SPDXRef-Package-hexclaw",
    "versionInfo":"0.5.0",
    "downloadLocation":"NOASSERTION",
    "filesAnalyzed":false
  }]
}`, generatedAt)
	if err := os.WriteFile(filepath.Join(dir, "sbom.spdx.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	res := CheckSBOMFresh(dir, "sbom.spdx.json", 7).Run(context.Background())
	if res.Status != StatusPass {
		t.Fatalf("fresh SPDX SBOM should pass: %+v", res)
	}
}

func TestCheckSBOMFreshRejectsStaleDocumentEvenWithRecentMTime(t *testing.T) {
	dir := t.TempDir()
	generatedAt := time.Now().UTC().Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	doc := fmt.Sprintf(`{
  "bomFormat": "CycloneDX",
  "specVersion": "1.5",
  "serialNumber": "urn:uuid:3e3dd57d-d388-4f5a-acfd-5788f7720031",
  "version": 1,
  "metadata": {"timestamp": %q},
  "components": [{"type":"application","name":"hexclaw","version":"0.5.0"}]
}`, generatedAt)
	if err := os.WriteFile(filepath.Join(dir, "sbom.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	res := CheckSBOMFresh(dir, "sbom.json", 7).Run(context.Background())
	if res.Status != StatusFail || !strings.Contains(res.Message, "过期") {
		t.Fatalf("stale generation timestamp must fail: %+v", res)
	}
}

func TestCheckSBOMFreshRejectsNonSBOMJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sbom.json"), []byte(`{"hello":"world"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	res := CheckSBOMFresh(dir, "sbom.json", 7).Run(context.Background())
	if res.Status != StatusFail || !strings.Contains(res.Message, "格式") {
		t.Fatalf("arbitrary non-empty JSON must not satisfy SBOM gate: %+v", res)
	}
}

func TestCheckSBOMFreshRejectsUnknownSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	doc := fmt.Sprintf(`{
  "bomFormat": "CycloneDX",
  "specVersion": "9.9",
  "serialNumber": "urn:uuid:3e3dd57d-d388-4f5a-acfd-5788f7720031",
  "version": 1,
  "metadata": {"timestamp": %q},
  "components": []
}`, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, "sbom.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	res := CheckSBOMFresh(dir, "sbom.json", 7).Run(context.Background())
	if res.Status != StatusFail || !strings.Contains(res.Detail, "unsupported") {
		t.Fatalf("unknown schema version must fail closed: %+v", res)
	}
}

func TestCheckSBOMFreshRejectsEmptyInventory(t *testing.T) {
	generatedAt := time.Now().UTC().Format(time.RFC3339)
	tests := []struct {
		name string
		doc  string
	}{
		{
			name: "CycloneDX",
			doc: fmt.Sprintf(`{
  "bomFormat":"CycloneDX",
  "specVersion":"1.7",
  "serialNumber":"urn:uuid:3e3dd57d-d388-4f5a-acfd-5788f7720031",
  "version":1,
  "metadata":{"timestamp":%q},
  "components":[]
}`, generatedAt),
		},
		{
			name: "SPDX",
			doc: fmt.Sprintf(`{
  "spdxVersion":"SPDX-2.3",
  "SPDXID":"SPDXRef-DOCUMENT",
  "dataLicense":"CC0-1.0",
  "name":"hexclaw",
  "documentNamespace":"https://example.invalid/spdx/hexclaw/empty",
  "creationInfo":{"created":%q,"creators":["Tool: test"]},
  "packages":[]
}`, generatedAt),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "sbom.json"), []byte(tt.doc), 0o600); err != nil {
				t.Fatal(err)
			}

			res := CheckSBOMFresh(dir, "sbom.json", 7).Run(context.Background())
			if res.Status != StatusFail || !strings.Contains(res.Detail, "must not be empty") {
				t.Fatalf("empty %s inventory must fail closed: %+v", tt.name, res)
			}
		})
	}
}
