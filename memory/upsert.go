package memory

import (
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/memory/recall"
)

// dedupUpsertThreshold 字符二元组相似度阈值：>= 视为同一事实的改写 → update 就地。
const dedupUpsertThreshold = 0.65

// UpsertEntryForRole 是程序化记忆写入的**单写原子入口**（增量 4，方案坑A「单写记忆协调器」）。
//
// 无主语的薄包装：转调 UpsertStructuredEntryForRole（subject=""）。无主语 → 永不触发 supersede，
// 行为与历史一致（insert / update 就地 / discard），保证既有抽取写链零回归。
//
// 返回动作："insert" | "update" | "discard"。注入是增强，出错返回 err 但绝不破坏既有文件。
func (fm *FileMemory) UpsertEntryForRole(content, memType, source, role string) (string, error) {
	return fm.UpsertStructuredEntryForRole(content, memType, source, role, "")
}

// UpsertStructuredEntryForRole 是带主语的单写原子入口（增量 B：解锁 G3 时序取代留史）。
//
// 在 FileMemory 写锁内一次性完成「解析现有 → dedupUpsert 判定 → insert/update/discard/**supersede**」：
//   - **supersede**（同非空 Subject 异值，矛盾）：旧条标 ValidTo 留史、新条带 Supersedes/Subject 落盘
//     （方案 §7ter G3，矛盾=取代非覆盖，召回默认只取 currently-valid）。
//   - update（语义重叠无主语冲突）：就地整合。discard（重复）。insert（新事实）。
//
// 比对集已映射 ValidTo → DedupUpsert 的 IsCurrentlyValid 自动跳过已失效旧条（不会反复 supersede 历史条）。
//
// 返回动作："insert" | "update" | "discard" | "supersede"。
func (fm *FileMemory) UpsertStructuredEntryForRole(content, memType, source, role, subject string) (string, error) {
	if memType == "" {
		memType = "fact"
	}
	if source == "" {
		source = "manual"
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return "discard", nil
	}

	fm.mu.Lock()
	defer fm.mu.Unlock()

	now := time.Now()
	existing := recallEntriesForDedup(fm.parseEntriesForRoleUnlocked(role))
	cand := recall.Entry{Content: content, Type: recall.Type(memType), Subject: strings.TrimSpace(subject)}
	d := recall.DedupUpsert(cand, existing, now,
		recall.DedupOptions{SimFn: recall.LexicalSim, JaccardThreshold: dedupUpsertThreshold})

	switch d.Action {
	case recall.ActionDiscard:
		return "discard", nil
	case recall.ActionSupersede:
		if d.TargetID != "" {
			if err := fm.supersedeEntryUnlocked(d.TargetID, content, memType, source, role, cand.Subject, now); err != nil {
				return "", err
			}
			return "supersede", nil
		}
	case recall.ActionUpdate:
		if d.TargetID != "" {
			// 修缺陷I：就地整合须保留旧 meta（Pinned/ValidFrom/HitCount/Subject），否则置顶/留史锚被静默清掉。
			preserved := fm.readEntryMetaUnlocked(d.TargetID)
			if cand.Subject != "" {
				preserved.Subject = cand.Subject // 新主语若有则采纳
			}
			if err := fm.rewriteEntryContentMetaUnlocked(d.TargetID, content, preserved); err == nil {
				return "update", nil
			}
			// 更新失败 → 退化为插入，不丢信息。
		}
	}

	targetDir := fm.roleDir(role)
	if isGlobalMemType(memType) {
		targetDir = fm.roleDir("")
	}
	fm.evictIfNeededUnlocked(targetDir)
	if err := fm.appendStructuredEntryUnlocked(targetDir, memType, source, content, EntryMeta{Subject: cand.Subject}); err != nil {
		return "", err
	}
	return "insert", nil
}

// recallEntriesForDedup 映射文件条目为 recall.Entry（dedup/supersede 所需字段：含 Subject 与 G3 时效，
// 使 DedupUpsert 能按主语判矛盾、并据 ValidFrom/ValidTo 跳过未生效/已失效条目）。
func recallEntriesForDedup(parsed []MemoryEntry) []recall.Entry {
	out := make([]recall.Entry, 0, len(parsed))
	for _, e := range parsed {
		out = append(out, toRecallEntry(e))
	}
	return out
}

// toRecallEntry 把文件存储层条目映射为召回内核一等模型（dedup / 反思 / 打分共用）。
// 时序字段（CreatedAt/ValidFrom/ValidTo）按 RFC3339 解析，旧条目无值 → 零值（向后兼容）。
func toRecallEntry(e MemoryEntry) recall.Entry {
	return recall.Entry{
		ID:          e.ID,
		Type:        recall.Type(e.Type),
		Subject:     e.Subject,
		Content:     e.Content,
		Pinned:      e.Pinned,
		RecallCount: e.HitCount,
		Confidence:  e.Confidence,
		CreatedAt:   parseMetaTime(e.CreatedAt),
		UpdatedAt:   parseMetaTime(e.UpdatedAt),
		ValidFrom:   parseMetaTime(e.ValidFrom),
		ValidTo:     parseMetaTimePtr(e.ValidTo),
		Supersedes:  e.Supersedes,
	}
}

// ToRecallEntry 是 toRecallEntry 的导出版，供 engine 召回路径复用——**单一映射器、杜绝字段漂移**
// （修缺陷A：engine 旧自有 toRecallEntries 丢 Pinned/ValidTo/Subject → 取代失效旧值再注入、置顶失灵）。
// 额外锚定 AccessedAt（recency 衰减锚点；engine 召回路径需要，内部淘汰路径不依赖、保持原行为）。
func ToRecallEntry(e MemoryEntry) recall.Entry {
	re := toRecallEntry(e)
	if re.AccessedAt.IsZero() {
		if re.AccessedAt = re.UpdatedAt; re.AccessedAt.IsZero() {
			re.AccessedAt = re.CreatedAt
		}
	}
	return re
}

// parseMetaTime 解析内联 meta 的 RFC3339 时序字段；空/非法 → 零值（向后兼容旧条目）。
func parseMetaTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(metaTimeFormat, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseMetaTimePtr 同 parseMetaTime，但返回指针（nil=至今有效，对齐 recall.Entry.ValidTo 语义）。
func parseMetaTimePtr(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(metaTimeFormat, s)
	if err != nil {
		return nil
	}
	return &t
}
