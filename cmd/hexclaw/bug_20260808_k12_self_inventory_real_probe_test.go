package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/transport"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12engineadapter "github.com/hexagon-codes/hexclaw/scenarios/k12/engineadapter"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// TestBUG20260808K12WholePageSelfInventory_RealHexClawGPT 只验证该缺陷所改动的识题边界。
// 它有意在解题和批改前停止，并且绝不初始化钉钉适配器，避免真实 VLM 回执被无关的
// 逐题执行时间掩盖。
func TestBUG20260808K12WholePageSelfInventory_RealHexClawGPT(t *testing.T) {
	if os.Getenv("HEXCLAW_REAL_K12_SELF_INVENTORY") != "1" {
		t.Skip("set HEXCLAW_REAL_K12_SELF_INVENTORY=1 to run the real K12 recognition probe")
	}
	imagePath := strings.TrimSpace(os.Getenv("HEXCLAW_K12_PHOTO_IMAGE"))
	if imagePath == "" {
		t.Fatal("HEXCLAW_K12_PHOTO_IMAGE is required")
	}
	raw, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatalf("read probe image: error_type=%T", err)
	}
	if sum := sha256.Sum256(raw); fmt.Sprintf("%x", sum) != k12AnsweredFixtureSHA256 {
		t.Fatal("real self-inventory probe requires the frozen public answered worksheet fixture")
	}
	mime := http.DetectContentType(raw)
	if !strings.HasPrefix(strings.ToLower(mime), "image/") {
		t.Fatalf("probe input is not an image: mime=%q", mime)
	}

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("load local HexClaw config: error_type=%T", err)
	}
	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Fatalf("build configured provider router: error_type=%T", err)
	}
	route, err := router.DefaultRouteForCapabilities(
		config.LLMModelCapabilityText,
		config.LLMModelCapabilityVision,
	)
	if err != nil {
		t.Fatalf("resolve configured vision route: error_type=%T", err)
	}
	if route.ProviderName != "hexclaw-gpt" || route.Model != "gpt-5.6-sol" {
		t.Fatalf("real self-inventory probe route=%q/%q, want hexclaw-gpt/gpt-5.6-sol", route.ProviderName, route.Model)
	}
	snapshot, err := resolveK12GradingModelSnapshot(router, k12.GradingModelSnapshot{
		Provider: route.ProviderName,
		Model:    route.Model,
	})
	if err != nil {
		t.Fatalf("freeze configured recognizing route: error_type=%T", err)
	}

	var calls atomic.Int32
	recognizer := k12engineadapter.NewRecognizerAdapter(func(
		ctx context.Context,
		image []byte,
		prompt string,
	) (string, error) {
		callOrdinal := calls.Add(1)
		// 这里仍然只是协议诊断，而非持久化的阶段回执。每次物理调用仍使用准确冻结的
		// 生产请求策略和单次调用上限，外层预算则允许获准的串行回退完成语义诊断。
		ctx, cancel := context.WithTimeout(ctx, time.Duration(snapshot.TimeoutMS)*time.Millisecond)
		defer cancel()
		ctx = k12.WithGradingModelSnapshot(ctx, snapshot)
		ctx = k12.WithGradingModelRequestPolicy(ctx, snapshot.RecognizingRequestPolicy)
		callStarted := time.Now()
		response, completeErr := completeK12VisionRequest(ctx, route.Provider, route.Model, image, prompt)
		if completeErr != nil {
			var providerErr *transport.ProviderError
			if errors.As(completeErr, &providerErr) {
				t.Logf("real recognition provider failure: status_code=%d action=%q elapsed=%s retry_after=%s request_id_present=%t",
					providerErr.StatusCode, providerErr.Action, providerErr.Elapsed.Round(time.Millisecond),
					providerErr.RetryAfter.Round(time.Millisecond), providerErr.RequestID != "")
			}
			return "", fmt.Errorf("real recognition provider completion failed: %T", completeErr)
		}
		t.Logf("real recognition provider success: elapsed=%s response_chars=%d",
			time.Since(callStarted).Round(time.Millisecond), len([]rune(response)))
		logK12ProviderResponseSafeDiagnostic(t, callOrdinal, response, "5/7−1/5=")
		return response, nil
	})

	ctx, cancel := context.WithTimeout(t.Context(), 9*time.Minute)
	defer cancel()
	questions, err := recognizer.Recognize(ctx, raw)
	if err != nil {
		t.Logf(
			"safe recognition failure diagnostic: protocol_invalid=%t category=%s",
			errors.Is(err, k12.ErrRecognitionProtocolInvalid),
			k12RecognitionFailureCategory(err),
		)
		t.Fatalf("real self-inventory recognition failed: error_type=%T calls=%d", err, calls.Load())
	}
	if got := calls.Load(); got != 1 && got != 7 {
		t.Fatalf("real self-inventory physical recognition calls=%d, want exactly 1 or 7", got)
	}
	assertKnownAnsweredWorksheetRecognitionSet(t, questions)
	t.Logf(
		"REAL_K12_SELF_INVENTORY_PASS route=%s/%s physical_calls=%d questions=%d no_dingtalk_send=true",
		route.ProviderName,
		route.Model,
		calls.Load(),
		len(questions),
	)
}

var k12SafeQuestionField = regexp.MustCompile(`"question"\s*:\s*("(?:\\.|[^"\\])*")`)

