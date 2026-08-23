package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
	fixtureID := strings.TrimSpace(os.Getenv("HEXCLAW_K12_PHOTO_FIXTURE_ID"))
	fixture, err := k12SelfInventoryFixtureFor(fixtureID, raw)
	if err != nil {
		t.Fatal(err)
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
	providerName := strings.TrimSpace(os.Getenv("HEXCLAW_REAL_LLM_PROVIDER"))
	modelName := strings.TrimSpace(os.Getenv("HEXCLAW_REAL_LLM_MODEL"))
	if providerName == "" || modelName == "" {
		t.Fatal("HEXCLAW_REAL_LLM_PROVIDER and HEXCLAW_REAL_LLM_MODEL are required")
	}
	snapshot, err := k12SelfInventorySnapshot(router, providerName, modelName)
	if err != nil {
		t.Fatalf("freeze configured recognizing route: error_type=%T", err)
	}
	provider, found := router.Get(snapshot.Provider)
	if !found || provider == nil {
		t.Fatal("frozen recognizing provider is unavailable")
	}

	var calls atomic.Int32
	recognizer := k12engineadapter.NewRecognizerAdapter(func(
		ctx context.Context,
		image []byte,
		prompt string,
	) (string, error) {
		callOrdinal := calls.Add(1)
		// 单次 whole-page 诊断只允许首个 Provider 调用，后续回退在测试回调内停止。
		if callOrdinal > 1 && os.Getenv("HEXCLAW_REAL_K12_SELF_INVENTORY_WHOLE_PAGE_ONLY") == "1" {
			return "", errors.New("whole-page-only diagnostic stopped bounded recovery before provider call")
		}
		// 这里仍然只是协议诊断，而非持久化的阶段回执。每次物理调用仍使用准确冻结的
		// 生产请求策略和单次调用上限，外层预算仅允许已授权的协议级串行回退完成诊断。
		ctx, cancel := context.WithTimeout(ctx, time.Duration(snapshot.TimeoutMS)*time.Millisecond)
		defer cancel()
		ctx = k12.WithGradingModelSnapshot(ctx, snapshot)
		ctx = k12.WithGradingModelRequestPolicy(ctx, snapshot.RecognizingRequestPolicy)
		callStarted := time.Now()
		response, completeErr := completeK12VisionRequest(ctx, provider, snapshot.Model, image, prompt)
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
		logK12ProviderResponseSafeDiagnostic(t, callOrdinal, response, fixture.diagnosticQuestion)
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
	fixture.assertQuestions(t, questions)
	t.Logf(
		"REAL_K12_SELF_INVENTORY_PASS fixture=%s route=%s/%s physical_calls=%d questions=%d no_dingtalk_send=true",
		fixtureID,
		snapshot.Provider,
		snapshot.Model,
		calls.Load(),
		len(questions),
	)
}

type k12SelfInventoryFixture struct {
	sha256             string
	diagnosticQuestion string
	assertQuestions    func(*testing.T, []k12usecase.RecognizedQuestion)
}

var k12SelfInventoryFixtures = map[string]k12SelfInventoryFixture{
	"clear": {
		sha256:             "0c4b1a972319203b1483ffbce43e8835b1367be53edceea23c89368a2f2bc861",
		diagnosticQuestion: "5/7−1/5=",
		assertQuestions:    assertKnownAnsweredWorksheetRecognitionSet,
	},
	"messy": {
		sha256:             "78cf3a1b5c52e12ca17ca13aa71c7a9439baed244e88b438aa2f1f70cd782fb5",
		diagnosticQuestion: "5/7−1/5=",
		assertQuestions:    assertKnownAnsweredWorksheetRecognitionSet,
	},
	"blank": {
		sha256:             "76c3bbab79486619d680114b8c182c0e23d15ce305239dc762819a5f0407eed7",
		diagnosticQuestion: "4.5×2=",
		assertQuestions:    assertKnownBlankWorksheetRecognitionSet,
	},
}

func k12SelfInventoryFixtureFor(
	fixtureID string,
	raw []byte,
) (k12SelfInventoryFixture, error) {
	fixture, ok := k12SelfInventoryFixtures[strings.TrimSpace(fixtureID)]
	if !ok {
		return k12SelfInventoryFixture{}, fmt.Errorf(
			"HEXCLAW_K12_PHOTO_FIXTURE_ID must select a frozen K12 fixture",
		)
	}
	sum := sha256.Sum256(raw)
	if fmt.Sprintf("%x", sum) != fixture.sha256 {
		return k12SelfInventoryFixture{}, fmt.Errorf(
			"real self-inventory probe fixture digest does not match its frozen manifest",
		)
	}
	return fixture, nil
}

// k12SelfInventorySnapshot 固定使用调用者显式指定的 provider/model，避免 Provider
// 模型数组顺序影响真实探针。
func k12SelfInventorySnapshot(
	router *llmrouter.Selector,
	providerName, modelName string,
) (k12.GradingModelSnapshot, error) {
	return resolveK12GradingModelSnapshot(router, k12.GradingModelSnapshot{
		Provider: strings.TrimSpace(providerName),
		Model:    strings.TrimSpace(modelName),
	})
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
	fingerprints := make([]string, 0, 32)
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
		keySum := sha256.Sum256([]byte(key))
		fingerprints = append(fingerprints, fmt.Sprintf("%x:%d", keySum[:6], len([]rune(key))))
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
	if questionCount%2 == 0 && questionCount > 0 {
		half := questionCount / 2
		mismatches := make([]int, 0, half)
		for index := 0; index < half; index++ {
			if fingerprints[index] != fingerprints[index+half] {
				mismatches = append(mismatches, index+1)
			}
		}
		t.Logf(
			"safe whole-page pair diagnostic: call=%d items_per_array=%d mismatch_indexes=%v first_array=%v second_array=%v",
			callOrdinal, half, mismatches, fingerprints[:half], fingerprints[half:],
		)
	}
	logK12WholePageIdentitySafeDiagnostic(t, callOrdinal, response)
}

func logK12WholePageIdentitySafeDiagnostic(t *testing.T, callOrdinal int32, response string) {
	t.Helper()
	start, end := strings.Index(response, "{"), strings.LastIndex(response, "}")
	if start < 0 || end <= start {
		return
	}
	type identity struct {
		SourceNumberPath []string `json:"source_number_path"`
		DisplayLabel     string   `json:"display_label"`
		Question         string   `json:"question"`
	}
	var envelope struct {
		PrintedInventory []identity `json:"printed_inventory"`
		Questions        []identity `json:"questions"`
	}
	if json.Unmarshal([]byte(response[start:end+1]), &envelope) != nil ||
		len(envelope.PrintedInventory) != len(envelope.Questions) {
		return
	}
	mismatches := make([]int, 0, len(envelope.Questions))
	identityPairs := make([]string, 0, len(envelope.Questions))
	for index := range envelope.Questions {
		questionJSON, _ := json.Marshal(envelope.Questions[index])
		inventoryJSON, _ := json.Marshal(envelope.PrintedInventory[index])
		questionSum := sha256.Sum256(questionJSON)
		inventorySum := sha256.Sum256(inventoryJSON)
		if questionSum != inventorySum {
			mismatches = append(mismatches, index+1)
		}
		identityPairs = append(
			identityPairs,
			fmt.Sprintf("%x/%x", questionSum[:6], inventorySum[:6]),
		)
	}
	t.Logf(
		"safe whole-page identity diagnostic: call=%d mismatch_indexes=%v identity_pairs=%v",
		callOrdinal, mismatches, identityPairs,
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

func assertKnownBlankWorksheetRecognitionSet(
	t *testing.T,
	questions []k12usecase.RecognizedQuestion,
) {
	t.Helper()
	// 两道应用题可按 compound parent/independent child 形态输出；父题干与两个独立
	// 子问同时保留时每题占三项，因此不把领域拆分方式误写成固定总数。
	if len(questions) < 24 || len(questions) > 28 {
		t.Fatalf("real blank self-inventory question count=%d want range=[24,28]", len(questions))
	}
	got := make(map[string]struct{}, len(questions))
	joined := ""
	for index, question := range questions {
		key := canonicalK12PhotoProbeText(question.Question)
		if _, duplicate := got[key]; duplicate {
			t.Fatalf("real blank self-inventory duplicate question index=%d", index+1)
		}
		for _, metadata := range []string{"日期", "姓名", "时间", "暑假作业", "day1"} {
			if strings.Contains(key, canonicalK12PhotoProbeText(metadata)) {
				t.Fatalf("real blank self-inventory item=%d contains worksheet metadata", index+1)
			}
		}
		got[key] = struct{}{}
		joined += key
		// 原图第 6 题的作答区存在可见灰色涂写；只有该项允许模型判为 present/unclear。
		if index == 5 {
			continue
		}
		if question.AnswerState != k12usecase.AnswerStateBlank || strings.TrimSpace(question.StudentAnswer) != "" {
			t.Fatalf("real blank self-inventory item=%d contains a student answer", index+1)
		}
	}
	for _, cue := range []string{
		"4.5×2", "194−64.8÷1.8×0.9", "6x+15×7=141",
		"棱长是6dm", "至少需要玻璃多少平方米", "144l", "水面高度是多少分米",
		"最大公约数是13", "最小公倍数是72", "（）排", "（）号",
	} {
		if !strings.Contains(joined, canonicalK12PhotoProbeText(cue)) {
			t.Fatalf("real blank self-inventory lost application-problem cue")
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
