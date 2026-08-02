// verify-release 是 v0.4.0 #46+#47 串联的发版工具链 CLI。
//
// 一次运行串完三件事：
//
//  1. release.Gate（Default10WithReal） —— 静态门禁：版本一致 / 测试 / lint / 安全扫描
//  2. eval.V04SuiteFull —— 12 条 evalcase（含 5 条真业务路径）回归
//  3. release.NewRollout（DefaultStages） —— canary 状态机 dry-run（不真发布，仅串通流程）
//
// 任一阶段非 0 退出码即阻断发版。CI 流水线集成：
//
//	go run ./cmd/verify-release -repo $PWD -version 0.5.0-beta \
//	    -version-files hexclaw.go,cmd/hexclaw/main.go,api/openapi.yaml
//
// flag eval.framework.v1 在本工具内部强制 ON（CI 必须跑 eval；不接受 OFF 跳过）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/eval"
	"github.com/hexagon-codes/hexclaw/featureflag"
	_ "github.com/hexagon-codes/hexclaw/featureflag/production"
	"github.com/hexagon-codes/hexclaw/release"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
	storagesqlite "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

func main() {
	var (
		repoRoot     = flag.String("repo", ".", "仓库根目录（用于版本一致检查）")
		version      = flag.String("version", "", "期望发版版本（必填，如 0.5.0-beta）")
		versionFiles = flag.String("version-files", "package.json", "逗号分隔的版本文件列表（在仓库根目录下相对路径）")
		sbomFile     = flag.String("sbom-file", "sbom.cdx.json", "CycloneDX/SPDX JSON SBOM（相对仓库根目录）")
		sbomMaxAge   = flag.Int("sbom-max-age-days", 7, "SBOM 文档生成时间允许的最大天数")
		canaryDryRun = flag.Bool("canary-dry-run", true, "canary 阶段做 dry-run（不真发布）")
		failFast     = flag.Bool("fail-fast", true, "Gate 阶段任一 FAIL 立即返回")
	)
	flag.Parse()

	if *version == "" {
		fmt.Fprintln(os.Stderr, "❌ -version 必填（如 0.5.0-beta）")
		os.Exit(2)
	}

	files := splitCSV(*versionFiles)
	ctx := context.Background()

	// ============== Phase 1: 静态门禁 ==============
	fmt.Printf("=== Phase 1: release.Gate (Default10WithReal) ===\n")
	checks, err := buildReleaseChecks(*repoRoot, *version, files, *sbomFile, *sbomMaxAge)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Gate configuration failed: %v\n", err)
		os.Exit(2)
	}
	gate := &release.Gate{
		Checks:   checks,
		FailFast: *failFast,
	}
	rep, err := gate.Run(ctx)
	printGateReport(rep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Gate failed: %v\n", err)
		os.Exit(1)
	}
	if err := requireCompleteReleaseGate(rep); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Gate incomplete: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✅ Gate passed")

	// ============== Phase 2: Eval 套件 ==============
	fmt.Printf("\n=== Phase 2: eval.V04SuiteFull (mock + real) ===\n")
	evalCtx := featureflag.WithContext(ctx, featureflag.NewStatic(featureflag.Registered(), map[string]bool{
		eval.FlagEvalFrameworkV1: true,
	}))
	suite := eval.V04SuiteFull()
	evalRep, err := suite.Run(evalCtx)
	printEvalReport(evalRep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Eval failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ Eval passed (%d/%d)\n", evalRep.Passed, evalRep.Total)

	// ============== Phase 3: Canary dry-run ==============
	fmt.Printf("\n=== Phase 3: release.Rollout (DefaultStages) ===\n")
	if err := canaryDryRunFlow(ctx, *canaryDryRun); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Canary failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✅ Canary dry-run completed")

	fmt.Println("\n🎉 verify-release 全部 3 阶段通过；可以发版。")
}

func buildReleaseChecks(repoRoot, version string, versionFiles []string, sbomFile string, sbomMaxAgeDays int) ([]release.Check, error) {
	checks := release.BuildDefault10WithReal(repoRoot, version, versionFiles)
	replacements := []release.Check{
		release.CheckConfigValidated(func(ctx context.Context) error {
			return (config.BuiltinValidator{}).DryRun(ctx, config.DefaultConfig())
		}),
		release.CheckMigrationReady(verifySQLiteMigrations),
		release.CheckSBOMFresh(repoRoot, sbomFile, sbomMaxAgeDays),
		release.CheckFlagStageAudit(featureflag.Registered(), []string{eval.FlagEvalFrameworkV1}),
	}
	var err error
	for _, replacement := range replacements {
		checks, err = replaceReleaseCheck(checks, replacement)
		if err != nil {
			return nil, err
		}
	}
	return checks, nil
}

