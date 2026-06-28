package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeConsolidator 是确定性的注入 LLM（零网络）。返回固定 reply / err，记录调用次数与 prompt。
type fakeConsolidator struct {
	reply   string
	err     error
	calls   int
	prompts []string
}

func (f *fakeConsolidator) Complete(_ context.Context, prompt string) (string, error) {
	f.calls++
	f.prompts = append(f.prompts, prompt)
	return f.reply, f.err
}

// 折叠友好调参：仅靠「常驻保证带」保护，关掉高召回/新近保护，便于断言核心折叠路径。
func foldableOpts() DreamOptions {
	return DreamOptions{
		ClusterSimThreshold: 0.35,
		MinClusterSize:      2,
		MaxClustersPerRun:   4,
		MaxClusterSize:      8,
		ProtectRecallCount:  1000, // 实际关闭高召回保护
		ProtectRecency:      0,    // 关闭新近保护
	}
}

func countWithValidTo(entries []MemoryEntry) int {
	n := 0
	for _, e := range entries {
		if e.ValidTo != "" {
			n++
		}
	}
	return n
}

// 深相命脉：一簇同主语的相关记忆 → LLM 合成一条整合条目；原条 supersede 留史（标 ValidTo，不硬删），
// 活跃集（当前有效）收缩。
func TestDeepReflect_ConsolidatesCluster(t *testing.T) {
	fm := newFM(t)
	const summary = "用户的界面偏好：深色主题、紧凑布局、较大字号"
	fake := &fakeConsolidator{reply: summary}
	fm.WithConsolidator(fake).WithDreamOptions(foldableOpts())

	for _, c := range []string{"用户喜欢深色主题", "用户喜欢紧凑布局", "用户喜欢较大字号"} {
		if err := fm.SaveStructuredEntry(c, "fact", "manual", "", EntryMeta{Subject: "界面偏好"}); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now()
	rep, err := fm.DeepReflectRole(context.Background(), "", now)
	if err != nil {
		t.Fatalf("DeepReflectRole: %v", err)
	}
	if rep.Consolidated != 1 || rep.Folded != 3 || rep.Synthesized != 1 {
		t.Fatalf("应整合 1 簇 / 折叠 3 条 / 合成 1 条，得 %+v", rep)
	}
	if fake.calls != 1 {
		t.Fatalf("LLM 应被调用 1 次，得 %d", fake.calls)
	}
	if len(fake.prompts) == 0 || !strings.Contains(fake.prompts[0], "深色主题") {
		t.Fatalf("prompt 应携带原始记忆素材，得 %q", fake.prompts)
	}

	all := fm.ParseEntries()
	if len(all) != 4 { // 留史：3 条原条（失效）+ 1 条整合（有效），绝不硬删
		t.Fatalf("留史：应剩 4 条（3 史 + 1 整合），得 %d: %v", len(all), contents(all))
	}
	if n := countWithValidTo(all); n != 3 {
		t.Fatalf("3 条原条应标 ValidTo 留史，得 %d", n)
	}
	valid := currentlyValid(all, now)
	if len(valid) != 1 { // 活跃集收缩 3 → 1
		t.Fatalf("活跃集应收缩为 1，得 %d: %v", len(valid), contents(valid))
	}
	if valid[0].Content != summary {
		t.Fatalf("整合条正文应为 LLM 合成结果，得 %q", valid[0].Content)
	}
	if valid[0].Supersedes == "" {
		t.Fatalf("整合条应带 Supersedes（留史链锚点），得 %+v", valid[0])
	}
	if valid[0].Subject != "界面偏好" {
		t.Fatalf("整合条应继承共同主语，得 %q", valid[0].Subject)
	}
}

// 聚类路径②：无主语但内容相似（中间带，非近重复）→ 经相似度边聚为一簇并整合。
func TestDeepReflect_SimilarityClustering(t *testing.T) {
	fm := newFM(t)
	fake := &fakeConsolidator{reply: "用户喜欢喝各类咖啡（美式/拿铁/卡布奇诺）"}
	fm.WithConsolidator(fake).WithDreamOptions(foldableOpts())

	for _, c := range []string{"用户喜欢喝美式咖啡", "用户喜欢喝拿铁咖啡", "用户喜欢喝卡布奇诺咖啡"} {
		if err := fm.SaveStructuredEntry(c, "fact", "manual", "", EntryMeta{}); err != nil { // 无 Subject
			t.Fatal(err)
		}
	}

	now := time.Now()
	rep, err := fm.DeepReflectRole(context.Background(), "", now)
	if err != nil {
		t.Fatalf("DeepReflectRole: %v", err)
	}
	if rep.Consolidated != 1 || rep.Folded != 3 {
		t.Fatalf("相似簇应整合 1 簇 / 折叠 3 条，得 %+v", rep)
	}
	if v := currentlyValid(fm.ParseEntries(), now); len(v) != 1 {
		t.Fatalf("活跃集应收缩为 1，得 %v", contents(v))
	}
}

// 权重：高召回（RecallCount/HitCount ≥ 阈值）的条目受保护、不折叠（保留原文、仍有效），
// 仅低召回兄弟被合成取代。
func TestDeepReflect_ProtectsHighRecall(t *testing.T) {
	fm := newFM(t)
	fake := &fakeConsolidator{reply: "整合的低召回偏好"}
	opts := foldableOpts()
	opts.ProtectRecallCount = 3 // 开启高召回保护
	fm.WithConsolidator(fake).WithDreamOptions(opts)

	// 高召回条（hc=5 ≥ 3）应被保护；两条低召回（hc=0）被折叠。
	if err := fm.SaveStructuredEntry("用户极常用深色主题", "fact", "manual", "", EntryMeta{Subject: "界面偏好", HitCount: 5}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"用户偶尔用紧凑布局", "用户偶尔用较大字号"} {
		if err := fm.SaveStructuredEntry(c, "fact", "manual", "", EntryMeta{Subject: "界面偏好"}); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now()
	rep, err := fm.DeepReflectRole(context.Background(), "", now)
	if err != nil {
		t.Fatalf("DeepReflectRole: %v", err)
	}
	if rep.Folded != 2 || rep.Consolidated != 1 {
		t.Fatalf("应仅折叠 2 条低召回、保护 1 条高召回，得 %+v", rep)
	}

	valid := currentlyValid(fm.ParseEntries(), now)
	if len(valid) != 2 { // 高召回（保留）+ 整合条 = 2；活跃集 3 → 2
		t.Fatalf("活跃集应为 2（高召回 + 整合），得 %v", contents(valid))
	}
	hot := findByContent(valid, "极常用深色主题")
	if hot == nil || hot.ValidTo != "" || hot.HitCount != 5 {
		t.Fatalf("高召回条应原文保留、仍有效、HitCount 持久化，得 %+v", hot)
	}
}

// 权重：常驻保证带（resident 类型 / Pinned）永不被深相折叠（镜像 recall.SelectResident 入选范围）。
func TestDeepReflect_ProtectsResidentBand(t *testing.T) {
	fm := newFM(t)
	fake := &fakeConsolidator{reply: "整合的饮食偏好"}
	fm.WithConsolidator(fake).WithDreamOptions(foldableOpts())

	// resident 类型（preference）+ Pinned 条：均应被保护、不进簇。
	if err := fm.SaveStructuredEntry("用户偏好极简风格", "preference", "manual", "", EntryMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := fm.SaveStructuredEntry("务必周五前交付", "fact", "manual", "", EntryMeta{Pinned: true}); err != nil {
		t.Fatal(err)
	}
	// 两条检索层 fact（同主语）→ 可折叠。
	for _, c := range []string{"用户爱吃辣", "用户爱吃川菜"} {
		if err := fm.SaveStructuredEntry(c, "fact", "manual", "", EntryMeta{Subject: "饮食"}); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now()
	rep, err := fm.DeepReflectRole(context.Background(), "", now)
	if err != nil {
		t.Fatalf("DeepReflectRole: %v", err)
	}
	if rep.Folded != 2 || rep.Consolidated != 1 {
		t.Fatalf("应仅折叠 2 条检索层 fact、保护常驻带，得 %+v", rep)
	}
	all := fm.ParseEntries()
	if pref := findByContent(all, "极简风格"); pref == nil || pref.ValidTo != "" {
		t.Fatalf("preference（常驻类型）应原文保留、仍有效，得 %+v", pref)
	}
	if pin := findByContent(all, "周五前交付"); pin == nil || pin.ValidTo != "" {
		t.Fatalf("Pinned 条应原文保留、仍有效，得 %+v", pin)
	}
}

// 权重：很新（recency 高）的记忆默认受保护、不折叠（深相保守，避免折叠仍在演化的新事实）。
// 同一组数据：now=创建时刻 → 全受保护、0 折叠；now=远期（已陈旧）→ 折叠发生。
func TestDeepReflect_ProtectsRecentByDefault(t *testing.T) {
	fm := newFM(t)
	fake := &fakeConsolidator{reply: "整合结果"}
	fm.WithConsolidator(fake) // 默认调参：ProtectRecency=0.9

	for _, c := range []string{"用户喜欢深色主题", "用户喜欢紧凑布局", "用户喜欢较大字号"} {
		if err := fm.SaveStructuredEntry(c, "fact", "manual", "", EntryMeta{Subject: "界面偏好"}); err != nil {
			t.Fatal(err)
		}
	}

	// 刚创建即"很新" → 默认保护，0 折叠。
	repFresh, err := fm.DeepReflectRole(context.Background(), "", time.Now())
	if err != nil {
		t.Fatalf("DeepReflectRole(fresh): %v", err)
	}
	if repFresh.Consolidated != 0 || repFresh.Folded != 0 {
		t.Fatalf("很新记忆应默认受保护、0 折叠，得 %+v", repFresh)
	}

	// 远期视角（recency 已衰减）→ 解除新近保护，折叠发生。
	future := time.Now().Add(60 * 24 * time.Hour)
	repStale, err := fm.DeepReflectRole(context.Background(), "", future)
	if err != nil {
		t.Fatalf("DeepReflectRole(stale): %v", err)
	}
	if repStale.Consolidated != 1 || repStale.Folded != 3 {
		t.Fatalf("陈旧后应整合 1 簇 / 折叠 3 条，得 %+v", repStale)
	}
}

// 安全降级：未注入 ConsolidateLLM → 深相为 no-op（零 report、nil err、记忆零改动）。
func TestDeepReflect_NilLLMNoOp(t *testing.T) {
	fm := newFM(t) // 不注入 consolidator
	fm.WithDreamOptions(foldableOpts())
	for _, c := range []string{"用户喜欢深色主题", "用户喜欢紧凑布局", "用户喜欢较大字号"} {
		_ = fm.SaveStructuredEntry(c, "fact", "manual", "", EntryMeta{Subject: "界面偏好"})
	}
	before := contents(fm.ParseEntries())

	now := time.Now()
	if rep, err := fm.DeepReflectAll(context.Background(), now); err != nil || rep.Total() != 0 {
		t.Fatalf("nil LLM 深相应 no-op，得 rep=%+v err=%v", rep, err)
	}
	if rep, err := fm.DeepReflectRole(context.Background(), "", now); err != nil || rep.Total() != 0 {
		t.Fatalf("nil LLM DeepReflectRole 应 no-op，得 rep=%+v err=%v", rep, err)
	}
	if after := contents(fm.ParseEntries()); len(after) != len(before) {
		t.Fatalf("nil LLM 不应改动记忆：before=%v after=%v", before, after)
	}
}

// 鲁棒：LLM 报错 → 跳过该簇（记日志、不静默丢、不折叠、不损坏文件），记忆保持原状。
func TestDeepReflect_LLMErrorSkips(t *testing.T) {
	fm := newFM(t)
	fake := &fakeConsolidator{err: errors.New("boom")}
	fm.WithConsolidator(fake).WithDreamOptions(foldableOpts())
	for _, c := range []string{"用户喜欢深色主题", "用户喜欢紧凑布局"} {
		_ = fm.SaveStructuredEntry(c, "fact", "manual", "", EntryMeta{Subject: "界面偏好"})
	}

	now := time.Now()
	rep, err := fm.DeepReflectRole(context.Background(), "", now)
	if err != nil {
		t.Fatalf("LLM 错误不应使整轮失败（应跳过该簇），得 err=%v", err)
	}
	if rep.Skipped != 1 || rep.Consolidated != 0 || rep.Folded != 0 {
		t.Fatalf("应跳过 1 簇、零折叠，得 %+v", rep)
	}
	if v := currentlyValid(fm.ParseEntries(), now); len(v) != 2 {
		t.Fatalf("LLM 失败后记忆应保持原状（2 条有效），得 %v", contents(v))
	}
}

// 留史复用 + 两相合成：深相折叠（标 ValidTo 留史）→ 机械轻相把失效原条归档（复用既有 staleHistory 路径）。
func TestDeepThenLight_FoldedOriginalsArchived(t *testing.T) {
	fm := newFM(t)
	fake := &fakeConsolidator{reply: "整合的界面偏好"}
	fm.WithConsolidator(fake).WithDreamOptions(foldableOpts())
	for _, c := range []string{"用户喜欢深色主题", "用户喜欢紧凑布局", "用户喜欢较大字号"} {
		_ = fm.SaveStructuredEntry(c, "fact", "manual", "", EntryMeta{Subject: "界面偏好"})
	}

	now := time.Now()
	if _, err := fm.DeepReflectRole(context.Background(), "", now); err != nil {
		t.Fatalf("deep: %v", err)
	}
	if n := len(fm.ParseEntries()); n != 4 { // 3 史 + 1 整合
		t.Fatalf("深相后活跃文件应有 4 条（含留史），得 %d", n)
	}

	// 机械轻相：失效（ValidTo）原条走既有 staleHistory → 归档（留史到归档文件，非删）。
	rep, err := fm.ReflectRole("", now)
	if err != nil {
		t.Fatalf("light: %v", err)
	}
	if rep.Archived != 3 {
		t.Fatalf("3 条失效原条应被机械轻相归档，得 %+v", rep)
	}
	if n := len(fm.ParseEntries()); n != 1 {
		t.Fatalf("归档后活跃区应仅剩整合条，得 %d", n)
	}
	arch := globalArchive(t, fm)
	for _, c := range []string{"深色主题", "紧凑布局", "较大字号"} {
		if !strings.Contains(arch, c) {
			t.Fatalf("原条 %q 应留史到归档文件（复用 supersede-留史），归档=%q", c, arch)
		}
	}
}

// 不变量：机械轻相路径保持「只搬运/标记，绝不改写正文」——独立条正文逐字节不变；同主语异值仍机械 supersede。
// 同时验证：注入 consolidator 不改变机械路径行为（两相解耦）。
func TestDeepReflect_PreservesMechanicalInvariant(t *testing.T) {
	fm := newFM(t)
	fm.WithConsolidator(&fakeConsolidator{reply: "不应被机械路径使用"}) // 注入但只跑机械相
	_ = fm.SaveStructuredEntry("用户住在北京", "fact", "manual", "", EntryMeta{Subject: "居住地"})
	_ = fm.SaveStructuredEntry("用户住在上海", "fact", "manual", "", EntryMeta{Subject: "居住地"})
	const standalone = "数据库是 PostgreSQL"
	_ = fm.SaveStructuredEntry(standalone, "fact", "manual", "", EntryMeta{})

	now := time.Now()
	rep, err := fm.ReflectRole("", now)
	if err != nil {
		t.Fatalf("mechanical reflect: %v", err)
	}
	if rep.Superseded != 1 {
		t.Fatalf("机械相应仍按既有逻辑 supersede 1 条，得 %+v", rep)
	}
	// 核心不变量：机械相绝不改写正文（独立条逐字节不变）。
	if e := findByContent(fm.ParseEntries(), "PostgreSQL"); e == nil || e.Content != standalone {
		t.Fatalf("机械相不应改写正文，得 %+v", e)
	}
}

// StartDreaming 生命周期契约（确定性，不依赖墙钟/调度）：返回可用 stop；ctx 取消后退出；stop 幂等。
func TestStartDreaming_Lifecycle(t *testing.T) {
	fm := newFM(t).WithConsolidator(&fakeConsolidator{reply: "x"})
	ctx, cancel := context.WithCancel(context.Background())

	stop := fm.StartDreaming(ctx, time.Hour, 24*time.Hour) // 长间隔：测试期内不触发 tick
	if stop == nil {
		t.Fatal("StartDreaming 应返回非 nil stop")
	}
	cancel()
	stop()
	stop() // 幂等：再次调用不 panic

	stop2 := fm.StartDreaming(context.Background(), time.Millisecond, 2*time.Millisecond)
	stop2()
}
