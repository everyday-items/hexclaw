package k12

import (
	"context"
	"errors"
	"fmt"
)

var (
	ErrRecognitionProtocolInvalid = errors.New(
		"structured recognition protocol invalid",
	)
	ErrRecognitionPhysicalCallBeforeSend = errors.New(
		"recognition physical call failed before provider send",
	)
	ErrRecognitionPhysicalFallbackUnauthorized = errors.New(
		"recognition physical fallback is not durably authorized",
	)
)

// RecognitionPhysicalUnit is the explicit identity of one actual structured
// recognition provider request. It must be supplied by the recognition
// algorithm; adapters must never infer it from prompt text.
type RecognitionPhysicalUnit string

const (
	RecognitionPhysicalUnitWholePage        RecognitionPhysicalUnit = "whole_page"
	RecognitionPhysicalUnitSegment1         RecognitionPhysicalUnit = "segment_1"
	RecognitionPhysicalUnitSegment2         RecognitionPhysicalUnit = "segment_2"
	RecognitionPhysicalUnitSegment3         RecognitionPhysicalUnit = "segment_3"
	RecognitionPhysicalUnitSegment4         RecognitionPhysicalUnit = "segment_4"
	RecognitionPhysicalUnitSegment5         RecognitionPhysicalUnit = "segment_5"
	RecognitionPhysicalUnitPrintedInventory RecognitionPhysicalUnit = "printed_inventory"
)

func (u RecognitionPhysicalUnit) Valid() bool {
	switch u {
	case RecognitionPhysicalUnitWholePage,
		RecognitionPhysicalUnitSegment1,
		RecognitionPhysicalUnitSegment2,
		RecognitionPhysicalUnitSegment3,
		RecognitionPhysicalUnitSegment4,
		RecognitionPhysicalUnitSegment5,
		RecognitionPhysicalUnitPrintedInventory:
		return true
	default:
		return false
	}
}

func RecognitionPhysicalSegmentUnit(index int) (RecognitionPhysicalUnit, bool) {
	switch index {
	case 1:
		return RecognitionPhysicalUnitSegment1, true
	case 2:
		return RecognitionPhysicalUnitSegment2, true
	case 3:
		return RecognitionPhysicalUnitSegment3, true
	case 4:
		return RecognitionPhysicalUnitSegment4, true
	case 5:
		return RecognitionPhysicalUnitSegment5, true
	default:
		return "", false
	}
}

// RecognitionPhysicalCall carries only the transient input needed to bind a
// durable digest. Image bytes are never persisted by this contract.
type RecognitionPhysicalCall struct {
	Unit  RecognitionPhysicalUnit
	Image []byte
}

type RecognitionPhysicalCallResult struct {
	Payload      string
	InvocationID string
}

type RecognitionPhysicalCallExecutor interface {
	ExecuteRecognitionPhysicalCall(
		context.Context,
		RecognitionPhysicalCall,
		func(context.Context) (string, error),
	) (RecognitionPhysicalCallResult, error)
}

// RecognitionPhysicalFallbackAuthorizer is the optional durable control-plane
// capability used after a succeeded whole-page response is proven to violate
// the structured-result protocol. Merely choosing a segment unit never grants
// permission to issue another Provider request.
type RecognitionPhysicalFallbackAuthorizer interface {
	AuthorizeRecognitionPhysicalFallback(
		context.Context,
		RecognitionPhysicalCallResult,
	) error
}

type recognitionPhysicalCallContextKey struct{}
type recognitionPhysicalTransportSendBoundaryContextKey struct{}

type RecognitionPhysicalBeforeSendHook func(context.Context) error

// RecognitionPhysicalTransportSendBinder is the adapter seam that binds one
// durable authorization hook to the actual Provider transport. The K12
// domain/usecase layers own the hook semantics but never import ai-core.
type RecognitionPhysicalTransportSendBinder func(
	context.Context,
	RecognitionPhysicalBeforeSendHook,
) context.Context

func WithRecognitionPhysicalCallExecutor(
	ctx context.Context,
	executor RecognitionPhysicalCallExecutor,
) context.Context {
	return context.WithValue(ctx, recognitionPhysicalCallContextKey{}, executor)
}

