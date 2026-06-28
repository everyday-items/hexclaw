package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 增量 G③：USER.md 画像 LLM 蒸馏。这些测试钉死：
//   ①证据不足不蒸馏（防杜撰）②落 Pinned 画像条 ③再蒸馏原地更新不重复 ④画像不喂回自身
//   ⑤长度封顶 ⑥空合成跳过 ⑦绕过 dedup（稳定画像必相似，不可被 discard）⑧失败不破坏既有记忆。

// recordingSyn 记录收到的 facts/prev，返回预置 out/err（不调真 LLM）。
type recordingSyn struct {
	facts []string
	prev  string
	out   string
	err   error
	calls int
}

func (r *recordingSyn) Synthesize(_ context.Context, facts []string, prev string) (string, error) {
	r.calls++
	r.facts = append([]string(nil), facts...)
	r.prev = prev
	return r.out, r.err
}

func profileEntry(t *testing.T, fm *FileMemory) (MemoryEntry, bool) {
	t.Helper()
	for _, e := range fm.ParseEntries() {
		if e.Subject == ProfileSubject {
			return e, true
		}
	}
	return MemoryEntry{}, false
}

func now() time.Time { return time.Now() }

// ① 证据不足（< MinFacts）→ 不蒸馏、不写画像。
func TestDistillProfile_GatesOnMinFacts(t *testing.T) {
	fm := newFM(t)
	mustSaveProfileFact(t, fm, "用户是 Go 工程师", "identity")
	syn := &recordingSyn{out: "不该被调用"}
	act, err := fm.DistillProfileForRole(context.Background(), "", syn, DistillProfileConfig{MinFacts: 3}, now())
	if err != nil || act != "skip" {
		t.Fatalf("证据不足应 skip，得 act=%q err=%v", act, err)
	}
	if syn.calls != 0 {
		t.Fatalf("证据不足不应调 synthesizer，调了 %d 次", syn.calls)
	}
	if _, ok := profileEntry(t, fm); ok {
		t.Fatal("证据不足不应写画像条")
	}
}

// ② 足够事实 → 落 Pinned identity 画像条，正文来自 synthesizer。
func TestDistillProfile_WritesPinnedProfile(t *testing.T) {
	fm := newFM(t)
	mustSaveProfileFact(t, fm, "用户是 Go 后端工程师", "identity")
	mustSaveProfileFact(t, fm, "用户对花生过敏", "fact")
	mustSaveProfileFact(t, fm, "用户偏好简洁代码风格", "preference")

	syn := &recordingSyn{out: "Go 后端工程师；对花生过敏；偏好简洁代码风格"}
	act, err := fm.DistillProfileForRole(context.Background(), "", syn, DistillProfileConfig{}, now())
	if err != nil {
		t.Fatalf("蒸馏出错: %v", err)
	}
	if act != "insert" {
		t.Fatalf("首次应 insert，得 %q", act)
	}
	e, ok := profileEntry(t, fm)
	if !ok {
		t.Fatal("应写入画像条")
	}
	if !e.Pinned {
		t.Fatal("画像条应 Pinned（进常驻保证带）")
	}
	if e.Type != "identity" {
		t.Fatalf("画像条应为 identity，得 %q", e.Type)
	}
	if !strings.Contains(e.Content, "花生过敏") {
		t.Fatalf("画像正文应为 synthesizer 输出，得 %q", e.Content)
	}
	// synthesizer 收到的应是描述性事实（不含画像自身）。
	if len(syn.facts) != 3 {
		t.Fatalf("应喂入 3 条事实，得 %d: %v", len(syn.facts), syn.facts)
	}
}

