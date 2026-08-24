package dingtalk

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	agentengine "github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

func TestDingTalkImageURLCannotBypassMediaUpload(t *testing.T) {
	client := newTestAdapter()
	client.queue = nil
	fake := newFakeConversationOpenAPI()
	client.openAPI = fake

	err := client.Send(context.Background(), "parent", &adapter.Reply{
		Content: "## 作品点评",
		Attachments: []adapter.Attachment{{
			Type: "image",
			Name: "creative-work.png",
			Mime: "image/png",
			URL:  "https://internal.invalid/creative-work.png",
			Data: base64.StdEncoding.EncodeToString([]byte("image-bytes")),
		}},
	})

	if err != nil {
		t.Fatalf("发送带图片字节的 Markdown 回复: %v", err)
	}
	fake.mu.Lock()
	uploads := append([]adapter.Attachment(nil), fake.uploads...)
	fake.mu.Unlock()
	// URL 只是内部附件元数据，不能冒充钉钉媒体引用并绕过真实上传。
	if len(uploads) != 1 {
		t.Fatalf("DingTalk UploadImage 调用次数=%d，期望 1", len(uploads))
	}
	calls := fake.SendCalls()
	if len(calls) != 1 {
		t.Fatalf("SendOTO 调用次数=%d，期望 1", len(calls))
	}
	if strings.Contains(calls[0].Text, "https://internal.invalid/creative-work.png") {
		t.Fatalf("Attachment.URL 绕过了 DingTalk UploadImage: %q", calls[0].Text)
	}
}

func TestDingTalkRejectsInternalReferencesInVisibleMarkdownBeforeSend(t *testing.T) {
	references := []struct {
		name  string
		value string
	}{
		{name: "asset URI", value: "asset://child/0123456789abcdef.png"},
		{name: "file URI", value: "file:///Users/private/creative-work.png"},
		{name: "POSIX path", value: "/Users/private/creative-work.png"},
		{name: "opt path", value: "/opt/hexclaw/creative-work.png"},
		{name: "etc path", value: "/etc/hexclaw/creative-work.png"},
		{name: "Windows path", value: `C:\Users\private\creative-work.png`},
		{name: "UNC path", value: `\\server\share\creative-work.png`},
		{name: "protected asset URL", value: "http://127.0.0.1:16060/api/k12/assets/internal.png"},
		{name: "blob URL", value: "blob:https://desktop.invalid/internal-image"},
		{name: "data URL", value: "data:image/png;base64,aW50ZXJuYWw="},
	}
	for _, reference := range references {
		reference := reference
		t.Run(reference.name, func(t *testing.T) {
			client := newTestAdapter()
			client.queue = nil
			fake := newFakeDingtalkOpenAPI("test-token")
			client.openAPI = fake

			err := client.Send(context.Background(), "parent", &adapter.Reply{
				Content: "原图：" + reference.value,
			})
			calls := fake.SendCalls()
			if len(calls) > 0 && strings.Contains(calls[0].Text, reference.value) {
				t.Errorf("DingTalk Markdown 暴露内部引用 %q: %q", reference.value, calls[0].Text)
			}
			if err == nil {
				t.Error("包含内部引用的正文应在 SendOTO 前失败")
			}
			if len(calls) != 0 {
				t.Errorf("包含内部引用的正文 SendOTO 调用次数=%d，期望 0", len(calls))
			}
		})
	}
}

