package engineadapter

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// TestRealAnswerAnchoring is the opt-in release gate for the complete production vision architecture:
// six fixed core-recognition calls (five segments plus print-only inventory), one page-batch geometry
// call, two parallel broad handwriting readers and, for each answer that remains unresolved, one
// isolated pair of high-resolution original/stroke readers focused only on that answer.
func TestRealAnswerAnchoring(t *testing.T) {
	if os.Getenv("HEXCLAW_REAL_BBOX_EVAL") != "1" {
		t.Skip("set HEXCLAW_REAL_BBOX_EVAL=1 to run the real OpenAI answer-anchor gate")
	}
	imagePath := strings.TrimSpace(os.Getenv("HEXCLAW_REAL_BBOX_IMAGE"))
	if imagePath == "" {
		t.Fatal("HEXCLAW_REAL_BBOX_IMAGE is required")
	}
	imageBytes, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatalf("read real bbox fixture: error_type=%T", err)
	}
	appCfg, err := config.Load("")
	if err != nil {
		t.Fatalf("load real bbox config: error_type=%T", err)
	}
	applyRealBBoxProviderOverride(t, appCfg)
	selector, err := llmrouter.New(appCfg.LLM)
	if err != nil {
		t.Fatalf("build real bbox provider selector: error_type=%T", err)
	}
	provider, providerName, model := selectRealBBoxProvider(t, selector)

	var calls atomic.Int32
	vision := func(ctx context.Context, image []byte, prompt string) (string, error) {
		call := calls.Add(1)
		mime := http.DetectContentType(image)
		dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(image)
		response, completeErr := provider.Complete(ctx, llm.CompletionRequest{
			Model: model,
			Messages: []llm.Message{{
				Role: llm.RoleUser,
				MultiContent: []llm.ContentPart{
					llm.NewTextPart(prompt),
					llm.NewImageURLPart(dataURL, "high"),
				},
			}},
		})
		if completeErr != nil {
			t.Logf("call=%d provider=%s model=%s error_type=%T", call, providerName, model, completeErr)
			return "", fmt.Errorf("real bbox provider completion failed: %T", completeErr)
		}
		t.Logf("call=%d provider=%s model=%s response_chars=%d", call, providerName, model, len([]rune(response.Content)))
		return response.Content, nil
	}

	ctx := t.Context()
	adapter := NewRecognizerAdapter(vision)
	startedAt := time.Now()
	questions, err := adapter.Recognize(ctx, imageBytes)
	if err != nil {
		t.Fatalf("real bbox recognition failed: error_type=%T", err)
	}
	coreCallCount := int32(len(denseWorksheetRanges) + 1)
	if calls.Load() != coreCallCount {
		t.Fatalf("dense core recognition calls=%d want=%d (5 segments + printed inventory)",
			calls.Load(), coreCallCount)
	}
	present, unclear := 0, 0
	seenQuestions := make(map[string]int, len(questions))
	for i, question := range questions {
		if question.BBox != nil {
			t.Fatalf("core question %d leaked unverified bbox: state=%s question_chars=%d answer_chars=%d raw_chars=%d",
				i+1, question.AnswerState, len([]rune(question.Question)),
				len([]rune(question.StudentAnswer)), len([]rune(question.RawTranscription)))
		}
		key := recognizedQuestionKey(question.Question)
		if previous, duplicate := seenQuestions[key]; key != "" && duplicate {
			t.Fatalf("core questions %d and %d are duplicate variants", previous+1, i+1)
		}
		seenQuestions[key] = i
		if question.AnswerState == usecase.AnswerStatePresent {
			present++
		} else if question.AnswerState == usecase.AnswerStateUnclear {
			unclear++
		}
		t.Logf("core[%d] state=%s question_chars=%d answer_chars=%d", i+1, question.AnswerState,
			len([]rune(question.Question)), len([]rune(question.StudentAnswer)))
	}
	answerCandidates := present + unclear
	if len(questions) != 16 || answerCandidates != 15 {
		t.Fatalf("real answered worksheet contract drift: questions=%d want=16 present=%d unclear=%d candidates=%d want=15",
			len(questions), present, unclear, answerCandidates)
	}
	expectedQuestions := []string{
		"4÷0.5=",
		"10×0.01=",
		"4.7+2.3=",
		"1.8×50=",
		"3.25+0.75=",
		"5/7−1/5=",
		"7−5/7=",
		"0.5+1/3=",
		"4/5+2/5=",
		"8.7×17.4−8.7×7.4",
		"15.02−6.8−1.02",
		"0.25+11/15+4/15+3/4",
		"1、一个数的3/8是24，求这个数？",
		"2、8的1/4的4/5是多少？",
		"一个周长是300米的长方形鱼塘，长是宽的2倍。如果每平方米产鱼2.25千克，一共产鱼多少千克？",
		"在下列六个数：5、6、12、14、23、29中划去数（ ）后，能使其中3个数的和为另外2个数和的2倍。",
	}
	for expectedIndex, expected := range expectedQuestions {
		if _, ok := seenQuestions[recognizedQuestionKey(expected)]; !ok {
			t.Fatalf("real answered worksheet lost or corrupted expected question index=%d: questions=%d",
				expectedIndex+1, len(questions))
		}
	}

	anchored, err := adapter.AnchorAnswers(ctx, imageBytes, questions)
	if err != nil {
		t.Fatalf("real answer anchoring failed: error_type=%T", err)
	}
	baseCalls := int32(len(denseWorksheetRanges) + 4)
	extraCalls := calls.Load() - baseCalls
	if extraCalls < 0 || extraCalls%answerTranscriptionFocusCalls != 0 ||
		extraCalls > int32(answerCandidates*answerTranscriptionFocusCalls*2) {
		t.Fatalf("full page call count=%d has invalid focused-pair expansion from base=%d",
			calls.Load(), baseCalls)
	}
	boxed := 0
	anchoredPresent := 0
	anchoredByKey := make(map[string]usecase.RecognizedQuestion, len(anchored))
	for i, question := range anchored {
		if question.BBox != nil {
			boxed++
		}
		if question.AnswerState == usecase.AnswerStatePresent {
			anchoredPresent++
		}
		anchoredByKey[recognizedQuestionKey(question.Question)] = question
		t.Logf("anchored[%d] state=%s question_chars=%d answer_chars=%d bbox=%t",
			i+1, question.AnswerState, len([]rune(question.Question)),
			len([]rune(question.StudentAnswer)), question.BBox != nil)
	}
	expectedFinalAnswers := map[string]string{
		"4÷0.5=":              "8",
		"10×0.01=":            "0.1",
		"4.7+2.3=":            "7",
		"1.8×50=":             "90",
		"3.25+0.75=":          "4",
		"5/7−1/5=":            "18/35",
		"7−5/7=":              "6 2/7",
		"0.5+1/3=":            "2/3",
		"4/5+2/5=":            "6/5",
		"8.7×17.4−8.7×7.4":    "88",
		"15.02−6.8−1.02":      "7.2",
		"0.25+11/15+4/15+3/4": "2",
		"1、一个数的3/8是24，求这个数？": "64",
		"2、8的1/4的4/5是多少？":    "8/5",
		"一个周长是300米的长方形鱼塘，长是宽的2倍。如果每平方米产鱼2.25千克，一共产鱼多少千克？": "225kg",
	}
	for question, expected := range expectedFinalAnswers {
		item := anchoredByKey[recognizedQuestionKey(question)]
		actual := finalTranscribedAnswer(item.StudentAnswer)
		if canonicalTranscribedAnswer(actual) != canonicalTranscribedAnswer(expected) {
			t.Fatalf("real handwriting final answer drift: question_key_chars=%d answer_chars=%d final_chars=%d want_chars=%d state=%s",
				len([]rune(recognizedQuestionKey(question))), len([]rune(item.StudentAnswer)),
				len([]rune(actual)), len([]rune(expected)), item.AnswerState)
		}
	}
	divisionAnswer := anchoredByKey[recognizedQuestionKey("1、一个数的3/8是24，求这个数？")].StudentAnswer
	divisionAnswerKey := recognizedQuestionKey(divisionAnswer)
	if strings.Contains(divisionAnswerKey, recognizedQuestionKey("3/8×8")) {
		t.Fatalf("isolated handwriting transcription still contains printed-question contamination: answer_chars=%d",
			len([]rune(divisionAnswer)))
	}
	firstFractionAnswer := anchoredByKey[recognizedQuestionKey("5/7−1/5=")].StudentAnswer
	if recognizedQuestionKey(firstFractionAnswer) != recognizedQuestionKey("18/35") {
		t.Fatalf("isolated handwriting transcription mismatch: answer_chars=%d want_chars=%d",
			len([]rune(firstFractionAnswer)), len([]rune("18/35")))
	}
	t.Logf("questions=%d core_present=%d core_unclear=%d anchored=%d calls=%d elapsed=%s",
		len(questions), present, unclear, boxed, calls.Load(), time.Since(startedAt).Round(time.Millisecond))
	if boxed != answerCandidates || anchoredPresent != answerCandidates {
		t.Fatalf("real answered worksheet retained anchors=%d present=%d want=%d",
			boxed, anchoredPresent, answerCandidates)
	}
}

