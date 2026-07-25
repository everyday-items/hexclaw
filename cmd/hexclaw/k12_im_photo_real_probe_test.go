package main

// 真实模型、本地图片、零钉钉发送的 K12 直连探针。
//
// 它覆盖真实批改 provider 与钉钉结果投影，但不会构造/启动 DingtalkAdapter，
// 因此绝不调用钉钉发送或媒体上传接口。统一 ImageTask 入口本身由隔离的确定性测试覆盖；
// 本探针只在显式授权真实模型时验证其下游能力。运行示例：
//
//   HEXCLAW_K12_PHOTO_PROBE=1 \
//   HEXCLAW_K12_PHOTO_IMAGE=/tmp/hexclaw-k12-photo-probe.jpg \
//   HEXCLAW_K12_PHOTO_OUTPUT=/tmp/hexclaw-k12-direct-reply.png \
//   HEXCLAW_K12_PHOTO_MARKDOWN_OUTPUT=/tmp/hexclaw-k12-direct-reply.md \
//   go test ./cmd/hexclaw -run TestK12DingtalkPhotoDirectRoute_RealModel_NoSend -v -count=1 -timeout 12m

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	k12engineadapter "github.com/hexagon-codes/hexclaw/scenarios/k12/engineadapter"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/builtin"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
	"github.com/hexagon-codes/toolkit/os/sandbox"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

const k12AnsweredFixtureSHA256 = "78cf3a1b5c52e12ca17ca13aa71c7a9439baed244e88b438aa2f1f70cd782fb5"

