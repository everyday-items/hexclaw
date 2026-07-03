package memory

// BUG-20260704 用户画像"主语隔离缺失"：周期画像蒸馏把**被谈论的第三方具名人物**
// （用户手动存入/对话谈及的他人简介、人设）当成**软件使用者本人**的特质，蒸馏进
// "用户画像"。
//
// 根因（两层）：
//   ① 提取器把对话中第三方人物的身份误归给"用户"（prompt 层，engine 包测）。
//   ② collectProfileInputs 不加区分地收集全部 identity/preference/fact/context，
//      蒸馏器假设记忆库里所有描述性事实都在描述"同一个用户" —— 无主语隔离。
//
// 本文件钉死缺陷②的确定性防线：**主语归属为第三方具名人物（Subject 以
// PersonSubjectPrefix 打头）的事实，绝不进画像蒸馏输入**。用 recordingSyn 桩断言
// collectProfileInputs 实际喂给 synthesizer 的 facts 列表，稳定复现、不依赖真实 LLM。
//
// 注：以下测试数据全为虚构占位（张三/李四/老王等通用测试人名），仅为演示"第三方
// 人物主语被隔离"这一机制，与任何真实个人无关。

import (
	"context"
	"strings"
	"testing"
	"time"
)

// saveFactWithSubject 存一条带主语归属的结构化事实（复刻提取器 [主语] 前缀落库路径）。
func saveFactWithSubject(t *testing.T, fm *FileMemory, content, memType, subject string) {
	t.Helper()
	if err := fm.SaveStructuredEntry(content, memType, "manual", "", EntryMeta{Subject: subject}); err != nil {
		t.Fatalf("save %q: %v", content, err)
	}
}

func TestBug20260704_ProfileExcludesThirdPartyPersonFacts(t *testing.T) {
	fm := newFM(t)

	// 第三方具名人物事实（主语归属为具体人名，非当前使用者）—— 修复后不得进画像。
	saveFactWithSubject(t, fm, "张三是某公司的联合创始人，兼产品文档贡献者", "fact", PersonSubjectPrefix+"张三")
	saveFactWithSubject(t, fm, "李四自称夜猫子，偏爱手冲咖啡与爵士乐", "fact", PersonSubjectPrefix+"李四")

	// 当前使用者本人的真实事实 —— 必须进画像。
	mustSaveProfileFact(t, fm, "用户偏好简洁代码风格", "preference")
	mustSaveProfileFact(t, fm, "用户的项目使用 Vue 3 与 TypeScript", "fact")
	mustSaveProfileFact(t, fm, "用户辅导作业指定 homework-tutor-primary 技能限定小学范围", "preference")

	syn := &recordingSyn{out: "用户偏好简洁代码风格；项目使用 Vue 3 与 TypeScript"}
	if _, err := fm.DistillProfileForRole(context.Background(), "", syn, DistillProfileConfig{MinFacts: 2}, time.Now()); err != nil {
		t.Fatalf("distill: %v", err)
	}
	if syn.calls != 1 {
		t.Fatalf("前置：synthesizer 应被调用一次（证据足够），实际 %d 次", syn.calls)
	}

	joined := strings.Join(syn.facts, " || ")
	// 第三方人物特质绝不能作为使用者画像素材喂进蒸馏器。
	for _, leaked := range []string{"张三", "联合创始人", "李四", "夜猫子", "咖啡", "爵士"} {
		if strings.Contains(joined, leaked) {
			t.Errorf("🔴BUG-20260704：第三方人物特质 %q 被喂进画像蒸馏输入——collectProfileInputs 未按主语归属隔离第三方人物（把被谈论的人蒸馏成使用者）。facts=%v", leaked, syn.facts)
		}
	}
	// 使用者本人事实必须保留（隔离不能误伤真实画像素材）。
	for _, keep := range []string{"简洁代码", "Vue 3", "homework-tutor-primary"} {
		if !strings.Contains(joined, keep) {
			t.Errorf("🔴BUG-20260704：使用者本人事实 %q 被隔离误伤（应保留进画像）。facts=%v", keep, syn.facts)
		}
	}
}

// 单人物主语的最小契约：只有一条第三方人物事实 + 足量用户事实时，人物事实不进画像。
func TestBug20260704_CollectProfileInputs_SkipsPersonSubject(t *testing.T) {
	fm := newFM(t)
	saveFactWithSubject(t, fm, "老王是隔壁团队的后端负责人", "fact", PersonSubjectPrefix+"老王")
	mustSaveProfileFact(t, fm, "用户是 Go 后端工程师", "identity")
	mustSaveProfileFact(t, fm, "用户住在杭州", "context")
	mustSaveProfileFact(t, fm, "用户偏好简洁回答", "preference")

	facts, _ := fm.collectProfileInputs("", time.Now())
	for _, f := range facts {
		if strings.Contains(f, "老王") {
			t.Errorf("🔴BUG-20260704：第三方人物（Subject=%q主语归属）进了画像输入 facts=%v", PersonSubjectPrefix+"老王", facts)
		}
	}
	if len(facts) != 3 {
		t.Errorf("应只收 3 条使用者本人事实（人物条被隔离），实得 %d 条：%v", len(facts), facts)
	}
}
