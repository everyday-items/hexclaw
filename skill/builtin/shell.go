package builtin

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/skill"
)

// ShellSkill Shell 命令执行 Skill
//
// 在受限环境中执行 Shell 命令。安全措施：
//   - 命令白名单（仅允许安全的只读/计算类命令）
//   - 超时限制（默认 30 秒）
//   - 输出大小限制（最大 64KB）
//   - 环境变量清洗
//   - 配置开关（默认关闭）
type ShellSkill struct {
	timeout time.Duration
}

// NewShellSkill 创建 Shell 执行 Skill
func NewShellSkill() *ShellSkill {
	return &ShellSkill{
		timeout: 30 * time.Second,
	}
}

func (s *ShellSkill) Name() string        { return "shell" }
func (s *ShellSkill) Description() string { return "执行 Shell 命令，返回命令输出" }

// Match 匹配 Shell 命令
//
// 触发格式: /sh <command> 或 /shell <command>
func (s *ShellSkill) Match(content string) bool {
	lower := strings.ToLower(content)
	return strings.HasPrefix(lower, "/sh ") || strings.HasPrefix(lower, "/shell ")
}

// Execute 执行 Shell 命令
func (s *ShellSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return &skill.Result{Content: "请提供命令，格式：/sh ls -la"}, nil
	}

	// 提取命令内容
	command := extractShellCommand(query)
	if command == "" {
		return &skill.Result{Content: "请提供要执行的命令"}, nil
	}

	// 安全检查：白名单模式
	if reason := checkAllowed(command); reason != "" {
		return &skill.Result{
			Content:  fmt.Sprintf("**安全拦截**: %s\n\n仅允许安全的只读命令。", reason),
			Metadata: map[string]string{"status": "blocked"},
		}, nil
	}

	output, err := s.runShell(ctx, command)
	if err != nil {
		return &skill.Result{
			Content:  fmt.Sprintf("**执行失败**\n```\n$ %s\n%s\n```", command, err.Error()),
			Metadata: map[string]string{"status": "error"},
		}, nil
	}

	return &skill.Result{
		Content:  fmt.Sprintf("```\n$ %s\n%s\n```", command, truncateOutput(output, 64*1024)),
		Metadata: map[string]string{"status": "ok"},
	}, nil
}

// runShell 执行 Shell 命令
func (s *ShellSkill) runShell(ctx context.Context, command string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)

	// 清洗环境变量
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"LANG=en_US.UTF-8",
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
		return "", fmt.Errorf("执行超时（限制 %v）", s.timeout)
	}

	if err != nil {
		if output != "" {
			return "", fmt.Errorf("%s", output)
		}
		return "", err
	}

	return output, nil
}

// extractShellCommand 从用户输入中提取命令
func extractShellCommand(input string) string {
	for _, prefix := range []string{"/sh ", "/shell "} {
		if strings.HasPrefix(strings.ToLower(input), prefix) {
			return strings.TrimSpace(input[len(prefix):])
		}
	}
	return strings.TrimSpace(input)
}

// allowedCommands 白名单：仅允许真正只读/无副作用的命令
//
// 已移除所有具有写能力或数据窃取风险的命令：
//   python/python3/go/node/sed/awk/wget/cp/mv/mkdir/touch/
//   tar/zip/unzip/gzip/gunzip/tee/xargs/yes/curl 等
//
// curl 已移除：即使拦截 -o/-O，仍可通过 -d @file / -F file=@path /
// --data-binary @path 窃取本地文件到外部服务器，风险不可控。
var allowedCommands = map[string]bool{
	// 文件查看（只读）
	"ls": true, "cat": true, "head": true, "tail": true,
	"file": true, "wc": true, "du": true, "df": true,
	// 搜索/过滤（只读）
	"find": true, "grep": true,
	// 排序/去重/文本处理
	"sort": true, "uniq": true, "jq": true, "tree": true,
	// 系统信息（只读）
	"date": true, "uptime": true, "uname": true, "whoami": true,
	"env": true, "ps": true, "top": true, "free": true, "which": true,
	// 文本输出
	"echo": true,
	// 网络诊断（只读）
	"ping": true, "dig": true, "nslookup": true,
	"traceroute": true, "ifconfig": true, "ip": true, "ss": true, "netstat": true,
	// Git（只读子命令，危险子命令另行拦截）
	"git": true,
	// 路径/标识
	"pwd": true,
}

