package cron

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
)

func TestModelLogRedactionCompileFailureOmitsRawModelOutput(t *testing.T) {
	const rawModelOutput = "compiler-model-output-must-not-be-returned-or-logged"
	compiler := NewLLMCompilerStatic(&seqProvider{responses: []string{rawModelOutput, rawModelOutput}}, "test-model")

	_, err := compiler.Compile(context.Background(), "生成一个独特的天气采集自动化任务", CompileHints{})
	if err == nil {
		t.Fatal("unsalvageable compiler output should fail")
	}
	if strings.Contains(err.Error(), rawModelOutput) {
		t.Fatalf("compile error retained raw model output: %q", err)
	}
	if !strings.Contains(err.Error(), "output_len="+strconv.Itoa(len(rawModelOutput))) {
		t.Fatalf("compile error omitted output length diagnostic: %q", err)
	}
}

func TestModelLogRedactionSafeCompileLogErrorOmitsProviderBody(t *testing.T) {
	const rawBody = `{"error":{"message":"provider-body-must-not-reach-cron-log"}}`
	got := safeCompileLogError(&llm.ProviderError{
		StatusCode: 429,
		Status:     "429 Too Many Requests",
		Body:       rawBody,
	})
	if strings.Contains(got, "provider-body-must-not-reach-cron-log") || strings.Contains(got, rawBody) {
		t.Fatalf("compile log error retained provider body: %q", got)
	}
	if got != "provider_http_429" {
		t.Fatalf("compile log error = %q, want provider_http_429", got)
	}
}
