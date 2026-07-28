package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/toolkit/lang/stringx"
)

// ═══════════════════════════════════════════════════════════
// 第一层：单元测试 — extractThinkTags 函数的纯逻辑验证
// ═══════════════════════════════════════════════════════════

func TestExtractThinkTags_Unit(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		wantContent   string
		wantReasoning string
	}{
		// ── 基本提取 ─────────────────────────────
		{"think 标签基本提取", "<think>思考</think>回复", "回复", "思考"},
		{"thinking 标签基本提取", "<thinking>reasoning</thinking>reply", "reply", "reasoning"},
		{"前导空白", "  \n<think>思考</think>回复", "回复", "思考"},
		{"前导制表符", "\t<think>思考</think>回复", "回复", "思考"},

		// ── 真实模型输出 ─────────────────────────
		{
			"glm-z1-airx 真实响应（API 抓取）",
			"\n<think>用户问了一个非常简单的数学问题：\"1+1等于几\"。这是一个基本的算术问题，答案很明显是2。\n\n我不需要使用任何工具来回答这个问题，因为这是一个常识性的数学问题。我可以直接回答。</think>\n1+1等于2。\n\n这是一个基本的数学运算，两个1相加的结果是2。",
			"1+1等于2。\n\n这是一个基本的数学运算，两个1相加的结果是2。",
			"用户问了一个非常简单的数学问题：\"1+1等于几\"。这是一个基本的算术问题，答案很明显是2。\n\n我不需要使用任何工具来回答这个问题，因为这是一个常识性的数学问题。我可以直接回答。",
		},
		{
			"DeepSeek R1 格式",
			"<think>\nLet me think step by step.\n1. First\n2. Second\n</think>\nThe answer is 42.",
			"The answer is 42.",
			"Let me think step by step.\n1. First\n2. Second",
		},
		{
			"glm-z1 杭帮菜真实截图",
			"<think>用户问的是\"杭帮菜的正确吃法\"，这是一个关于杭州菜系的问题。我需要查看用户上传的文档\"杭帮菜的正确吃法\"，因为这是一个已上传的文档，我应该基于这个文档来回答问题。\n\n让我先读取这个文档的内容。</think>\n我来帮您查看关于杭帮菜正确吃法的文档内容。",
			"我来帮您查看关于杭帮菜正确吃法的文档内容。",
			"用户问的是\"杭帮菜的正确吃法\"，这是一个关于杭州菜系的问题。我需要查看用户上传的文档\"杭帮菜的正确吃法\"，因为这是一个已上传的文档，我应该基于这个文档来回答问题。\n\n让我先读取这个文档的内容。",
		},

		// ── 安全：不误匹配 ───────────────────────
		{"正文中间的 <think> 不匹配", "关于 XML，<think> 标签用于思考。</think>示例。", "关于 XML，<think> 标签用于思考。</think>示例。", ""},
		{"代码块中的 <think> 不匹配", "```xml\n<think>example</think>\n```", "```xml\n<think>example</think>\n```", ""},
		{"文字后的 <think> 不匹配", "Hello <think>world</think> end", "Hello <think>world</think> end", ""},
		{"Markdown 标题后的 <think>", "# Title\n<think>body</think>", "# Title\n<think>body</think>", ""},
		{"HTML 注释后的 <think>", "<!-- comment --><think>body</think>", "<!-- comment --><think>body</think>", ""},
		{"用户消息讨论 think 标签", "请问 <think> 标签是什么意思?", "请问 <think> 标签是什么意思?", ""},

		// ── 普通模型（无 think 标签）─────────────
		{"普通中文文本", "这是一个普通回复。", "这是一个普通回复。", ""},
		{"普通英文文本", "Hello world, this is a test.", "Hello world, this is a test.", ""},
		{"纯 Markdown", "# Title\n- item1\n- item2\n\n```code```", "# Title\n- item1\n- item2\n\n```code```", ""},
		{"含 HTML 标签（非 think）", "<p>paragraph</p><br>text", "<p>paragraph</p><br>text", ""},
		{"空字符串", "", "", ""},
		{"纯空白", "   \n\t  ", "", ""},

		// ── 边界情况 ────────────────────────────
		{"空 think 块", "<think></think>正式回复", "正式回复", ""},
		{"空 thinking 块", "<thinking></thinking>正式回复", "正式回复", ""},
		{"仅思考无回复", "<think>只有思考</think>", "", "只有思考"},
		{"仅思考带尾部换行", "<think>思考</think>\n\n", "", "思考"},
		{"仅思考带尾部空格", "<think>思考</think>   ", "", "思考"},
		{"未闭合标签", "<think>正在思考中...", "", "正在思考中..."},
		{"未闭合 thinking", "<thinking>partial", "", "partial"},
		{"仅开标签", "<think>", "", ""},
		{"多行 reasoning", "<think>行1\n行2\n行3</think>回复", "回复", "行1\n行2\n行3"},
		{
			"reasoning 含特殊字符",
			"<think>引号\"和反斜杠\\和换行\n和制表符\t</think>回复",
			"回复",
			"引号\"和反斜杠\\和换行\n和制表符", // TrimSpace 去除尾部制表符
		},
		{
			"reasoning 含 XML 标签",
			"<think>看到 <tool_call> 需要调用</think>好的",
			"好的",
			"看到 <tool_call> 需要调用",
		},
		{
			"content 含 Markdown 代码块",
			"<think>思考</think>```python\nprint('hello')\n```",
			"```python\nprint('hello')\n```",
			"思考",
		},
		{
			"长文本 reasoning（>1KB）",
			"<think>" + strings.Repeat("这是一段很长的思考过程。", 100) + "</think>最终回复",
			"最终回复",
			strings.TrimSpace(strings.Repeat("这是一段很长的思考过程。", 100)),
		},

		// ── thinking 优先于 think ────────────────
		{
			"thinking 优先匹配",
			"<thinking>reason</thinking>reply with <think>not this</think>",
			"reply with <think>not this</think>",
			"reason",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, reasoning := extractThinkTags(tt.input)
			if content != tt.wantContent {
				t.Errorf("content:\n  got:  %q\n  want: %q", content, tt.wantContent)
			}
			if reasoning != tt.wantReasoning {
				t.Errorf("reasoning:\n  got:  %q\n  want: %q", reasoning, tt.wantReasoning)
			}
		})
	}
}

