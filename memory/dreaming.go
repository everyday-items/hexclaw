package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/memory/recall"
	"github.com/hexagon-codes/toolkit/util/logger"
)

// 深度整合（"做梦" / deep dreaming）阶段（对标 OpenClaw 的 deep/REM dreaming，按本仓约定改造）。
//
// 反思分两相，**叠加而非替换**：
//
//	① 轻相（既有 reflect.go，**严格机械化、零 LLM**）：去重 / 时序取代留史 / 晋升降级 / 归档。
//	   硬约束「只搬运/标记，绝不改写正文」不变（见 reflect.go）。
//	② 深相（本文件，**LLM 驱动**）：把一簇相关/冗余的活跃记忆经 LLM **合成**为一条整合条目，
//	   并用既有 supersede-留史写链取代原条（标 ValidTo 留史、不硬删）。深相是显式区别于机械相的
//	   「允许生成新内容」阶段。
//
// 可插拔 + 安全降级：未注入 ConsolidateLLM → 深相为 no-op（仅机械相运行）；opt-in、默认关，
// 与 StartReflection 同形态。provider 无关、LLM 经接口注入 → 确定性可测、测试零网络。
//
// 权重：以 recency + 召回频次（RecallCount/HitCount）选择整合对象——偏好大/冗余的簇；
// 保护「常驻保证带」（Pinned/常驻类型，镜像 recall.SelectResident 的入选逻辑）与高价值条目
// （高召回/很新者不折叠）。LLM 调用有界（簇大小上限 + 每轮簇数上限），失败记日志不静默丢。

// ConsolidateLLM 是深度整合阶段的可插拔 LLM（provider 无关，形态对齐生态其它注入点）。
//
// nil → 深相降级为 no-op。实现须把 prompt 送模型并返回合成正文；出错返回 err（调用方记日志跳过该簇）。
type ConsolidateLLM interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// DreamOptions 深度整合调参（保守默认；深相低频稀疏）。
type DreamOptions struct {
	ClusterSimThreshold float64 // 内容相似度聚类阈值（中间带：相关但非近重复）；<=0 → 0.35
	MinClusterSize      int     // 一簇可折叠成员数下限（< 此值不整合）；<=0 → 2
	MaxClustersPerRun   int     // 每轮最多整合的簇数（界 LLM 调用）；<=0 → 4
	MaxClusterSize      int     // 单簇最多送入 LLM 的成员数（界 prompt/调用）；<=0 → 8
	ProtectRecallCount  int     // RecallCount ≥ 此值的高召回条目受保护、不折叠；<=0 → 3
	ProtectRecency      float64 // Recency ≥ 此值的"很新"条目受保护、不折叠；<=0 → 关闭新近保护
	LambdaPerDay        float64 // recency 衰减率；<=0 → recall.DefaultLambdaPerDay
}

// DefaultDreamOptions 返回保守默认（深相低频、稀疏、宁缺毋滥）。
// ProtectRecency=0.9 ≈ 创建 ~4.5 天内的"很新"记忆不折叠（可能仍在演化）。
func DefaultDreamOptions() DreamOptions {
	return DreamOptions{
		ClusterSimThreshold: 0.35,
		MinClusterSize:      2,
		MaxClustersPerRun:   4,
		MaxClusterSize:      8,
		ProtectRecallCount:  3,
		ProtectRecency:      0.9,
		LambdaPerDay:        recall.DefaultLambdaPerDay,
	}
}

func (o DreamOptions) clusterSim() float64 {
	if o.ClusterSimThreshold <= 0 {
		return 0.35
	}
	return o.ClusterSimThreshold
}

func (o DreamOptions) minClusterSize() int {
	if o.MinClusterSize <= 0 {
		return 2
	}
	return o.MinClusterSize
}

func (o DreamOptions) maxClustersPerRun() int {
	if o.MaxClustersPerRun <= 0 {
		return 4
	}
	return o.MaxClustersPerRun
}

