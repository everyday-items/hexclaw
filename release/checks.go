// checks.go 实现 v0.4.0 #46 Default10 的真实 check 函数。
//
// 这些 Check 在 verify-release / CI 脚本中被注入到 Default10()，替换默认的
// "skip" 占位实现。每个 check 是独立可测试的：传 repoRoot 即可在测试 / CI 中
// 调用同一份逻辑。
//
// 不接 hexclaw runtime —— 这些都是发版前的 SDLC 检查，由 CI / 发版脚本运行。
package release

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/featureflag"
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

// CheckTestsCoverage runs the same statement-coverage calculation used by Go's
// cover profile. The profile is created outside the repository and removed on
// return so the release gate cannot accidentally validate a stale artifact.
func CheckTestsCoverage(repoRoot string, minPercent float64) FuncCheck {
	return FuncCheck{
		N:    "tests-coverage",
		Desc: fmt.Sprintf("覆盖率 ≥ %.0f%%", minPercent),
		Fn: func(ctx context.Context) Result {
			if minPercent < 0 || minPercent > 100 {
				return Result{
					Status:  StatusFail,
					Message: "覆盖率阈值配置非法",
					Detail:  "minPercent must be between 0 and 100",
				}
			}

			profile, err := os.CreateTemp("", "hexclaw-cover-*.out")
			if err != nil {
				return Result{Status: StatusFail, Message: "创建覆盖率报告失败", Detail: err.Error()}
			}
			profilePath := profile.Name()
			if err := profile.Close(); err != nil {
				_ = os.Remove(profilePath)
				return Result{Status: StatusFail, Message: "创建覆盖率报告失败", Detail: err.Error()}
			}
			defer os.Remove(profilePath)

			cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "-covermode=atomic", "-coverprofile="+profilePath, "./...")
			cmd.Dir = repoRoot
			cmd.Env = overrideEnvironment(os.Environ(), "GOWORK", "off")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdout, err := cmd.Output()
			if err != nil {
				return Result{
					Status:  StatusFail,
					Message: "覆盖率测试失败",
					Detail:  truncate(string(stdout)+"\n"+stderr.String(), 4000),
				}
			}

			file, err := os.Open(profilePath)
			if err != nil {
				return Result{Status: StatusFail, Message: "读取覆盖率报告失败", Detail: err.Error()}
			}
			covered, total, err := parseGoCoverageProfile(file)
			closeErr := file.Close()
			if err != nil {
				return Result{Status: StatusFail, Message: "覆盖率报告非法", Detail: err.Error()}
			}
			if closeErr != nil {
				return Result{Status: StatusFail, Message: "关闭覆盖率报告失败", Detail: closeErr.Error()}
			}
			percent := 100 * float64(covered) / float64(total)
			detail := fmt.Sprintf("statement_coverage=%.2f%% threshold=%.2f%% covered=%d total=%d", percent, minPercent, covered, total)
			if percent+1e-9 < minPercent {
				return Result{Status: StatusFail, Message: "测试覆盖率低于阈值", Detail: detail}
			}
			return Result{Status: StatusPass, Message: "测试覆盖率达到阈值", Detail: detail}
		},
	}
}