// ── 幂等性测试 ──────────────────────────────────────────

func TestExtractThinkTags_Idempotent(t *testing.T) {
	inputs := []string{
		"<think>思考</think>回复",
		"普通文本",
		"<thinking>r</thinking>c",
		"",
	}
	for _, input := range inputs {
		c1, r1 := extractThinkTags(input)
		c2, r2 := extractThinkTags(c1)
		if c2 != c1 || r2 != "" {
			t.Errorf("非幂等: input=%q → c1=%q → c2=%q r2=%q", input, c1, c2, r2)
		}
		_ = r1
	}
}

// ── 对称性测试 ──────────────────────────────────────────

func TestExtractThinkTags_NoInfoLoss(t *testing.T) {
	inputs := []struct {
		raw          string
		wantContains string
	}{
		{"<think>ABC</think>DEF", "ABCDEF"},
		{"<thinking>X</thinking>Y", "XY"},
		{"普通文本", "普通文本"},
	}
	for _, tt := range inputs {
		content, reasoning := extractThinkTags(tt.raw)
		combined := strings.ReplaceAll(reasoning+content, " ", "")
		want := strings.ReplaceAll(tt.wantContains, " ", "")
		if !strings.Contains(combined, want) {
			t.Errorf("信息丢失: raw=%q → reasoning+content=%q, want contains %q", tt.raw, reasoning+content, tt.wantContains)
		}
	}
}

// ═══════════════════════════════════════════════════════════
// 第二层：模块测试 — 模拟 pipeStream 中的完整处理流程
// ═══════════════════════════════════════════════════════════

