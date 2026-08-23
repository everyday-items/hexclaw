package main

import (
	"context"
	"encoding/base64"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"

	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type k12VisionRequestCaptureProvider struct {
	name              string
	request           llm.CompletionRequest
	operationSafety   llm.OperationSafety
	headerBudget      time.Duration
	hasHeaderBudget   bool
	hasCallerDeadline bool
	egressRequests    []egress.Request
	hasEgressRequest  bool
	calls             int
}

func (p *k12VisionRequestCaptureProvider) Name() string {
	if p.name != "" {
		return p.name
	}
	return "capture"
}

func (p *k12VisionRequestCaptureProvider) Complete(
	ctx context.Context,
	req llm.CompletionRequest,
) (*llm.CompletionResponse, error) {
	p.calls++
	p.request = req
	p.operationSafety = llm.OperationSafetyFromContext(ctx)
	p.headerBudget, p.hasHeaderBudget = egress.ProviderRequestResponseHeaderTimeoutFromContext(ctx)
	_, p.hasCallerDeadline = ctx.Deadline()
	p.egressRequests, p.hasEgressRequest = egress.RequestsFromContext(ctx)
	return &llm.CompletionResponse{Content: "captured"}, nil
}

func (*k12VisionRequestCaptureProvider) Stream(
	context.Context,
	llm.CompletionRequest,
) (*llm.Stream, error) {
	return nil, nil
}

func (*k12VisionRequestCaptureProvider) Models() []llm.ModelInfo { return nil }

func (*k12VisionRequestCaptureProvider) CountTokens([]llm.Message) (int, error) {
	return 0, nil
}

// REG-K12-RECOGNIZING-REAL-PROBE-PARITY-20260808-001：生产代码和两种真实模型探针
// 必须使用同一个 K12 视觉请求构建器。探针自行组装 CompletionRequest 时可能静默
// 丢失强类型识题策略，并验证了不同的 gpt-5.6-sol 线路契约。
func TestBUG20260808K12ProductionAndRealProbesUseOneVisionRequestBuilder(t *testing.T) {
	t.Parallel()

	for _, file := range []string{
		"main.go",
		"k12_im_photo_real_probe_test.go",
		"bug_20260808_k12_self_inventory_real_probe_test.go",
	} {
		file := file
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(raw), "completeK12VisionRequest("); got != 1 {
				t.Fatalf("canonical K12 vision request builder calls=%d, want exactly 1", got)
			}
		})
	}

	for file, forbidden := range map[string]string{
		"k12_im_photo_real_probe_test.go":                    "provider.Complete(visionCtx",
		"bug_20260808_k12_self_inventory_real_probe_test.go": "route.Provider.Complete(ctx",
	} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("%s still assembles a direct provider completion", file)
		}
	}
}

func TestBUG20260808K12VisionRequestBuilderPreservesTheProductionWireContract(t *testing.T) {
	provider := &k12VisionRequestCaptureProvider{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snapshot := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    "gpt-5.6-sol",
		Route:                    "hexclaw-gpt/gpt-5.6-sol",
		TimeoutMS:                120_000,
		RecognizingRequestPolicy: k12.ApprovedRecognizingRequestPolicy(),
	}
	ctx = k12.WithGradingModelSnapshot(ctx, snapshot)
	ctx = k12.WithGradingModelRequestPolicy(ctx, snapshot.RecognizingRequestPolicy)

	image := []byte("\x89PNG\r\n\x1a\n")
	content, err := completeK12VisionRequest(ctx, provider, snapshot.Model, image, "recognize")
	if err != nil {
		t.Fatal(err)
	}
	if content != "captured" || provider.calls != 1 {
		t.Fatalf("content=%q calls=%d", content, provider.calls)
	}
	if provider.operationSafety != llm.OperationSafetyNonIdempotent {
		t.Fatalf("operation safety=%q, want non-idempotent", provider.operationSafety)
	}
	if !provider.hasCallerDeadline || !provider.hasHeaderBudget || provider.headerBudget <= 0 {
		t.Fatalf("deadline=%t response-header budget=%s ok=%t",
			provider.hasCallerDeadline, provider.headerBudget, provider.hasHeaderBudget)
	}
	if !provider.hasEgressRequest || len(provider.egressRequests) != 1 ||
		provider.egressRequests[0].Purpose != egress.PurposeVisionOCR ||
		provider.egressRequests[0].DataClass != egress.ClassSensitiveMedia {
		t.Fatalf("egress requests=%+v ok=%t", provider.egressRequests, provider.hasEgressRequest)
	}
	if deadline, ok := ctx.Deadline(); !ok || provider.headerBudget > time.Until(deadline)+20*time.Millisecond {
		t.Fatalf("response-header budget=%s outlives caller deadline", provider.headerBudget)
	}
	if !reflect.DeepEqual(provider.request.Metadata, map[string]any{"thinking": "off"}) {
		t.Fatalf("metadata=%v, want exact recognizing policy", provider.request.Metadata)
	}
	if provider.request.ReasoningPolicyScope != llm.ReasoningPolicyScopeStructuredVisionRecognition {
		t.Fatalf("reasoning scope=%q", provider.request.ReasoningPolicyScope)
	}
	if provider.request.Model != snapshot.Model || len(provider.request.Messages) != 1 {
		t.Fatalf("model=%q messages=%d", provider.request.Model, len(provider.request.Messages))
	}
	message := provider.request.Messages[0]
	if message.Role != llm.RoleUser || len(message.MultiContent) != 2 ||
		message.MultiContent[0].Text != "recognize" || message.MultiContent[1].ImageURL == nil ||
		message.MultiContent[1].ImageURL.Detail != "high" {
		t.Fatalf("multimodal request=%+v", message)
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(message.MultiContent[1].ImageURL.URL, prefix) {
		t.Fatalf("image URL has wrong MIME prefix")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(
		message.MultiContent[1].ImageURL.URL,
		prefix,
	))
	if err != nil || !reflect.DeepEqual(decoded, image) {
		t.Fatalf("image payload mismatch: err=%v", err)
	}
}

func TestK12VisionLogIdentitySeparatesFrozenRouteFromCompatibleAdapterName(t *testing.T) {
	ctx := k12.WithGradingModelSnapshot(context.Background(), k12.GradingModelSnapshot{
		Provider: "hexclaw-gpt",
		Model:    "gpt-5.6-sol",
	})
	routeProvider, adapterProvider := k12ProviderLogIdentity(
		ctx,
		&k12VisionRequestCaptureProvider{name: "openai"},
	)
	if routeProvider != "hexclaw-gpt" || adapterProvider != "openai" {
		t.Fatalf("log identity route=%q adapter=%q", routeProvider, adapterProvider)
	}
}

func TestBUG20260808K12VisionRequestBuilderDoesNotInventARecognizingPolicy(t *testing.T) {
	provider := &k12VisionRequestCaptureProvider{}
	if _, err := completeK12VisionRequest(
		context.Background(),
		provider,
		"other-vision-model",
		[]byte("\x89PNG\r\n\x1a\n"),
		"caption",
	); err != nil {
		t.Fatal(err)
	}
	if provider.request.Metadata != nil || provider.request.ReasoningPolicyScope != "" {
		t.Fatalf("unscoped request inherited policy: metadata=%v scope=%q",
			provider.request.Metadata, provider.request.ReasoningPolicyScope)
	}
	if provider.hasHeaderBudget {
		t.Fatalf("deadline-free request extended transport by %s", provider.headerBudget)
	}
}
