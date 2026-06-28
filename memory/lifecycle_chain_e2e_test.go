package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestMemoryLifecycle_StringChain_E2E —— 交接任务 #2「把功能串起来」。
//
// 把记忆系统全生命周期当**一条用户旅程**，在【真文件 DB·非 :memory:】上端到端、多轮、含一次【重开 DB】地跑：
//
//	相①写入 → 相②召回(LoadContext + BM25 Search) → 相③轻相 reflect(零 LLM)
//	  → 相④深相 dream(fake 合成·留史) → 相⑤画像 distill(fake) → 相⑥淘汰保护 → 相⑦重开 DB(持久·有界)
//
// 专注【状态累积正确 + 跨相不变量】：每一相都不破坏其它相的产物；淘汰永不动 Pinned/rule/identity/画像；
// 留史(ValidTo)不硬删；重开后受保护条目仍在且活跃集有界（373MB 类状态累积不变量）。
// 深/画像两相的【合成质量】另由真机门 S11 / dreaming_real 覆盖；本测用确定性 fake，只验链路与状态。
func TestMemoryLifecycle_StringChain_E2E(t *testing.T) {
	dir := t.TempDir() // 真文件目录，跨「重启」复用
	const role = ""    // 全局角色

	open := func(maxMem int) *FileMemory {
		t.Helper()
		fm, err := New(Options{Enabled: true, Dir: dir, MaxMemory: maxMem})
		if err != nil {
			t.Fatalf("open FileMemory: %v", err)
		}
		return fm
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	// hasActive 判断某子串是否存在于【当前活跃】条目中（ParseEntriesForRole 只返回活跃集）。
	hasActive := func(fm *FileMemory, substr string) bool {
		for _, e := range fm.ParseEntriesForRole(role) {
			if strings.Contains(e.Content, substr) {
				return true
			}
		}
		return false
	}
	assertSurvive := func(fm *FileMemory, phase string, subs ...string) {
		t.Helper()
		for _, s := range subs {
			if !hasActive(fm, s) {
				t.Errorf("%s：受保护/关键条目「%s」不应丢失", phase, s)
			}
		}
	}

	// ── 相①写入：身份 + 置顶健康事实 + 硬规则 + 偏好 + 一簇同主语「项目技术栈」可折叠事实 ──
	fm := open(50)
	must(fm.SaveStructuredEntry("用户名叫小明", "identity", "manual", role, EntryMeta{Subject: "姓名"}))
	must(fm.SaveStructuredEntry("用户对青霉素过敏", "fact", "manual", role, EntryMeta{Subject: "过敏", Pinned: true}))
	must(fm.SaveStructuredEntry("回答务必使用中文", "rule", "manual", role, EntryMeta{}))
	must(fm.SaveEntryForRole("用户喜欢简洁的回答", "preference", "manual", role))
	// ★用【无主语·内容相似】簇做可折叠素材：同主语会被【相③轻相 reflect 的 G3 时序 supersede】先折叠掉
	// （仅留最新一条→深相无≥2 候选可合成）。无主语簇轻相不动（supersede 按主语），深相靠相似度边聚成簇再合成——
	// 这正是真实管线顺序（轻相高频在前、深相低频在后）下深相 synthesize-fold 的有效作用域。
	for _, c := range []string{"用户喜欢喝美式咖啡", "用户喜欢喝拿铁咖啡", "用户喜欢喝卡布奇诺咖啡"} {
		must(fm.SaveStructuredEntry(c, "fact", "manual", role, EntryMeta{}))
	}

	// ── 相②召回：LoadContext 含全部活跃事实；Search(BM25) 命中关键词 ──
	ctxStr := fm.LoadContextForRole(role)
	for _, want := range []string{"小明", "青霉素", "中文", "简洁", "美式", "拿铁", "卡布奇诺"} {
		if !strings.Contains(ctxStr, want) {
			t.Errorf("相②召回：LoadContext 缺 %q\n%s", want, ctxStr)
		}
	}
	if hits := fm.Search("过敏"); len(hits) == 0 {
		t.Error("相②召回：BM25 Search 关键词「过敏」零命中")
	}
	if n := len(fm.ParseEntriesForRole(role)); n != 7 {
		t.Fatalf("相①写入后活跃集应为 7（无淘汰、无 write-time supersede），实际 %d", n)
	}

	// ── 相③轻相 reflect（零 LLM 机械整理）：不破坏 Pinned/rule/identity，活跃集不丢健康事实 ──
	if _, err := fm.ReflectRole(role, time.Now()); err != nil {
		t.Fatalf("相③轻相 reflect: %v", err)
	}
	assertSurvive(fm, "相③轻相后", "青霉素", "小明", "中文")

	// ── 相④深相 dream（fake 合成）：3 条同主语「项目技术栈」→ 折叠成 1 条整合 + 原 3 条 supersede 留史 ──
	fake := &fakeConsolidator{reply: "用户喜欢喝各类咖啡（美式/拿铁/卡布奇诺）"}
	fm.WithConsolidator(fake).WithDreamOptions(foldableOpts())
	drep, err := fm.DeepReflectRole(context.Background(), role, time.Now())
	if err != nil {
		t.Fatalf("相④深相 dream: %v", err)
	}
	if drep.Total() < 1 || fake.calls < 1 {
		t.Fatalf("相④深相应至少折叠 1 簇且调一次合成 LLM，得 consolidated=%d calls=%d", drep.Total(), fake.calls)
	}
	if n := countWithValidTo(fm.ParseEntriesForRole(role)); n < 3 {
		t.Errorf("相④深相留史：被取代的 3 条原条应标 ValidTo（留史不硬删），得 %d", n)
	}
	if !strings.Contains(fm.LoadContextForRole(role), "咖啡") {
		t.Error("相④深相：整合条「咖啡」未进活跃召回")
	}
	assertSurvive(fm, "相④深相后", "青霉素", "小明", "中文") // 不同主语 + Pinned/rule/identity 绝不被折叠

	// ── 相⑤画像 distill（fake synth）：从累积有效事实蒸馏画像，落 Pinned 画像条 ──
	syn := &recordingSyn{out: "画像：小明，偏好简洁回答，爱喝各类咖啡，对青霉素过敏。"}
	act, err := fm.DistillProfileForRole(context.Background(), role, syn, DistillProfileConfig{MinFacts: 3}, time.Now())
	if err != nil {
		t.Fatalf("相⑤画像 distill: %v", err)
	}
	if act != "insert" && act != "update" {
		t.Fatalf("相⑤画像应写入(insert/update)，得 %q（有效事实数应 ≥3）", act)
	}
	if syn.calls < 1 {
		t.Error("相⑤画像：合成器未被调用")
	}
	if pe, ok := profileEntry(t, fm); !ok || !pe.Pinned {
		t.Fatalf("相⑤画像条应存在且 Pinned，得 ok=%v pinned=%v", ok, pe.Pinned)
	}

	// ── 相⑥淘汰保护：重开到小容量 MaxMemory=6，灌一批琐碎 fact 触发淘汰 ──
	// 不变量：Pinned(过敏/画像) / rule(中文) / identity(小明) 永不被挤掉；留史(失效)条优先被剪。
	fmSmall := open(6)
	for i := range 25 {
		_ = fmSmall.SaveEntryForRole(fmt.Sprintf("琐碎闲聊事实-%02d", i), "fact", "chat_extract", role)
	}
	assertSurvive(fmSmall, "相⑥淘汰后", "青霉素", "小明", "中文")
	if pe, ok := profileEntry(t, fmSmall); !ok || pe.Status == "archived" {
		t.Errorf("相⑥淘汰：Pinned 画像条不应被淘汰，ok=%v status=%q", ok, pe.Status)
	}

	// ── 相⑦重开 DB（状态累积·持久·有界）：所有受保护条目仍在，活跃集有界 ──
	fmReopen := open(6)
	assertSurvive(fmReopen, "相⑦重开后", "青霉素", "小明", "中文")
	if _, ok := profileEntry(t, fmReopen); !ok {
		t.Error("相⑦重开：Pinned 画像条应持久存在")
	}
	activeReopen := len(fmReopen.ParseEntriesForRole(role))
	// 有界：受保护 4 条(identity/allergy/rule/profile) + 容量内琐碎；宽松上界 MaxMemory + 受保护富余。
	if activeReopen > 6+8 {
		t.Errorf("相⑦重开：活跃集应有界(~MaxMemory+受保护)，却膨胀到 %d（状态累积无界=373MB 类回归）", activeReopen)
	}
	t.Logf("🟢 串链 E2E：write→recall→轻相→深相(留史)→画像→淘汰保护→重开 全链状态累积正确；重开活跃=%d(有界)", activeReopen)
}
