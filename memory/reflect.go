package memory

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/memory/recall"
	"github.com/hexagon-codes/toolkit/util/logger"
)

// 增量 B：反思运行时接线（方案 §4.4.2 / §4.7 R3 / §7-核心 修正2）。
//
// 把 recall.Reflect（**严格机械化、零 LLM**的整合算法）接到文件存储：在写锁内
// parse → Reflect → **原子 apply Op**（supersede 留史 / dedup 删 / promote-demote 改 tier / archive 移档）。
// apply 用**单遍重建**目标 MEMORY.md，从原始解析一次性算清「改 meta / 删行 / 移档」，避免逐 op 删行
// 导致的行号 ID 漂移。由 StartReflection 周期后台触发（默认关、opt-in，方案 §4.4.2「cron 0 3」）。
//
// 反思**只搬运/标记，绝不改写正文**（Op 类型层面无 Content 字段）—— 与 recall.Reflect 同一硬约束。

// ReflectReport 一次反思的落地统计（供日志/可观测）。
type ReflectReport struct {
	Scanned    int // 扫描的活跃条目数
	Superseded int // 时序取代（旧条标失效留史）
	Deduped    int // 近重复删并
	Promoted   int // 晋升常驻（Pinned）
	Demoted    int // 降级检索（preference→context）
	Archived   int // 归档（陈旧 / 失效历史）
}

// Total 返回本次实际落地的操作总数。
func (r ReflectReport) Total() int {
	return r.Superseded + r.Deduped + r.Promoted + r.Demoted + r.Archived
}

func (r *ReflectReport) add(o ReflectReport) {
	r.Scanned += o.Scanned
	r.Superseded += o.Superseded
	r.Deduped += o.Deduped
	r.Promoted += o.Promoted
	r.Demoted += o.Demoted
	r.Archived += o.Archived
}

// defaultReflectDedup 反思去重选项：CJK 字符二元组相似度，与单写器同口径。
func defaultReflectDedup() recall.DedupOptions {
	return recall.DedupOptions{SimFn: recall.LexicalSim, JaccardThreshold: dedupUpsertThreshold}
}

// ReflectRole 对单角色视图做一次机械化反思整合（线程安全，自持写锁）。
// role="" → 反思 _global 目录。确定性、可重复调用（幂等收敛）。
func (fm *FileMemory) ReflectRole(role string, now time.Time) (ReflectReport, error) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	return fm.reflectDirUnlocked(fm.roleDir(role), now)
}

