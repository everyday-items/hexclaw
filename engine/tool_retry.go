package engine

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// ToolRetryConfig configures per-tool timeout and retry behavior.
type ToolRetryConfig struct {
	DefaultTimeout  time.Duration // default 30s
	DefaultRetries  int           // default 0 (no retry)
	TimeoutOverrides map[string]time.Duration // tool_name → custom timeout
	RetryOverrides   map[string]int           // tool_name → max retries
}

// DefaultToolRetryConfig returns sensible defaults.
func DefaultToolRetryConfig() ToolRetryConfig {
	return ToolRetryConfig{
		DefaultTimeout: 30 * time.Second,
		DefaultRetries: 0,
		TimeoutOverrides: map[string]time.Duration{
			"browser":    120 * time.Second,
			"code_exec":  60 * time.Second,
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

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 1s, 2s, 4s
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			log.Printf("[retry] %s attempt %d/%d after %v", toolName, attempt+1, maxRetries+1, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}

		execCtx, cancel := context.WithTimeout(ctx, timeout)
		result, err := execFn(execCtx)
		cancel()

		if err == nil {
			return result, nil
		}
		lastErr = err

		// Only retry on transient errors
		if !isTransientError(err) {
			return "", err
		}
	}

	return "", fmt.Errorf("tool %q failed after %d attempts: %w", toolName, maxRetries+1, lastErr)
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