func TestDingTalkRejectsNonImageAttachmentPathBeforeSend(t *testing.T) {
	references := []struct {
		name  string
		value string
	}{
		{name: "asset URI", value: "asset://child/report.pdf"},
		{name: "file URI", value: "file:///Users/private/report.pdf"},
		{name: "POSIX path", value: "/Users/private/report.pdf"},
		{name: "opt path", value: "/opt/hexclaw/report.pdf"},
		{name: "etc path", value: "/etc/hexclaw/report.pdf"},
		{name: "Windows path", value: `C:\Users\private\report.pdf`},
		{name: "UNC path", value: `\\server\share\report.pdf`},
		{name: "protected asset URL", value: "http://127.0.0.1:16060/api/k12/assets/report.pdf"},
		{name: "blob URL", value: "blob:https://desktop.invalid/report"},
		{name: "data URL", value: "data:application/pdf;base64,cGRm"},
	}
	for _, reference := range references {
		reference := reference
		t.Run(reference.name, func(t *testing.T) {
			client := newTestAdapter()
			client.queue = nil
			fake := newFakeDingtalkOpenAPI("test-token")
			client.openAPI = fake

			err := client.Send(context.Background(), "parent", &adapter.Reply{
				Content: "## 作品点评",
				Attachments: []adapter.Attachment{{
					Type: "file",
					Name: reference.value,
					Mime: "application/pdf",
					Data: base64.StdEncoding.EncodeToString([]byte("pdf")),
				}},
			})
			calls := fake.SendCalls()
			if len(calls) > 0 && strings.Contains(calls[0].Text, reference.value) {
				t.Errorf("非图片附件名被拼入 DingTalk Markdown: %q", calls[0].Text)
			}
			if err == nil {
				t.Error("非图片附件应在 SendOTO 前失败")
			}
			if len(calls) != 0 {
				t.Errorf("非图片附件 SendOTO 调用次数=%d，期望 0", len(calls))
			}
		})
	}
}

func TestDingTalkRejectsReplyAttachmentWithoutArtifactPartBeforeSend(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("image-bytes"))
	sum := sha256.Sum256([]byte(encoded))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	assetID := "attachment:" + hex.EncodeToString(sum[:])
	canonical, err := messagecontent.New(
		messagecontent.ProducerK12,
		"zh-CN",
		"## 作品点评",
		[]messagecontent.AttachmentRef{{
			AssetID: assetID,
			Name:    "creative-work.png",
			MIME:    "image/png",
			Digest:  digest,
			AltText: "creative-work.png",
		}},
	)
	if err != nil {
		t.Fatalf("构造 canonical content: %v", err)
	}
	manifest, err := messagecontent.BuildManifest(canonical, messagecontent.RenderRequest{
		Surface:         messagecontent.SurfaceChannel,
		RendererVersion: "dingtalk-sample-markdown-v1",
		Capabilities: messagecontent.CapabilitySnapshot{
			Markdown:    true,
			UnicodeMath: true,
			Attachments: true,
		},
		// 回复携带图片，manifest 却只有 Markdown；缺少对应 Artifact 不能算完整投影。
		Parts: []messagecontent.RenderPart{{Kind: messagecontent.PartMarkdown, Text: canonical.Markdown}},
	})
	if err != nil {
		t.Fatalf("构造缺少 Artifact 的基线 manifest: %v", err)
	}

	client := newTestAdapter()
	client.queue = nil
	fake := newFakeConversationOpenAPI()
	client.openAPI = fake
	err = client.Send(context.Background(), "parent", &adapter.Reply{
		Content:        canonical.Markdown,
		MessageContent: &canonical,
		RenderManifest: &manifest,
		Attachments: []adapter.Attachment{{
			Type: "image",
			Name: "creative-work.png",
			Mime: "image/png",
			Data: encoded,
		}},
	})
	if err == nil {
		t.Error("缺少对应 PartArtifact 的回复应在 SendOTO 前失败")
	}
	fake.mu.Lock()
	uploads := len(fake.uploads)
	fake.mu.Unlock()
	if uploads != 0 {
		t.Errorf("渲染证据不完整时 UploadImage 调用次数=%d，期望 0", uploads)
	}
	if calls := fake.SendCalls(); len(calls) != 0 {
		t.Errorf("渲染证据不完整时 SendOTO 调用次数=%d，期望 0", len(calls))
	}
}