// dangerousSubcommands 即使主命令在白名单，某些子命令仍需拦截
var dangerousSubcommands = map[string][]string{
	"git": {
		"push", "remote add", "remote set-url",
		"reset", "checkout", "clean", "rebase",
		"commit", "merge", "stash", "cherry-pick",
		"tag", "branch -D", "branch -d",
		"clone", "pull", "submodule", "config",
	},
}

// checkAllowed 白名单安全检查
//
// 只允许白名单中的命令，拒绝所有其他命令。
// 同时拦截管道链中的任何非白名单命令和危险模式。
func checkAllowed(command string) string {
	lower := strings.TrimSpace(command)
	if lower == "" {
		return "命令为空"
	}

	// 拦截危险字符/模式（命令替换、eval 等）
	if reason := checkDangerousPatterns(lower); reason != "" {
		return reason
	}

	// 按管道和分号拆分，每段都必须通过白名单
	segments := splitCommandSegments(lower)
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		cmd := extractBaseCommand(seg)
		if cmd == "" {
			continue
		}
		if !allowedCommands[cmd] {
			return fmt.Sprintf("命令 %q 不在白名单中", cmd)
		}
		// 检查子命令限制
		if blocked, ok := dangerousSubcommands[cmd]; ok {
			rest := strings.TrimSpace(seg[len(cmd):])
			for _, sub := range blocked {
				if strings.HasPrefix(rest, sub) {
					return fmt.Sprintf("命令 %s %s 被禁止", cmd, sub)
				}
			}
		}
	}

	return ""
}

// checkDangerousPatterns 拦截无法通过白名单防御的危险模式
func checkDangerousPatterns(cmd string) string {
	// 反引号命令替换
	if strings.Contains(cmd, "`") {
		return "禁止使用反引号命令替换"
	}
	// $() 命令替换
	if strings.Contains(cmd, "$(") {
		return "禁止使用 $() 命令替换"
	}
	// eval
	if strings.HasPrefix(strings.TrimSpace(cmd), "eval ") || strings.Contains(cmd, "; eval ") {
		return "禁止使用 eval"
	}
	// 输出重定向（> 和 >>）
	if strings.Contains(cmd, ">") {
		return "禁止使用输出重定向 > / >>"
	}
	// 输入重定向 here-doc
	if strings.Contains(cmd, "<<") {
		return "禁止使用 here-doc 重定向 <<"
	}
	// 进程替换
	if strings.Contains(cmd, "<(") || strings.Contains(cmd, ">(") {
		return "禁止使用进程替换 <() / >()"
	}
	// && 和 || 条件链（由 splitCommandSegments 统一处理，但在此提前拦截）
	if strings.Contains(cmd, "&&") {
		return "禁止使用 && 命令链"
	}
	if strings.Contains(cmd, "||") {
		return "禁止使用 || 命令链"
	}
	return ""
}

// splitCommandSegments 按管道、分号、&& 和 || 拆分命令
func splitCommandSegments(cmd string) []string {
	// 先将 && 和 || 替换为分号，再统一按 | 和 ; 拆分
	cmd = strings.ReplaceAll(cmd, "&&", ";")
	cmd = strings.ReplaceAll(cmd, "||", ";")
	var segments []string
	for _, part := range strings.Split(cmd, "|") {
		for _, sub := range strings.Split(part, ";") {
			sub = strings.TrimSpace(sub)
			if sub != "" {
				segments = append(segments, sub)
			}
		}
	}
	return segments
}

// extractBaseCommand 从命令段中提取基础命令名
//
// 跳过环境变量赋值（VAR=val cmd）和路径前缀（/usr/bin/cmd）
func extractBaseCommand(segment string) string {
	fields := strings.Fields(segment)
	for _, f := range fields {
		// 跳过环境变量赋值
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "-") {
			continue
		}
		// 取 basename（处理 /usr/bin/ls 这种情况）
		base := f
		if idx := strings.LastIndex(f, "/"); idx >= 0 {
			base = f[idx+1:]
		}
		return strings.ToLower(base)
	}
	return ""
}
