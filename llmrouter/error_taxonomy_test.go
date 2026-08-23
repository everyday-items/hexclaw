package llmrouter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
)

func TestClassifyLLMErrorUsesStructuredCauses(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantCode      LLMErrorCode
		wantStatus    int
		wantRetryable bool
		wantKnown     bool
	}{
		{
			name:          "wrapped capability mismatch",
			err:           fmt.Errorf("completion failed: %w", ErrModelCapabilityMismatch),
			wantCode:      LLMErrorCodeModelCapabilityMismatch,
			wantStatus:    http.StatusUnprocessableEntity,
			wantRetryable: false,
			wantKnown:     true,
		},
		{
			name: "wrapped provider rate limit",
			err: fmt.Errorf("completion failed: %w", &llm.ProviderError{
				Provider: "test", StatusCode: http.StatusTooManyRequests,
			}),
			wantCode:      LLMErrorCodeUpstreamRateLimited,
			wantStatus:    http.StatusTooManyRequests,
			wantRetryable: true,
			wantKnown:     true,
		},
		{
			name: "structured pool code is pool exhausted",
			err: fmt.Errorf("completion failed: %w", &llm.ProviderError{
				Provider: "test", StatusCode: http.StatusServiceUnavailable,
				Body: `{"error":{"code":"pool_exhausted"}}`,
			}),
			wantCode:      LLMErrorCodeUpstreamPoolExhausted,
			wantStatus:    http.StatusServiceUnavailable,
			wantRetryable: true,
			wantKnown:     true,
		},
		{
			name: "structured account pool code is pool exhausted",
			err: fmt.Errorf("completion failed: %w", &llm.ProviderError{
				Provider: "test", StatusCode: http.StatusServiceUnavailable,
				Body: `{"error":{"code":"account_pool_exhausted"}}`,
			}),
			wantCode:      LLMErrorCodeUpstreamPoolExhausted,
			wantStatus:    http.StatusServiceUnavailable,
			wantRetryable: true,
			wantKnown:     true,
		},
		{
			name: "structured no available accounts type is pool exhausted",
			err: fmt.Errorf("completion failed: %w", &llm.ProviderError{
				Provider: "test", StatusCode: http.StatusServiceUnavailable,
				Body: `{"error":{"type":"no_available_accounts"}}`,
			}),
			wantCode:      LLMErrorCodeUpstreamPoolExhausted,
			wantStatus:    http.StatusServiceUnavailable,
			wantRetryable: true,
			wantKnown:     true,
		},
		{
			name: "generic service unavailable is unavailable",
			err: fmt.Errorf("completion failed: %w", &llm.ProviderError{
				Provider: "test", StatusCode: http.StatusServiceUnavailable,
				Body: `{"error":{"type":"api_error","message":"No available accounts"}}`,
			}),
			wantCode:      LLMErrorCodeUpstreamUnavailable,
			wantStatus:    http.StatusServiceUnavailable,
			wantRetryable: true,
			wantKnown:     true,
		},
		{
			name: "structured gateway error is unavailable",
			err: fmt.Errorf("completion failed: %w", &llm.ProviderError{
				Provider: "test", StatusCode: http.StatusBadGateway,
			}),
			wantCode:      LLMErrorCodeUpstreamUnavailable,
			wantStatus:    http.StatusServiceUnavailable,
			wantRetryable: true,
			wantKnown:     true,
		},
		{
			name:          "no configured provider is unavailable",
			err:           fmt.Errorf("selection failed: %w", ErrNoProvider),
			wantCode:      LLMErrorCodeUpstreamUnavailable,
			wantStatus:    http.StatusServiceUnavailable,
			wantRetryable: true,
			wantKnown:     true,
		},
		{
			name:          "deadline is unavailable",
			err:           fmt.Errorf("completion failed: %w", context.DeadlineExceeded),
			wantCode:      LLMErrorCodeUpstreamUnavailable,
			wantStatus:    http.StatusServiceUnavailable,
			wantRetryable: true,
			wantKnown:     true,
		},
		{
			name:      "unstructured user facing text is not classified",
			err:       errors.New("429 Too Many Requests"),
			wantKnown: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, known := ClassifyLLMError(tt.err)
			if known != tt.wantKnown {
				t.Fatalf("known=%t, want %t; classification=%+v", known, tt.wantKnown, got)
			}
			if !known {
				return
			}
			if got.Code != tt.wantCode || got.HTTPStatus != tt.wantStatus || got.Retryable != tt.wantRetryable {
				t.Fatalf("classification=%+v, want code=%s status=%d retryable=%t", got, tt.wantCode, tt.wantStatus, tt.wantRetryable)
			}
		})
	}
}