// ③ 再蒸馏（事实变化）→ 原地更新同一画像条，不重复、仍 Pinned。
func TestDistillProfile_UpdatesInPlace(t *testing.T) {
	fm := newFM(t)
	mustSaveProfileFact(t, fm, "用户是 Go 工程师", "identity")
	mustSaveProfileFact(t, fm, "用户住在北京", "fact")
	mustSaveProfileFact(t, fm, "用户偏好简洁代码", "preference")

	syn := &recordingSyn{out: "Go 工程师；住在北京"}
	if act, _ := fm.DistillProfileForRole(context.Background(), "", syn, DistillProfileConfig{}, now()); act != "insert" {
		t.Fatalf("首次应 insert，得 %q", act)
	}

	// 新增「搬到上海」事实，重新蒸馏。
	mustSaveProfileFact(t, fm, "用户搬到了上海", "fact")
	syn.out = "Go 工程师；已搬到上海（曾住北京）"
	act, err := fm.DistillProfileForRole(context.Background(), "", syn, DistillProfileConfig{}, now())
	if err != nil {
		t.Fatalf("二次蒸馏出错: %v", err)
	}
	if act != "update" {
		t.Fatalf("二次应原地 update，得 %q", act)
	}

	// 画像条唯一（未重复），正文已更新，仍 Pinned。
	var count int
	var prof MemoryEntry
	for _, e := range fm.ParseEntries() {
		if e.Subject == ProfileSubject {
			count++
			prof = e
		}
	}
	if count != 1 {
		t.Fatalf("画像条应唯一（原地更新），得 %d 条", count)
	}
	if !prof.Pinned {
		t.Fatal("更新后仍应 Pinned")
	}
	if !strings.Contains(prof.Content, "上海") {
		t.Fatalf("画像应已时序更新含上海，得 %q", prof.Content)
	}
	// 第二次 synthesizer 应收到上一版画像作 prev。
	if !strings.Contains(syn.prev, "住在北京") {
		t.Fatalf("二次蒸馏应把上版画像作 prev 传入，得 %q", syn.prev)
	}
	// prev/facts 都不含画像自身（防自我放大）：facts 应是 4 条事实，不含画像。
	for _, f := range syn.facts {
		if strings.Contains(f, "Go 工程师；") {
			t.Fatalf("facts 不应含画像自身: %v", syn.facts)
		}
	}
}

// ④ 画像超长 → 按 MaxRunes 封顶（防 LLM 跑飞）。
func TestDistillProfile_RespectsMaxRunes(t *testing.T) {
	fm := newFM(t)
	for _, c := range []string{"用户是工程师", "用户住在杭州", "用户喜欢登山"} {
		mustSaveProfileFact(t, fm, c, "fact")
	}
	syn := &recordingSyn{out: strings.Repeat("很长的画像", 200)} // 远超上限
	if _, err := fm.DistillProfileForRole(context.Background(), "", syn, DistillProfileConfig{MaxRunes: 50}, now()); err != nil {
		t.Fatalf("蒸馏出错: %v", err)
	}
	e, ok := profileEntry(t, fm)
	if !ok {
		t.Fatal("应写画像")
	}
	if n := len([]rune(e.Content)); n > 50 {
		t.Fatalf("画像应被封顶到 50 rune，实际 %d", n)
	}
}

// ⑤ 合成为空 → 跳过、不写空画像。
func TestDistillProfile_SkipsEmptySynthesis(t *testing.T) {
	fm := newFM(t)
	for _, c := range []string{"用户是工程师", "用户住在杭州", "用户喜欢登山"} {
		mustSaveProfileFact(t, fm, c, "fact")
	}
	syn := &recordingSyn{out: "   "}
	act, _ := fm.DistillProfileForRole(context.Background(), "", syn, DistillProfileConfig{}, now())
	if act != "skip" {
		t.Fatalf("空合成应 skip，得 %q", act)
	}
	if _, ok := profileEntry(t, fm); ok {
		t.Fatal("空合成不应写画像")
	}
}

