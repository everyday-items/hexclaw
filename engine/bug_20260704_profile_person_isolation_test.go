package engine

// BUG-20260704 用户画像"主语隔离缺失" —— prompt 层（治本 + 兜住无结构标记的存量脏数据）。
//
// 确定性层（memory 包 collectProfileInputs 排除 PersonSubjectPrefix 主语）只能拦住
// **已被打上主语标记**的第三方人物事实。要从源头不误归、并兜住库里已存的无标记裸事实，
// 还需两个 prompt 各加一道"第三方人物隔离"约束：
//   - 提取器 memoryExtractionSystemPrompt：对话里谈到的其他人不得写成"用户是…"，
//     需记住第三方人物资料时用 `[人物:名]` 前缀标注主语。
//   - 蒸馏器 profileSynthSystemPrompt：画像只描述当前使用者本人，忽略事实中的第三方
//     具名人物，绝不把他人头衔/特质安到使用者身上。
//
// 本文件是 prompt 契约的确定性回归锁（断言指令存在）；真实 LLM 行为验证见
// TestBug20260704_ProfileSynth_RealLLM_IgnoresThirdParty（HEXCLAW_REAL_LLM_EVAL 门控）。

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/memory"
)

// newRealLLMEngineForMemory 建一个仅接了真实 provider/router 的最小 ReActEngine
// （CompleteOnce 只依赖 router；store/skills 不需要）。HEXCLAW_REAL_LLM_* 门控。
func newRealLLMEngineForMemory(t *testing.T) *ReActEngine {
	t.Helper()
	cfg, err := config.Load(memEvalEnv("HEXCLAW_REAL_LLM_CONFIG", ""))
	if err != nil {
		t.Skipf("load config: %v", err)
	}
	provider := memEvalEnv("HEXCLAW_REAL_LLM_PROVIDER", "Ollama (本地)")
	model := memEvalEnv("HEXCLAW_REAL_LLM_MODEL", "qwen3.5:9b")
	pc, ok := cfg.LLM.Providers[provider]
	if !ok {
		t.Skipf("provider %q not in config", provider)
	}
	pc.Model = model
	cfg.LLM.Providers[provider] = pc
	cfg.LLM.Default = provider
	for name := range cfg.LLM.Providers { // 隔离到选中 provider
		p := cfg.LLM.Providers[name]
		en := name == provider
		p.Enabled = &en
		cfg.LLM.Providers[name] = p
	}
	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Skipf("router: %v", err)
	}
	return NewReActEngine(cfg, router, nil, nil)
}

func TestBug20260704_ExtractionPrompt_IsolatesThirdPartyPerson(t *testing.T) {
	p := memoryExtractionSystemPrompt
	// 必须明确"只提取使用者本人"且第三方人物不归属给"用户"。
	if !strings.Contains(p, "本人") {
		t.Error("🔴BUG-20260704：提取 prompt 未强调只记录使用者本人信息")
	}
	if !strings.Contains(p, "其他人") && !strings.Contains(p, "第三方") {
		t.Error("🔴BUG-20260704：提取 prompt 未指示对话中谈到的其他人不要归属给使用者本人")
	}
	// 必须给出第三方人物的主语标注约定，与 memory.PersonSubjectPrefix 对齐（收集层据此隔离）。
	if !strings.Contains(p, memory.PersonSubjectPrefix) {
		t.Errorf("🔴BUG-20260704：提取 prompt 未给出第三方人物主语标注约定 %q（收集层无法确定性隔离）", memory.PersonSubjectPrefix)
	}
}

func TestBug20260704_ProfileSynthPrompt_IsolatesThirdPartyPerson(t *testing.T) {
	p := profileSynthSystemPrompt
	if !strings.Contains(p, "本人") {
		t.Error("🔴BUG-20260704：蒸馏 prompt 未强调只描述使用者本人")
	}
	if !strings.Contains(p, "第三方") && !strings.Contains(p, "其他人") {
		t.Error("🔴BUG-20260704：蒸馏 prompt 未指示忽略事实中的第三方具名人物（把被谈论的人蒸馏成使用者的直接根因）")
	}
}

// 真实 LLM 行为验证：喂**无主语标记的裸第三方人物事实**（模拟存量脏数据 / 提取器漏标）
// + 使用者本人事实，蒸馏输出必须不含第三方人物特质、且含使用者本人特征。
// 数据为虚构占位（张三/李四），与任何真实个人无关。
func TestBug20260704_ProfileSynth_RealLLM_IgnoresThirdParty(t *testing.T) {
	if memEvalEnv("HEXCLAW_REAL_LLM_EVAL", "") != "1" {
		t.Skip("set HEXCLAW_REAL_LLM_EVAL=1 to run the real-LLM profile isolation test (spends tokens)")
	}
	eng := newRealLLMEngineForMemory(t)
	syn := NewProfileSynthesizer(eng)

	facts := []string{
		// 无主语标记的裸第三方人物简介（库里存量脏数据的形态）。
		"张三是某公司的联合创始人，也是本项目文档的贡献者",
		"李四自称夜猫子，偏爱手冲咖啡与爵士乐",
		// 使用者本人事实。
		"用户偏好简洁的代码风格",
		"用户的项目使用 Vue 3 与 TypeScript",
		"用户辅导作业时指定 homework-tutor-primary 技能并限定小学范围",
	}
	out, err := syn.Synthesize(context.Background(), facts, "")
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	t.Logf("画像输出：%s", out)
	for _, leaked := range []string{"张三", "李四", "联合创始人", "夜猫子", "咖啡", "爵士"} {
		if strings.Contains(out, leaked) {
			t.Errorf("🔴BUG-20260704：画像把第三方人物特质 %q 安到了使用者头上：%s", leaked, out)
		}
	}
	if !strings.Contains(out, "Vue") && !strings.Contains(out, "简洁") && !strings.Contains(out, "小学") {
		t.Errorf("画像应保留使用者本人特征（Vue/简洁/辅导小学），实得：%s", out)
	}
}
