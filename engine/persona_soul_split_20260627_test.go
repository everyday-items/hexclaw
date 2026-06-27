package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// 重构（2026-06-27）：默认人设拆成「人设(SOUL，给用户编辑) + 运行手册(工具纪律，固定)」。
//
// 不变量③（修复 + 防回归）：用户自定义 SOUL.md 时，工具纪律仍被附加——
// 改了人设也不丢「别谎报存盘 / 导出指引」等纪律。
// 此前 buildStreamMessages 的自定义 SOUL 分支是 decorateSystemPrompt(soul,…)，把工具纪律整体丢掉，
// 自定义人设的用户会失去这些纪律 → 本测试钉死「自定义 SOUL 也带运行手册」。
func TestCustomSoul_StillGetsOperatingManual(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := config.WriteSoul("你是定制小蟹。只说人话，别啰嗦。"); err != nil {
		t.Fatalf("WriteSoul: %v", err)
	}
	t.Cleanup(func() { _ = config.WriteSoul("") })

	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{"test": {APIKey: "sk-test", Model: "test-model"}}
	router, _ := llmrouter.New(cfg.LLM)
	dir := t.TempDir()
	store, _ := sqlitestore.New(filepath.Join(dir, "test.db"))
	t.Cleanup(func() { store.Close() })
	store.Init(context.Background())
	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())

	msgs := eng.buildStreamMessages(context.Background(), "", nil, "", "你好", nil, nil)
	sys := msgs[0].Content

	if !strings.Contains(sys, "你是定制小蟹") {
		t.Fatalf("自定义 SOUL 未被应用")
	}
	// 修复点：自定义人设也要带上工具纪律。
	if !strings.Contains(sys, "严禁") || !strings.Contains(sys, "Download") {
		t.Errorf("BUG: 自定义 SOUL 丢了工具纪律（别谎报存盘 / 导出指引）——改人设不应丢能力纪律")
	}
	// 自定义人设本体必须在工具纪律之前（人设打底、手册附加）。
	if i, j := strings.Index(sys, "你是定制小蟹"), strings.Index(sys, "工具使用偏好"); i < 0 || j < 0 || i > j {
		t.Errorf("装配顺序应为「人设 → 运行手册」")
	}
}

// 不变量①：拆分后，引擎实际下发的完整默认 system prompt 仍含全部工具纪律——拆分不得丢能力指引。
func TestDefaultSystemPrompt_RetainsOperatingManual(t *testing.T) {
	full := DefaultSystemPrompt()
	for _, must := range []string{
		"hexclaw.net",            // 官网锚点
		"严禁",                    // 别谎报存盘红线
		"write_file",             // 工具偏好
		"code_exec",              // 代码执行偏好
		"明确点名要某种可下载格式", // PDF/Word 导出指引
		"Download",               // 导出入口
		"file_edit",              // 精确改文件
	} {
		if !strings.Contains(full, must) {
			t.Errorf("默认 system prompt 拆分后丢了工具纪律关键项: %q", must)
		}
	}
}

// 不变量②：给用户编辑的「人设(DefaultSoul)」是角色，不是工具手册——
// 含角色要素，但不混入 write_file/code_exec 等工具操作细节；且显著短于完整 prompt。
func TestDefaultSoul_IsPersonaNotManual(t *testing.T) {
	soul := DefaultSoul()
	for _, must := range []string{"小蟹", "河蟹", "hexclaw.net", "官网", "信条"} {
		if !strings.Contains(soul, must) {
			t.Errorf("默认人设缺角色要素: %q", must)
		}
	}
	for _, mustNot := range []string{"write_file", "code_exec", "file_edit", "严禁"} {
		if strings.Contains(soul, mustNot) {
			t.Errorf("默认人设(给用户编辑的)不应混入工具手册项: %q", mustNot)
		}
	}
	if len(soul) >= len(DefaultSystemPrompt()) {
		t.Errorf("人设应短于完整 system prompt（魂 vs 手册），soul=%d full=%d", len(soul), len(DefaultSystemPrompt()))
	}
}

// 不变量④（本地化 A · 先 en）：user_locale=en 时下发**原生英文人设**，不是中文人设机翻；
// 且仍带运行手册纪律 + EN locale 指令；中文人设的签名不出现（证明确实换成了 EN 原生）。
func TestEnLocale_GetsNativeEnglishPersona(t *testing.T) {
	got := systemPrompt(map[string]string{"user_locale": "en"})

	for _, must := range []string{
		"Little Crab",      // EN 角色名
		"Hard claws",       // EN 声音：钳
		"local-first",      // EN 定位
		"Creed:",           // EN 信条
		"hexclaw.net",      // 官网锚点
	} {
		if !strings.Contains(got, must) {
			t.Errorf("en 人设缺原生英文要素: %q", must)
		}
	}
	// 仍附加运行手册（工具纪律对所有 locale 一致）。
	if !strings.Contains(got, "严禁") || !strings.Contains(got, "Download") {
		t.Errorf("en 下发的 system prompt 丢了运行手册工具纪律")
	}
	// EN locale 指令在位。
	if !strings.Contains(got, "respond in English") {
		t.Errorf("en locale 指令缺失")
	}
	// 不应再是中文人设机翻：中文人设独有签名不出现。
	if strings.Contains(got, "私人 AI 搭子") || strings.Contains(got, "钳得住活，锁得住数据") {
		t.Errorf("en 仍混入中文人设签名（应为原生 EN 人设）")
	}
}