func TestK12DingtalkPhotoDirectRoute_RealModel_NoSend(t *testing.T) {
	if os.Getenv("HEXCLAW_K12_PHOTO_PROBE") != "1" {
		t.Skip("set HEXCLAW_K12_PHOTO_PROBE=1 to run the real-model/no-DingTalk-send probe")
	}
	imagePath := strings.TrimSpace(os.Getenv("HEXCLAW_K12_PHOTO_IMAGE"))
	if imagePath == "" {
		t.Fatal("HEXCLAW_K12_PHOTO_IMAGE is required")
	}
	raw, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatalf("read probe image: error_type=%T", err)
	}
	if mime := http.DetectContentType(raw); !strings.HasPrefix(strings.ToLower(mime), "image/") {
		t.Fatalf("probe input is not an image: mime=%q", mime)
	}

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("load local HexClaw config: error_type=%T", err)
	}
	applyK12PhotoProbeProviderOverride(t, cfg)
	cfg.Compaction.Enabled = false
	cfg.LLM.Tools.Enabled = "on"
	realRouter, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Fatalf("build real provider router: error_type=%T", err)
	}

	ctx := context.Background()
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "k12-photo-probe.db"))
	if err != nil {
		t.Fatalf("create isolated store: error_type=%T", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init isolated store: error_type=%T", err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT OR IGNORE INTO agents(name) VALUES('child-tutor')`); err != nil {
		t.Fatalf("seed probe agent: error_type=%T", err)
	}

	skills := skill.NewRegistry()
	sandboxCfg := sandbox.Config{Workspace: t.TempDir(), Timeout: 30}
	sb, err := sandbox.New(sandboxCfg)
	if err != nil {
		t.Fatalf("create code sandbox: error_type=%T", err)
	}
	if err := skills.Register(builtin.NewCodeExecSkill(sb, sandboxCfg)); err != nil {
		t.Fatalf("register code_exec: error_type=%T", err)
	}

	react := engine.NewReActEngine(cfg, realRouter, store, skills)
	if err := react.Start(ctx); err != nil {
		t.Fatalf("start real ReAct engine: error_type=%T", err)
	}
	t.Cleanup(func() { _ = react.Stop(context.Background()) })
	react.SetToolCollector(engine.NewToolCollector(skills, nil, 40))
	react.SetToolExecutor(engine.NewToolExecutor(skills, nil))

	subagents := engine.NewSubAgentRegistry(filepath.Join(t.TempDir(), "subagent-runs.json"))
	execSubagent := func(runCtx context.Context, spec engine.SubAgentSpec) (engine.SubAgentResult, error) {
		msg := &adapter.Message{
			ID:       "photo-probe-" + idgen.NanoID(),
			Platform: adapter.PlatformAPI,
			UserID:   "system",
			Content:  spec.Task,
			Metadata: map[string]string{},
		}
		engine.ApplySpecToMessage(msg, spec)
		reply, processErr := react.Process(runCtx, msg)
		if processErr != nil {
			return engine.SubAgentResult{}, processErr
		}
		return engine.SubAgentResult{Output: reply.Content, SessionID: msg.SessionID}, nil
	}
	solveSkill := engine.NewSolveSkill(execSubagent, subagents)

	var visionCalls atomic.Int32
	vision := func(visionCtx context.Context, image []byte, prompt string) (string, error) {
		call := visionCalls.Add(1)
		provider, model, routeErr := react.RouteForVision(visionCtx)
		if routeErr != nil {
			return "", routeErr
		}
		mime := http.DetectContentType(image)
		if !strings.HasPrefix(strings.ToLower(mime), "image/") {
			mime = "image/jpeg"
		}
		dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(image)
		t.Logf("vision call: call=%d provider=%q model=%q bytes=%d", call, provider.Name(), model, len(image))
		resp, completeErr := provider.Complete(visionCtx, hexagon.CompletionRequest{
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
			t.Logf("vision provider error_type=%T", completeErr)
			return "", fmt.Errorf("photo probe provider completion failed: %T", completeErr)
		}
		return resp.Content, nil
	}

	recognizer := k12engineadapter.NewRecognizerAdapter(vision)
	runtime, err := assembly.Wire(
		store.DB(),
		classifiedSolveExecutor{next: solveSkill},
		assembly.WithRecognizer(recognizer),
		assembly.WithAnswerAnchorer(recognizer),
		assembly.WithPhotoAnnotator(k12engineadapter.NewPhotoAnnotator()),
	)
	if err != nil {
		t.Fatalf("wire K12 runtime: error_type=%T", err)
	}

	var result k12usecase.PhotoGradeResult
	process := func(processCtx context.Context, req k12usecase.PhotoGradeRequest) (k12usecase.PhotoGradeResult, error) {
		var processErr error
		result, processErr = runtime.Deps.GradeHomeworkPhoto(processCtx, req)
		return result, processErr
	}
	msg := k12PhotoTestMessage()
	msg.SessionID = "real-photo-probe"
	msg.Attachments[0].Data = base64.StdEncoding.EncodeToString(raw)

	started := time.Now()
	result, err = process(t.Context(), k12usecase.PhotoGradeRequest{
		AgentName: "child-tutor", Grade: "五年级下",
		SourceSession: msg.SessionID, Image: raw,
	})
	reply := k12PhotoReply(result)
	handled := true
	if err != nil {
		t.Fatalf("direct K12 photo route failed: failure_kind=%s vision_calls=%d error_type=%T",
			classifyK12PhotoProbeFailure(err), visionCalls.Load(), err)
	}
	if !handled || reply == nil {
		t.Fatalf("direct K12 photo route not handled: handled=%v reply_present=%v", handled, reply != nil)
	}

	statusCounts := map[k12usecase.PhotoItemStatus]int{}
	verifiedVerdicts := 0
	for i, item := range result.Items {
		statusCounts[item.Status]++
		if item.Status == k12usecase.PhotoCorrect || item.Status == k12usecase.PhotoWrong {
			verifiedVerdicts++
		}
		t.Logf("item[%d] status=%s bbox=%t knowledge_points=%d out_of_scope=%t verdict=%s evidence=%s answer_chars=%d question_chars=%d warning_chars=%d",
			i+1, item.Status, item.Recognized.BBox != nil, len(item.Recognized.KnowledgePoints),
			strings.TrimSpace(item.Grade.OutOfScopeKP) != "", item.Grade.Evidence.Verdict,
			item.Grade.Evidence.EvidenceType, len([]rune(item.Recognized.StudentAnswer)),
			len([]rune(item.Recognized.Question)), len([]rune(item.Warning)))
	}
	t.Logf("result: elapsed=%s mode=%s items=%d statuses=%v markdown_chars=%d annotated=%v attachments=%d",
		time.Since(started).Round(time.Millisecond), result.Mode, len(result.Items), statusCounts,
		len([]rune(reply.Content)), result.AnnotatedImage != nil, len(reply.Attachments))
	inputSum := sha256.Sum256(raw)
	if fmt.Sprintf("%x", inputSum) == k12AnsweredFixtureSHA256 {
		assertKnownAnsweredWorksheetSemantics(t, result)
	}
	if markdownOutput := strings.TrimSpace(os.Getenv("HEXCLAW_K12_PHOTO_MARKDOWN_OUTPUT")); markdownOutput != "" {
		if err := writeK12PhotoProbeMarkdown(markdownOutput, reply.Content); err != nil {
			t.Fatalf("write probe Markdown: error_type=%T", err)
		}
		t.Logf("MARKDOWN_PRODUCED: chars=%d", len([]rune(reply.Content)))
	}

	if result.AnnotatedImage == nil {
		if len(reply.Attachments) != 0 {
			t.Fatalf("reply has %d attachment(s) without annotated image", len(reply.Attachments))
		}
		if result.Mode == k12usecase.PhotoModeGrade && verifiedVerdicts > 0 {
			t.Fatalf("answered sheet has %d verified verdicts but no correction-image attachment", verifiedVerdicts)
		}
		t.Log("NO_ATTACHMENT: solve-only or no verified verdict; no fake correction image was produced")
		return
	}
	if len(reply.Attachments) != 1 {
		t.Fatalf("annotated image was produced but reply attachments=%d", len(reply.Attachments))
	}
	att := reply.Attachments[0]
	decoded, err := base64.StdEncoding.DecodeString(att.Data)
	if err != nil {
		t.Fatalf("decode reply attachment: %v", err)
	}
	if string(decoded) != string(result.AnnotatedImage.Data) {
		t.Fatal("reply attachment bytes differ from PhotoAnnotator output")
	}
	output := strings.TrimSpace(os.Getenv("HEXCLAW_K12_PHOTO_OUTPUT"))
	if output == "" {
		ext := ".png"
		if strings.EqualFold(att.Mime, "image/jpeg") {
			ext = ".jpg"
		}
		output = filepath.Join(os.TempDir(), "hexclaw-k12-direct-reply"+ext)
	}
	if err := os.WriteFile(output, decoded, 0o600); err != nil {
		t.Fatalf("write probe output: error_type=%T", err)
	}
	sum := sha256.Sum256(decoded)
	t.Logf("ATTACHMENT_PRODUCED: mime=%q bytes=%d sha256=%x", att.Mime, len(decoded), sum[:8])
}

func assertKnownAnsweredWorksheetSemantics(t *testing.T, result k12usecase.PhotoGradeResult) {
	t.Helper()
	expected := []struct {
		question string
		status   k12usecase.PhotoItemStatus
	}{
		{"4÷0.5=", k12usecase.PhotoCorrect},
		{"10×0.01=", k12usecase.PhotoCorrect},
		{"4.7+2.3=", k12usecase.PhotoCorrect},
		{"1.8×50=", k12usecase.PhotoCorrect},
		{"3.25+0.75=", k12usecase.PhotoCorrect},
		{"5/7−1/5=", k12usecase.PhotoCorrect},
		{"7−5/7=", k12usecase.PhotoCorrect},
		{"0.5+1/3=", k12usecase.PhotoWrong},
		{"4/5+2/5=", k12usecase.PhotoCorrect},
		{"8.7×17.4−8.7×7.4", k12usecase.PhotoWrong},
		{"15.02−6.8−1.02", k12usecase.PhotoCorrect},
		{"0.25+11/15+4/15+3/4", k12usecase.PhotoCorrect},
		{"1、一个数的3/8是24，求这个数？", k12usecase.PhotoCorrect},
		{"2、8的1/4的4/5是多少？", k12usecase.PhotoCorrect},
		{"一个周长是300米的长方形鱼塘，长是宽的2倍。如果每平方米产鱼2.25千克，一共产鱼多少千克？", k12usecase.PhotoWrong},
		{"在下列六个数：5、6、12、14、23、29中划去数（ ）后，能使其中3个数的和为另外2个数和的2倍。", k12usecase.PhotoUnanswered},
	}
	if result.Mode != k12usecase.PhotoModeGrade || len(result.Items) != len(expected) {
		t.Fatalf("known answered worksheet shape drift: mode=%s items=%d want=%d",
			result.Mode, len(result.Items), len(expected))
	}
	itemsByQuestion := make(map[string]k12usecase.PhotoGradeItem, len(result.Items))
	for itemIndex, item := range result.Items {
		key := canonicalK12PhotoProbeText(item.Recognized.Question)
		if _, duplicate := itemsByQuestion[key]; duplicate {
			t.Fatalf("known answered worksheet has duplicate question at item=%d", itemIndex+1)
		}
		itemsByQuestion[key] = item
	}
	statusCounts := map[k12usecase.PhotoItemStatus]int{}
	for expectedIndex, want := range expected {
		key := canonicalK12PhotoProbeText(want.question)
		item, ok := itemsByQuestion[key]
		if !ok {
			t.Fatalf("known answered worksheet lost/corrupted question index=%d: items=%d",
				expectedIndex+1, len(result.Items))
		}
		if item.Status != want.status {
			t.Fatalf("question index=%d status=%s want=%s answer_chars=%d warning_chars=%d",
				expectedIndex+1, item.Status, want.status, len([]rune(item.Recognized.StudentAnswer)),
				len([]rune(item.Warning)))
		}
		if want.status != k12usecase.PhotoUnanswered && item.Recognized.BBox == nil {
			t.Fatalf("answered question index=%d has no verified image anchor", expectedIndex+1)
		}
		statusCounts[item.Status]++
	}
	if statusCounts[k12usecase.PhotoCorrect] != 12 ||
		statusCounts[k12usecase.PhotoWrong] != 3 ||
		statusCounts[k12usecase.PhotoUnanswered] != 1 {
		t.Fatalf("known answered worksheet status totals=%v want correct=12 wrong=3 unanswered=1", statusCounts)
	}
	firstFraction := itemsByQuestion[canonicalK12PhotoProbeText("5/7−1/5=")].Recognized.StudentAnswer
	if canonicalK12PhotoProbeText(firstFraction) != canonicalK12PhotoProbeText("18/35") {
		t.Fatalf("first fraction transcription mismatch: answer_chars=%d", len([]rune(firstFraction)))
	}
	decimalAnswer := itemsByQuestion[canonicalK12PhotoProbeText("10×0.01=")].Recognized.StudentAnswer
	if canonicalK12PhotoProbeText(decimalAnswer) != canonicalK12PhotoProbeText("0.1") {
		t.Fatalf("writer-specific 0/6 transcription mismatch: answer_chars=%d", len([]rune(decimalAnswer)))
	}
	divisionAnswer := canonicalK12PhotoProbeText(
		itemsByQuestion[canonicalK12PhotoProbeText("一个数的3/8是24，求这个数？")].Recognized.StudentAnswer,
	)
	if (divisionAnswer != canonicalK12PhotoProbeText("64") &&
		!strings.Contains(divisionAnswer, canonicalK12PhotoProbeText("24÷3×8=64"))) ||
		strings.Contains(divisionAnswer, canonicalK12PhotoProbeText("3/8×8")) {
		t.Fatalf("printed fraction leaked into isolated handwriting transcription: answer_chars=%d",
			len([]rune(divisionAnswer)))
	}
	if result.AnnotatedImage == nil {
		t.Fatal("known answered worksheet has verified verdicts but no annotated image")
	}
}

func canonicalK12PhotoProbeText(value string) string {
	value = strings.TrimSpace(adapter.NormalizeMathText(value))
	value = strings.Map(func(char rune) rune {
		if unicode.IsSpace(char) {
			return -1
		}
		return char
	}, value)
	runes := []rune(value)
	index := 0
	for index < len(runes) && runes[index] >= '0' && runes[index] <= '9' {
		index++
	}
	if index > 0 && index < len(runes) && strings.ContainsRune("、.．", runes[index]) {
		value = string(runes[index+1:])
	}
	value = strings.NewReplacer(
		"＋", "+", "－", "-", "−", "-", "×", "*", "÷", "/",
		"＝", "=", "？", "", "?", "", "，", ",", "；", ";",
	).Replace(value)
	return strings.TrimSuffix(strings.ToLower(value), "=")
}

func applyK12PhotoProbeProviderOverride(t *testing.T, cfg *config.Config) {
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
		model = "gpt-5.6"
	}
	if cfg.LLM.Providers == nil {
		cfg.LLM.Providers = make(map[string]config.LLMProviderConfig)
	}
	for name, providerCfg := range cfg.LLM.Providers {
		if !strings.EqualFold(strings.TrimSpace(name), providerName) {
			disabled := false
			providerCfg.Enabled = &disabled
			cfg.LLM.Providers[name] = providerCfg
		}
	}
	enabled := true
	cfg.LLM.Providers[providerName] = config.LLMProviderConfig{
		APIKey: apiKey, BaseURL: baseURL, Model: model, Compatible: "openai", Enabled: &enabled,
	}
	cfg.LLM.Default = providerName
	cfg.LLM.ReasoningProvider = providerName
	cfg.LLM.ReasoningModel = model
}