func verifySQLiteMigrations(ctx context.Context) error {
	if len(migrate.All) == 0 {
		return errors.New("migration registry is empty")
	}
	for i, migration := range migrate.All {
		want := i + 1
		if migration.Version != want {
			return fmt.Errorf("migration registry is not contiguous at index %d: got v%d want v%d", i, migration.Version, want)
		}
		if strings.TrimSpace(migration.Description) == "" {
			return fmt.Errorf("migration v%d has no description", migration.Version)
		}
	}

	dir, err := os.MkdirTemp("", "hexclaw-migration-gate-*")
	if err != nil {
		return fmt.Errorf("create migration sandbox: %w", err)
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "migration-readiness.db")

	// First boot proves a clean install; the second boot proves the persisted
	// ledger is restart-safe and does not replay already committed migrations.
	if err := verifySQLiteMigrationBoot(ctx, dbPath); err != nil {
		return fmt.Errorf("fresh install: %w", err)
	}
	if err := verifySQLiteMigrationBoot(ctx, dbPath); err != nil {
		return fmt.Errorf("reopen: %w", err)
	}
	return nil
}

func verifySQLiteMigrationBoot(ctx context.Context, dbPath string) error {
	store, err := storagesqlite.New(dbPath)
	if err != nil {
		return fmt.Errorf("open SQLite: %w", err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		return err
	}

	var count, minVersion, maxVersion int
	if err := store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MIN(version), 0), COALESCE(MAX(version), 0)
		FROM schema_migrations`).Scan(&count, &minVersion, &maxVersion); err != nil {
		return fmt.Errorf("read migration ledger: %w", err)
	}
	if count != len(migrate.All) || minVersion != 1 || maxVersion != migrate.All[len(migrate.All)-1].Version {
		return fmt.Errorf("migration ledger mismatch: count=%d/%d range=v%d..v%d", count, len(migrate.All), minVersion, maxVersion)
	}
	for _, migration := range migrate.All {
		var description string
		if err := store.DB().QueryRowContext(ctx,
			"SELECT description FROM schema_migrations WHERE version = ?", migration.Version,
		).Scan(&description); err != nil {
			return fmt.Errorf("read migration v%d ledger: %w", migration.Version, err)
		}
		if description != migration.Description {
			return fmt.Errorf("migration v%d description mismatch", migration.Version)
		}
	}

	quickRows, err := store.DB().QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return fmt.Errorf("SQLite quick_check: %w", err)
	}
	for quickRows.Next() {
		var result string
		if err := quickRows.Scan(&result); err != nil {
			quickRows.Close()
			return fmt.Errorf("scan SQLite quick_check: %w", err)
		}
		if result != "ok" {
			quickRows.Close()
			return fmt.Errorf("SQLite quick_check failed: %s", result)
		}
	}
	if err := quickRows.Err(); err != nil {
		quickRows.Close()
		return fmt.Errorf("SQLite quick_check iteration: %w", err)
	}
	if err := quickRows.Close(); err != nil {
		return fmt.Errorf("close SQLite quick_check: %w", err)
	}

	foreignKeyRows, err := store.DB().QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("SQLite foreign_key_check: %w", err)
	}
	if foreignKeyRows.Next() {
		foreignKeyRows.Close()
		return errors.New("SQLite foreign_key_check reported a violation")
	}
	if err := foreignKeyRows.Err(); err != nil {
		foreignKeyRows.Close()
		return fmt.Errorf("SQLite foreign_key_check iteration: %w", err)
	}
	if err := foreignKeyRows.Close(); err != nil {
		return fmt.Errorf("close SQLite foreign_key_check: %w", err)
	}
	return nil
}

func replaceReleaseCheck(checks []release.Check, replacement release.Check) ([]release.Check, error) {
	if replacement == nil || strings.TrimSpace(replacement.Name()) == "" {
		return nil, fmt.Errorf("replacement check is nil or unnamed")
	}
	out := append([]release.Check(nil), checks...)
	replaced := 0
	for i, check := range out {
		if check != nil && check.Name() == replacement.Name() {
			out[i] = replacement
			replaced++
		}
	}
	if replaced != 1 {
		return nil, fmt.Errorf("check %q replacement count=%d, want 1", replacement.Name(), replaced)
	}
	return out, nil
}

func requireCompleteReleaseGate(report *release.Report) error {
	if report == nil {
		return fmt.Errorf("release gate report is nil")
	}
	counts := make(map[string]int, len(report.Results))
	var skipped []string
	for _, result := range report.Results {
		counts[result.Name]++
		if result.Result.Status == release.StatusSkip {
			skipped = append(skipped, result.Name)
		}
	}
	var invalidCardinality []string
	for _, required := range release.Default10() {
		if count := counts[required.Name()]; count != 1 {
			invalidCardinality = append(invalidCardinality, fmt.Sprintf("%s(count=%d)", required.Name(), count))
		}
	}
	if len(invalidCardinality) > 0 || len(skipped) > 0 {
		parts := make([]string, 0, 2)
		if len(invalidCardinality) > 0 {
			parts = append(parts, "missing/duplicated required checks: "+strings.Join(invalidCardinality, ", "))
		}
		if len(skipped) > 0 {
			parts = append(parts, fmt.Sprintf("%d required checks are not implemented: %s", len(skipped), strings.Join(skipped, ", ")))
		}
		return errors.New(strings.Join(parts, "; "))
	}
	return nil
}

// canaryDryRunFlow 在 dry-run=true 时只验证 stage 推进逻辑（用 0 dwell + 永远健康
// 的 gate）；dry-run=false 时按真实 DefaultStages 推进（生产慎用，需真实 healthGate）。
func canaryDryRunFlow(ctx context.Context, dryRun bool) error {
	stages := release.DefaultStages()
	if dryRun {
		// 把 MinDuration 全部清零，让本工具能在 1s 内串完 4 阶段
		for i := range stages {
			stages[i].MinDuration = 0
		}
	}

	healthAlwaysOK := release.HealthFunc(func(_ context.Context, _ release.CanaryStage) error { return nil })
	rollout, err := release.NewRollout(stages, healthAlwaysOK)
	if err != nil {
		return err
	}

	for {
		if err := rollout.Advance(ctx); err != nil {
			return fmt.Errorf("Advance: %w", err)
		}
		fmt.Printf("  → state=%s percent=%d stage=%q\n",
			rollout.State(), rollout.CurrentPercent(), rollout.CurrentStage().Name)
		if rollout.State() == release.RolloutCompleted {
			return nil
		}
		// 防御：理论上 stages 已知有限，这里加硬上限避免无限循环
		if rollout.CurrentPercent() == 100 {
			return nil
		}
		time.Sleep(10 * time.Millisecond) // 让日志清晰可读
	}
}

func printGateReport(rep *release.Report) {
	if rep == nil {
		return
	}
	for _, r := range rep.Results {
		mark := "  "
		switch r.Result.Status {
		case release.StatusPass:
			mark = "✅"
		case release.StatusWarn:
			mark = "⚠️ "
		case release.StatusFail:
			mark = "❌"
		case release.StatusSkip:
			mark = "⏭ "
		}
		fmt.Printf("  %s %-32s %s\n", mark, r.Name, r.Result.Message)
		if r.Result.Detail != "" {
			fmt.Printf("       %s\n", r.Result.Detail)
		}
	}
	fmt.Printf("  -- pass=%d warn=%d fail=%d skip=%d (%v)\n",
		rep.PassCount, rep.WarnCount, rep.FailCount, rep.SkipCount, rep.Duration)
}

func printEvalReport(rep *eval.Report) {
	if rep == nil {
		return
	}
	for _, r := range rep.Results {
		mark := "✅"
		if !r.Pass {
			mark = "❌"
		}
		fmt.Printf("  %s %-40s (%v)\n", mark, r.CaseID, r.Duration)
		for _, f := range r.Failures {
			fmt.Printf("       ↳ %s\n", f)
		}
	}
}

func splitCSV(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// _ 防 unused import 警告 —— errors 留作后续 errors.As(GateFailure) 使用。
var _ = errors.New
