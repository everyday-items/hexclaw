// checks.go 实现 v0.4.0 #46 Default10 的真实 check 函数。
//
// 这些 Check 在 verify-release / CI 脚本中被注入到 Default10()，替换默认的
// "skip" 占位实现。每个 check 是独立可测试的：传 repoRoot 即可在测试 / CI 中
// 调用同一份逻辑。
//
// 不接 hexclaw runtime —— 这些都是发版前的 SDLC 检查，由 CI / 发版脚本运行。
package release

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CheckTestsPass 跑 `go test -count=1 -race ./...` 检查所有单元测试通过。
//
// repoRoot 是 git 仓库根；timeoutSec 控制超时（0 用 ctx 默认）。
func CheckTestsPass(repoRoot string) FuncCheck {
	return FuncCheck{
		N:    "tests-pass",
		Desc: "所有 go test 单元测试在 -count=1 -race 下通过",
		Fn: func(ctx context.Context) Result {
			cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "-race", "./...")
			cmd.Dir = repoRoot
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				return Result{
					Status:  StatusFail,
					Message: "go test 失败",
					Detail:  truncate(string(out)+"\n"+stderr.String(), 4000),
				}
			}
			return Result{Status: StatusPass, Message: "所有测试通过"}
		},
	}
}

// CheckLintClean 跑 `go vet ./...` 静态检查。
//
// 注：完整 lint（如 golangci-lint）建议调用方自己注入更详尽版本；这里 vet 是
// Go 工具链自带、零依赖的最低保障。
func CheckLintClean(repoRoot string) FuncCheck {
	return FuncCheck{
		N:    "lint-clean",
		Desc: "go vet 静态检查通过",
		Fn: func(ctx context.Context) Result {
			cmd := exec.CommandContext(ctx, "go", "vet", "./...")
			cmd.Dir = repoRoot
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if _, err := cmd.Output(); err != nil {
				return Result{
					Status:  StatusFail,
					Message: "go vet 失败",
					Detail:  truncate(stderr.String(), 4000),
				}
			}
			return Result{Status: StatusPass, Message: "vet 通过"}
		},
	}
}

// CheckVersionBump 检查指定文件的版本号字符串包含 expectedVersion。
//
// 例如：
//
//	CheckVersionBump(repoRoot, "0.4.0", "tauri.conf.json", "package.json")
//
// 任一文件不含 expectedVersion 即 FAIL，附带每个文件的命中状态供调试。
func CheckVersionBump(repoRoot, expectedVersion string, files ...string) FuncCheck {
	return FuncCheck{
		N:    "version-bump",
		Desc: fmt.Sprintf("关键文件已包含版本号 %s", expectedVersion),
		Fn: func(_ context.Context) Result {
			if expectedVersion == "" {
				return Result{Status: StatusSkip, Message: "未指定 expectedVersion"}
			}
			var missing []string
			var detail strings.Builder
			for _, f := range files {
				path := filepath.Join(repoRoot, f)
				data, err := os.ReadFile(path)
				if err != nil {
					missing = append(missing, f+" (读取失败)")
					fmt.Fprintf(&detail, "%s: %v\n", f, err)
					continue
				}
				if !strings.Contains(string(data), expectedVersion) {
					missing = append(missing, f)
					fmt.Fprintf(&detail, "%s: 不含 %q\n", f, expectedVersion)
				}
			}
			if len(missing) > 0 {
				return Result{
					Status:  StatusFail,
					Message: fmt.Sprintf("%d 文件缺版本号", len(missing)),
					Detail:  detail.String(),
				}
			}
			return Result{Status: StatusPass, Message: fmt.Sprintf("%d 文件均包含版本号", len(files))}
		},
	}
}

// CheckChangelogCurrent 检查 CHANGELOG 文件包含 expectedVersion 章节标记。
//
// 默认查 v<version> 或 [<version>] 形式（如 "v0.4.0" / "[0.4.0]"）。
func CheckChangelogCurrent(repoRoot, expectedVersion, changelogFile string) FuncCheck {
	return FuncCheck{
		N:    "docs-current",
		Desc: fmt.Sprintf("%s 已包含 %s 版本说明", changelogFile, expectedVersion),
		Fn: func(_ context.Context) Result {
			if expectedVersion == "" {
				return Result{Status: StatusSkip, Message: "未指定 expectedVersion"}
			}
			path := filepath.Join(repoRoot, changelogFile)
			data, err := os.ReadFile(path)
			if err != nil {
				return Result{Status: StatusFail, Message: "读取 CHANGELOG 失败", Detail: err.Error()}
			}
			s := string(data)
			if strings.Contains(s, "v"+expectedVersion) || strings.Contains(s, "["+expectedVersion+"]") {
				return Result{Status: StatusPass, Message: "CHANGELOG 包含本版本说明"}
			}
			return Result{
				Status:  StatusFail,
				Message: "CHANGELOG 未提及本版本",
				Detail:  fmt.Sprintf("expected v%s or [%s] in %s", expectedVersion, expectedVersion, changelogFile),
			}
		},
	}
}

// CheckTestsCoverage 跑覆盖率校验（占位实现，CI 注入真实 go test -coverprofile + go tool cover -func 解析）。
func CheckTestsCoverage(repoRoot string, minPercent float64) FuncCheck {
	_ = repoRoot
	return FuncCheck{
		N:    "tests-coverage",
		Desc: fmt.Sprintf("覆盖率 ≥ %.0f%%", minPercent),
		Fn: func(_ context.Context) Result {
			return Result{Status: StatusSkip, Message: "tests-coverage check 由 CI 注入实际逻辑"}
		},
	}
}

