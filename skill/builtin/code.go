package builtin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/toolkit/lang/stringx"
)

// CodeSkill 代码执行 Skill
//
// 支持在宿主机上执行代码片段，当前支持：
//   - Go: go run
//   - Python: python3
//   - JavaScript/TypeScript: node
//
// 安全措施：
//   - 超时限制（默认 30 秒）
//   - stdout/stderr 实时写入上限 1MB（limitedWriter），防止 OOM
//   - 临时目录隔离 + 最小化环境变量
//   - 配置开关（默认关闭）
//
// WARNING: 此 Skill 直接在宿主机上执行任意代码，不提供内核级沙箱。
// 仅应在容器化或已隔离的沙箱环境中启用。
type CodeSkill struct {
	timeout time.Duration
}

// NewCodeSkill 创建代码执行 Skill
func NewCodeSkill() *CodeSkill {
	return &CodeSkill{
		timeout: 30 * time.Second,
	}
}

func (s *CodeSkill) Name() string { return "code" }
func (s *CodeSkill) Description() string {
	return "执行代码片段（Go/Python/JavaScript），返回运行结果"
}

// ToolDefinition 返回代码执行工具的 LLM 定义
func (s *CodeSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("code", "执行代码片段（Go/Python/JavaScript），返回运行结果", &llm.Schema{
		Type: "object",
		Properties: map[string]*llm.Schema{
			"language": {Type: "string", Description: "编程语言", Enum: []any{"python", "javascript", "go"}},
			"code":     {Type: "string", Description: "要执行的代码"},
		},
		Required: []string{"language", "code"},
	})
}

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

	stdout := newLimitedWriter()
	stderr := newLimitedWriter()
	cmd.Stdout = stdout
	cmd.Stderr = stderr

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
	const suffix = "\n... (输出已截断)"
	if maxLen <= 0 {
		return ""
	}
	if len([]rune(s)) <= maxLen {
		return s
	}
	return stringx.TruncateWithSuffix(s, maxLen, suffix)
}

// limitedWriter 是一个带上限的 io.Writer。
// 写入超过 maxBytes 字节后，后续写入被静默丢弃，并在最终输出中追加截断提示。
// 设计目的：在子进程仍在运行期间即时限制缓冲区增长，防止 OOM。
type limitedWriter struct {
	buf       bytes.Buffer
	maxBytes  int
	truncated bool
}

const outputLimitBytes = 1 << 20 // 1 MB

func newLimitedWriter() *limitedWriter {
	return &limitedWriter{maxBytes: outputLimitBytes}
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	remaining := w.maxBytes - w.buf.Len()
	if remaining <= 0 {
		w.truncated = true
		// 丢弃多余写入，但向调用方报告全部字节已"消费"，避免 exec 报错
		return len(p), nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		w.truncated = true
	}
	n, err := w.buf.Write(p)
	return n, err
}

// String 返回已捕获的输出，超限时附加截断提示。
func (w *limitedWriter) String() string {
	s := w.buf.String()
	if w.truncated {
		s += "\n... (输出已超过 1MB，已截断)"
	}
	return s
}

// Len 返回当前缓冲字节数。
func (w *limitedWriter) Len() int { return w.buf.Len() }

// ensure limitedWriter satisfies io.Writer
var _ io.Writer = (*limitedWriter)(nil)