func TestDingTalkImageAttachmentPathNameDoesNotLeakToMultipartOrMarkdown(t *testing.T) {
	tests := []struct {
		name       string
		attachment string
	}{
		{name: "POSIX path", attachment: "/opt/hexclaw/creative-work.png"},
		{name: "Windows path", attachment: `C:\Users\private\creative-work.png`},
		{name: "UNC path", attachment: `\\server\share\creative-work.png`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			requestCount := 0
			multipartName := ""
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCount++
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Errorf("解析 multipart: %v", err)
					http.Error(w, "bad multipart", http.StatusBadRequest)
					return
				}
				file, header, err := r.FormFile("media")
				if err != nil {
					t.Errorf("读取 media part: %v", err)
					http.Error(w, "missing media", http.StatusBadRequest)
					return
				}
				_ = file.Close()
				multipartName = header.Filename
				_ = json.NewEncoder(w).Encode(map[string]any{
					"errcode":  0,
					"errmsg":   "ok",
					"media_id": "@media-safe-name",
				})
			}))
			defer srv.Close()

			attachment := adapter.Attachment{
				Type: "image",
				Name: tt.attachment,
				Mime: "image/png",
				Data: base64.StdEncoding.EncodeToString(testPNGBytes(t)),
			}
			_, err := uploadDingtalkImage(context.Background(), srv.Client(), srv.URL, "test-token", attachment)
			if err != nil {
				if requestCount != 0 {
					t.Errorf("附件名被拒绝前已发起 multipart 上传，调用次数=%d", requestCount)
				}
			} else if multipartName != "creative-work.png" {
				t.Errorf("multipart 文件名泄露完整路径: got %q, want %q", multipartName, "creative-work.png")
			}

			client := newTestAdapter()
			client.queue = nil
			fake := newFakeConversationOpenAPI()
			client.openAPI = fake
			err = client.Send(context.Background(), "parent", &adapter.Reply{
				Content:     "## 作品点评",
				Attachments: []adapter.Attachment{attachment},
			})
			calls := fake.SendCalls()
			if err != nil {
				if len(calls) != 0 {
					t.Errorf("附件名被拒绝后 SendOTO 调用次数=%d，期望 0", len(calls))
				}
				return
			}
			if len(calls) != 1 {
				t.Fatalf("SendOTO 调用次数=%d，期望 1", len(calls))
			}
			if strings.Contains(calls[0].Text, tt.attachment) {
				t.Errorf("DingTalk Markdown 泄露图片附件完整路径: %q", calls[0].Text)
			}
		})
	}
}

func TestEngineSkillReplyFlowsToDingTalkSampleMarkdown(t *testing.T) {
	store, err := sqlitestore.New(filepath.Join(t.TempDir(), "engine.db"))
	if err != nil {
		t.Fatalf("创建测试存储: %v", err)
	}
	defer store.Close()
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化测试存储: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"test": {APIKey: "sk-test", Model: "test-model"},
	}
	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Fatalf("创建测试路由: %v", err)
	}
	skills := skill.NewRegistry()
	if err := skills.Register(&outboundContractEchoSkill{}); err != nil {
		t.Fatalf("注册测试 Skill: %v", err)
	}
	eng := agentengine.NewReActEngine(cfg, router, store, skills)
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("启动测试 Engine: %v", err)
	}
	defer eng.Stop(context.Background())

	reply, err := eng.Process(context.Background(), &adapter.Message{
		ID:       "skill-outbound-1",
		Platform: adapter.PlatformAPI,
		UserID:   "parent",
		Content:  "/outbound-echo hello",
		Metadata: map[string]string{"user_locale": "zh-CN"},
	})
	if err != nil {
		t.Fatalf("执行 Skill 快速路径: %v", err)
	}
	if reply.MessageContent == nil || reply.RenderManifest == nil {
		t.Fatalf("Engine Skill Reply 缺少完整渲染证据: %#v", reply)
	}
	if reply.MessageContent.ProducerKind != messagecontent.ProducerSkill {
		t.Fatalf("Engine Skill ProducerKind=%q，期望 %q", reply.MessageContent.ProducerKind, messagecontent.ProducerSkill)
	}

	client := newTestAdapter()
	client.queue = nil
	fake := newFakeDingtalkOpenAPI("test-token")
	client.openAPI = fake
	if err := client.Send(context.Background(), "parent", reply); err != nil {
		t.Fatalf("Engine Skill Reply 发送到 DingTalk adapter: %v", err)
	}
	calls := fake.SendCalls()
	if len(calls) != 1 || calls[0].MsgKey != "sampleMarkdown" || calls[0].Text != "echo: hello" {
		t.Fatalf("Engine Skill Reply 未贯通 sampleMarkdown: %#v", calls)
	}
}

