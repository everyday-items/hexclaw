package engine

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestToolRetry_SuccessNoRetry(t *testing.T) {
	w := NewToolRetryWrapper(DefaultToolRetryConfig())

	callCount := 0
	result, err := w.Execute(context.Background(), "search", func(ctx context.Context) (string, error) {
		callCount++
		return "ok", nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok" {
		t.Fatalf("expected 'ok', got %q", result)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 call, got %d", callCount)
	}
}

func TestToolRetry_TransientRetry(t *testing.T) {
	cfg := DefaultToolRetryConfig()
	cfg.RetryOverrides["flaky"] = 2
	cfg.TimeoutOverrides["flaky"] = 5 * time.Second
	w := NewToolRetryWrapper(cfg)

	callCount := 0
	result, err := w.Execute(context.Background(), "flaky", func(ctx context.Context) (string, error) {
		callCount++
		if callCount < 3 {
			return "", fmt.Errorf("connection refused")
		}
		return "success", nil
	})

	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if result != "success" {
		t.Fatalf("expected 'success', got %q", result)
	}
	if callCount != 3 {
		t.Fatalf("expected 3 calls (1 + 2 retries), got %d", callCount)
	}
}

func TestToolRetry_PermanentErrorNoRetry(t *testing.T) {
	cfg := DefaultToolRetryConfig()
	cfg.RetryOverrides["test"] = 3
	w := NewToolRetryWrapper(cfg)

	callCount := 0
	_, err := w.Execute(context.Background(), "test", func(ctx context.Context) (string, error) {
		callCount++
		return "", fmt.Errorf("invalid parameter: missing required field")
	})

	if err == nil {
		t.Fatal("expected error for permanent failure")
	}
	if callCount != 1 {
		t.Fatalf("expected 1 call (no retry for permanent error), got %d", callCount)
	}
}

func TestToolRetry_Timeout(t *testing.T) {
	cfg := ToolRetryConfig{
		DefaultTimeout: 100 * time.Millisecond,
		DefaultRetries: 0,
		TimeoutOverrides: map[string]time.Duration{},
		RetryOverrides:   map[string]int{},
	}
	w := NewToolRetryWrapper(cfg)

	_, err := w.Execute(context.Background(), "slow", func(ctx context.Context) (string, error) {
		select {
		case <-time.After(5 * time.Second):
			return "done", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})

	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestIsTransientError(t *testing.T) {
	transient := []string{
		"connection refused",
		"429 Too Many Requests",
		"503 Service Unavailable",
		"context deadline exceeded",
		"temporary network error",
	}
	for _, msg := range transient {
		if !isTransientError(fmt.Errorf("%s", msg)) {
			t.Errorf("expected transient: %s", msg)
		}
	}

	permanent := []string{
		"invalid parameter",
		"permission denied",
		"not found",
	}
	for _, msg := range permanent {
		if isTransientError(fmt.Errorf("%s", msg)) {
			t.Errorf("expected permanent: %s", msg)
		}
	}
}