// WithRecognitionPhysicalTransportSendBoundary requires the physical child
// sent CAS to be performed by ai-core's shared HTTP transport immediately
// before http.Client.Do, rather than eagerly at the adapter boundary.
func WithRecognitionPhysicalTransportSendBoundary(
	ctx context.Context,
	binder RecognitionPhysicalTransportSendBinder,
) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if binder == nil {
		return ctx
	}
	return context.WithValue(
		ctx,
		recognitionPhysicalTransportSendBoundaryContextKey{},
		binder,
	)
}

func RecognitionPhysicalTransportSendBoundaryFromContext(
	ctx context.Context,
) (RecognitionPhysicalTransportSendBinder, bool) {
	if ctx == nil {
		return nil, false
	}
	binder, ok := ctx.Value(
		recognitionPhysicalTransportSendBoundaryContextKey{},
	).(RecognitionPhysicalTransportSendBinder)
	return binder, ok && binder != nil
}

func ExecuteRecognitionPhysicalCall(
	ctx context.Context,
	call RecognitionPhysicalCall,
	send func(context.Context) (string, error),
) (RecognitionPhysicalCallResult, error) {
	if !call.Unit.Valid() || len(call.Image) == 0 {
		return RecognitionPhysicalCallResult{}, fmt.Errorf(
			"invalid recognition physical call identity",
		)
	}
	if send == nil {
		return RecognitionPhysicalCallResult{}, fmt.Errorf(
			"recognition physical call sender is nil",
		)
	}
	if ctx != nil {
		if executor, ok := ctx.Value(
			recognitionPhysicalCallContextKey{},
		).(RecognitionPhysicalCallExecutor); ok && executor != nil {
			return executor.ExecuteRecognitionPhysicalCall(ctx, call, send)
		}
		if policy, ok := GradingModelRequestPolicyFromContext(ctx); ok &&
			NormalizeModelRequestPolicySnapshot(policy) ==
				ApprovedRecognizingRequestPolicy() {
			return RecognitionPhysicalCallResult{}, fmt.Errorf(
				"%w: approved structured-recognition policy requires a durable physical-call executor",
				ErrRecognitionPhysicalCallBeforeSend,
			)
		}
	}
	payload, err := send(ctx)
	return RecognitionPhysicalCallResult{Payload: payload}, err
}

// AuthorizeRecognitionPhysicalFallback binds the parser's protocol-invalid
// decision to the exact durable whole-page result. Approved recognizing calls
// fail closed when their executor does not implement this capability; legacy
// unscoped adapter tests keep their existing in-memory behavior.
func AuthorizeRecognitionPhysicalFallback(
	ctx context.Context,
	whole RecognitionPhysicalCallResult,
) error {
	if ctx != nil {
		if executor, ok := ctx.Value(
			recognitionPhysicalCallContextKey{},
		).(RecognitionPhysicalCallExecutor); ok && executor != nil {
			authorizer, supported :=
				executor.(RecognitionPhysicalFallbackAuthorizer)
			if !supported {
				return fmt.Errorf(
					"%w: durable executor lacks fallback authorization",
					ErrRecognitionPhysicalFallbackUnauthorized,
				)
			}
			if whole.InvocationID == "" {
				return fmt.Errorf(
					"%w: whole-page invocation identity is empty",
					ErrRecognitionPhysicalFallbackUnauthorized,
				)
			}
			if err := authorizer.AuthorizeRecognitionPhysicalFallback(
				ctx,
				whole,
			); err != nil {
				return fmt.Errorf(
					"%w: %v",
					ErrRecognitionPhysicalFallbackUnauthorized,
					err,
				)
			}
			return nil
		}
		if policy, ok := GradingModelRequestPolicyFromContext(ctx); ok &&
			NormalizeModelRequestPolicySnapshot(policy) ==
				ApprovedRecognizingRequestPolicy() {
			return fmt.Errorf(
				"%w: approved policy requires a durable executor",
				ErrRecognitionPhysicalFallbackUnauthorized,
			)
		}
	}
	return nil
}