func TestDingTalkProducerKindMatrixUsesSampleMarkdown(t *testing.T) {
	producers := messagecontent.ProducerKinds()
	if len(producers) != 10 {
		t.Fatalf("ProducerKind 数量=%d，期望 10: %#v", len(producers), producers)
	}
	for _, producer := range producers {
		producer := producer
		t.Run(string(producer), func(t *testing.T) {
			markdown := "## 来源\n\nproducer: " + string(producer)
			canonical, err := messagecontent.New(producer, "zh-CN", markdown, nil)
			if err != nil {
				t.Fatalf("构造 canonical content: %v", err)
			}
			manifest, err := messagecontent.BuildManifest(canonical, messagecontent.RenderRequest{
				Surface:         messagecontent.SurfaceDesktop,
				RendererVersion: "engine-markdown-v1",
				Capabilities: messagecontent.CapabilitySnapshot{
					Markdown: true,
					TeXMath:  true,
					MathML:   true,
				},
				Parts: []messagecontent.RenderPart{{Kind: messagecontent.PartMarkdown, Text: markdown}},
			})
			if err != nil {
				t.Fatalf("构造 Engine manifest: %v", err)
			}

			client := newTestAdapter()
			client.queue = nil
			fake := newFakeDingtalkOpenAPI("test-token")
			client.openAPI = fake
			reply := &adapter.Reply{
				Content:        markdown,
				MessageContent: &canonical,
				RenderManifest: &manifest,
			}
			if err := client.Send(context.Background(), "parent", reply); err != nil {
				t.Fatalf("发送 ProducerKind %q: %v", producer, err)
			}
			if reply.MessageContent == nil || reply.MessageContent.ProducerKind != producer {
				t.Fatalf("ProducerKind 投影漂移: %#v", reply.MessageContent)
			}
			if reply.RenderManifest == nil || reply.RenderManifest.Surface != messagecontent.SurfaceChannel {
				t.Fatalf("DingTalk channel manifest 缺失: %#v", reply.RenderManifest)
			}
			calls := fake.SendCalls()
			if len(calls) != 1 || calls[0].MsgKey != "sampleMarkdown" || calls[0].Text != markdown {
				t.Fatalf("ProducerKind %q 未使用 sampleMarkdown: %#v", producer, calls)
			}
		})
	}
}

func TestDingTalkUsesChannelManifestProjectionForCanonicalLaTeX(t *testing.T) {
	canonicalMarkdown := "## 解题步骤\n\n答案是 $\\frac{3}{4}$。"
	projectedMarkdown := "## 解题步骤\n\n答案是 3/4。"
	canonical, err := messagecontent.New(messagecontent.ProducerK12, "zh-CN", canonicalMarkdown, nil)
	if err != nil {
		t.Fatalf("构造 canonical Markdown: %v", err)
	}
	manifest, err := messagecontent.BuildManifest(canonical, messagecontent.RenderRequest{
		Surface:         messagecontent.SurfaceChannel,
		RendererVersion: "channel-markdown-readable-math-v1",
		Capabilities: messagecontent.CapabilitySnapshot{
			Markdown:    true,
			UnicodeMath: true,
		},
		Parts:          []messagecontent.RenderPart{{Kind: messagecontent.PartMarkdown, Text: projectedMarkdown}},
		FallbackReason: messagecontent.FallbackMathToReadableText,
	})
	if err != nil {
		t.Fatalf("构造 channel RenderManifest: %v", err)
	}

	client := newTestAdapter()
	client.queue = nil
	fake := newFakeDingtalkOpenAPI("test-token")
	client.openAPI = fake
	err = client.Send(context.Background(), "parent", &adapter.Reply{
		Content:        projectedMarkdown,
		MessageContent: &canonical,
		RenderManifest: &manifest,
	})
	if err != nil {
		t.Fatalf("合法 channel Markdown 投影不应被 DingTalk 拒绝: %v", err)
	}
	calls := fake.SendCalls()
	if len(calls) != 1 || calls[0].MsgKey != "sampleMarkdown" || calls[0].Text != projectedMarkdown {
		t.Fatalf("DingTalk 未发送 manifest 可见投影: %#v", calls)
	}
}