func parseGoCoverageProfile(r io.Reader) (covered, total uint64, err error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if lineNo == 1 {
			if !strings.HasPrefix(line, "mode: ") {
				return 0, 0, fmt.Errorf("line 1: missing coverage mode")
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return 0, 0, fmt.Errorf("line %d: malformed coverage row", lineNo)
		}
		statements, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr != nil || statements == 0 {
			return 0, 0, fmt.Errorf("line %d: invalid statement count", lineNo)
		}
		count, parseErr := strconv.ParseUint(fields[2], 10, 64)
		if parseErr != nil {
			return 0, 0, fmt.Errorf("line %d: invalid execution count", lineNo)
		}
		if ^uint64(0)-total < statements {
			return 0, 0, fmt.Errorf("line %d: statement count overflow", lineNo)
		}
		total += statements
		if count > 0 {
			covered += statements
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, fmt.Errorf("scan coverage profile: %w", err)
	}
	if lineNo == 0 || total == 0 {
		return 0, 0, fmt.Errorf("coverage profile contains no statements")
	}
	return covered, total, nil
}

func overrideEnvironment(environ []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
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

// CheckMigrationReady executes the repository-specific migration readiness
// proof. HexClaw uses forward-only numbered SQLite migrations: the release
// proof is clean installation, persisted-ledger reopen, and database integrity,
// rather than an unsafe claim that every DDL change has a destructive down path.
func CheckMigrationReady(verifyFn func(context.Context) error) FuncCheck {
	return FuncCheck{
		N:    "migration-ready",
		Desc: "迁移通过 clean-install、reopen 与 integrity 验证",
		Fn: func(ctx context.Context) Result {
			if verifyFn == nil {
				return Result{Status: StatusSkip, Message: "未注入迁移校验"}
			}
			if err := verifyFn(ctx); err != nil {
				return Result{Status: StatusFail, Message: "迁移校验失败", Detail: err.Error()}
			}
			return Result{Status: StatusPass, Message: "迁移安装、重启与完整性验证通过"}
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

const maxSBOMSize = 64 << 20

// CheckSBOMFresh 校验 SBOM 满足受支持的 CycloneDX/SPDX JSON 基础身份合同，且文档内
// generation timestamp 在 maxAgeDays 之内。完整标准合规仍应由发布流水线使用官方
// JSON Schema validator 证明；本检查不把浅层 JSON 解码冒充完整 schema validation。
// 文件 mtime 不是可信的生成证据：
// checkout、copy 或 touch 都能刷新它，却不会刷新组件清单。
func CheckSBOMFresh(repoRoot, sbomFile string, maxAgeDays int) FuncCheck {
	return FuncCheck{
		N:    "sbom-fresh",
		Desc: fmt.Sprintf("%s 有受支持的 SBOM 身份合同且生成时间在 %d 天内", sbomFile, maxAgeDays),
		Fn: func(_ context.Context) Result {
			path := filepath.Join(repoRoot, sbomFile)
			info, err := os.Lstat(path)
			if err != nil {
				return Result{Status: StatusFail, Message: "SBOM 文件缺失", Detail: err.Error()}
			}
			if !info.Mode().IsRegular() {
				return Result{Status: StatusFail, Message: "SBOM 文件类型非法", Detail: info.Mode().String()}
			}
			file, err := os.Open(path)
			if err != nil {
				return Result{Status: StatusFail, Message: "SBOM 文件读取失败", Detail: err.Error()}
			}
			defer file.Close()
			openedInfo, err := file.Stat()
			if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
				detail := "file identity changed while opening"
				if err != nil {
					detail = err.Error()
				}
				return Result{Status: StatusFail, Message: "SBOM 文件身份非法", Detail: detail}
			}
			if openedInfo.Size() <= 0 || openedInfo.Size() > maxSBOMSize {
				return Result{
					Status:  StatusFail,
					Message: "SBOM 文件大小非法",
					Detail:  fmt.Sprintf("size=%d allowed=1..%d", openedInfo.Size(), maxSBOMSize),
				}
			}
			data, err := io.ReadAll(io.LimitReader(file, maxSBOMSize+1))
			if err != nil {
				return Result{Status: StatusFail, Message: "SBOM 文件读取失败", Detail: err.Error()}
			}
			if len(data) > maxSBOMSize {
				return Result{Status: StatusFail, Message: "SBOM 文件大小非法"}
			}

			format, generatedAt, err := parseSBOMDocument(data)
			if err != nil {
				return Result{Status: StatusFail, Message: "SBOM 格式非法", Detail: err.Error()}
			}
			if maxAgeDays <= 0 {
				return Result{Status: StatusFail, Message: "SBOM 新鲜度配置非法", Detail: "maxAgeDays must be positive"}
			}
			now := time.Now().UTC()
			if generatedAt.After(now.Add(5 * time.Minute)) {
				return Result{Status: StatusFail, Message: "SBOM 生成时间在未来", Detail: generatedAt.Format(time.RFC3339)}
			}
			maxAge := time.Duration(maxAgeDays) * 24 * time.Hour
			if now.Sub(generatedAt) > maxAge {
				return Result{
					Status:  StatusFail,
					Message: "SBOM 已过期",
					Detail:  fmt.Sprintf("format=%s generated_at=%s max_age=%s", format, generatedAt.Format(time.RFC3339), maxAge),
				}
			}
			return Result{
				Status:  StatusPass,
				Message: "SBOM 基础身份与生成时间有效",
				Detail:  fmt.Sprintf("format=%s generated_at=%s", format, generatedAt.Format(time.RFC3339)),
			}
		},
	}
}

func parseSBOMDocument(data []byte) (string, time.Time, error) {
	var doc struct {
		BOMFormat    string `json:"bomFormat"`
		SpecVersion  string `json:"specVersion"`
		SerialNumber string `json:"serialNumber"`
		Version      int    `json:"version"`
		Metadata     *struct {
			Timestamp string `json:"timestamp"`
		} `json:"metadata"`
		Components json.RawMessage `json:"components"`

		SPDXVersion       string `json:"spdxVersion"`
		SPDXID            string `json:"SPDXID"`
		DataLicense       string `json:"dataLicense"`
		Name              string `json:"name"`
		DocumentNamespace string `json:"documentNamespace"`
		CreationInfo      *struct {
			Created  string   `json:"created"`
			Creators []string `json:"creators"`
		} `json:"creationInfo"`
		Packages json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", time.Time{}, fmt.Errorf("decode JSON: %w", err)
	}

	cycloneDX := doc.BOMFormat != ""
	spdx := doc.SPDXVersion != ""
	if cycloneDX == spdx {
		return "", time.Time{}, fmt.Errorf("document must identify exactly one of CycloneDX or SPDX")
	}

	if cycloneDX {
		if doc.BOMFormat != "CycloneDX" {
			return "", time.Time{}, fmt.Errorf("invalid CycloneDX bomFormat")
		}
		if !supportedCycloneDXVersion(doc.SpecVersion) {
			return "", time.Time{}, fmt.Errorf("unsupported CycloneDX specVersion %q", doc.SpecVersion)
		}
		if !strings.HasPrefix(doc.SerialNumber, "urn:uuid:") || doc.Version < 1 {
			return "", time.Time{}, fmt.Errorf("CycloneDX serialNumber/version missing")
		}
		if doc.Metadata == nil || strings.TrimSpace(doc.Metadata.Timestamp) == "" {
			return "", time.Time{}, fmt.Errorf("CycloneDX metadata.timestamp missing")
		}
		if err := requireNonEmptyJSONArray(doc.Components, "CycloneDX components"); err != nil {
			return "", time.Time{}, err
		}
		generatedAt, err := time.Parse(time.RFC3339, doc.Metadata.Timestamp)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("CycloneDX metadata.timestamp: %w", err)
		}
		return "CycloneDX " + doc.SpecVersion, generatedAt.UTC(), nil
	}

	if !supportedSPDXVersion(doc.SPDXVersion) {
		return "", time.Time{}, fmt.Errorf("unsupported SPDX version %q", doc.SPDXVersion)
	}
	if doc.SPDXID != "SPDXRef-DOCUMENT" || doc.DataLicense != "CC0-1.0" {
		return "", time.Time{}, fmt.Errorf("invalid SPDX document identity/license")
	}
	if strings.TrimSpace(doc.Name) == "" || strings.TrimSpace(doc.DocumentNamespace) == "" {
		return "", time.Time{}, fmt.Errorf("SPDX name/documentNamespace missing")
	}
	if doc.CreationInfo == nil || strings.TrimSpace(doc.CreationInfo.Created) == "" || len(doc.CreationInfo.Creators) == 0 {
		return "", time.Time{}, fmt.Errorf("SPDX creationInfo missing")
	}
	if err := requireNonEmptyJSONArray(doc.Packages, "SPDX packages"); err != nil {
		return "", time.Time{}, err
	}
	generatedAt, err := time.Parse(time.RFC3339, doc.CreationInfo.Created)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("SPDX creationInfo.created: %w", err)
	}
	return doc.SPDXVersion, generatedAt.UTC(), nil
}

func supportedCycloneDXVersion(version string) bool {
	switch version {
	case "1.4", "1.5", "1.6", "1.7":
		return true
	default:
		return false
	}
}

func supportedSPDXVersion(version string) bool {
	switch version {
	case "SPDX-2.2", "SPDX-2.3":
		return true
	default:
		return false
	}
}

func requireNonEmptyJSONArray(raw json.RawMessage, field string) error {
	var entries []json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("%s must be an array", field)
	}
	if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
		return fmt.Errorf("%s must be an array", field)
	}
	if len(entries) == 0 {
		return fmt.Errorf("%s must not be empty", field)
	}
	return nil
}

