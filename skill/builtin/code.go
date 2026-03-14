package builtin

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/everyday-items/hexclaw/skill"
)

// CodeSkill 代码执行 Skill
//
// 支持在沙箱中执行代码片段，当前支持：
//   - Go: go run
//   - Python: python3
//   - JavaScript/TypeScript: node
//
// 安全措施：
//   - 超时限制（默认 30 秒）
//   - 输出大小限制（最大 64KB）
//   - 临时目录隔离
//   - 配置开关（默认关闭）
type CodeSkill struct {
	timeout time.Duration
}

// NewCodeSkill 创建代码执行 Skill
func NewCodeSkill() *CodeSkill {
	return &CodeSkill{
		timeout: 30 * time.Second,
	}
}

func (s *CodeSkill) Name() string        { return "code" }
func (s *CodeSkill) Description() string { return "执行代码片段（Go/Python/JavaScript），返回运行结果" }

// Match 匹配代码执行命令
//
// 触发格式: /run <lang> 或 /code <lang>
func (s *CodeSkill) Match(content string) bool {
	lower := strings.ToLower(content)
	return strings.HasPrefix(lower, "/run ") || strings.HasPrefix(lower, "/code ")
}

// Execute 执行代码
//
// args["query"] 包含完整用户输入，格式：/run <lang>\n<code>
func (s *CodeSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return &skill.Result{Content: "请提供代码，格式：/run go\n```\npackage main\nfmt.Println(\"hello\")\n```"}, nil
	}

	// 解析语言和代码
	lang, code := parseCodeInput(query)
	if code == "" {
		return &skill.Result{Content: "未检测到代码内容。格式：/run go\n```\nfmt.Println(\"hello\")\n```"}, nil
	}

	runner, ok := codeRunners[lang]
	if !ok {
		supported := make([]string, 0, len(codeRunners))
		for k := range codeRunners {
			supported = append(supported, k)
		}
		return &skill.Result{
			Content: fmt.Sprintf("不支持的语言 %q，当前支持：%s", lang, strings.Join(supported, ", ")),
		}, nil
	}

	// 在临时目录中执行
	tmpDir, err := os.MkdirTemp("", "hexclaw-code-*")
	if err != nil {
		return nil, fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	result, err := runner(ctx, s.timeout, tmpDir, code)
	if err != nil {
		return &skill.Result{
			Content:  fmt.Sprintf("**执行错误**\n```\n%s\n```", err.Error()),
			Metadata: map[string]string{"language": lang, "status": "error"},
		}, nil
	}

	return &skill.Result{
		Content:  fmt.Sprintf("**%s 执行结果**\n```\n%s\n```", lang, truncateOutput(result, 64*1024)),
		Metadata: map[string]string{"language": lang, "status": "ok"},
	}, nil
}

// codeRunner 代码执行函数签名
type codeRunner func(ctx context.Context, timeout time.Duration, dir, code string) (string, error)

// codeRunners 各语言的执行器
var codeRunners = map[string]codeRunner{
	"go":         runGo,
	"python":     runPython,
	"py":         runPython,
	"python3":    runPython,
	"javascript": runJavaScript,
	"js":         runJavaScript,
	"node":       runJavaScript,
}

// runGo 执行 Go 代码
func runGo(ctx context.Context, timeout time.Duration, dir, code string) (string, error) {
	// 如果代码不包含 package 声明，自动包装为 main 函数
	if !strings.HasPrefix(strings.TrimSpace(code), "package ") {
		code = "package main\n\nfunc main() {\n" + code + "\n}\n"
	}

	filePath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(filePath, []byte(code), 0o600); err != nil {
		return "", fmt.Errorf("写入代码文件失败: %w", err)
	}

	// 先用 goimports 自动补全 import（如果可用），否则降级直接运行
	if goimportsPath, err := exec.LookPath("goimports"); err == nil {
		_ = exec.CommandContext(ctx, goimportsPath, "-w", filePath).Run()
	}

	return runCommand(ctx, timeout, dir, "go", "run", filePath)
}

// runPython 执行 Python 代码
func runPython(ctx context.Context, timeout time.Duration, dir, code string) (string, error) {
	filePath := filepath.Join(dir, "script.py")
	if err := os.WriteFile(filePath, []byte(code), 0o600); err != nil {
		return "", fmt.Errorf("写入代码文件失败: %w", err)
	}

	return runCommand(ctx, timeout, dir, "python3", filePath)
}

// runJavaScript 执行 JavaScript 代码
func runJavaScript(ctx context.Context, timeout time.Duration, dir, code string) (string, error) {
	filePath := filepath.Join(dir, "script.js")
	if err := os.WriteFile(filePath, []byte(code), 0o600); err != nil {
		return "", fmt.Errorf("写入代码文件失败: %w", err)
	}

	return runCommand(ctx, timeout, dir, "node", filePath)
}

// runCommand 执行命令并捕获输出
func runCommand(ctx context.Context, timeout time.Duration, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir

	// 限制环境变量，移除敏感信息
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"TMPDIR=" + dir,
		"GOPATH=" + filepath.Join(dir, "gopath"),
		"GOCACHE=" + filepath.Join(dir, "gocache"),
		"GOMODCACHE=" + filepath.Join(dir, "modcache"),
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.String()
	}

	if ctx.Err() != nil {
		return "", fmt.Errorf("执行超时（限制 %v）", timeout)
	}

	if err != nil {
		if output != "" {
			return "", fmt.Errorf("%s", output)
		}
		return "", err
	}

	return output, nil
}

// parseCodeInput 从用户输入中解析语言和代码
//
// 支持格式：
//
//	/run go
//	fmt.Println("hello")
//
//	/run python
//	```python
//	print("hello")
//	```
func parseCodeInput(input string) (lang, code string) {
	// 去掉 /run 或 /code 前缀
	for _, prefix := range []string{"/run ", "/code "} {
		if strings.HasPrefix(strings.ToLower(input), prefix) {
			input = strings.TrimSpace(input[len(prefix):])
			break
		}
	}

	// 第一行是语言标识
	lines := strings.SplitN(input, "\n", 2)
	lang = strings.TrimSpace(strings.ToLower(lines[0]))
	if len(lines) < 2 {
		return lang, ""
	}

	code = strings.TrimSpace(lines[1])

	// 去掉 markdown 代码块标记
	if strings.HasPrefix(code, "```") {
		// 去掉开头的 ```lang
		firstNewline := strings.Index(code, "\n")
		if firstNewline > 0 {
			code = code[firstNewline+1:]
		}
		// 去掉结尾的 ```
		code = strings.TrimSuffix(code, "```")
		code = strings.TrimSpace(code)
	}

	return lang, code
}

// truncateOutput 截断过长的输出
func truncateOutput(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "\n... (输出已截断)"
}