func TestDingTalkNeverSendsReplyContentThatDiffersFromManifestProjection(t *testing.T) {
	const replyContent = "这段正文不能越过 manifest 发送"
	const manifestProjection = "这是 manifest 唯一允许的可见正文"
	canonical, err := messagecontent.New(messagecontent.ProducerK12, "zh-CN", replyContent, nil)
	if err != nil {
		t.Fatalf("构造 canonical Markdown: %v", err)
	}
	manifest, err := messagecontent.BuildManifest(canonical, messagecontent.RenderRequest{
		Surface:         messagecontent.SurfaceChannel,
		RendererVersion: "channel-markdown-readable-math-v1",
		Capabilities:    messagecontent.CapabilitySnapshot{Markdown: true, UnicodeMath: true},
		Parts:           []messagecontent.RenderPart{{Kind: messagecontent.PartMarkdown, Text: manifestProjection}},
	})
	if err != nil {
		t.Fatalf("构造 channel RenderManifest: %v", err)
	}

	client := newTestAdapter()
	client.queue = nil
	fake := newFakeDingtalkOpenAPI("test-token")
	client.openAPI = fake
	err = client.Send(context.Background(), "parent", &adapter.Reply{
		Content:        replyContent,
		MessageContent: &canonical,
		RenderManifest: &manifest,
	})
	calls := fake.SendCalls()
	if err != nil {
		if len(calls) != 0 {
			t.Fatalf("manifest 不一致被拒绝后仍调用 SendOTO: %#v", calls)
		}
		return
	}
	if len(calls) != 1 || calls[0].Text != manifestProjection {
		t.Fatalf("DingTalk 绕过 manifest 发送了 Reply.Content: %#v", calls)
	}
}

func TestDingTalkRebuildsIncompatibleManifestFromCanonicalMarkdown(t *testing.T) {
	const canonicalMarkdown = "## Canonical\n\nOnly this source may leave HexClaw."
	const tamperedReplyContent = "This mutable Reply.Content must not leave HexClaw."
	canonical, err := messagecontent.New(messagecontent.ProducerChat, "en", canonicalMarkdown, nil)
	if err != nil {
		t.Fatalf("构造 canonical Markdown: %v", err)
	}
	desktopManifest, err := messagecontent.BuildManifest(canonical, messagecontent.RenderRequest{
		Surface:         messagecontent.SurfaceDesktop,
		RendererVersion: "desktop-markdown-v1",
		Capabilities:    messagecontent.CapabilitySnapshot{Markdown: true, TeXMath: true},
		Parts:           []messagecontent.RenderPart{{Kind: messagecontent.PartMarkdown, Text: canonicalMarkdown}},
	})
	if err != nil {
		t.Fatalf("构造 Desktop manifest: %v", err)
	}

	client := newTestAdapter()
	client.queue = nil
	fake := newFakeDingtalkOpenAPI("test-token")
	client.openAPI = fake
	if err := client.Send(context.Background(), "parent", &adapter.Reply{
		Content:        tamperedReplyContent,
		MessageContent: &canonical,
		RenderManifest: &desktopManifest,
	}); err != nil {
		t.Fatalf("从 canonical 重建 channel manifest: %v", err)
	}
	calls := fake.SendCalls()
	if len(calls) != 1 || calls[0].Text != canonicalMarkdown {
		t.Fatalf("不兼容 manifest 重建时绕过 canonical source: %#v", calls)
	}
}