// ReflectAll 反思 _global 与全部角色目录（后台触发器调用）。任一目录出错即返回，已落地的不回滚。
func (fm *FileMemory) ReflectAll(now time.Time) (ReflectReport, error) {
	var total ReflectReport
	r, err := fm.ReflectRole("", now)
	total.add(r)
	if err != nil {
		return total, err
	}
	for _, role := range fm.listRoles() {
		r, err := fm.ReflectRole(role, now)
		total.add(r)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// StartReflection 启动周期反思整合后台循环（方案 §4.4.2「cron 0 3」低频后台、默认关、opt-in）。
//
// 镜像 storage/sqlite.StartMaintenance 的形态：ticker 驱动、不阻塞启动、返回 stop 用于优雅关闭。
// **不在启动时立即跑**（避免每次开机改动用户记忆；反思是低频维护，非启动副作用）。
// 整套 apply 经单写器写锁串行落盘，与在线写入互斥、不并发冲突。
func (fm *FileMemory) StartReflection(ctx context.Context, interval time.Duration) func() {
	// 墙钟持久化 + 开机补跑（scheduled_phase.go）：关机→重启不再清零计时。
	return fm.StartScheduledPhase(ctx, PhaseReflect, interval, 24*time.Hour, func(_ context.Context, _ time.Time) error {
		rep, err := fm.ReflectAll(nowFunc())
		if err != nil {
			logger.Warn("[memory.reflect] 反思整合失败", "error", err)
			return err
		}
		if rep.Total() > 0 {
			logger.Info("[memory.reflect] 反思整合完成",
				"scanned", rep.Scanned, "superseded", rep.Superseded, "deduped", rep.Deduped,
				"promoted", rep.Promoted, "demoted", rep.Demoted, "archived", rep.Archived)
		}
		return nil
	})
}

// listRoles 枚举 fm.dir 下的角色子目录（排除 _global）。
func (fm *FileMemory) listRoles() []string {
	ents, err := os.ReadDir(fm.dir)
	if err != nil {
		return nil
	}
	var roles []string
	for _, e := range ents {
		if e.IsDir() && e.Name() != "_global" {
			roles = append(roles, e.Name())
		}
	}
	sort.Strings(roles)
	return roles
}

// lineAction 累积对某条目首行/整块的待落地动作（单遍重建用）。删除优先于改写。
type lineAction struct {
	metaMutators []func(EntryMeta) EntryMeta // 改内联 meta（ValidTo/Supersedes/Pinned）
	newType      string                      // 非空 → 改 type 标记（demote: preference→context）
	remove       bool                        // dedup 删并
	archive      bool                        // 移到归档文件（陈旧 / 失效历史，可恢复）
}

// reflectDirUnlocked 反思单个目录的活跃记忆（调用方已持写锁）。
func (fm *FileMemory) reflectDirUnlocked(dir string, now time.Time) (ReflectReport, error) {
	var rep ReflectReport
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
	rep.Scanned = len(memEntries)
	if rep.Scanned == 0 {
		return rep, nil
	}

	// 映射 → recall.Entry，并按当前有效性分桶：有效条目进 Reflect；失效历史（被 supersede）直接归档释放活跃区。
	byID := make(map[string]int, len(memEntries)) // recall.ID → 起始行号
	var valid []recall.Entry
	staleHistory := make(map[int]bool) // 行号 → 失效历史，待归档
	for _, me := range memEntries {
		re := toRecallEntry(me)
		line := me.lineIdx
		byID[re.ID] = line
		if recall.IsCurrentlyValid(re, now) {
			valid = append(valid, re)
		} else if !re.Pinned {
			staleHistory[line] = true
		}
	}

	ops := recall.Reflect(valid, now, recall.DefaultLifecyclePolicy(), defaultReflectDedup())

	actions := make(map[int]*lineAction)
	act := func(line int) *lineAction {
		a := actions[line]
		if a == nil {
			a = &lineAction{}
			actions[line] = a
		}
		return a
	}

	vt := now.UTC().Format(metaTimeFormat)
	for _, op := range ops {
		switch op.Action {
		case recall.OpSupersede:
			oldLine, ok := byID[op.TargetID]
			if !ok {
				continue
			}
			act(oldLine).metaMutators = append(act(oldLine).metaMutators, func(m EntryMeta) EntryMeta {
				m.ValidTo = vt
				return m
			})
			if freshLine, ok := byID[op.OtherID]; ok {
				tid := op.TargetID
				act(freshLine).metaMutators = append(act(freshLine).metaMutators, func(m EntryMeta) EntryMeta {
					m.Supersedes = tid
					return m
				})
			}
			rep.Superseded++
		case recall.OpDedup:
			if line, ok := byID[op.TargetID]; ok {
				act(line).remove = true
				rep.Deduped++
			}
		case recall.OpPromote:
			if line, ok := byID[op.TargetID]; ok {
				act(line).metaMutators = append(act(line).metaMutators, func(m EntryMeta) EntryMeta {
					m.Pinned = true
					return m
				})
				rep.Promoted++
			}
		case recall.OpDemote:
			if line, ok := byID[op.TargetID]; ok {
				// 降级：常驻 preference → 检索层 context（type 标记机械搬运，非内容改写）。
				act(line).newType = string(recall.TypeContext)
				rep.Demoted++
			}
		case recall.OpArchive:
			if line, ok := byID[op.TargetID]; ok {
				act(line).archive = true
				rep.Archived++
			}
		}
	}
	// 失效历史归档（已被 supersede 的旧条，留史到归档文件、释放活跃区）。
	for line := range staleHistory {
		act(line).archive = true
		rep.Archived++
	}

	if len(actions) == 0 {
		return rep, nil
	}

	// 单遍重建：从原始 blocks 一次性算清「改写 / 删除 / 归档」，行号在本遍内全程稳定。
	lines := strings.Split(rawStr, "\n")
	removed := make(map[int]bool)
	var archivedTexts []string
	for _, b := range splitEntryBlocks(rawStr) {
		a := actions[b.startLine]
		if a == nil {
			continue
		}
		if a.remove || a.archive {
			for i := b.startLine; i < b.endLine; i++ {
				removed[i] = true
			}
			if a.archive {
				archivedTexts = append(archivedTexts, extractLineRange(lines, b.startLine, b.endLine))
			}
			continue // 删除/归档优先，跳过 meta 改写
		}
		// 类型在首行的 [type:source] 头里 → 改首行；内联 meta 标签在块尾正文行 → 改最后正文行
		// （多行条目首行≠尾行；单行二者重合，与历史逐字节一致）。
		if a.newType != "" {
			lines[b.startLine] = rewriteFirstLineType(lines[b.startLine], a.newType)
		}
		if len(a.metaMutators) > 0 {
			mi := metaLineIdx(lines, b.startLine, b.endLine)
			lines[mi] = rewriteFirstLineMetaMulti(lines[mi], a.metaMutators)
		}
	}

	newActive := make([]string, 0, len(lines))
	for i, ln := range lines {
		if removed[i] {
			continue
		}
		newActive = append(newActive, ln)
	}

	// 先写归档（失败也只是少归档，不丢失），再写活跃区；若活跃区写失败，条目同存两处（dup>loss）。
	if len(archivedTexts) > 0 {
		archivePath := filepath.Join(dir, memoryArchiveFile)
		existing, _ := os.ReadFile(archivePath)
		var sb strings.Builder
		sb.Write(existing)
		for _, t := range archivedTexts {
			sb.WriteString(strings.TrimRight(t, "\n"))
			sb.WriteString("\n")
		}
		if err := atomicWriteFile(archivePath, []byte(sb.String()), 0644); err != nil {
			return rep, err
		}
		fm.capArchiveUnlocked(dir) // 修缺陷B：反思批量归档后裁剪到 ArchiveMax
	}
	if err := atomicWriteFile(activePath, []byte(strings.Join(newActive, "\n")), 0644); err != nil {
		return rep, err
	}
	return rep, nil
}

// rewriteFirstLineMetaMulti 顺序叠加多个 meta 改写到条目首行。
func rewriteFirstLineMetaMulti(firstLine string, muts []func(EntryMeta) EntryMeta) string {
	return rewriteFirstLineMeta(firstLine, func(m EntryMeta) EntryMeta {
		for _, fn := range muts {
			m = fn(m)
		}
		return m
	})
}

// rewriteFirstLineType 改写条目首行的 type 标记（保留时间戳 / source / 正文 / 内联 meta）。
// 仅换 `[type:source]` 的 type 段；无法解析则原样返回。
func rewriteFirstLineType(firstLine, newType string) string {
	line := strings.TrimSpace(firstLine)
	prefix := ""
	line = strings.TrimPrefix(line, "- ")
	prefix = "- "
	if strings.HasPrefix(line, "[") { // [HH:MM]
		if idx := strings.Index(line, "]"); idx > 0 && idx <= 6 {
			prefix += line[:idx+1] + " "
			line = strings.TrimSpace(line[idx+1:])
		}
	}
	if strings.HasPrefix(line, "[") { // [type:source]
		if idx := strings.Index(line, "]"); idx > 0 {
			meta := line[1:idx]
			if parts := strings.SplitN(meta, ":", 2); len(parts) == 2 {
				rest := strings.TrimSpace(line[idx+1:])
				return prefix + "[" + newType + ":" + parts[1] + "] " + rest
			}
		}
	}
	return firstLine
}