func selectRealBBoxProvider(t *testing.T, selector *llmrouter.Selector) (hexagon.Provider, string, string) {
	t.Helper()
	if selector == nil {
		t.Fatal("real bbox selector is nil")
	}
	providerName := strings.TrimSpace(os.Getenv("HEXCLAW_REAL_LLM_PROVIDER"))
	if providerName == "" {
		providerName = selector.DefaultName()
	}
	provider, ok := selector.Get(providerName)
	if !ok || provider == nil {
		t.Fatalf("real bbox provider %q is unavailable", providerName)
	}
	model := strings.TrimSpace(os.Getenv("HEXCLAW_REAL_LLM_MODEL"))
	if model == "" {
		model = selector.ProviderModel(providerName)
	}
	if model == "" {
		t.Fatalf("real bbox provider %q has no model", providerName)
	}
	return provider, providerName, model
}

func applyRealBBoxProviderOverride(t *testing.T, appCfg *config.Config) {
	t.Helper()
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("HEXCLAW_REAL_LLM_BASE_URL")), "/")
	if baseURL == "" {
		return
	}
	if !strings.HasSuffix(strings.ToLower(baseURL), "/v1") {
		baseURL += "/v1"
	}
	providerName := strings.TrimSpace(os.Getenv("HEXCLAW_REAL_LLM_PROVIDER"))
	if providerName == "" {
		providerName = "openai"
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		t.Fatalf("HEXCLAW_REAL_LLM_BASE_URL requires OPENAI_API_KEY for provider %q", providerName)
	}
	model := strings.TrimSpace(os.Getenv("HEXCLAW_REAL_LLM_MODEL"))
	if model == "" {
		model = "gpt-5.6-sol"
	}
	if appCfg.LLM.Providers == nil {
		appCfg.LLM.Providers = make(map[string]config.LLMProviderConfig)
	}
	for name, providerCfg := range appCfg.LLM.Providers {
		if !strings.EqualFold(strings.TrimSpace(name), providerName) {
			disabled := false
			providerCfg.Enabled = &disabled
			appCfg.LLM.Providers[name] = providerCfg
		}
	}
	enabled := true
	appCfg.LLM.Providers[providerName] = config.LLMProviderConfig{
		APIKey: apiKey, BaseURL: baseURL, Model: model, Compatible: "openai", Enabled: &enabled,
	}
	appCfg.LLM.Default = providerName
	appCfg.LLM.ReasoningProvider = providerName
	appCfg.LLM.ReasoningModel = model
}