func TestDingTalkProviderPayloadPreservesManifestMarkdownExactly(t *testing.T) {
	const projectedMarkdown = `## 转义示例\n\n保留字面量，不得在 provider builder 二次改写。`
	canonical, err := messagecontent.New(messagecontent.ProducerChat, "zh-CN", projectedMarkdown, nil)
	if err != nil {
		t.Fatalf("构造 canonical Markdown: %v", err)
	}
	manifest, err := messagecontent.BuildManifest(canonical, messagecontent.RenderRequest{
		Surface:         messagecontent.SurfaceChannel,
		RendererVersion: "channel-markdown-readable-math-v1",
		Capabilities:    messagecontent.CapabilitySnapshot{Markdown: true, UnicodeMath: true},
		Parts:           []messagecontent.RenderPart{{Kind: messagecontent.PartMarkdown, Text: projectedMarkdown}},
	})
	if err != nil {
		t.Fatalf("构造 channel manifest: %v", err)
	}

	client := newTestAdapter()
	client.queue = nil
	fake := newFakeDingtalkOpenAPI("test-token")
	client.openAPI = fake
	if err := client.Send(context.Background(), "parent", &adapter.Reply{
		Content:        projectedMarkdown,
		MessageContent: &canonical,
		RenderManifest: &manifest,
	}); err != nil {
		t.Fatalf("发送 channel manifest: %v", err)
	}
	calls := fake.SendCalls()
	if len(calls) != 1 || calls[0].Text != projectedMarkdown {
		t.Fatalf("provider payload 未逐字使用 manifest PartMarkdown: %#v", calls)
	}
}

func TestDingTalkThinkingFeedbackCanonicalizesBeforeProviderSend(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位当前测试文件")
	}
	sourcePath := filepath.Join(filepath.Dir(testFile), "dingtalk.go")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("读取 DingTalk 生产文件: %v", err)
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, sourcePath, source, 0)
	if err != nil {
		t.Fatalf("解析 DingTalk 生产文件: %v", err)
	}

	// 处理中占位允许直连 provider 以取得撤回 ID，但必须先经过终态同一 canonical helper。
	for _, functionName := range []string{"sendThinkingFeedback", "sendThinkingFeedbackForEvent"} {
		function := findTestFunctionDecl(parsed, functionName)
		if function == nil {
			t.Fatalf("未找到 %s", functionName)
		}
		ensurePosition := token.NoPos
		firstProviderPosition := token.NoPos
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch callee := call.Fun.(type) {
			case *ast.Ident:
				if callee.Name == "ensureDingTalkRenderEvidence" {
					ensurePosition = call.Pos()
				}
			case *ast.SelectorExpr:
				if (callee.Sel.Name == "SendOTO" || callee.Sel.Name == "SendGroup") &&
					(firstProviderPosition == token.NoPos || call.Pos() < firstProviderPosition) {
					firstProviderPosition = call.Pos()
				}
			}
			return true
		})
		if ensurePosition == token.NoPos {
			t.Errorf("%s 未经过 ensureDingTalkRenderEvidence", functionName)
		} else if firstProviderPosition != token.NoPos && ensurePosition > firstProviderPosition {
			t.Errorf("%s 在 canonical 校验前调用了 provider", functionName)
		}
	}

	client := newTestAdapter()
	client.queue = nil
	fake := newFakeDingtalkOpenAPI("test-token")
	client.openAPI = fake
	if key := client.sendThinkingFeedback(context.Background(), "parent-1"); key == "" {
		t.Error("OTO 处理中占位未返回撤回 ID")
	}
	if key := client.sendThinkingFeedbackForEvent(context.Background(), dtEvent{
		ConversationType: "1",
		SenderStaffId:    "parent-2",
	}); key == "" {
		t.Error("OTO event 处理中占位未返回撤回 ID")
	}
	calls := fake.SendCalls()
	if len(calls) != 2 {
		t.Fatalf("处理中占位 SendOTO 调用次数=%d，期望 2", len(calls))
	}
	for index, call := range calls {
		if call.MsgKey != "sampleMarkdown" {
			t.Errorf("处理中占位 %d MsgKey=%q，期望 sampleMarkdown", index, call.MsgKey)
		}
	}
}

