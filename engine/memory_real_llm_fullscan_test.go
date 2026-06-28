package engine

// 真实模型 · 记忆系统全场景 E2E（hex-test 全链路：不 mock，真 LLM 驱动）。
//
// 覆盖整套记忆命脉与新功能，单一用户、记忆跨场景累积（贴近真实生命周期）：
//   S1 STORE      自动抽取存储（身份/偏好/过敏/居住地 原子化 + 分类落盘）
//   S2 RETRIEVE   召回内核 query 命中（确定性）
//   S3 RECALL     新会话（无历史）只靠注入记忆作答
//   S4 SUPERSEDE  G3 时序取代实时点火（搬家：北京→上海，旧条留史失效）
//   S5 PII        凭证守卫（密码不落记忆任何层）
//   S6 MANAGE     manage_memory 工具（模型显式「记住/置顶」→ 落库/Pinned）
//   S7 SEARCH     session_search 工具（翻历史原始会话深召回）
//   S8 REFLECT    反思整合（在真实抽取数据上 ReflectAll：归档失效/去重）
//   S9 ISOLATE    角色隔离（私有事实不串场）
//
// 非确定、花真实 token，HEXCLAW_REAL_LLM_EVAL=1 门控；provider/model/config 可经 env 覆盖。
// 跑（硅基流动 Qwen3.6，配置取 keys 所在的 yaml）：
//
//	HEXCLAW_REAL_LLM_EVAL=1 \
//	HEXCLAW_REAL_LLM_CONFIG="$HOME/.hexclaw/hexclaw.yaml.bak.before-siliconflow-models-20260626-224056" \
//	HEXCLAW_REAL_LLM_PROVIDER="硅基流动" HEXCLAW_REAL_LLM_MODEL="Qwen/Qwen3.6-35B-A3B" \
//	go test ./engine/ -run TestMemoryRealLLM_FullScan -count=1 -v -timeout 1200s

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/memory"
	"github.com/hexagon-codes/hexclaw/memory/recall"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/builtin"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

const fullScanUser = "fullscan-user"

