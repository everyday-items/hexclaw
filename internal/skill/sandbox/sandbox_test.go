package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/everyday-items/hexclaw/internal/config"
	"github.com/everyday-items/hexclaw/internal/skill"
)

// mockSkill 测试用 Skill
type mockSkill struct {
	name     string
	execFn   func(ctx context.Context, args map[string]any) (*skill.Result, error)
}

func (s *mockSkill) Name() string        { return s.name }
func (s *mockSkill) Description() string { return "test" }
func (s *mockSkill) Match(_ string) bool { return false }
func (s *mockSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	return s.execFn(ctx, args)
}

// TestSandbox_Normal 测试正常执行
func TestSandbox_Normal(t *testing.T) {
	sb, err := New(config.SandboxConfig{Timeout: "5s"})
	if err != nil {
		t.Fatal(err)
	}

	sk := &mockSkill{
		name: "test",
		execFn: func(_ context.Context, _ map[string]any) (*skill.Result, error) {
			return &skill.Result{Content: "hello"}, nil
		},
	}

	result := sb.Execute(context.Background(), sk, nil)
	if result.Error != nil {
		t.Fatalf("期望成功，实际错误: %v", result.Error)
	}
	if result.Result.Content != "hello" {
		t.Fatalf("期望 hello，实际 %s", result.Result.Content)
	}
	if result.Recovered {
		t.Fatal("不应有 panic 恢复")
	}
}

// TestSandbox_Timeout 测试超时
func TestSandbox_Timeout(t *testing.T) {
	sb, err := New(config.SandboxConfig{Timeout: "100ms"})
	if err != nil {
		t.Fatal(err)
	}

	sk := &mockSkill{
		name: "slow",
		execFn: func(ctx context.Context, _ map[string]any) (*skill.Result, error) {
			select {
			case <-time.After(5 * time.Second):
				return &skill.Result{Content: "done"}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}

	result := sb.Execute(context.Background(), sk, nil)
	if result.Error == nil {
		t.Fatal("期望超时错误")
	}
}

// TestSandbox_PanicRecovery 测试 panic 恢复
func TestSandbox_PanicRecovery(t *testing.T) {
	sb, err := New(config.SandboxConfig{Timeout: "5s"})
	if err != nil {
		t.Fatal(err)
	}

	sk := &mockSkill{
		name: "panicker",
		execFn: func(_ context.Context, _ map[string]any) (*skill.Result, error) {
			panic("测试 panic")
		},
	}

	result := sb.Execute(context.Background(), sk, nil)
	if result.Error == nil {
		t.Fatal("期望从 panic 恢复的错误")
	}
	if !result.Recovered {
		t.Fatal("应标记为 recovered")
	}
}

// TestSandbox_DomainWhitelist 测试域名白名单
func TestSandbox_DomainWhitelist(t *testing.T) {
	sb, err := New(config.SandboxConfig{
		Timeout: "5s",
		Network: config.SandboxNetwork{
			AllowedDomains: []string{"api.example.com"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	sk := &mockSkill{
		name: "net",
		execFn: func(_ context.Context, _ map[string]any) (*skill.Result, error) {
			return &skill.Result{Content: "ok"}, nil
		},
	}

	// 白名单域名应通过
	result := sb.Execute(context.Background(), sk, map[string]any{"url": "https://api.example.com/data"})
	if result.Error != nil {
		t.Fatalf("白名单域名应通过: %v", result.Error)
	}

	// 非白名单域名应拒绝
	result = sb.Execute(context.Background(), sk, map[string]any{"url": "https://evil.com/data"})
	if result.Error == nil {
		t.Fatal("非白名单域名应拒绝")
	}
}
