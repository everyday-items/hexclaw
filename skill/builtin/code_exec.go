package builtin

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/sandbox"
	"github.com/hexagon-codes/hexclaw/skill"
)

// CodeExecSkill 代码执行工具
//
// 在 Go 原生沙箱中执行 Python/JavaScript/Go 代码。
// 含依赖自动安装: ModuleNotFoundError → pip install → 重试。
// 对标 Codex CodeExecSkill。
type CodeExecSkill struct {
	sb sandbox.Sandbox
}

// NewCodeExecSkill 创建代码执行 Skill
func NewCodeExecSkill(sb sandbox.Sandbox) *CodeExecSkill {
	return &CodeExecSkill{sb: sb}
}

func (s *CodeExecSkill) Name() string        { return "code_exec" }
func (s *CodeExecSkill) Description() string  { return "Execute code in a sandboxed environment (Python, JavaScript, Go)" }
func (s *CodeExecSkill) Match(_ string) bool  { return false } // LLM-only, no keyword trigger

func (s *CodeExecSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("code_exec", "Execute code in a sandboxed environment. Supports Python, JavaScript, and Go.", &llm.Schema{
		Type: "object",
		Properties: map[string]*llm.Schema{
			"language": {Type: "string", Description: "Programming language: python, javascript, go"},
			"code":     {Type: "string", Description: "Source code to execute"},
		},
		Required: []string{"language", "code"},
	})
}

func (s *CodeExecSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	language, _ := args["language"].(string)
	code, _ := args["code"].(string)

	if language == "" || code == "" {
		return nil, fmt.Errorf("language and code are required")
	}

	// 第一次执行
	result, err := s.sb.ExecCode(ctx, language, code)
	if err != nil {
		return nil, fmt.Errorf("code execution failed: %w", err)
	}

	output := result.Stdout
	if result.Stderr != "" {
		output += "\n[stderr]: " + result.Stderr
	}

	// 检测缺失依赖 → 自动安装 → 重试 (仅 1 次)
	if result.ExitCode != 0 {
		if missingPkgs := detectMissingPackages(language, result.Stderr); len(missingPkgs) > 0 {
			installCmd := buildInstallCommand(language, missingPkgs)
			installResult, installErr := s.sb.Exec(ctx, "sh", []string{"-c", installCmd})
			if installErr == nil && installResult.ExitCode == 0 {
				// 重试
				result, err = s.sb.ExecCode(ctx, language, code)
				if err != nil {
					return nil, fmt.Errorf("code execution failed after install: %w", err)
				}
				output = fmt.Sprintf("[auto-installed %v]\n%s", missingPkgs, result.Stdout)
				if result.Stderr != "" {
					output += "\n[stderr]: " + result.Stderr
				}
			}
		}
	}

	if result.ExitCode != 0 {
		output = fmt.Sprintf("[exit code %d]\n%s", result.ExitCode, output)
	}

	return &skill.Result{Content: output}, nil
}

// Python: "ModuleNotFoundError: No module named 'pandas'"
var pyModuleNotFound = regexp.MustCompile(`(?:ModuleNotFoundError|ImportError):\s+No module named '([^']+)'`)

// Node: "Cannot find module 'lodash'"
var nodeModuleNotFound = regexp.MustCompile(`Cannot find module '([^']+)'`)

func detectMissingPackages(language, stderr string) []string {
	var re *regexp.Regexp
	switch language {
	case "python", "python3":
		re = pyModuleNotFound
	case "javascript", "node", "js":
		re = nodeModuleNotFound
	default:
		return nil
	}

	matches := re.FindAllStringSubmatch(stderr, -1)
	seen := make(map[string]bool)
	var pkgs []string
	for _, m := range matches {
		if len(m) >= 2 {
			pkg := strings.Split(m[1], ".")[0] // "foo.bar" → "foo"
			if !seen[pkg] {
				seen[pkg] = true
				pkgs = append(pkgs, pkg)
			}
		}
	}
	return pkgs
}

func buildInstallCommand(language string, pkgs []string) string {
	switch language {
	case "python", "python3":
		return "pip install " + strings.Join(pkgs, " ")
	case "javascript", "node", "js":
		return "npm install --no-save " + strings.Join(pkgs, " ")
	default:
		return ""
	}
}