func (o DreamOptions) maxClusterSize() int {
	if o.MaxClusterSize <= 0 {
		return 8
	}
	return o.MaxClusterSize
}

func (o DreamOptions) protectRecall() int {
	if o.ProtectRecallCount <= 0 {
		return 3
	}
	return o.ProtectRecallCount
}

func (o DreamOptions) lambda() float64 {
	if o.LambdaPerDay <= 0 {
		return recall.DefaultLambdaPerDay
	}
	return o.LambdaPerDay
}

// DreamReport 一次深度整合的落地统计（供日志/可观测）。
type DreamReport struct {
	Clusters     int // 识别出的可整合簇数（折叠成员 ≥ MinClusterSize）
	Consolidated int // 实际整合落地的簇数
	Folded       int // 被折叠（supersede 留史）的原始条目数
	Synthesized  int // 新生成的整合条目数（== Consolidated）
	Skipped      int // 因 LLM 失败/空结果跳过的簇数
}

// Total 返回本次实际落地的整合簇数。
func (r DreamReport) Total() int { return r.Consolidated }

func (r *DreamReport) add(o DreamReport) {
	r.Clusters += o.Clusters
	r.Consolidated += o.Consolidated
	r.Folded += o.Folded
	r.Synthesized += o.Synthesized
	r.Skipped += o.Skipped
}

// WithConsolidator 注入深度整合 LLM（builder 形态，opt-in）。
// 未注入 → 深相 no-op（graceful degrade，仅机械反思运行）。返回 fm 便于链式调用。
func (fm *FileMemory) WithConsolidator(llm ConsolidateLLM) *FileMemory {
	fm.mu.Lock()
	fm.consolidator = llm
	fm.mu.Unlock()
	return fm
}

// WithDreamOptions 覆盖深度整合调参（不调用 → DefaultDreamOptions）。
func (fm *FileMemory) WithDreamOptions(opts DreamOptions) *FileMemory {
	fm.mu.Lock()
	o := opts
	fm.dreamOpts = &o
	fm.mu.Unlock()
	return fm
}

// effectiveDreamOpts 解析生效调参（调用方已持锁）。
func (fm *FileMemory) effectiveDreamOpts() DreamOptions {
	if fm.dreamOpts != nil {
		return *fm.dreamOpts
	}
	return DefaultDreamOptions()
}

// DeepReflectRole 对单角色视图做一次 LLM 驱动的深度整合（线程安全，自持写锁）。
// role="" → _global。未注入 ConsolidateLLM → no-op（返回零 report、nil err，graceful degrade）。
func (fm *FileMemory) DeepReflectRole(ctx context.Context, role string, now time.Time) (DreamReport, error) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if fm.consolidator == nil {
		return DreamReport{}, nil
	}
	return fm.deepReflectDirUnlocked(ctx, fm.roleDir(role), now, fm.effectiveDreamOpts())
}