func TestPipeStreamLogic_Module(t *testing.T) {
	tests := []struct {
		name               string
		rawContent         string
		rawReasoning       string
		wantFinalContent   string
		wantFinalReasoning string
		wantDiagnostic     bool
	}{
		{
			name:               "场景A: 普通模型，无 think 标签",
			rawContent:         "你好，有什么可以帮你的？",
			wantFinalContent:   "你好，有什么可以帮你的？",
			wantFinalReasoning: "",
			wantDiagnostic:     false,
		},
		{
			name:               "场景B: 智谱 glm-z1 — content 含 <think>，reasoning 为空",
			rawContent:         "<think>让我想想这个问题...</think>答案是42",
			wantFinalContent:   "答案是42",
			wantFinalReasoning: "让我想想这个问题...",
			wantDiagnostic:     false,
		},
		{
			name:               "场景C: Ollama 原生 thinking — 双字段都有值",
			rawContent:         "1+1等于2。",
			rawReasoning:       "这是简单的数学。",
			wantFinalContent:   "1+1等于2。",
			wantFinalReasoning: "这是简单的数学。",
			wantDiagnostic:     false,
		},
		{
			name:               "场景D: Bug#2 — Ollama 仅输出 reasoning，content 空",
			rawContent:         "",
			rawReasoning:       "深入思考但未生成正式回复。",
			wantFinalContent:   "",
			wantFinalReasoning: "深入思考但未生成正式回复。",
			wantDiagnostic:     false, // 有 reasoning，不应报错
		},
		{
			name:               "场景E: 真正空回复 — 都空",
			rawContent:         "",
			rawReasoning:       "",
			wantFinalContent:   "",
			wantFinalReasoning: "",
			wantDiagnostic:     true, // 应报错
		},
		{
			name:               "场景F: Ollama 已有 reasoning + content 含 <think>（不覆盖）",
			rawContent:         "<think>新思考</think>正式回复",
			rawReasoning:       "Ollama 原生思考",
			wantFinalContent:   "正式回复",
			wantFinalReasoning: "Ollama 原生思考", // 不被覆盖
			wantDiagnostic:     false,
		},
		{
			name:               "场景G: content 仅含 <think> 无正文 + reasoning 空 → 提取后有值",
			rawContent:         "<think>只有思考</think>",
			rawReasoning:       "",
			wantFinalContent:   "",
			wantFinalReasoning: "只有思考",
			wantDiagnostic:     false, // 提取出 reasoning，不应报错
		},
		{
			name:               "场景H: 空 <think> 块 + reasoning 空 → 应报错",
			rawContent:         "<think></think>",
			rawReasoning:       "",
			wantFinalContent:   "",
			wantFinalReasoning: "",
			wantDiagnostic:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 完全模拟 react.go pipeStream 中修复后的逻辑
			var fullReasoning strings.Builder
			if tt.rawReasoning != "" {
				fullReasoning.WriteString(tt.rawReasoning)
			}

			content := tt.rawContent

			// 步骤1: extractThinkTags 兜底提取（与 react.go 一致）
			if cleaned, extracted := extractThinkTags(content); extracted != "" && fullReasoning.Len() == 0 {
				fullReasoning.WriteString(extracted)
				content = cleaned
			} else {
				content = cleaned
			}

			// 步骤2: 空内容诊断（与 react.go 一致）
			shouldDiag := content == "" && fullReasoning.Len() == 0

			if content != tt.wantFinalContent {
				t.Errorf("content:\n  got:  %q\n  want: %q", content, tt.wantFinalContent)
			}
			gotReasoning := fullReasoning.String()
			if gotReasoning != tt.wantFinalReasoning {
				t.Errorf("reasoning:\n  got:  %q\n  want: %q", gotReasoning, tt.wantFinalReasoning)
			}
			if shouldDiag != tt.wantDiagnostic {
				t.Errorf("diagnostic:\n  got:  %v\n  want: %v", shouldDiag, tt.wantDiagnostic)
			}
		})
	}
}

// ── 后端完整累积后提取测试 ──────────────────────────────