func logK12ProviderResponseSafeDiagnostic(
	t *testing.T,
	callOrdinal int32,
	response,
	want string,
) {
	t.Helper()
	wantKey := canonicalK12PhotoProbeText(want)
	bestDistance := -1
	bestKey := ""
	questionCount := 0
	for _, match := range k12SafeQuestionField.FindAllStringSubmatch(response, -1) {
		if len(match) != 2 {
			continue
		}
		question, err := strconv.Unquote(match[1])
		if err != nil {
			continue
		}
		questionCount++
		key := canonicalK12PhotoProbeText(question)
		distance := k12SelfInventoryEditDistance([]rune(wantKey), []rune(key))
		if bestDistance < 0 || distance < bestDistance {
			bestDistance = distance
			bestKey = key
		}
	}
	sum := sha256.Sum256([]byte(bestKey))
	t.Logf(
		"safe provider response diagnostic: call=%d question_fields=%d best_chars=%d best_key_sha256_prefix=%x best_edit_distance=%d slash_count=%d equals_count=%d minus_count=%d",
		callOrdinal,
		questionCount,
		len([]rune(bestKey)),
		sum[:6],
		bestDistance,
		strings.Count(bestKey, "/"),
		strings.Count(bestKey, "="),
		strings.Count(bestKey, "-"),
	)
}

func k12RecognitionFailureCategory(err error) string {
	message := err.Error()
	switch {
	case strings.Contains(message, "重复原卷题号"):
		return "duplicate_source_number"
	case strings.Contains(message, "识题结果存在重复题目"):
		return "duplicate_question"
	case strings.Contains(message, "来源字段"):
		return "source_fact_conflict"
	case strings.Contains(message, "source_number_path/display_label"):
		return "source_number_pair"
	case strings.Contains(message, "source_number_path 含空 token"):
		return "source_number_empty_token"
	case strings.Contains(message, "source_section_path/source_section_label"):
		return "source_section_pair"
	case strings.Contains(message, "source_section_path 含空 token"):
		return "source_section_empty_token"
	case strings.Contains(message, "source_section_label conflicts"):
		return "source_section_conflict"
	case strings.Contains(message, "dangling parent_problem_id"):
		return "dangling_compound_parent"
	case strings.Contains(message, "ambiguous problem_id"):
		return "ambiguous_compound_parent"
	case strings.Contains(message, "duplicate subproblem_no"):
		return "duplicate_subproblem_number"
	case strings.Contains(message, "unsupported problem_kind"):
		return "unsupported_problem_kind"
	case strings.Contains(message, "compound parent"):
		return "invalid_compound_parent"
	case strings.Contains(message, "bbox"):
		return "invalid_bbox"
	case strings.Contains(message, "recognition_confidence"):
		return "invalid_confidence"
	case strings.Contains(message, "分片"):
		return "segment_protocol"
	case errors.Is(err, k12.ErrRecognitionProtocolInvalid):
		return "other_protocol"
	default:
		return "non_protocol"
	}
}

func assertKnownAnsweredWorksheetRecognitionSet(t *testing.T, questions []k12usecase.RecognizedQuestion) {
	t.Helper()
	want := []string{
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
	if len(questions) != len(want) {
		t.Fatalf("real self-inventory question count=%d want=%d", len(questions), len(want))
	}
	got := make(map[string]struct{}, len(questions))
	for index, question := range questions {
		key := canonicalK12PhotoProbeText(question.Question)
		if _, duplicate := got[key]; duplicate {
			t.Fatalf("real self-inventory duplicate question index=%d", index+1)
		}
		got[key] = struct{}{}
	}
	for index, question := range want {
		if _, present := got[canonicalK12PhotoProbeText(question)]; !present {
			logK12SelfInventorySafeDiagnostics(t, questions, question)
			t.Fatalf("real self-inventory lost/corrupted question index=%d", index+1)
		}
	}
}

func logK12SelfInventorySafeDiagnostics(
	t *testing.T,
	questions []k12usecase.RecognizedQuestion,
	want string,
) {
	t.Helper()
	wantKey := canonicalK12PhotoProbeText(want)
	for index, question := range questions {
		key := canonicalK12PhotoProbeText(question.Question)
		sum := sha256.Sum256([]byte(key))
		t.Logf(
			"safe question diagnostic: ordinal=%d chars=%d key_sha256_prefix=%x edit_distance=%d slash_count=%d equals_count=%d minus_count=%d answer_state=%s",
			index+1,
			len([]rune(key)),
			sum[:6],
			k12SelfInventoryEditDistance([]rune(wantKey), []rune(key)),
			strings.Count(key, "/"),
			strings.Count(key, "="),
			strings.Count(key, "-"),
			question.AnswerState,
		)
	}
}

func k12SelfInventoryEditDistance(left, right []rune) int {
	previous := make([]int, len(right)+1)
	current := make([]int, len(right)+1)
	for index := range previous {
		previous[index] = index
	}
	for leftIndex, leftRune := range left {
		current[0] = leftIndex + 1
		for rightIndex, rightRune := range right {
			cost := 0
			if leftRune != rightRune {
				cost = 1
			}
			best := current[rightIndex] + 1
			if candidate := previous[rightIndex+1] + 1; candidate < best {
				best = candidate
			}
			if candidate := previous[rightIndex] + cost; candidate < best {
				best = candidate
			}
			current[rightIndex+1] = best
		}
		previous, current = current, previous
	}
	return previous[len(right)]
}