func writeK12PhotoProbeMarkdown(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	// WriteFile keeps the existing mode when the caller reuses an output path.
	// Force the probe artifact back to owner-only permissions on every run.
	return os.Chmod(path, 0o600)
}

// classifyK12PhotoProbeFailure converts the nested production error into a
// fixed, non-sensitive diagnostic vocabulary. It deliberately never returns
// err.Error(): provider bodies, OCR text and child content must not enter CI
// logs merely to make this opt-in probe debuggable.
func classifyK12PhotoProbeFailure(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "未识别到可处理的题目") ||
		strings.Contains(message, "未识别到可作答的独立题目"):
		return "recognition_empty"
	case strings.Contains(message, "解析识题结果失败") ||
		strings.Contains(message, "解析印刷题清单失败"):
		return "recognition_protocol"
	case strings.Contains(message, "duplicate problem_id") ||
		strings.Contains(message, "recognition_confidence") ||
		strings.Contains(message, "unsupported problem_kind") ||
		strings.Contains(message, "cannot have parent/subproblem_no") ||
		strings.Contains(message, "cannot own answer/parent/subproblem_no") ||
		strings.Contains(message, "needs an existing compound parent") ||
		strings.Contains(message, "duplicate subproblem_no") ||
		strings.Contains(message, "needs attempt_id") ||
		strings.Contains(message, "duplicate attempt_id") ||
		strings.Contains(message, "must share page_asset_id"):
		return "recognition_identity_invalid"
	case strings.Contains(message, "视觉模型调用失败") ||
		strings.Contains(message, "photo probe provider completion failed"):
		return "vision_provider"
	case strings.Contains(message, "作答坐标") || strings.Contains(message, "anchor"):
		return "answer_anchor"
	default:
		return "unknown"
	}
}