func TestFullAccumulationThenExtract_Module(t *testing.T) {
	// 后端在 pipeStream 中先完整累积 fullContent，流式结束后才调用 extractThinkTags
	// 模拟多个 chunk 的拼接结果
	chunks := []string{
		"\n<thi",
		"nk>用户问的是杭帮菜",
		"的正确吃法。",
		"</think>\n",
		"我来帮您查看文档。",
	}

	var buf string
	for _, chunk := range chunks {
		buf += chunk
	}

	// 累积完成后一次性提取
	content, reasoning := extractThinkTags(buf)
	if content != "我来帮您查看文档。" {
		t.Errorf("content = %q, want '我来帮您查看文档。'", content)
	}
	if !strings.Contains(reasoning, "杭帮菜") {
		t.Errorf("reasoning 应包含 '杭帮菜'，got: %q", reasoning[:min(80, len(reasoning))])
	}
	if strings.Contains(content, "<think>") {
		t.Error("content 不应包含 <think> 标签")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ── 修复前后对比 ───────────────────────────────────────

func TestBeforeAfterComparison_Module(t *testing.T) {
	type scenario struct {
		name       string
		content    string
		reasoning  string
		bugBefore  string // 修复前的问题描述
		fixedAfter string // 修复后的期望行为
	}

	scenarios := []scenario{
		{
			name:       "Bug#1: glm-z1 <think> 原样显示",
			content:    "<think>思考过程</think>正式回复",
			reasoning:  "",
			bugBefore:  "content 包含原始 <think> 标签，用户看到原始 XML",
			fixedAfter: "content 干净，reasoning 被提取",
		},
		{
			name:       "Bug#2: qwen3.5 空内容误报",
			content:    "",
			reasoning:  "模型输出了深度思考",
			bugBefore:  "生成「模型未返回有效内容」错误消息",
			fixedAfter: "有 reasoning 时不报错",
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			var fullReasoning strings.Builder
			if s.reasoning != "" {
				fullReasoning.WriteString(s.reasoning)
			}
			content := s.content

			// 修复后逻辑
			if cleaned, extracted := extractThinkTags(content); extracted != "" && fullReasoning.Len() == 0 {
				fullReasoning.WriteString(extracted)
				content = cleaned
			} else {
				content = cleaned
			}
			shouldDiag := content == "" && fullReasoning.Len() == 0

			// Bug#1 验证：content 不应包含 <think>
			if strings.Contains(content, "<think>") || strings.Contains(content, "<thinking>") {
				t.Errorf("修复失败 [%s]: content 仍包含 think 标签", s.bugBefore)
			}

			// Bug#2 验证：有 reasoning 时不应触发诊断
			if fullReasoning.Len() > 0 && shouldDiag {
				t.Errorf("修复失败 [%s]: 有 reasoning 但仍触发了空内容诊断", s.bugBefore)
			}

			t.Logf("✅ %s → %s", s.name, s.fixedAfter)
		})
	}
}

// ═══════════════════════════════════════════════════════════
// 第三层：系统测试 — 调用真实运行的 hexclaw API
// ═══════════════════════════════════════════════════════════

func TestRealAPI_GLMz1_SystemTest(t *testing.T) {
	if os.Getenv("HEXCLAW_REAL_API_TEST") != "1" {
		t.Skip("real API test requires HEXCLAW_REAL_API_TEST=1")
	}
	if testing.Short() {
		t.Skip("跳过系统测试（-short 模式）")
	}
	resp, err := callChatAPI("1+1等于几", "glm-z1-airx", "智谱 AI")
	if err != nil {
		t.Skipf("API 不可用: %v", err)
	}

	if strings.Contains(resp.Reply, "模型未返回有效内容") {
		t.Error("❌ 不应返回空内容诊断消息")
	}

	// 如果旧版引擎返回了原始 <think> 标签，验证前端兜底能处理
	if strings.Contains(resp.Reply, "<think>") {
		content, reasoning := extractThinkTags(resp.Reply)
		if strings.Contains(content, "<think>") {
			t.Error("❌ extractThinkTags 后 content 仍含 <think>")
		}
		if reasoning == "" {
			t.Error("❌ extractThinkTags 后 reasoning 为空")
		}
		t.Logf("✅ 前端兜底: reasoning=%s content=%s", stringx.TruncateWithSuffix(reasoning, 60, "..."), stringx.TruncateWithSuffix(content, 60, "..."))
	} else {
		t.Logf("✅ reply 干净: %s", stringx.TruncateWithSuffix(resp.Reply, 100, "..."))
	}
}

func TestRealAPI_Qwen35_SystemTest(t *testing.T) {
	if os.Getenv("HEXCLAW_REAL_API_TEST") != "1" {
		t.Skip("real API test requires HEXCLAW_REAL_API_TEST=1")
	}
	if testing.Short() {
		t.Skip("跳过系统测试（-short 模式）")
	}
	resp, err := callChatAPI("1+1等于几", "qwen3.5:9b", "Ollama (本地)")
	if err != nil {
		t.Skipf("API 不可用: %v", err)
	}

	if strings.Contains(resp.Reply, "模型未返回有效内容") {
		t.Error("❌ Bug#2 未修复: 返回了空内容诊断消息")
	}
	if strings.Contains(resp.Reply, "<think>") {
		t.Error("❌ reply 不应含 <think> 标签（Ollama 应走原生 reasoning）")
	}
	t.Logf("✅ qwen3.5:9b: %s", stringx.TruncateWithSuffix(resp.Reply, 100, "..."))
}

// ─── 辅助函数 ────────────────────────────────────────

type chatAPIResponse struct {
	Reply    string            `json:"reply"`
	Metadata map[string]string `json:"metadata"`
}

func callChatAPI(message, model, provider string) (*chatAPIResponse, error) {
	body := fmt.Sprintf(`{"message":"%s","user_id":"test-system","model":"%s","provider":"%s"}`,
		message, model, provider)

	httpResp, err := (&http.Client{Timeout: 30 * time.Second}).Post("http://localhost:16060/api/v1/chat", "application/json", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	data, _ := io.ReadAll(httpResp.Body)
	if httpResp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d: %s", httpResp.StatusCode, string(data))
	}

	var result chatAPIResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ═══════════════════════════════════════════════════════════
// isLocalThinkingModel 单元测试
// ═══════════════════════════════════════════════════════════

func TestIsLocalThinkingModel(t *testing.T) {
	positive := []string{
		"qwen3:8b", "qwen3.5:9b", "qwen3-coder:14b",
		"deepseek-r1:7b", "deepseek-r1:14b", "deepseek-r1",
		"qwq:32b", "QWQ",
		"gemma4:e4b", "gemma4:26b", "gemma4:31b", "Gemma4:E2B",
	}
	for _, m := range positive {
		if !isLocalThinkingModel(m) {
			t.Errorf("isLocalThinkingModel(%q) = false, want true", m)
		}
	}

	negative := []string{
		"llama3.3:70b", "phi4", "mistral", "gemma3:12b",
		"command-r", "starcoder2", "nomic-embed-text",
		"gpt-4o", "claude-3.5-sonnet",
	}
	for _, m := range negative {
		if isLocalThinkingModel(m) {
			t.Errorf("isLocalThinkingModel(%q) = true, want false", m)
		}
	}
}

func TestNeedsNoThinkInjection(t *testing.T) {
	// Qwen3/DeepSeek-R1/QWQ 需要 /no_think 注入
	needsInjection := []string{
		"qwen3:8b", "qwen3.5:9b", "deepseek-r1:7b", "qwq:32b",
	}
	for _, m := range needsInjection {
		if !needsNoThinkInjection(m) {
			t.Errorf("needsNoThinkInjection(%q) = false, want true", m)
		}
	}

	// Gemma 4 通过 Ollama 模板层控制 thinking，不需要 /no_think 注入
	noInjection := []string{
		"gemma4:e4b", "gemma4:26b", "gemma4:31b", "Gemma4:E2B",
		"llama3.3:70b", "phi4", "gemma3:12b",
	}
	for _, m := range noInjection {
		if needsNoThinkInjection(m) {
			t.Errorf("needsNoThinkInjection(%q) = true, want false", m)
		}
	}
}
