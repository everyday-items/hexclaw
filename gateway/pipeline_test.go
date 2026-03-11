package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/config"
)

// mockStore 测试用存储 mock
type mockStore struct {
	userCost   float64
	globalCost float64
	costErr    error
}

func (m *mockStore) Init(_ context.Context) error                         { return nil }
func (m *mockStore) Close() error                                         { return nil }
func (m *mockStore) CreateSession(_ context.Context, _ *mockSession) error { return nil }
func (m *mockStore) GetSession(_ context.Context, _ string) (*mockSession, error) {
	return nil, nil
}
func (m *mockStore) ListSessions(_ context.Context, _ string, _, _ int) ([]*mockSession, error) {
	return nil, nil
}
func (m *mockStore) DeleteSession(_ context.Context, _ string) error { return nil }
func (m *mockStore) SaveMessage(_ context.Context, _ *mockMessageRecord) error {
	return nil
}
func (m *mockStore) ListMessages(_ context.Context, _ string, _, _ int) ([]*mockMessageRecord, error) {
	return nil, nil
}
func (m *mockStore) SaveCost(_ context.Context, _ *mockCostRecord) error { return nil }
func (m *mockStore) GetUserCost(_ context.Context, _ string, _ time.Time) (float64, error) {
	return m.userCost, m.costErr
}
func (m *mockStore) GetGlobalCost(_ context.Context, _ time.Time) (float64, error) {
	return m.globalCost, m.costErr
}
func (m *mockStore) WithTx(_ context.Context, fn func(interface{}) error) error { return nil }

// mock 类型占位（避免引入 storage 包的循环依赖）
type mockSession struct{}
type mockMessageRecord struct{}
type mockCostRecord struct{}

func TestPipeline_EmptyConfig(t *testing.T) {
	cfg := &config.SecurityConfig{}
	p := NewPipeline(cfg, nil)

	// 空配置应只有审计层
	if len(p.layers) != 1 {
		t.Fatalf("期望 1 层（审计），实际 %d 层", len(p.layers))
	}
	if p.layers[0].Name() != "audit" {
		t.Fatalf("期望审计层，实际 %s", p.layers[0].Name())
	}
}

func TestPipeline_AuthLayer_Pass(t *testing.T) {
	layer := NewAuthLayer(config.AuthConfig{Enabled: true})

	// 平台消息带 UserID 应通过
	msg := &adapter.Message{
		Platform: adapter.PlatformFeishu,
		UserID:   "user-1",
	}
	if err := layer.Check(context.Background(), msg); err != nil {
		t.Fatalf("平台消息应通过: %v", err)
	}
}

func TestPipeline_AuthLayer_Reject(t *testing.T) {
	layer := NewAuthLayer(config.AuthConfig{Enabled: true})

	// API 消息无 Token 应拒绝
	msg := &adapter.Message{
		Platform: adapter.PlatformAPI,
		UserID:   "user-1",
	}
	err := layer.Check(context.Background(), msg)
	if err == nil {
		t.Fatal("API 消息无 Token 应拒绝")
	}
	gwErr, ok := err.(*GatewayError)
	if !ok {
		t.Fatalf("应返回 GatewayError，实际 %T", err)
	}
	if gwErr.Code != "missing_token" {
		t.Fatalf("期望 missing_token，实际 %s", gwErr.Code)
	}
}

func TestPipeline_AuthLayer_Anonymous(t *testing.T) {
	layer := NewAuthLayer(config.AuthConfig{Enabled: true, AllowAnonymous: true})

	msg := &adapter.Message{Platform: adapter.PlatformAPI}
	if err := layer.Check(context.Background(), msg); err != nil {
		t.Fatalf("匿名模式应通过: %v", err)
	}
}

func TestPipeline_RateLimitLayer(t *testing.T) {
	layer := NewRateLimitLayer(config.RateLimitConfig{RequestsPerMinute: 2})

	msg := &adapter.Message{UserID: "user-1"}
	ctx := context.Background()

	// 前 2 次应通过
	for i := 0; i < 2; i++ {
		if err := layer.Check(ctx, msg); err != nil {
			t.Fatalf("第 %d 次请求应通过: %v", i+1, err)
		}
	}

	// 第 3 次应被限流
	err := layer.Check(ctx, msg)
	if err == nil {
		t.Fatal("第 3 次请求应被限流")
	}
	gwErr, ok := err.(*GatewayError)
	if !ok {
		t.Fatalf("应返回 GatewayError，实际 %T", err)
	}
	if gwErr.Code != "minute_exceeded" {
		t.Fatalf("期望 minute_exceeded，实际 %s", gwErr.Code)
	}
}

func TestGatewayError(t *testing.T) {
	e := &GatewayError{
		Layer:   "auth",
		Code:    "missing_token",
		Message: "请提供认证 Token",
	}
	expected := "auth: 请提供认证 Token"
	if e.Error() != expected {
		t.Fatalf("期望 %q，实际 %q", expected, e.Error())
	}
}
