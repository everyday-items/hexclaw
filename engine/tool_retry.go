package engine

import (
	"context"
	"fmt"
	"github.com/hexagon-codes/toolkit/util/logger"
	"strings"
	"time"

	"github.com/hexagon-codes/toolkit/util/retry"
)

// ToolRetryConfig configures per-tool timeout and retry behavior.
type ToolRetryConfig struct {
	DefaultTimeout   time.Duration            // default 30s
	DefaultRetries   int                      // default 0 (no retry)
	TimeoutOverrides map[string]time.Duration // tool_name → custom timeout
	RetryOverrides   map[string]int           // tool_name → max retries
}

// DefaultToolRetryConfig returns sensible defaults.
func DefaultToolRetryConfig() ToolRetryConfig {
	return ToolRetryConfig{
		DefaultTimeout: 30 * time.Second,
		DefaultRetries: 0,
		TimeoutOverrides: map[string]time.Duration{
			"browser":   120 * time.Second,
			"code_exec": 60 * time.Second,
		},
		RetryOverrides: map[string]int{
			"search":  2, // network tool → retry on transient errors
			"browser": 1,
		},
	}
}

// ToolRetryWrapper wraps tool execution with timeout and retry logic.
type ToolRetryWrapper struct {
	cfg ToolRetryConfig
}

// NewToolRetryWrapper creates a retry wrapper.
func NewToolRetryWrapper(cfg ToolRetryConfig) *ToolRetryWrapper {
	return &ToolRetryWrapper{cfg: cfg}
}

// Execute wraps a tool execution with timeout + exponential backoff retry.
// Uses toolkit/util/retry for backoff and jitter.
func (w *ToolRetryWrapper) Execute(
	ctx context.Context,
	toolName string,
	execFn func(ctx context.Context) (string, error),
) (string, error) {
	timeout := w.cfg.DefaultTimeout
	if override, ok := w.cfg.TimeoutOverrides[toolName]; ok {
		timeout = override
	}

	maxRetries := w.cfg.DefaultRetries
	if override, ok := w.cfg.RetryOverrides[toolName]; ok {
		maxRetries = override
	}

	var result string
	err := retry.DoWithContext(ctx, func() error {
		execCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		var execErr error
		result, execErr = execFn(execCtx)
		return execErr
	},
		retry.Attempts(maxRetries+1),
		retry.Delay(time.Second),
		retry.MaxDelay(8*time.Second),
		retry.Multiplier(2.0),
		retry.RetryIf(isTransientError),
		retry.OnRetry(func(n int, err error) {
			logger.Info("[retry]", "toolName", toolName, "value", n+1, "value", maxRetries+1, "error", err)
		}),
	)

	if err != nil {
		return "", fmt.Errorf("tool %q failed: %w", toolName, err)
	}
	return result, nil
}

// isTransientError checks if an error is worth retrying.
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	transientPatterns := []string{
		"timeout", "deadline exceeded",
		"connection refused", "connection reset",
		"429", "too many requests",
		"503", "service unavailable",
		"502", "bad gateway",
		"temporary", "transient",
	}
	for _, pattern := range transientPatterns {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}