var (
	flagNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	semverPattern   = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
)

// CheckFlagStageAudit 校验 flag 元数据及所有 alpha flag 的显式审计白名单。
// 白名单必须与已注册的 alpha flag 集合完全一致；任何漂移都阻断发版。
func CheckFlagStageAudit(allRegistered []featureflag.Flag, reviewedAlphaFlags []string) FuncCheck {
	return FuncCheck{
		N:    "flag-stage-audit",
		Desc: "所有 alpha flag 已 review 升级目标",
		Fn: func(_ context.Context) Result {
			registered := make(map[string]featureflag.Flag, len(allRegistered))
			alpha := make(map[string]struct{})
			var issues []string
			for i, f := range allRegistered {
				name := strings.TrimSpace(f.Name)
				if name == "" || name != f.Name || !flagNamePattern.MatchString(name) {
					issues = append(issues, fmt.Sprintf("registered flag[%d] has invalid name %q", i, f.Name))
				}
				if _, exists := registered[f.Name]; exists {
					issues = append(issues, fmt.Sprintf("registered flag %q is duplicated", f.Name))
				} else {
					registered[f.Name] = f
				}
				if strings.TrimSpace(f.Description) == "" {
					issues = append(issues, fmt.Sprintf("registered flag %q has empty description", f.Name))
				}
				if !semverPattern.MatchString(f.SinceVersion) {
					issues = append(issues, fmt.Sprintf("registered flag %q has invalid since version %q", f.Name, f.SinceVersion))
				}
				switch f.Stage {
				case featureflag.StageAlpha:
					alpha[f.Name] = struct{}{}
				case featureflag.StageBeta, featureflag.StageGA, featureflag.StageDeprecated:
				default:
					issues = append(issues, fmt.Sprintf("registered flag %q has invalid stage %d", f.Name, f.Stage))
				}
			}

			reviewed := make(map[string]struct{}, len(reviewedAlphaFlags))
			for i, name := range reviewedAlphaFlags {
				if strings.TrimSpace(name) == "" || strings.TrimSpace(name) != name {
					issues = append(issues, fmt.Sprintf("reviewed alpha flag[%d] has invalid name %q", i, name))
				}
				if _, exists := reviewed[name]; exists {
					issues = append(issues, fmt.Sprintf("reviewed alpha flag %q is duplicated", name))
					continue
				}
				reviewed[name] = struct{}{}
				registeredFlag, exists := registered[name]
				if !exists {
					issues = append(issues, fmt.Sprintf("reviewed alpha flag %q is not registered", name))
					continue
				}
				if registeredFlag.Stage != featureflag.StageAlpha {
					issues = append(issues, fmt.Sprintf("reviewed flag %q is %s, not alpha", name, registeredFlag.Stage))
				}
			}
			for name := range alpha {
				if _, exists := reviewed[name]; !exists {
					issues = append(issues, fmt.Sprintf("alpha flag %q is not reviewed", name))
				}
			}

			if len(issues) > 0 {
				sort.Strings(issues)
				return Result{
					Status:  StatusFail,
					Message: fmt.Sprintf("feature flag stage audit failed with %d issue(s)", len(issues)),
					Detail:  strings.Join(issues, "; "),
				}
			}
			return Result{Status: StatusPass, Message: "所有 alpha flag 已审计"}
		},
	}
}

// BuildDefault10WithReal 把 Default10 中的占位 check 替换为真实实现。
//
// 调用方传入 repoRoot + expectedVersion + 关键版本文件名，返回的 []Check
// 可直接传给 Gate.Run。其它 5 项（signatures / migration / config / sbom /
// flag-stage-audit）的工厂函数（CheckXxx）也已提供，调用方按发布环境注入。
func BuildDefault10WithReal(repoRoot, expectedVersion string, versionFiles []string) []Check {
	checks := Default10()
	for i := range checks {
		switch checks[i].Name() {
		case "tests-pass":
			checks[i] = CheckTestsPass(repoRoot)
		case "tests-coverage":
			checks[i] = CheckTestsCoverage(repoRoot, 70)
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
