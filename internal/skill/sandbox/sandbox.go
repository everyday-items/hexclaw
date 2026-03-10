// Package sandbox 提供 Skill 沙箱执行环境
//
// 确保第三方 Skill 在受限环境中运行：
//   - 超时控制：防止 Skill 长时间阻塞
//   - Panic 恢复：捕获 Skill 中的 panic，不影响主进程
//   - 网络白名单：限制 Skill 可访问的域名
//   - 文件路径白名单：限制 Skill 可访问的文件路径
//
// 当前版本为纯 Go 实现（进程内沙箱）。
// v2 版本计划支持 Docker/Wasm 隔离。
package sandbox

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/everyday-items/hexclaw/internal/config"
	"github.com/everyday-items/hexclaw/internal/skill"
)

// Sandbox Skill 沙箱执行器
type Sandbox struct {
	cfg     config.SandboxConfig
	timeout time.Duration
}

// New 创建沙箱执行器
func New(cfg config.SandboxConfig) (*Sandbox, error) {
	timeout := 30 * time.Second // 默认 30 秒
	if cfg.Timeout != "" {
		d, err := time.ParseDuration(cfg.Timeout)
		if err != nil {
			return nil, fmt.Errorf("解析超时配置 %q 失败: %w", cfg.Timeout, err)
		}
		timeout = d
	}

	return &Sandbox{
		cfg:     cfg,
		timeout: timeout,
	}, nil
}

// Result 沙箱执行结果
type Result struct {
	*skill.Result              // 嵌入原始 Skill 结果
	Duration  time.Duration    // 执行耗时
	Recovered bool            // 是否从 panic 恢复
	Error     error            // 执行错误
}

// Execute 在沙箱中执行 Skill
//
// 限制：
//   - 超时控制（context.WithTimeout）
//   - Panic 恢复（recover）
//   - 网络域名白名单校验
//   - 文件路径白名单校验
func (s *Sandbox) Execute(ctx context.Context, sk skill.Skill, args map[string]any) *Result {
	start := time.Now()

	// 超时控制
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	// 网络白名单检查
	if url, ok := args["url"].(string); ok {
		if !s.isAllowedDomain(url) {
			return &Result{
				Duration: time.Since(start),
				Error:    fmt.Errorf("域名不在白名单中: %s", url),
			}
		}
	}

	// 文件路径白名单检查
	if path, ok := args["path"].(string); ok {
		if !s.isAllowedPath(path) {
			return &Result{
				Duration: time.Since(start),
				Error:    fmt.Errorf("路径不在白名单中: %s", path),
			}
		}
	}

	// 在 goroutine 中执行，捕获 panic
	type execResult struct {
		result    *skill.Result
		err       error
		recovered bool
	}
	ch := make(chan execResult, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("沙箱: Skill %q panic 已恢复: %v", sk.Name(), r)
				ch <- execResult{
					err:       fmt.Errorf("Skill 执行异常: %v", r),
					recovered: true,
				}
			}
		}()

		result, err := sk.Execute(ctx, args)
		ch <- execResult{result: result, err: err}
	}()

	// 等待执行完成或超时
	select {
	case res := <-ch:
		return &Result{
			Result:    res.result,
			Duration:  time.Since(start),
			Recovered: res.recovered,
			Error:     res.err,
		}
	case <-ctx.Done():
		return &Result{
			Duration: time.Since(start),
			Error:    fmt.Errorf("Skill %q 执行超时（限制 %v）", sk.Name(), s.timeout),
		}
	}
}

// isAllowedDomain 检查域名是否在白名单中
//
// 如果白名单为空，则允许所有域名。
func (s *Sandbox) isAllowedDomain(urlStr string) bool {
	if len(s.cfg.Network.AllowedDomains) == 0 {
		return true // 无限制
	}

	for _, domain := range s.cfg.Network.AllowedDomains {
		if strings.Contains(urlStr, domain) {
			return true
		}
	}
	return false
}

// isAllowedPath 检查文件路径是否在白名单中
//
// 如果白名单为空，则允许所有路径。
func (s *Sandbox) isAllowedPath(path string) bool {
	if len(s.cfg.Filesystem.AllowedPaths) == 0 {
		return true // 无限制
	}

	for _, allowed := range s.cfg.Filesystem.AllowedPaths {
		if strings.HasPrefix(path, allowed) {
			return true
		}
	}
	return false
}
