package engineadapter

import (
	"errors"

	"github.com/hexagon-codes/ai-core/llm"
)

type definitiveProviderResponseError struct {
	statusCode int
	cause      error
}

func (e *definitiveProviderResponseError) Error() string {
	return e.cause.Error()
}

func (e *definitiveProviderResponseError) Unwrap() error {
	return e.cause
}

func (e *definitiveProviderResponseError) ProviderResponseStatusCode() int {
	return e.statusCode
}

// providerResponseError translates the concrete ai-core transport error into
// the usecase port's narrow outcome contract shared by solve, grade, and
// recognition adapters. Transport failures remain untyped because their
// execution outcome may be ambiguous after a request was sent.
func providerResponseError(err error) error {
	if err == nil {
		return nil
	}
	var providerErr *llm.ProviderError
	if errors.As(err, &providerErr) && providerErr != nil && providerErr.StatusCode > 0 {
		return &definitiveProviderResponseError{statusCode: providerErr.StatusCode, cause: err}
	}
	return err
}