// ⑥ synthesizer 出错 → skip + 返回 err，既有记忆不受损。
func TestDistillProfile_SynthErrorPreservesMemory(t *testing.T) {
	fm := newFM(t)
	for _, c := range []string{"用户是工程师", "用户住在杭州", "用户喜欢登山"} {
		mustSaveProfileFact(t, fm, c, "fact")
	}
	before := len(fm.ParseEntries())
	syn := &recordingSyn{err: context.DeadlineExceeded}
	act, err := fm.DistillProfileForRole(context.Background(), "", syn, DistillProfileConfig{}, now())
	if act != "skip" || err == nil {
		t.Fatalf("出错应 skip+err，得 act=%q err=%v", act, err)
	}
	if got := len(fm.ParseEntries()); got != before {
		t.Fatalf("出错不应改动既有记忆：%d → %d", before, got)
	}
}

// ⑦ 绕过 dedup：直接两次写近乎相同画像，仍更新（不被 discard）。
func TestUpsertProfile_BypassesDedup(t *testing.T) {
	fm := newFM(t)
	if act, _ := fm.UpsertProfileForRole("Go 工程师；住在北京", ""); act != "insert" {
		t.Fatalf("首次应 insert")
	}
	// 仅一字之差——若走 dedup 会被 discard；画像路径应原地 update。
	act, err := fm.UpsertProfileForRole("Go 工程师；现住在北京", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if act != "update" {
		t.Fatalf("近义画像应原地 update（绕过 dedup），得 %q", act)
	}
	e, _ := profileEntry(t, fm)
	if !strings.Contains(e.Content, "现住在北京") {
		t.Fatalf("画像应更新为新正文，得 %q", e.Content)
	}
}

// ⑧ DistillProfileAll：global 与角色各自蒸馏出独立画像（不串场）。
func TestDistillProfileAll_PerRole(t *testing.T) {
	fm := newFM(t)
	for _, c := range []string{"用户是工程师", "用户住在杭州", "用户喜欢登山"} {
		mustSaveProfileFact(t, fm, c, "fact")
	}
	// 角色私有事实（写到角色目录）。
	for _, c := range []string{"客服机器人语气要专业", "客服只回订单问题", "客服不闲聊"} {
		if err := fm.SaveEntryForRole(c, "context", "manual", "support"); err != nil {
			t.Fatalf("save role fact: %v", err)
		}
	}
	syn := &recordingSyn{out: "画像合成结果"}
	if err := fm.DistillProfileAll(context.Background(), syn, DistillProfileConfig{}, now()); err != nil {
		t.Fatalf("DistillProfileAll: %v", err)
	}
	// global 与 support 角色都应有画像（synthesizer 被调多次）。
	if syn.calls < 2 {
		t.Fatalf("global + 角色应各蒸馏一次，synthesizer 调用 %d 次", syn.calls)
	}
	if _, ok := profileEntry(t, fm); !ok {
		t.Fatal("global 应有画像")
	}
}

// ⑨ StartProfileDistillation：nil synthesizer → no-op stop；起停不 panic。
func TestStartProfileDistillation_NilAndStop(t *testing.T) {
	fm := newFM(t)
	stop := fm.StartProfileDistillation(context.Background(), 0, nil, DistillProfileConfig{})
	stop() // nil syn → no-op，可安全调用

	syn := &recordingSyn{out: "x"}
	stop2 := fm.StartProfileDistillation(context.Background(), time.Hour, syn, DistillProfileConfig{})
	stop2() // 不在启动时立即跑 → synthesizer 不应被调
	if syn.calls != 0 {
		t.Fatalf("StartProfileDistillation 不应在启动时立即蒸馏，却调了 %d 次", syn.calls)
	}
}

func mustSaveProfileFact(t *testing.T, fm *FileMemory, content, memType string) {
	t.Helper()
	if err := fm.SaveEntryForRole(content, memType, "manual", ""); err != nil {
		t.Fatalf("存事实失败: %v", err)
	}
}