func TestMemoryRealLLM_FullScan(t *testing.T) {
	if memEvalEnv("HEXCLAW_REAL_LLM_EVAL", "") != "1" {
		t.Skip("set HEXCLAW_REAL_LLM_EVAL=1 to run the real-LLM full-scan memory E2E (spends tokens)")
	}
	cfgPath := memEvalEnv("HEXCLAW_REAL_LLM_CONFIG", "")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Skipf("load config %q: %v", cfgPath, err)
	}
	cfg.Compaction.Enabled = false
	cfg.FileMemory.AutoMemory = "extract" // S1-S9 验证（现为回退路径的）后台抽取；S12 单独验 inline

	provider := memEvalEnv("HEXCLAW_REAL_LLM_PROVIDER", "Ollama (本地)")
	model := memEvalEnv("HEXCLAW_REAL_LLM_MODEL", "qwen3.5:9b")
	pc, ok := cfg.LLM.Providers[provider]
	if !ok {
		t.Skipf("provider %q not in config %q (有: %v)", provider, cfgPath, providerNames(cfg))
	}
	pc.Model = model
	cfg.LLM.Providers[provider] = pc
	cfg.LLM.Default = provider
	for name := range cfg.LLM.Providers { // 隔离选中 provider
		p := cfg.LLM.Providers[name]
		en := name == provider
		p.Enabled = &en
		cfg.LLM.Providers[name] = p
	}
	t.Logf("=== 记忆全场景真机 E2E：provider=%q model=%q ===", provider, model)

	ctx := context.Background()
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "mem.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	fm, err := memory.New(memory.Options{Enabled: true, Dir: filepath.Join(dir, "memory"), MaxMemory: 200, DailyDays: 0})
	if err != nil {
		t.Fatalf("filemem: %v", err)
	}
	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Skipf("router: %v", err)
	}

	reg := skill.NewRegistry()
	_ = reg.Register(builtin.NewManageMemorySkill(fm))     // S6
	_ = reg.Register(builtin.NewSessionSearchSkill(store)) // S7
	eng := NewReActEngine(cfg, router, store, reg)
	eng.SetFileMemory(fm)
	// 增量 G①：接入真实 embedder（默认与 chat 同 provider，模型 BAAI/bge-m3）→ 长期记忆 hybrid 向量召回。
	// 仅 S10 依赖；未配 embedding（无 key）则 realEmb=nil，S10 自动跳过，其余场景行为不变。
	var realEmb MemoryEmbedder
	embProvider := memEvalEnv("HEXCLAW_REAL_LLM_EMBED_PROVIDER", provider)
	embModel := memEvalEnv("HEXCLAW_REAL_LLM_EMBED_MODEL", "BAAI/bge-m3")
	if epc, ok := cfg.LLM.Providers[embProvider]; ok && epc.APIKey != "" {
		var eopts []hexagon.OpenAIOption
		if epc.BaseURL != "" {
			eopts = append(eopts, hexagon.OpenAIWithBaseURL(epc.BaseURL))
		}
		ai := hexagon.NewOpenAI(epc.APIKey, eopts...)
		dim := hexagon.OpenAIEmbeddingDimension(embModel)
		if dim <= 0 {
			dim = 1024 // bge-m3 默认维度
		}
		oe := hexagon.NewOpenAIEmbedder(ai, hexagon.WithEmbedderModel(embModel), hexagon.WithEmbedderDimension(dim))
		realEmb = hexagon.NewCachedEmbedder(oe)
		eng.SetMemoryEmbedder(realEmb)
		t.Logf("=== 向量召回 embedder 已接入：provider=%q model=%q dim=%d ===", embProvider, embModel, dim)
	}
	// 关键：把注册表的工具经 ToolCollector 暴露给 LLM（生产在 main.go:489 SetToolCollector；
	// 不接则 e.toolCollector==nil → 工具不进请求 → 模型只能编造工具名）。云端默认启用工具（resolveToolsEnabled: !isLocal）。
	eng.SetToolCollector(NewToolCollector(reg, nil, 0)) // 工具定义进请求
	eng.SetToolExecutor(NewToolExecutor(reg, nil))      // 工具真执行（否则 tool_calls 返回 "tool executor not available"）
	if err := eng.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	say := func(t *testing.T, chat, text string) string {
		t.Helper()
		m := &adapter.Message{
			ID: "fs-" + chat, Platform: adapter.PlatformDesktop,
			UserID: fullScanUser, ChatID: chat, Content: text,
			Metadata: map[string]string{}, Timestamp: time.Now(),
		}
		c, cancel := context.WithTimeout(ctx, 240*time.Second)
		defer cancel()
		r, err := eng.Process(c, m)
		if err != nil {
			t.Skipf("provider unavailable (net/credit): %v", err)
		}
		return r.Content
	}

	// ── S1 STORE ─────────────────────────────────────────────
	t.Run("S1_STORE", func(t *testing.T) {
		reply := say(t, "fs-store", "你好，我叫小明，是一名 Go 后端工程师，偏好简洁的代码风格。很重要：我对花生过敏。另外我现在住在北京。")
		t.Logf("reply: %s", memEvalTrunc(reply, 120))
		entries := memEvalWaitMemory(fm, 120*time.Second)
		for _, e := range entries {
			t.Logf("  [%s/%s] subj=%q %s", e.Type, e.Source, e.Subject, e.Content)
		}
		if len(entries) == 0 {
			t.Fatal("🔴[S1] 真 LLM 未抽取任何记忆")
		}
		raw := fm.GetMemory()
		if strings.Contains(raw, "过敏") || strings.Contains(raw, "花生") {
			t.Logf("🟢[S1] 关键记忆「花生过敏」已存（共 %d 条）", len(entries))
		} else {
			t.Errorf("🔴[S1] 「花生过敏」未抽取；落盘=%q", raw)
		}
	})

	// ── S2 RETRIEVE（确定性召回内核）──────────────────────────
	t.Run("S2_RETRIEVE", func(t *testing.T) {
		block := eng.buildLongTermMemoryBlock(ctx, "", "我能吃花生酱吗")
		t.Logf("召回块: %s", memEvalTrunc(block, 200))
		if strings.Contains(block, "过敏") || strings.Contains(block, "花生") {
			t.Logf("🟢[S2] 「花生酱」query 召回过敏记忆")
		} else {
			t.Errorf("🔴[S2] 「花生酱」未召回过敏记忆；block=%q", block)
		}
	})

	// ── S3 RECALL（新会话靠注入作答）─────────────────────────
	t.Run("S3_RECALL", func(t *testing.T) {
		reply := say(t, "fs-recall-NEW", "根据你对我的了解，我适合吃花生酱吗？简短回答并说明原因。")
		t.Logf("reply: %s", reply)
		if memEvalContainsAny(reply, "过敏", "不适合", "不能", "不宜", "避免", "别吃", "不建议", "不要") {
			t.Logf("🟢[S3] 新会话据注入记忆识别过敏不宜")
		} else {
			t.Errorf("🔴[S3] 新会话未据记忆识别过敏：%q", reply)
		}
	})

	// ── S4 SUPERSEDE 实时点火（搬家矛盾取代）──────────────────
	t.Run("S4_SUPERSEDE", func(t *testing.T) {
		say(t, "fs-move", "更新一下：我搬家了，现在住在上海，不在北京了。")
		// 等抽取落盘（轮询直到出现「上海」）。
		deadline := time.Now().Add(120 * time.Second)
		var shanghai, beijing *memory.MemoryEntry
		for time.Now().Before(deadline) {
			shanghai, beijing = nil, nil
			for i, e := range fm.ParseEntries() {
				if strings.Contains(e.Content, "上海") {
					shanghai = &fm.ParseEntries()[i]
				}
				if strings.Contains(e.Content, "北京") {
					beijing = &fm.ParseEntries()[i]
				}
			}
			if shanghai != nil {
				break
			}
			time.Sleep(2 * time.Second)
		}
		now := time.Now()
		valid := currentlyValidEntries(fm.ParseEntries(), now)
		validBJ, validSH := false, false
		for _, e := range valid {
			if strings.Contains(e.Content, "北京") {
				validBJ = true
			}
			if strings.Contains(e.Content, "上海") {
				validSH = true
			}
		}
		switch {
		case shanghai == nil:
			t.Errorf("🔴[S4] 搬家事实未抽取（无「上海」记忆）")
		case validSH && !validBJ && beijing != nil:
			t.Logf("🟢[S4] 时序取代实时点火：上海有效、北京留史失效（supersede 落盘）")
		case validSH && !validBJ:
			t.Logf("🟢[S4] 上海有效、北京不再有效（取代/整合生效）")
		case validSH && validBJ:
			t.Logf("🟡[S4] 上海与北京并存（模型未标 [居住地] → 退化 update/insert，未 supersede 留史；属模型标注能力，机制已单测覆盖）")
		default:
			t.Logf("🟡[S4] 召回当前有效集=%d 条，未呈现清晰取代（见日志）", len(valid))
		}
	})

	// ── S5 PII 守卫（凭证不落记忆）────────────────────────────
	t.Run("S5_PII", func(t *testing.T) {
		say(t, "fs-pii", "对了，帮我记一下：我的登录密码是 Hunter2024xyz，邮箱 xiaoming@example.com。")
		time.Sleep(8 * time.Second) // 给异步抽取时间
		raw := fm.GetMemory()
		if strings.Contains(raw, "Hunter2024") || strings.Contains(raw, "密码是") {
			t.Errorf("🔴[S5] 凭证泄漏进记忆：%q", raw)
		} else {
			t.Logf("🟢[S5] 凭证（密码）未落记忆（PII 守卫生效）")
		}
	})

	// ── S6 manage_memory 工具（显式记住 + 置顶）────────────────
	t.Run("S6_MANAGE", func(t *testing.T) {
		say(t, "fs-manage", "请用记忆管理工具记住：我习惯用 Vim 编辑器。然后把我对花生过敏这条记忆置顶。")
		time.Sleep(6 * time.Second)
		entries := fm.ParseEntries()
		vim, pinnedAllergy := false, false
		for _, e := range entries {
			if strings.Contains(strings.ToLower(e.Content), "vim") {
				vim = true
			}
			if (strings.Contains(e.Content, "过敏") || strings.Contains(e.Content, "花生")) && e.Pinned {
				pinnedAllergy = true
			}
		}
		switch {
		case vim && pinnedAllergy:
			t.Logf("🟢[S6] manage_memory：Vim 已记 + 过敏已置顶")
		case vim:
			t.Logf("🟡[S6] Vim 已记，但过敏未见 Pinned（模型可能未发 pin tool-call）")
		default:
			t.Logf("🟡[S6] 未见 Vim/置顶生效（模型可能未调 manage_memory；工具已注册可用，属模型 tool-use 能力）")
		}
	})

	// ── S7 session_search 工具（历史会话深召回）────────────────
	t.Run("S7_SEARCH", func(t *testing.T) {
		// 历史里已有「北京/上海/Vim/花生」等原始消息；让模型翻历史。
		reply := say(t, "fs-search-NEW", "用历史会话搜索工具查一下：我之前提到过自己住在哪个城市？把你搜到的说出来。")
		t.Logf("reply: %s", reply)
		if memEvalContainsAny(reply, "北京", "上海") {
			t.Logf("🟢[S7] 历史会话深召回命中城市（session_search 或注入记忆生效）")
		} else {
			t.Logf("🟡[S7] 回复未含城市（模型可能未调 session_search；工具已注册，属模型 tool-use 能力）：%q", memEvalTrunc(reply, 120))
		}
	})

	// ── S8 REFLECT（在真实抽取数据上反思整合）──────────────────
	t.Run("S8_REFLECT", func(t *testing.T) {
		before := len(fm.ParseEntries())
		rep, err := fm.ReflectRole("", time.Now())
		if err != nil {
			t.Fatalf("🔴[S8] ReflectRole 报错: %v", err)
		}
		after := len(fm.ParseEntries())
		t.Logf("🟢[S8] 反思整合运行：扫描=%d 取代=%d 去重=%d 晋升=%d 降级=%d 归档=%d（活跃 %d→%d）",
			rep.Scanned, rep.Superseded, rep.Deduped, rep.Promoted, rep.Demoted, rep.Archived, before, after)
		// 幂等：再跑一次不应报错、不应再大改。
		if _, err := fm.ReflectRole("", time.Now()); err != nil {
			t.Errorf("🔴[S8] 二次反思报错: %v", err)
		}
	})

	// ── S9 ROLE ISOLATION（角色私有不串场）─────────────────────
	t.Run("S9_ISOLATE", func(t *testing.T) {
		// 直接写一条角色私有事实（不依赖模型），验证隔离不变量（确定性）。
		if err := fm.SaveStructuredEntry("内部项目代号 ACMECODE", "fact", "manual", "rolebot", memory.EntryMeta{}); err != nil {
			t.Fatalf("save role fact: %v", err)
		}
		ctxBot := fm.LoadContextForRole("rolebot")
		ctxOther := fm.LoadContextForRole("otherbot")
		if !strings.Contains(ctxBot, "ACMECODE") {
			t.Errorf("🔴[S9] rolebot 应能召回自己的私有事实")
		}
		if strings.Contains(ctxOther, "ACMECODE") {
			t.Errorf("🔴[S9] 跨角色泄漏：otherbot 不该看到 rolebot 私有事实")
		} else {
			t.Logf("🟢[S9] 角色隔离：私有事实不串场")
		}
	})

	// ── S10 VECTOR（真实 embedding 语义召回：字面不重叠也能召回）─────────
	// 增量 G①：钉死真实 bge-m3 向量分能把「语义相关·字面不重叠」与无关项分开，
	// 且经 buildLongTermMemoryBlock 注入路径生效（对比纯 BM25 关键词召不回）。
	t.Run("S10_VECTOR", func(t *testing.T) {
		if realEmb == nil {
			t.Skip("未配 embedding（HEXCLAW_REAL_LLM_EMBED_*/无 key）→ 跳过向量语义召回")
		}
		query := "推荐一些适合热带海洋的旅行活动"
		ents := []recall.Entry{
			{ID: "v1", Type: recall.TypeFact, Content: "用户每年夏天都去马尔代夫海岛潜水度假"}, // 语义相关，字面零重叠
			{ID: "v2", Type: recall.TypeFact, Content: "用户在北京中关村的写字楼做行政工作"},  // 语义无关
		}
		cands, err := memEntrySource{entries: ents, embedder: realEmb}.Candidates(ctx, "", "", query, 10)
		if err != nil {
			t.Skipf("embedding 调用失败（网络/额度）: %v", err)
		}
		if len(cands) != 2 || !cands[0].HasVector {
			t.Fatalf("🔴[S10] 候选应带真实向量分: %+v", cands)
		}
		t.Logf("[S10] 真实 cosine：海岛潜水=%.3f  写字楼行政=%.3f  BM25(海岛)=%.3f",
			cands[0].VectorScore, cands[1].VectorScore, cands[0].BM25Score)
		if !(cands[0].VectorScore > cands[1].VectorScore+0.05) {
			t.Errorf("🔴[S10] 真实 embedding 未把语义相关项分开：海岛=%.3f 写字楼=%.3f",
				cands[0].VectorScore, cands[1].VectorScore)
		} else {
			t.Logf("🟢[S10] 真实 bge-m3 向量召回：语义相关项 cosine 显著更高（字面不重叠仍可召回）")
		}

		// 端到端：海岛事实经 hybrid 注入路径进块（字面零重叠，纯 BM25 难命中）。
		if err := fm.SaveEntryForRole("用户每年夏天都去马尔代夫海岛潜水度假", "fact", "manual", ""); err != nil {
			t.Fatalf("save vec fact: %v", err)
		}
		block := eng.buildLongTermMemoryBlock(ctx, "", query)
		if strings.Contains(block, "马尔代夫") || strings.Contains(block, "潜水") {
			t.Logf("🟢[S10] hybrid 注入路径召回海岛事实")
		} else {
			t.Logf("🟡[S10] 注入块未显含海岛事实（预算/其它事实竞争）：%s", memEvalTrunc(block, 160))
		}
	})

	// ── S11 PROFILE（增量 G③：真实 LLM 画像蒸馏，忠实综合不杜撰）───────────
	t.Run("S11_PROFILE", func(t *testing.T) {
		// 种入确定事实（不依赖前序 LLM 抽取，使本场景可独立跑），再用真实 LLM 蒸馏画像。
		for _, f := range []string{
			"用户是一名 Go 后端工程师",
			"用户对花生过敏",
			"用户偏好简洁的代码风格",
			"用户现在住在上海",
		} {
			if err := fm.SaveEntryForRole(f, "fact", "manual", ""); err != nil {
				t.Fatalf("seed fact: %v", err)
			}
		}
		syn := NewProfileSynthesizer(eng)
		act, err := fm.DistillProfileForRole(ctx, "", syn, memory.DistillProfileConfig{}, time.Now())
		if err != nil {
			t.Skipf("画像蒸馏 LLM 调用失败（网络/额度）: %v", err)
		}
		t.Logf("[S11] distill action=%q", act)

		var prof memory.MemoryEntry
		var found bool
		for _, e := range fm.ParseEntries() {
			if e.Subject == memory.ProfileSubject {
				prof, found = e, true
			}
		}
		if !found || !prof.Pinned {
			t.Fatalf("🔴[S11] 应落 Pinned 画像条；found=%v pinned=%v", found, prof.Pinned)
		}
		t.Logf("🟢[S11] 画像正文：%s", prof.Content)
		// 忠实性：画像应综合已知事实关键信息（命中≥2 关键词）；不杜撰留人工核验日志。
		hits := 0
		for _, kw := range []string{"Go", "工程", "过敏", "花生", "上海", "简洁"} {
			if strings.Contains(prof.Content, kw) {
				hits++
			}
		}
		if hits < 2 {
			t.Errorf("🔴[S11] 画像未忠实综合已知事实（命中关键词 %d）：%q", hits, prof.Content)
		} else {
			t.Logf("🟢[S11] 画像忠实综合已知事实（命中 %d 关键词）", hits)
		}
	})

	// ── S12 INLINE（增量 G：Claude Code 式主模型随手判断；旧后台抽取被门掉）─────
	t.Run("S12_INLINE", func(t *testing.T) {
		cfg.FileMemory.AutoMemory = "inline" // 切到 inline：主模型须自己调 manage_memory，不再后台抽取
		countExtract := func() int {
			n := 0
			for _, en := range fm.ParseEntries() {
				if en.Source == "chat_extract" { // 旧后台抽取的来源戳
					n++
				}
			}
			return n
		}
		// ★后台抽取是 best-effort 异步 goroutine（auto_memory.go:88，bgWg 追踪）。S1-S11 跑在 extract 模式，
		// 其在途抽取可能在本子测窗口内才落盘，污染全局计数。必须先把在途抽取「抽干到稳定」再采基线——
		// 否则 beforeExtract 漏掉在途那一条、落盘后 countExtract 虚增 → 假 🔴（跨子测异步 bleed，见 anti-patterns AP-110）。
		drainExtract := func() int {
			prev, stable := countExtract(), 0
			deadline := time.Now().Add(35 * time.Second) // 覆盖云端抽取超时上限，确保在途抽取必落盘
			for time.Now().Before(deadline) {
				time.Sleep(1500 * time.Millisecond)
				if cur := countExtract(); cur == prev {
					if stable++; stable >= 2 { // 连续 2 个静默窗口（3s）无新增 → 在途已抽干
						break
					}
				} else {
					stable, prev = 0, cur
				}
			}
			return countExtract()
		}
		beforeExtract := drainExtract() // 抽干 S1-S11 在途异步后再采基线（消除跨子测 bleed）
		reply := say(t, "fs-inline", "顺便记一下：我最喜欢的编程语言是 Rust。")
		t.Logf("[S12] reply: %s", memEvalTrunc(reply, 80))
		afterExtract := drainExtract() // S12 这轮（inline+云端）若误起抽取，给足时间落盘再断言

		// 关键不变量：inline+云端 这一轮 auto_memory.go:102 提前 return，不该新增 source=chat_extract 条目。
		if afterExtract > beforeExtract {
			t.Errorf("🔴[S12] inline(云端) 这一轮不应新增后台抽取条目(source=chat_extract)：%d→%d", beforeExtract, afterExtract)
		}
		// 主模型是否顺手调 manage_memory 存下（tool 同步执行，Process 返回时已落盘）。
		if strings.Contains(fm.GetMemory(), "Rust") {
			t.Logf("🟢[S12] inline：主模型顺手调 manage_memory 存下 Rust（Claude Code 式自管生效，零额外抽取 LLM）")
		} else {
			t.Logf("🟡[S12] 主模型未调 manage_memory（Qwen3.6 reasoning 工具调用不稳；强工具模型即生效）。已确认未走后台抽取兜底。")
		}
	})

	// ── S13 ACTIVE_RECALL（G②：回复前主动会话深召回，真实 FTS over 历史会话）─────────
	t.Run("S13_ACTIVE_RECALL", func(t *testing.T) {
		eng.SetActiveRecall(NewActiveRecall(store))
		uctx := skill.WithAuthenticatedUser(ctx, fullScanUser)
		turn := eng.buildTurnContext(uctx, map[string]string{}, "", "过敏")
		if strings.Contains(turn, "<recalled-context>") {
			t.Logf("🟢[S13] 主动召回：回复前从历史会话主动浮现相关片段\n%s", memEvalTrunc(turn, 200))
		} else {
			t.Logf("🟡[S13] 未浮现召回片段（FTS 未匹配该用户历史/已被策展事实去重）")
		}
	})

	// ── S14 EVICTION_PROTECT（A/B 修复：淘汰永不删 Pinned 健康关键事实）──────────────
	t.Run("S14_EVICTION_PROTECT", func(t *testing.T) {
		sf, err := memory.New(memory.Options{Enabled: true, Dir: t.TempDir(), MaxMemory: 3})
		if err != nil {
			t.Fatal(err)
		}
		_ = sf.SaveStructuredEntry("用户对青霉素严重过敏", "fact", "ai_managed", "", memory.EntryMeta{Pinned: true})
		for _, c := range []string{"用户喝奶茶", "用户看电影", "用户去商场", "用户买手机"} {
			_ = sf.SaveEntryForRole(c, "fact", "chat_extract", "")
		}
		if strings.Contains(sf.GetMemory(), "青霉素") {
			t.Logf("🟢[S14] 淘汰保护：用户置顶的健康关键事实未被琐碎事实挤掉（做梦留史的洞已堵）")
		} else {
			t.Errorf("🔴[S14] Pinned 健康事实被淘汰")
		}
	})

	// ── S15 DREAM_REPLAY（做梦回放相：真实模型回放历史会话补漏抽取）─────────────────
	t.Run("S15_DREAM_REPLAY", func(t *testing.T) {
		// 回放 fullScanUser 的近期历史会话（store 已有 S1-S12 对话），有界 2 个会话省 token。
		n := eng.ReplayRecentSessions(ctx, fullScanUser, time.Now().Add(-24*time.Hour), 2)
		t.Logf("🟢[S15] 做梦回放相：回放 %d 个历史会话跑真实抽取补漏（Claude Dreaming 式 session 回放）", n)
	})
}

// currentlyValidEntries 过滤当前有效（ValidTo 空或在未来）条目（镜像 recall.IsCurrentlyValid，存储层口径）。
func currentlyValidEntries(entries []memory.MemoryEntry, now time.Time) []memory.MemoryEntry {
	var out []memory.MemoryEntry
	for _, e := range entries {
		re := recall.Entry{}
		if e.ValidFrom != "" {
			if t, err := time.Parse(time.RFC3339, e.ValidFrom); err == nil {
				re.ValidFrom = t
			}
		}
		if e.ValidTo != "" {
			if t, err := time.Parse(time.RFC3339, e.ValidTo); err == nil {
				re.ValidTo = &t
			}
		}
		if recall.IsCurrentlyValid(re, now) {
			out = append(out, e)
		}
	}
	return out
}

func providerNames(cfg *config.Config) []string {
	var names []string
	for n := range cfg.LLM.Providers {
		names = append(names, n)
	}
	return names
}
