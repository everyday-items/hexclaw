package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/knowledge"
)

type knowledgeOCRCaptureProvider struct {
	request llm.CompletionRequest
	err     error
}

func (*knowledgeOCRCaptureProvider) Name() string { return "capture" }

func (p *knowledgeOCRCaptureProvider) Complete(
	_ context.Context,
	request llm.CompletionRequest,
) (*llm.CompletionResponse, error) {
	p.request = request
	if p.err != nil {
		return nil, p.err
	}
	return &llm.CompletionResponse{Content: "第 1 题：\\(a \\div b = a/b\\)"}, nil
}

func (*knowledgeOCRCaptureProvider) Stream(
	context.Context,
	llm.CompletionRequest,
) (*llm.Stream, error) {
	return nil, nil
}

func (*knowledgeOCRCaptureProvider) Models() []llm.ModelInfo { return nil }

func (*knowledgeOCRCaptureProvider) CountTokens([]llm.Message) (int, error) { return 0, nil }

func TestKnowledgeOCRAdapterUsesFaithfulTextbookTranscriptionAndRealRouteReceipt(t *testing.T) {
	provider := &knowledgeOCRCaptureProvider{}
	result, err := completeKnowledgePDFPageOCR(
		context.Background(), provider, "hexclaw-gpt", "gpt-5.6-sol",
		[]byte("rendered-page"), "image/png",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Content == "" || result.RouteReceipt != (knowledge.OCRRouteReceipt{
		Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
		Operation: knowledge.OCRRouteOperationPDFPage,
		Status:    knowledge.OCRRouteStatusSucceeded, Fake: false,
	}) {
		t.Fatalf("OCR result=%+v", result)
	}
	if provider.request.Model != "gpt-5.6-sol" || len(provider.request.Messages) != 1 ||
		len(provider.request.Messages[0].MultiContent) != 2 {
		t.Fatalf("OCR request=%+v", provider.request)
	}
	prompt := provider.request.Messages[0].MultiContent[0].Text
	for _, required := range []string{
		"Faithfully transcribe", "all visible text", "mathematical formulas",
		"question numbers", "tables", "original hierarchy", "Do not summarize",
		"Do not infer", "Respond in Chinese", "output only the transcription",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("OCR prompt missing %q: %q", required, prompt)
		}
	}
	for _, forbidden := range []string{"briefly describe", "main content", "for knowledge base retrieval"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("OCR prompt still asks for caption %q: %q", forbidden, prompt)
		}
	}
	for _, current := range prompt {
		if unicode.Is(unicode.Han, current) {
			t.Fatalf("OCR provider prompt must be written in English: %q", prompt)
		}
	}
}

func TestKnowledgeOCRAdapterDoesNotCreateSuccessReceiptOnProviderFailure(t *testing.T) {
	provider := &knowledgeOCRCaptureProvider{err: errors.New("provider unavailable")}
	result, err := completeKnowledgePDFPageOCR(
		context.Background(), provider, "hexclaw-gpt", "gpt-5.6-sol",
		[]byte("rendered-page"), "image/png",
	)
	if err == nil || result.Content != "" || result.RouteReceipt.Provider != "" ||
		result.RouteReceipt.Status != "" {
		t.Fatalf("failed OCR result=%+v err=%v", result, err)
	}
}