func TestDingTalkV05GroupEventsAndOutboundNeverCallProvider(t *testing.T) {
	tests := []struct {
		name string
		run  func(*DingtalkAdapter)
	}{
		{
			name: "inbound group event",
			run: func(client *DingtalkAdapter) {
				client.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
					return &adapter.Reply{Content: "群聊回复不得发送"}, nil
				}
				event := dtEvent{ConversationType: "2", ConversationId: "family-group", SenderStaffId: "parent"}
				event.Text.Content = "群聊问题"
				client.handleMessage(event)
			},
		},
		{
			name: "direct group final",
			run: func(client *DingtalkAdapter) {
				_ = client.sendReplyToEvent(context.Background(), dtEvent{
					ConversationType: "2",
					ConversationId:   "family-group",
				}, &adapter.Reply{Content: "群聊回复不得发送"})
			},
		},
		{
			name: "direct group progress",
			run: func(client *DingtalkAdapter) {
				_ = client.sendThinkingFeedbackForEvent(context.Background(), dtEvent{
					ConversationType: "2",
					ConversationId:   "family-group",
				})
			},
		},
		{
			name: "group queue target",
			run: func(client *DingtalkAdapter) {
				_ = client.Send(context.Background(), groupQueueTarget("family-group"), &adapter.Reply{
					Content: "群聊回复不得发送",
				})
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			client := newTestAdapter()
			client.queue = nil
			fake := newFakeConversationOpenAPI()
			client.openAPI = fake
			tt.run(client)
			assertNoDingTalkProviderCalls(t, fake)
		})
	}
}

func TestDingTalkV05OTOStillUsesSampleMarkdown(t *testing.T) {
	client := newTestAdapter()
	client.queue = nil
	fake := newFakeDingtalkOpenAPI("test-token")
	client.openAPI = fake
	if err := client.Send(context.Background(), "parent-1", &adapter.Reply{Content: "## 正常 OTO 回复"}); err != nil {
		t.Fatalf("发送 OTO 终态回复: %v", err)
	}
	if key := client.sendThinkingFeedbackForEvent(context.Background(), dtEvent{
		ConversationType: "1",
		SenderStaffId:    "parent-2",
	}); key == "" {
		t.Fatal("发送 OTO 处理中占位未返回撤回 ID")
	}
	calls := fake.SendCalls()
	if len(calls) != 2 {
		t.Fatalf("OTO SendOTO 调用次数=%d，期望 2", len(calls))
	}
	for index, call := range calls {
		if call.MsgKey != "sampleMarkdown" {
			t.Errorf("OTO 消息 %d MsgKey=%q，期望 sampleMarkdown", index, call.MsgKey)
		}
	}
}

func TestDingTalkInternalReferenceGuardKeepsNormalMarkdownLinks(t *testing.T) {
	client := newTestAdapter()
	client.queue = nil
	fake := newFakeDingtalkOpenAPI("test-token")
	client.openAPI = fake

	const content = "## 参考资料\n\n- [课程页面](https://example.test/docs/api/v1)\n- 分数：3/4\n- 价格：$5"
	if err := client.Send(context.Background(), "parent", &adapter.Reply{Content: content}); err != nil {
		t.Fatalf("正常 Markdown 链接不应被内部引用校验拒绝: %v", err)
	}
	calls := fake.SendCalls()
	if len(calls) != 1 || calls[0].MsgKey != "sampleMarkdown" || calls[0].Text != content {
		t.Fatalf("正常 Markdown 投影漂移: %#v", calls)
	}
}

func findTestFunctionDecl(file *ast.File, name string) *ast.FuncDecl {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == name {
			return function
		}
	}
	return nil
}

func assertNoDingTalkProviderCalls(t *testing.T, fake *fakeConversationOpenAPI) {
	t.Helper()
	if calls := fake.SendCalls(); len(calls) != 0 {
		t.Errorf("v0.5 群聊触发 SendOTO: %#v", calls)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.groupSends) != 0 {
		t.Errorf("v0.5 群聊触发 SendGroup: %#v", fake.groupSends)
	}
	if len(fake.uploads) != 0 {
		t.Errorf("v0.5 群聊触发 UploadImage: %#v", fake.uploads)
	}
}

type outboundContractEchoSkill struct{}

func (*outboundContractEchoSkill) Name() string        { return "outbound-echo" }
func (*outboundContractEchoSkill) Description() string { return "回显出站测试内容" }
func (*outboundContractEchoSkill) Match(content string) bool {
	return strings.HasPrefix(content, "/outbound-echo ")
}
func (*outboundContractEchoSkill) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	query, _ := args["query"].(string)
	return &skill.Result{Content: "echo: " + strings.TrimPrefix(query, "/outbound-echo ")}, nil
}
func (*outboundContractEchoSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("outbound-echo", "回显出站测试内容", nil)
}
