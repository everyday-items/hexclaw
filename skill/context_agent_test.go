package skill

import (
	"context"
	"testing"
)

func TestRoutedAgent_RoundTrip(t *testing.T) {
	ctx := WithRoutedAgent(context.Background(), "mingming")
	if got := RoutedAgentName(ctx); got != "mingming" {
		t.Errorf("应取回已 stamp 的 agent, got %q", got)
	}
}

func TestRoutedAgent_EmptyIsNoop(t *testing.T) {
	// 空名不 stamp（消息未路由到命名 Agent）。
	ctx := WithRoutedAgent(context.Background(), "")
	if got := RoutedAgentName(ctx); got != "" {
		t.Errorf("空名应不 stamp, got %q", got)
	}
	// 未 stamp 的 ctx 取回空串（默认助理/直调）。
	if got := RoutedAgentName(context.Background()); got != "" {
		t.Errorf("裸 ctx 应空, got %q", got)
	}
}