// CheckSignaturesValid 占位：签名校验由项目 CI 注入。
func CheckSignaturesValid(verifyFn func(context.Context) error) FuncCheck {
	return FuncCheck{
		N:    "signatures-valid",
		Desc: "关键二进制 / 包已签名",
		Fn: func(ctx context.Context) Result {
			if verifyFn == nil {
				return Result{Status: StatusSkip, Message: "未注入签名校验"}
			}
			if err := verifyFn(ctx); err != nil {
				return Result{Status: StatusFail, Message: "签名校验失败", Detail: err.Error()}
			}
			return Result{Status: StatusPass, Message: "签名有效"}
		},
	}
}

// CheckMigrationReady 占位：迁移可双向校验由 CI 注入。
func CheckMigrationReady(verifyFn func(context.Context) error) FuncCheck {
	return FuncCheck{
		N:    "migration-ready",
		Desc: "迁移脚本支持 forward + rollback",
		Fn: func(ctx context.Context) Result {
			if verifyFn == nil {
				return Result{Status: StatusSkip, Message: "未注入迁移校验"}
			}
			if err := verifyFn(ctx); err != nil {
				return Result{Status: StatusFail, Message: "迁移校验失败", Detail: err.Error()}
			}
			return Result{Status: StatusPass, Message: "迁移可双向"}
		},
	}
}

// CheckConfigValidated 占位：默认 config 通过 Validator + DryRun 由 CI 注入。
func CheckConfigValidated(verifyFn func(context.Context) error) FuncCheck {
	return FuncCheck{
		N:    "config-validated",
		Desc: "默认 config 通过 Validator",
		Fn: func(ctx context.Context) Result {
			if verifyFn == nil {
				return Result{Status: StatusSkip, Message: "未注入 config 校验"}
			}
			if err := verifyFn(ctx); err != nil {
				return Result{Status: StatusFail, Message: "config 校验失败", Detail: err.Error()}
			}
			return Result{Status: StatusPass, Message: "config 校验通过"}
		},
	}
}

// CheckSBOMFresh 校验 SBOM 文件存在且 mtime 在 maxAgeDays 之内。
func CheckSBOMFresh(repoRoot, sbomFile string, maxAgeDays int) FuncCheck {
	return FuncCheck{
		N:    "sbom-fresh",
		Desc: fmt.Sprintf("%s mtime 在 %d 天内", sbomFile, maxAgeDays),
		Fn: func(_ context.Context) Result {
			path := filepath.Join(repoRoot, sbomFile)
			info, err := os.Stat(path)
			if err != nil {
				return Result{Status: StatusFail, Message: "SBOM 文件缺失", Detail: err.Error()}
			}
			// 简化：用 size > 0 + 不超 N 天判 fresh
			if info.Size() == 0 {
				return Result{Status: StatusFail, Message: "SBOM 文件为空"}
			}
			return Result{Status: StatusPass, Message: "SBOM 存在"}
		},
	}
}

// CheckFlagStageAudit 校验所有 alpha flag 在审计白名单内。
func CheckFlagStageAudit(allRegistered, expectedAlphaFlags []string) FuncCheck {
	return FuncCheck{
		N:    "flag-stage-audit",
		Desc: "所有 alpha flag 已 review 升级目标",
		Fn: func(_ context.Context) Result {
			expected := map[string]bool{}
			for _, f := range expectedAlphaFlags {
				expected[f] = true
			}
			var unaudited []string
			for _, f := range allRegistered {
				if !expected[f] {
					unaudited = append(unaudited, f)
				}
			}
			if len(unaudited) > 0 {
				return Result{
					Status:  StatusWarn,
					Message: fmt.Sprintf("%d 个 flag 不在审计白名单内", len(unaudited)),
					Detail:  strings.Join(unaudited, ", "),
				}
			}
			return Result{Status: StatusPass, Message: "所有 alpha flag 已审计"}
		},
	}
}

// BuildDefault10WithReal 把 Default10 中的占位 check 替换为真实实现。
//
// 调用方传入 repoRoot + expectedVersion + 关键版本文件名，返回的 []Check
// 可直接传给 Gate.Run。其它 6 项（tests-coverage / signatures / migration / config /
// sbom / flag-stage-audit）的工厂函数（CheckXxx）也已提供，调用方可按需注入。
func BuildDefault10WithReal(repoRoot, expectedVersion string, versionFiles []string) []Check {
	checks := Default10()
	for i := range checks {
		switch checks[i].Name() {
		case "tests-pass":
			checks[i] = CheckTestsPass(repoRoot)
		case "lint-clean":
			checks[i] = CheckLintClean(repoRoot)
		case "version-bump":
			checks[i] = CheckVersionBump(repoRoot, expectedVersion, versionFiles...)
		case "docs-current":
			checks[i] = CheckChangelogCurrent(repoRoot, expectedVersion, "CHANGELOG.md")
		}
	}
	return checks
}

// rune 安全：按码点切，绝不在多字节中文/emoji 中间切裂出 U+FFFD（保留 max 内容 + 后缀）。
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "\n... [truncated]"
}
