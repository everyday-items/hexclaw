package llmrouter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/hexagon-codes/ai-core/llm"
)

// LLMErrorCode 是聊天链路向调用方公开的稳定错误代码。
type LLMErrorCode string

const (
	LLMErrorCodeModelCapabilityMismatch LLMErrorCode = "MODEL_CAPABILITY_MISMATCH"
	LLMErrorCodeUpstreamRateLimited     LLMErrorCode = "UPSTREAM_RATE_LIMITED"
	LLMErrorCodeUpstreamPoolExhausted   LLMErrorCode = "UPSTREAM_POOL_EXHAUSTED"
	LLMErrorCodeUpstreamUnavailable     LLMErrorCode = "UPSTREAM_UNAVAILABLE"
)

// LLMErrorClassification 是与传输协议无关的错误投影。
type LLMErrorClassification struct {
	Code       LLMErrorCode
	HTTPStatus int
	Retryable  bool
}

// ClassifyLLMError 仅依据错误链和结构化上游响应分类，不读取错误文案猜测状态。
func ClassifyLLMError(err error) (LLMErrorClassification, bool) {
	if err == nil {
		return LLMErrorClassification{}, false
	}
	if errors.Is(err, ErrModelCapabilityMismatch) || errors.Is(err, ErrNoCapableModel) {
		return LLMErrorClassification{
			Code:       LLMErrorCodeModelCapabilityMismatch,
			HTTPStatus: http.StatusUnprocessableEntity,
			Retryable:  false,
		}, true
	}

	var providerErr *llm.ProviderError
	if errors.As(err, &providerErr) && providerErr != nil {
		switch providerErr.StatusCode {
		case http.StatusTooManyRequests:
			return LLMErrorClassification{
				Code:       LLMErrorCodeUpstreamRateLimited,
				HTTPStatus: http.StatusTooManyRequests,
				Retryable:  true,
			}, true
		case http.StatusServiceUnavailable:
			if providerErrorHasPoolExhaustionCode(providerErr) {
				return LLMErrorClassification{
					Code:       LLMErrorCodeUpstreamPoolExhausted,
					HTTPStatus: http.StatusServiceUnavailable,
					Retryable:  true,
				}, true
			}
			return LLMErrorClassification{
				Code:       LLMErrorCodeUpstreamUnavailable,
				HTTPStatus: http.StatusServiceUnavailable,
				Retryable:  true,
			}, true
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout:
			return LLMErrorClassification{
				Code:       LLMErrorCodeUpstreamUnavailable,
				HTTPStatus: http.StatusServiceUnavailable,
				Retryable:  true,
			}, true
		}
	}

	if errors.Is(err, ErrNoProvider) || errors.Is(err, context.DeadlineExceeded) {
		return LLMErrorClassification{
			Code:       LLMErrorCodeUpstreamUnavailable,
			HTTPStatus: http.StatusServiceUnavailable,
			Retryable:  true,
		}, true
	}

	return LLMErrorClassification{}, false
}

// providerErrorHasPoolExhaustionCode 仅接受上游结构化错误对象中的固定枚举，避免依据文案误判。
func providerErrorHasPoolExhaustionCode(providerErr *llm.ProviderError) bool {
	if providerErr == nil || providerErr.Body == "" {
		return false
	}

	var payload struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(providerErr.Body), &payload); err != nil {
		return false
	}

	return isPoolExhaustionCode(payload.Error.Code) || isPoolExhaustionCode(payload.Error.Type)
}

func isPoolExhaustionCode(value string) bool {
	switch value {
	case "pool_exhausted", "account_pool_exhausted", "no_available_accounts":
		return true
	default:
		return false
	}
}
