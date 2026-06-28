package engine

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

// 增量 G（采纳 Claude Code 式「主模型随手判断」）：默认 inline 不再每轮另起 LLM 抽取。
// 这些测试钉死模式解析 + inline/off 下后台抽取真被门掉（省 token 的核心保证）。

func TestAutoMemoryMode_Resolves(t *testing.T) {
	cases := []struct {
		raw, want string
	}{
		{"", autoMemoryInline}, // 未配 → 默认 inline
		{"inline", autoMemoryInline},
		{"INLINE", autoMemoryInline},     // 大小写不敏感
		{" extract ", autoMemoryExtract}, // 去空白
		{"extract", autoMemoryExtract},
		{"off", autoMemoryOff},
		{"garbage", autoMemoryInline}, // 非法 → 安全回落 inline
	}
	for _, c := range cases {
		e := &ReActEngine{cfg: &config.Config{}}
		e.cfg.FileMemory.AutoMemory = c.raw
		if got := e.autoMemoryMode(); got != c.want {
			t.Errorf("autoMemoryMode(%q)=%q want %q", c.raw, got, c.want)
		}
	}
	// cfg 为 nil 也安全（默认 inline）。
	if got := (&ReActEngine{}).autoMemoryMode(); got != autoMemoryInline {
		t.Errorf("nil cfg → %q want inline", got)
	}
}

// off 模式 / 无路由 → autoExtractMemoryForRole 早退、不落库（确定性）。
// 注：inline 的「云端跳过 / 本地兜底」依赖真实路由，由真机测试覆盖（S12 云端不新增 chat_extract、
// TestAuditD 本地 qwen 兜底抽取），此处不构造假路由。
func TestAutoExtract_OffAndNoRouterNoOp(t *testing.T) {
	memorable := "我叫小明，是一名 Go 后端工程师，住在北京，我对花生过敏"
	// off 模式：即便文本明显可记，也不抽取。
	fm := newFileMem(t, 200)
	e := &ReActEngine{cfg: &config.Config{}}
	e.cfg.FileMemory.AutoMemory = "off"
	e.SetFileMemory(fm)
	e.autoExtractMemoryForRole(context.Background(), memorable, "好的，已了解。", "")
	if n := len(fm.ParseEntries()); n != 0 {
		t.Errorf("off 模式不应抽取，却写了 %d 条", n)
	}
	// inline 模式 + 无路由：安全早退不 panic、不落库。
	fm2 := newFileMem(t, 200)
	e2 := &ReActEngine{cfg: &config.Config{}}
	e2.cfg.FileMemory.AutoMemory = "inline"
	e2.SetFileMemory(fm2)
	e2.autoExtractMemoryForRole(context.Background(), memorable, "好的。", "")
	if n := len(fm2.ParseEntries()); n != 0 {
		t.Errorf("无路由应安全早退，却写了 %d 条", n)
	}
}
