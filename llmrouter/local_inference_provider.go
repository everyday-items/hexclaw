package llmrouter

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/localinfer"
)

var errNilLocalInferenceStream = errors.New("local inference provider returned a nil stream")

// These optional interfaces keep HexClaw source-compatible with ai-core
// v0.2.5 while using the additive lifecycle/error contract as soon as the
// sibling/newer ai-core is present. The fallback remains deadline-safe, but a
// published ai-core containing these methods is required for immediate
// Close-before-Start accounting.
type localInferenceStreamLifecycle interface {
	OnFirstChunk(func()) *llm.Stream
	OnTerminal(func()) *llm.Stream
}

type localInferenceStreamError interface {
	Err() error
}

const (
	qwen35Local9BModel       = "qwen3.5:9b"
	qwen35Local9BChatCeiling = 360 * time.Second
	// Unknown local models retain the pre-existing ten-minute Ollama transport
	// ceiling as a lifecycle safety bound; only the exact calibrated Qwen model
	// receives the new 360-second policy.
	defaultLocalStreamCeiling = 10 * time.Minute
)

func localChatBudget(model string) time.Duration {
	if strings.EqualFold(strings.TrimSpace(model), qwen35Local9BModel) {
		return qwen35Local9BChatCeiling
	}
	return 0
}

type localInferenceProvider struct {
	next           hexagon.Provider
	coordinator    *localinfer.Coordinator
	defaultModel   string
	budgetForModel func(string) time.Duration
}

func (p *localInferenceProvider) Name() string { return p.next.Name() }

func (p *localInferenceProvider) model(req llm.CompletionRequest) string {
	if model := strings.TrimSpace(req.Model); model != "" {
		return model
	}
	return strings.TrimSpace(p.defaultModel)
}

func (p *localInferenceProvider) budget(model string) time.Duration {
	if p.budgetForModel != nil {
		if budget := p.budgetForModel(model); budget > 0 {
			return budget
		}
	}
	return defaultLocalStreamCeiling
}

func (p *localInferenceProvider) Complete(
	ctx context.Context,
	req llm.CompletionRequest,
) (response *llm.CompletionResponse, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	budgetCtx, cancel := context.WithTimeout(ctx, p.budget(p.model(req)))
	defer cancel()
	operation := localinfer.OperationFromContext(budgetCtx, localinfer.OperationChat)
	callCtx, lease, err := p.coordinator.Acquire(budgetCtx, operation)
	if err != nil {
		return nil, err
	}
	defer func() { lease.Finish(err) }()
	response, err = p.next.Complete(callCtx, req)
	if response != nil {
		lease.MarkFirstOutput()
	}
	return response, err
}

func (p *localInferenceProvider) Stream(
	ctx context.Context,
	req llm.CompletionRequest,
) (stream *llm.Stream, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	budgetCtx, cancel := context.WithTimeout(ctx, p.budget(p.model(req)))
	operation := localinfer.OperationFromContext(budgetCtx, localinfer.OperationChat)
	callCtx, lease, err := p.coordinator.Acquire(budgetCtx, operation)
	if err != nil {
		cancel()
		return nil, err
	}
	owned := true
	defer func() {
		if owned {
			lease.Finish(err)
			cancel()
		}
	}()
	stream, err = p.next.Stream(callCtx, req)
	if err != nil {
		return nil, err
	}
	if stream == nil {
		err = errNilLocalInferenceStream
		return nil, err
	}
	finish := func() {
		terminalErr := callCtx.Err()
		if source, ok := any(stream).(localInferenceStreamError); ok && source.Err() != nil {
			terminalErr = source.Err()
		}
		lease.Finish(terminalErr)
		cancel()
	}
	_, enhanced := any(stream).(localInferenceStreamLifecycle)
	if enhanced {
		lifecycle := any(stream).(localInferenceStreamLifecycle)
		lifecycle.OnFirstChunk(lease.MarkFirstOutput)
		lifecycle.OnTerminal(finish)
	}
	// A caller may abandon the returned stream without ever starting it. The
	// model/parent deadline is still authoritative and must close the body,
	// publish terminal state, and return the physical permit.
	go func() {
		select {
		case <-stream.Done():
			if !enhanced {
				finish()
			}
		case <-callCtx.Done():
			// ai-core v0.2.5 has no Close-before-Start terminal publication.
			// Start only at the absolute deadline so normal callers retain the
			// documented window to install OnChunk/OnDone callbacks. A published
			// ai-core with lifecycle hooks removes this compatibility limitation.
			if !enhanced {
				stream.Start()
			}
			_ = stream.Close()
			if !enhanced {
				finish()
			}
		}
	}()
	owned = false
	return stream, nil
}

func (p *localInferenceProvider) Models() []llm.ModelInfo { return p.next.Models() }

func (p *localInferenceProvider) CountTokens(messages []llm.Message) (int, error) {
	return p.next.CountTokens(messages)
}
