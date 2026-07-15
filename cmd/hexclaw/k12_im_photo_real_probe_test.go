package main

// 真实模型、本地图片、零钉钉发送的 K12 直连探针。
//
// 它刻意从 maybeHandleK12DingtalkPhoto 进入，与生产 messageHandler 的 K12 分支相同；
// 但不会构造/启动 DingtalkAdapter，因此绝不调用钉钉发送或媒体上传接口。运行示例：
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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
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
		t.Fatalf("read probe image: %v", err)
	}
	if mime := http.DetectContentType(raw); !strings.HasPrefix(strings.ToLower(mime), "image/") {
		t.Fatalf("probe input is not an image: mime=%q", mime)
	}

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("load local HexClaw config: %v", err)
	}
	cfg.Compaction.Enabled = false
	cfg.LLM.Tools.Enabled = "on"
	realRouter, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Fatalf("build real provider router: %v", err)
	}

	ctx := context.Background()
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "k12-photo-probe.db"))
	if err != nil {
		t.Fatalf("create isolated store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init isolated store: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT OR IGNORE INTO agents(name) VALUES('child-tutor')`); err != nil {
		t.Fatalf("seed probe agent: %v", err)
	}

	skills := skill.NewRegistry()
	sandboxCfg := sandbox.Config{Workspace: t.TempDir(), Timeout: 30}
	sb, err := sandbox.New(sandboxCfg)
	if err != nil {
		t.Fatalf("create code sandbox: %v", err)
	}
	if err := skills.Register(builtin.NewCodeExecSkill(sb, sandboxCfg)); err != nil {
		t.Fatalf("register code_exec: %v", err)
	}

	react := engine.NewReActEngine(cfg, realRouter, store, skills)
	if err := react.Start(ctx); err != nil {
		t.Fatalf("start real ReAct engine: %v", err)
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

	vision := func(visionCtx context.Context, image []byte, prompt string) (string, error) {
		provider, model, routeErr := react.RouteForVision(visionCtx)
		if routeErr != nil {
			return "", routeErr
		}
		mime := http.DetectContentType(image)
		if !strings.HasPrefix(strings.ToLower(mime), "image/") {
			mime = "image/jpeg"
		}
		dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(image)
		callCtx, cancel := context.WithTimeout(visionCtx, 150*time.Second)
		defer cancel()
		t.Logf("vision call: provider=%q model=%q bytes=%d", provider.Name(), model, len(image))
		resp, completeErr := provider.Complete(callCtx, llm.CompletionRequest{
			Messages: []llm.Message{{
				Role: llm.RoleUser,
				MultiContent: []llm.ContentPart{
					llm.NewTextPart(prompt),
					llm.NewImageURLPart(dataURL, "auto"),
				},
			}},
		})
		if completeErr != nil {
			return "", completeErr
		}
		return resp.Content, nil
	}

	runtime, err := assembly.Wire(
		store.DB(),
		classifiedSolveExecutor{next: solveSkill},
		assembly.WithRecognizer(k12engineadapter.NewRecognizerAdapter(vision)),
		assembly.WithPhotoAnnotator(k12engineadapter.NewPhotoAnnotator()),
	)
	if err != nil {
		t.Fatalf("wire K12 runtime: %v", err)
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

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	started := time.Now()
	reply, handled, err := maybeHandleK12DingtalkPhoto(runCtx, msg, k12PhotoTestRouter(t, true, "k12-tutor"), process)
	if err != nil {
		t.Fatalf("direct K12 photo route: %v", err)
	}
	if !handled || reply == nil {
		t.Fatalf("direct K12 photo route not handled: handled=%v reply=%#v", handled, reply)
	}

	statusCounts := map[k12usecase.PhotoItemStatus]int{}
	verifiedVerdicts := 0
	for i, item := range result.Items {
		statusCounts[item.Status]++
		if item.Status == k12usecase.PhotoCorrect || item.Status == k12usecase.PhotoWrong {
			verifiedVerdicts++
		}
		t.Logf("item[%d] status=%s bbox=%v verdict=%s evidence=%s answer=%q question=%q",
			i+1, item.Status, item.Recognized.BBox != nil, item.Grade.Evidence.Verdict,
			item.Grade.Evidence.EvidenceType, clipPhotoProbe(item.Recognized.StudentAnswer, 40),
			clipPhotoProbe(item.Recognized.Question, 80))
	}
	t.Logf("result: elapsed=%s mode=%s items=%d statuses=%v markdown_chars=%d annotated=%v attachments=%d",
		time.Since(started).Round(time.Millisecond), result.Mode, len(result.Items), statusCounts,
		len([]rune(reply.Content)), result.AnnotatedImage != nil, len(reply.Attachments))
	if markdownOutput := strings.TrimSpace(os.Getenv("HEXCLAW_K12_PHOTO_MARKDOWN_OUTPUT")); markdownOutput != "" {
		if err := writeK12PhotoProbeMarkdown(markdownOutput, reply.Content); err != nil {
			t.Fatalf("write probe Markdown: %v", err)
		}
		t.Logf("MARKDOWN_PRODUCED: chars=%d output=%s", len([]rune(reply.Content)), markdownOutput)
	}

	if result.AnnotatedImage == nil {
		if len(reply.Attachments) != 0 {
			t.Fatalf("reply has attachment without annotated image: %#v", reply.Attachments)
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
		t.Fatalf("write probe output: %v", err)
	}
	sum := sha256.Sum256(decoded)
	t.Logf("ATTACHMENT_PRODUCED: name=%q mime=%q bytes=%d sha256=%x output=%s",
		att.Name, att.Mime, len(decoded), sum[:8], output)
}

func clipPhotoProbe(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return fmt.Sprintf("%s…", string(r[:n]))
}

func writeK12PhotoProbeMarkdown(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	// WriteFile keeps the existing mode when the caller reuses an output path.
	// Force the probe artifact back to owner-only permissions on every run.
	return os.Chmod(path, 0o600)
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
