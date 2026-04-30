// Package release 实现 v0.4.0 发版门禁（Gate #46）和 Canary 发布流程（Gate #47）。
//
// Gate 是发版前必过的检查列表 —— 每条 Check 是独立的、确定性的、可重复运行的
// 单元，签出 PASS / WARN / FAIL 三种结果。Runner 按声明顺序执行，可配置
// fail-fast（任一 FAIL 即终止）或 collect-all（跑完所有再返回汇总报告）。
//
// 默认 10 道门禁（Default10）覆盖：
//
//	1. tests-pass        — 所有单元测试在 -count=1 -race 下通过
//	2. tests-coverage    — 覆盖率达到目标阈值
//	3. lint-clean        — 静态检查（go vet + lint）通过
//	4. docs-current      — README / CHANGELOG 包含本版本说明
//	5. version-bump      — go.mod / package.json 等版本号已更新
//	6. signatures-valid  — 关键二进制 / 包已签名
//	7. migration-ready   — DB / config 迁移脚本可双向（forward + rollback）
//	8. config-validated  — 默认 config 通过 validator
//	9. sbom-fresh        — Software Bill of Materials 已重新生成
//	10. flag-stage-audit — 所有 alpha flag 在交付前都 review 过 stage 升级
//
// Gate 不在 production runtime 调用 —— 它由 CI / 发版脚本在打包前调。本包只提供
// 框架 + Default10 的占位 Check，具体检查实现由调用方（CI 脚本 / verify-release
// 工具）注入。
package release

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Status 是单条 Check 的结果。
type Status string

const (
	StatusPass Status = "pass"
	StatusWarn Status = "warn" // 警告：发版可继续，但记录到报告
	StatusFail Status = "fail" // 阻断：发版必须停止
	StatusSkip Status = "skip" // 跳过：环境不适用（如 OSS 版本无 enterprise 签名校验）
)

// Check 是单条门禁检查。
type Check interface {
	// Name 返回检查的稳定 ID（如 "tests-pass"），用于报告 / CI label。
	Name() string
	// Description 返回一句话描述。
	Description() string
	// Run 执行检查并返回结果。
	Run(ctx context.Context) Result
}

// Result 是 Check.Run 的输出。
type Result struct {
	Status   Status
	Message  string
	Detail   string // 可选 —— 失败时附带 stderr / 报告路径等
	Duration time.Duration
}

// Gate 是一组 Check + 运行策略。
type Gate struct {
	// Checks 按声明顺序执行。
	Checks []Check
	// FailFast=true 时任一 FAIL 立即返回；false 时跑完所有再汇总。
	FailFast bool
}

// Report 是 Gate.Run 的汇总输出。
type Report struct {
	Results []NamedResult
	// PassCount/WarnCount/FailCount/SkipCount 统计。
	PassCount, WarnCount, FailCount, SkipCount int
	// Duration 整体耗时。
	Duration time.Duration
	// Aborted 表示因 FailFast 而提前终止。
	Aborted bool
}

// NamedResult 把 Check.Name 和 Result 绑在一起，便于序列化报告。
type NamedResult struct {
	Name        string
	Description string
	Result      Result
}

// Run 按 Gate.Checks 顺序执行所有检查。返回的 error 在有任何 FAIL 时为非 nil（
// 即使 FailFast=false），让调用方一行 `if err := gate.Run(...)` 即可阻断 release。
func (g *Gate) Run(ctx context.Context) (*Report, error) {
	if g == nil {
		return nil, errors.New("nil Gate")
	}
	rep := &Report{}
	start := time.Now()

	for _, c := range g.Checks {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		t0 := time.Now()
		result := c.Run(ctx)
		result.Duration = time.Since(t0)
		rep.Results = append(rep.Results, NamedResult{
			Name:        c.Name(),
			Description: c.Description(),
			Result:      result,
		})
		switch result.Status {
		case StatusPass:
			rep.PassCount++
		case StatusWarn:
			rep.WarnCount++
		case StatusFail:
			rep.FailCount++
			if g.FailFast {
				rep.Aborted = true
				rep.Duration = time.Since(start)
				return rep, &GateFailure{Report: rep}
			}
		case StatusSkip:
			rep.SkipCount++
		}
	}

	rep.Duration = time.Since(start)
	if rep.FailCount > 0 {
		return rep, &GateFailure{Report: rep}
	}
	return rep, nil
}

// GateFailure 是有 Check 返回 FAIL 时 Run 返回的 error 类型，便于上层通过 errors.As 识别。
type GateFailure struct {
	Report *Report
}

// Error 实现 error 接口。
func (e *GateFailure) Error() string {
	if e == nil || e.Report == nil {
		return "release gate failed"
	}
	var fails []string
	for _, r := range e.Report.Results {
		if r.Result.Status == StatusFail {
			fails = append(fails, r.Name)
		}
	}
	return fmt.Sprintf("release gate failed: %d failures (%s)", e.Report.FailCount, strings.Join(fails, ", "))
}

// ============== 内置 Check：FuncCheck ==============

// FuncCheck 把一个普通函数适配为 Check —— 简化接入 CI 脚本生成的检查。
type FuncCheck struct {
	N    string
	Desc string
	Fn   func(ctx context.Context) Result
}

// Name 返回 N。
func (f FuncCheck) Name() string { return f.N }

// Description 返回 Desc。
func (f FuncCheck) Description() string { return f.Desc }

// Run 调 Fn。
func (f FuncCheck) Run(ctx context.Context) Result {
	if f.Fn == nil {
		return Result{Status: StatusSkip, Message: "no function provided"}
	}
	return f.Fn(ctx)
}

// ============== Default10 占位实现 ==============

// Default10 返回 10 道默认门禁的占位 Check 列表。
//
// 每条 Check 默认返回 StatusSkip + "not implemented" —— 调用方（CI 脚本 / verify-release）
// 应通过 NewFuncCheck 替换具体实现。这样保留协议同时不强制占用 host runtime 实现。
func Default10() []Check {
	specs := []struct {
		name string
		desc string
	}{
		{"tests-pass", "所有 go test 单元测试在 -count=1 -race 下通过"},
		{"tests-coverage", "测试覆盖率达到 target 阈值（默认 70%）"},
		{"lint-clean", "go vet + lint 静态检查通过"},
		{"docs-current", "README / CHANGELOG 已包含本版本说明"},
		{"version-bump", "go.mod / package.json 等版本号已更新"},
		{"signatures-valid", "关键二进制 / 包已签名（macOS notarize / sigstore）"},
		{"migration-ready", "DB / config 迁移脚本可双向（forward + rollback）"},
		{"config-validated", "默认 config 通过 Validator + DryRun"},
		{"sbom-fresh", "Software Bill of Materials 已重新生成"},
		{"flag-stage-audit", "所有 alpha flag 已 review 升级目标"},
	}
	out := make([]Check, 0, len(specs))
	for _, s := range specs {
		s := s
		out = append(out, FuncCheck{
			N:    s.name,
			Desc: s.desc,
			Fn: func(_ context.Context) Result {
				return Result{Status: StatusSkip, Message: "not implemented; inject in CI"}
			},
		})
	}
	return out
}