// DeepReflectAll 深度整合 _global 与全部角色目录。任一目录出错即返回，已落地不回滚。
// 未注入 LLM → 整体 no-op（不遍历角色）。
func (fm *FileMemory) DeepReflectAll(ctx context.Context, now time.Time) (DreamReport, error) {
	var total DreamReport
	fm.mu.RLock()
	has := fm.consolidator != nil
	fm.mu.RUnlock()
	if !has {
		return total, nil
	}

	r, err := fm.DeepReflectRole(ctx, "", now)
	total.add(r)
	if err != nil {
		return total, err
	}
	for _, role := range fm.listRoles() {
		r, err := fm.DeepReflectRole(ctx, role, now)
		total.add(r)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// dreamMember 是一条进入聚类的活跃候选记忆。
type dreamMember struct {
	me   MemoryEntry
	re   recall.Entry
	line int
}

// dreamPlan 是一簇的整合计划（LLM 已算出合成正文，待单遍落盘）。
type dreamPlan struct {
	foldLines   []int  // 待折叠（标 ValidTo 留史）的原始条目行号
	synthesized string // LLM 合成正文
	subject     string // 继承的共同主语（成员主语不一致则空）
	memType     string // 继承的类型（同为检索层 fact/context）
	source      string // 来源标记（reflection，标识机械/做梦产物可溯源）
	primaryID   string // 整合条目 Supersedes 指向的主原始条目 ID（留史链锚点）
}

// deepReflectDirUnlocked 对单个目录的活跃记忆做 LLM 深度整合（调用方已持写锁、且 consolidator 非 nil）。
//
// 与 reflectDirUnlocked 同构（目录范围、按行号操作、单遍落盘防行号漂移），但调用注入的 LLM 合成新正文，
// 并经既有 supersede-留史写链取代原条（标 ValidTo、不硬删；新条带 Supersedes/ValidFrom）。
func (fm *FileMemory) deepReflectDirUnlocked(ctx context.Context, dir string, now time.Time, opts DreamOptions) (DreamReport, error) {
	var rep DreamReport
	activePath := filepath.Join(dir, memoryActiveFile)
	raw, err := os.ReadFile(activePath)
	if err != nil {
		if os.IsNotExist(err) {
			return rep, nil
		}
		return rep, err
	}
	rawStr := string(raw)

	memEntries := fm.parseEntriesFromFileUnlocked(dir, memoryActiveFile, MemoryStatusActive, "m")
	if len(memEntries) == 0 {
		return rep, nil
	}

	// 候选：当前有效（G3）+ 非常驻（保护「常驻保证带」：Pinned/rule/identity/instruction/preference 永不折叠）。
	var cand []dreamMember
	for _, me := range memEntries {
		re := toRecallEntry(me)
		if !recall.IsCurrentlyValid(re, now) {
			continue
		}
		if re.IsResident() { // 常驻保证带：镜像 recall.SelectResident 的入选范围，深相不碰
			continue
		}
		cand = append(cand, dreamMember{me: me, re: re, line: me.lineIdx})
	}
	if len(cand) < opts.minClusterSize() {
		return rep, nil
	}

	// 聚类（并查集）：同非空 Subject 或内容相似度 ≥ 阈值 → 同簇。确定性（按行号有序遍历）。
	parent := make([]int, len(cand))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra == rb {
			return
		}
		if ra < rb {
			parent[rb] = ra
		} else {
			parent[ra] = rb
		}
	}
	thr := opts.clusterSim()
	for i := 0; i < len(cand); i++ {
		for j := i + 1; j < len(cand); j++ {
			si := strings.TrimSpace(cand[i].re.Subject)
			sj := strings.TrimSpace(cand[j].re.Subject)
			if si != "" && strings.EqualFold(si, sj) {
				union(i, j)
				continue
			}
			if recall.LexicalSim(cand[i].me.Content, cand[j].me.Content) >= thr {
				union(i, j)
			}
		}
	}
	groups := map[int][]int{}
	for i := range cand {
		r := find(i)
		groups[r] = append(groups[r], i)
	}

	// 权重保护：高召回（RecallCount≥阈值）或很新（Recency≥阈值）者不折叠，保留原文。
	isProtected := func(m dreamMember) bool {
		if m.re.RecallCount >= opts.protectRecall() {
			return true
		}
		if pr := opts.ProtectRecency; pr > 0 && recall.Recency(m.re, now, opts.lambda()) >= pr {
			return true
		}
		return false
	}

	// 收簇：仅保留可折叠成员数 ≥ MinClusterSize 的簇；按簇大小降序（偏好大/冗余簇）、稳定 tiebreak 最小行号。
	type cluster struct{ foldable []int }
	var clusters []cluster
	for _, idxs := range groups {
		sort.Ints(idxs)
		var fold []int
		for _, i := range idxs {
			if isProtected(cand[i]) {
				continue
			}
			fold = append(fold, i)
		}
		if len(fold) < opts.minClusterSize() {
			continue
		}
		clusters = append(clusters, cluster{foldable: fold})
		rep.Clusters++
	}
	if len(clusters) == 0 {
		return rep, nil
	}
	sort.SliceStable(clusters, func(a, b int) bool {
		if len(clusters[a].foldable) != len(clusters[b].foldable) {
			return len(clusters[a].foldable) > len(clusters[b].foldable)
		}
		return clusters[a].foldable[0] < clusters[b].foldable[0]
	})

	// 计划阶段：LLM 调用为只读，先全部算清再单遍落盘（避免逐簇写盘导致的行号 ID 漂移）。
	var plans []dreamPlan
	maxClusters := opts.maxClustersPerRun()
	maxSize := opts.maxClusterSize()
	for _, cl := range clusters {
		if len(plans) >= maxClusters {
			break
		}
		fold := cl.foldable
		if len(fold) > maxSize { // 界 LLM prompt：按行序取前 N
			fold = fold[:maxSize]
		}
		contents := make([]string, 0, len(fold))
		for _, i := range fold {
			contents = append(contents, cand[i].me.Content)
		}
		// 共同主语：成员主语一致则继承，否则空。
		subj := strings.TrimSpace(cand[fold[0]].re.Subject)
		for _, i := range fold[1:] {
			if !strings.EqualFold(strings.TrimSpace(cand[i].re.Subject), subj) {
				subj = ""
				break
			}
		}

		out, err := fm.consolidator.Complete(ctx, buildConsolidationPrompt(subj, contents))
		if err != nil {
			logger.Warn("[memory.dream] 整合 LLM 调用失败，跳过该簇", "error", err, "size", len(fold))
			rep.Skipped++
			continue
		}
		out = strings.TrimSpace(out)
		if out == "" || !isUsableSynthesis(out) { // 修缺陷D：拒答/胡话/碎片不折叠（否则好原条被标失效、替换成垃圾且不可恢复）
			logger.Warn("[memory.dream] 整合 LLM 返回空/拒答，跳过该簇", "size", len(fold))
			rep.Skipped++
			continue
		}

		lines := make([]int, len(fold))
		for k, i := range fold {
			lines[k] = cand[i].line
		}
		plans = append(plans, dreamPlan{
			foldLines:   lines,
			synthesized: out,
			subject:     subj,
			memType:     cand[fold[0]].me.Type, // 同为检索层；继承首成员类型
			source:      string(recall.SourceReflection),
			primaryID:   cand[fold[0]].me.ID, // 修缺陷H：取首折叠条的稳定 ID 作 Supersedes 溯源（行号会随后续追加/折叠漂移）
		})
	}
	if len(plans) == 0 {
		return rep, nil
	}

	// 事务化落盘（修缺陷E）：旧实现非事务两段——先一次性给所有原条标 ValidTo，再逐条 append 整合条；
	// 中途崩溃/出错 = 原条已失效、替代条未写 → 永久丢事实且无回滚。改为在内存里把「标记 + 追加整合条」
	// 算成最终全文，**单次原子写**（temp+rename）= all-or-nothing：要么全标记+全整合、要么原文不动。
	vt := now.UTC().Format(metaTimeFormat)
	fileLines := strings.Split(rawStr, "\n")
	// ① 内存里给所有待折叠原条标 ValidTo 留史（行内改写、行数不变 → 行号稳定）。
	for _, p := range plans {
		for _, ln := range p.foldLines {
			if ln < 0 || ln >= len(fileLines) {
				continue
			}
			_, be := findBlockByStartLine(rawStr, ln)
			mi := metaLineIdx(fileLines, ln, be) // 多行折叠条把 ValidTo 落最后正文行（=parse 读取处），不污染首行
			fileLines[mi] = rewriteFirstLineMeta(fileLines[mi], func(m EntryMeta) EntryMeta {
				m.ValidTo = vt
				return m
			})
		}
	}
	// ② 内存里追加整合新条（与 appendEntryUnlocked 同格式：空行分隔 + `- [时] [type:source] body`）。
	ts := now.Format("15:04")
	for _, p := range plans {
		meta := EntryMeta{Subject: p.subject, Supersedes: p.primaryID, ValidFrom: vt}
		if !strings.Contains(p.synthesized, "\n") { // 修缺陷H：合成条目铸稳定 ID（仅单行，多行不挂内联 meta）
			meta.ID = newStableID()
		}
		body := p.synthesized
		if tag := meta.serialize(); tag != "" {
			body = p.synthesized + " " + tag
		}
		fileLines = append(fileLines, "", fmt.Sprintf("- [%s] [%s:%s] %s", ts, p.memType, p.source, body))
	}
	// 单次原子写：标记与整合同生共死，杜绝部分落盘的「失效但无替代」中间态。
	if err := atomicWriteFile(activePath, []byte(strings.Join(fileLines, "\n")), 0644); err != nil {
		return rep, err
	}
	for _, p := range plans {
		rep.Consolidated++
		rep.Synthesized++
		rep.Folded += len(p.foldLines)
	}
	return rep, nil
}

// buildConsolidationPrompt 构造整合 prompt（provider 无关、强约束不臆造、只输出正文）。
func buildConsolidationPrompt(subject string, contents []string) string {
	var sb strings.Builder
	sb.WriteString("你是记忆整合助手。请把下列关于同一话题的零散记忆合并为**一条**简洁、连贯的长期记忆。\n")
	sb.WriteString("要求：保留全部关键信息、不丢事实、不臆造未提及的内容；只输出合并后的记忆正文，不要解释、不要列表标记。\n")
	if subject != "" {
		sb.WriteString("话题：" + subject + "\n")
	}
	sb.WriteString("\n零散记忆：\n")
	for _, c := range contents {
		sb.WriteString("- " + c + "\n")
	}
	return sb.String()
}

// StartDreaming 启动两相反思后台循环（opt-in、默认关，对齐 StartReflection 形态）。
//
//	lightInterval → 每 tick 跑机械轻相（ReflectAll，零 LLM）。
//	deepInterval  → 每 tick 跑 LLM 深相（DeepReflectAll）；未注入 ConsolidateLLM 时深相 no-op。
//
// 不在启动时立即跑（避免开机即改用户记忆）。深相默认稀疏（建议 deepInterval ≫ lightInterval）。
// 返回 stop 用于优雅关闭；与 StartReflection 一样经写锁串行落盘、与在线写入互斥。
func (fm *FileMemory) StartDreaming(ctx context.Context, lightInterval, deepInterval time.Duration) func() {
	// 两相各自墙钟持久化 + 开机补跑（scheduled_phase.go）：轻相(每天)/深相(每周) 关机→重启不再清零。
	stopLight := fm.StartScheduledPhase(ctx, PhaseReflect, lightInterval, 24*time.Hour, func(_ context.Context, _ time.Time) error {
		rep, err := fm.ReflectAll(nowFunc())
		if err != nil {
			logger.Warn("[memory.dream] 机械轻相失败", "error", err)
			return err
		}
		if rep.Total() > 0 {
			logger.Info("[memory.dream] 机械轻相完成",
				"scanned", rep.Scanned, "superseded", rep.Superseded, "deduped", rep.Deduped,
				"promoted", rep.Promoted, "demoted", rep.Demoted, "archived", rep.Archived)
		}
		return nil
	})
	stopDeep := fm.StartScheduledPhase(ctx, PhaseDream, deepInterval, 7*24*time.Hour, func(runCtx context.Context, _ time.Time) error {
		rep, err := fm.DeepReflectAll(runCtx, nowFunc())
		if err != nil {
			logger.Warn("[memory.dream] LLM 深相失败", "error", err)
			return err
		}
		if rep.Total() > 0 {
			logger.Info("[memory.dream] LLM 深相完成",
				"clusters", rep.Clusters, "consolidated", rep.Consolidated,
				"folded", rep.Folded, "skipped", rep.Skipped)
		}
		return nil
	})
	return func() { stopLight(); stopDeep() }
}