func TestWriteK12PhotoProbeMarkdown_OwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.md")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	const markdown = "## 作业解题\n\n**答案：** 9"
	if err := writeK12PhotoProbeMarkdown(path, markdown); err != nil {
		t.Fatalf("writeK12PhotoProbeMarkdown: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != markdown {
		t.Fatalf("Markdown content = %q, want %q", got, markdown)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o600 {
		t.Fatalf("Markdown mode = %o, want 600", gotMode)
	}
}

func TestClassifyK12PhotoProbeFailure_SanitizesKnownCauses(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", err: nil, want: "none"},
		{name: "deadline", err: fmt.Errorf("wrapped: %w", context.DeadlineExceeded), want: "deadline_exceeded"},
		{name: "canceled", err: fmt.Errorf("wrapped: %w", context.Canceled), want: "canceled"},
		{name: "empty recognition", err: fmt.Errorf("usecase: 未识别到可处理的题目"), want: "recognition_empty"},
		{name: "recognition protocol", err: fmt.Errorf("recognizer: 解析识题结果失败"), want: "recognition_protocol"},
		{name: "duplicate identity", err: fmt.Errorf("invalid input: duplicate problem_id"), want: "recognition_identity_invalid"},
		{name: "standalone identity", err: fmt.Errorf("standalone problem cannot have parent/subproblem_no"), want: "recognition_identity_invalid"},
		{name: "vision provider", err: fmt.Errorf("recognizer: 视觉模型调用失败"), want: "vision_provider"},
		{name: "anchor", err: fmt.Errorf("usecase: 作答坐标核验失败"), want: "answer_anchor"},
		{name: "unknown does not echo detail", err: fmt.Errorf("private upstream detail must stay hidden"), want: "unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyK12PhotoProbeFailure(test.err); got != test.want {
				t.Fatalf("classifyK12PhotoProbeFailure()=%q want=%q", got, test.want)
			}
		})
	}
}
