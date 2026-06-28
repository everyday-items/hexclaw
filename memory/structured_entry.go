package memory

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 存储格式地基（A）：把结构化元数据持久化进条目行尾的**内联 meta 标签**（markdown 不可见、向后兼容）。
// 标签形如 `<!--m:pin=1;subj=居住地;vf=...;vt=...;sup=m-3;conf=0.90-->`，旧条目无标签 → 字段取零值。
// 解锁：G3 时序留史（ValidFrom/ValidTo/Supersedes）+ Pinned（强制常驻，供 manage_memory/UI）。

const (
	entryMetaPrefix = "<!--m:"
	entryMetaSuffix = "-->"
)

// EntryMeta 是一条记忆的结构化元数据。
type EntryMeta struct {
	ID         string // 稳定内容寻址 ID（修缺陷H）：append 时生成、随条目持久化，删/改/移行后不漂移；旧条目无标签 → "" → 回退行号 ID。
	Pinned     bool
	Subject    string
	ValidFrom  string
	ValidTo    string
	Supersedes string
	Confidence float32
	HitCount   int // 召回频次（行为 importance / 深度整合权重的真实驱动；旧条目无标签 → 0）
}

func (m EntryMeta) isZero() bool {
	return m.ID == "" && !m.Pinned && m.Subject == "" && m.ValidFrom == "" &&
		m.ValidTo == "" && m.Supersedes == "" && m.Confidence == 0 && m.HitCount == 0
}

// serialize 编码为内联标签；全零返回 ""。Subject 经 url 转义防 `;=` 冲突。
func (m EntryMeta) serialize() string {
	if m.isZero() {
		return ""
	}
	var p []string
	if m.ID != "" {
		p = append(p, "eid="+m.ID)
	}
	if m.Pinned {
		p = append(p, "pin=1")
	}
	if m.Subject != "" {
		p = append(p, "subj="+url.QueryEscape(m.Subject))
	}
	if m.ValidFrom != "" {
		p = append(p, "vf="+m.ValidFrom)
	}
	if m.ValidTo != "" {
		p = append(p, "vt="+m.ValidTo)
	}
	if m.Supersedes != "" {
		p = append(p, "sup="+m.Supersedes)
	}
	if m.Confidence > 0 {
		p = append(p, "conf="+strconv.FormatFloat(float64(m.Confidence), 'f', 2, 32))
	}
	if m.HitCount > 0 {
		p = append(p, "hc="+strconv.Itoa(m.HitCount))
	}
	return entryMetaPrefix + strings.Join(p, ";") + entryMetaSuffix
}

// splitEntryMeta 从条目正文尾部分离内联 meta 标签，返回纯正文 + meta。
// 无标签 → 原样返回 + 零值 meta（向后兼容）。
func splitEntryMeta(content string) (string, EntryMeta) {
	trimmed := strings.TrimRight(content, " \t")
	if !strings.HasSuffix(trimmed, entryMetaSuffix) {
		return content, EntryMeta{}
	}
	i := strings.LastIndex(trimmed, entryMetaPrefix)
	if i < 0 {
		return content, EntryMeta{}
	}
	body := trimmed[i+len(entryMetaPrefix) : len(trimmed)-len(entryMetaSuffix)]
	pure := strings.TrimRight(trimmed[:i], " \t")
	var m EntryMeta
	for _, kv := range strings.Split(body, ";") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "eid":
			m.ID = v
		case "pin":
			m.Pinned = v == "1"
		case "subj":
			if s, err := url.QueryUnescape(v); err == nil {
				m.Subject = s
			}
		case "vf":
			m.ValidFrom = v
		case "vt":
			m.ValidTo = v
		case "sup":
			m.Supersedes = v
		case "conf":
			if f, err := strconv.ParseFloat(v, 32); err == nil {
				m.Confidence = float32(f)
			}
		case "hc":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				m.HitCount = n
			}
		}
	}
	return pure, m
}

// SaveStructuredEntry 保存带结构化元数据的记忆条目（地基 A 写入入口）。
// Pinned 或全局类型（identity/preference/instruction/rule）→ 落 _global 常驻。
func (fm *FileMemory) SaveStructuredEntry(content, memType, source, role string, meta EntryMeta) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	if memType == "" {
		memType = "fact"
	}
	if source == "" {
		source = "manual"
	}
	targetDir := fm.roleDir(role)
	if isGlobalMemType(memType) || meta.Pinned {
		targetDir = fm.roleDir("")
	}

	fm.mu.Lock()
	defer fm.mu.Unlock()
	fm.evictIfNeededUnlocked(targetDir)
	return fm.appendStructuredEntryUnlocked(targetDir, memType, source, content, meta)
}

// newStableID 生成一条记忆的稳定内容寻址 ID（修缺陷H）。append 时铸一次、随条目持久化，
// 删/改/移行后不漂移（取代「行号即 ID」——删上方条目使行号整体上移、过期 ID 打到错条目/落空）。
// 64 位随机熵（单用户桌面、记忆有界 → 碰撞概率可忽略），前缀 `e` 与行号 ID（`m-`/`a-`/`r-` + 数字）形态正交。
func newStableID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 几乎不会失败；极端兜底用纳秒时戳保证仍可寻址、不空 ID。
		return "e" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return "e" + hex.EncodeToString(b[:])
}

// appendStructuredEntryUnlocked 把正文 + 内联 meta 标签追加为一条结构化条目（调用方已持写锁）。
// 是所有新条目的**单写器**：PII 守卫（缺陷J）与稳定 ID 铸造（缺陷H）都收敛在此一处。
func (fm *FileMemory) appendStructuredEntryUnlocked(targetDir, memType, source, content string, meta EntryMeta) error {
	if LooksSensitive(content) { // 修缺陷J：PII/凭证守卫下沉到单写器选择点 → 覆盖 save/insert/supersede 全路径（与抽取路径一致：静默丢弃不落库）
		return nil
	}
	// 修缺陷H：单行新条目落盘前铸稳定 ID（已有 ID 的合成/迁移条目沿用）。
	// 内联 meta 是**单行构造**：parse 读块尾标签、rewriteFirstLineMeta 写首行——单行二者重合、多行会错位且互相污染。
	// 多行条目（手记/多行画像）按既有约定不挂内联 meta、回退行号 ID（与改动前行为一致，零回归）。
	if meta.ID == "" && !strings.Contains(content, "\n") {
		meta.ID = newStableID()
	}
	body := content
	if tag := meta.serialize(); tag != "" {
		body = content + " " + tag
	}
	return fm.appendEntryUnlocked(targetDir, memType, source, body)
}

// stripEntryPrefix 剥掉条目首行的 `- [HH:MM] [type:source] ` 前缀，返回「正文 + 可选 meta 标签」。
// 与 extractContentFromLine 的区别：本函数**保留**行尾 meta 标签，供原地改写时读取并重写。
func stripEntryPrefix(line string) string {
	line = strings.TrimPrefix(line, "- ")
	if strings.HasPrefix(line, "[") { // [HH:MM]
		if idx := strings.Index(line, "]"); idx > 0 && idx <= 6 {
			line = strings.TrimSpace(line[idx+1:])
		}
	}
	if strings.HasPrefix(line, "[") { // [type:source]
		if idx := strings.Index(line, "]"); idx > 0 {
			meta := line[1:idx]
			if parts := strings.SplitN(meta, ":", 2); len(parts) == 2 {
				line = strings.TrimSpace(line[idx+1:])
			}
		}
	}
	return line
}

// rewriteFirstLineMeta 改写条目首行的内联 meta（保留时间戳/类型前缀与正文，仅替换 meta 标签）。
// 确定性纯函数：取出现有 meta → mutate → 重新序列化拼回正文尾部。**不改正文、不改行数。**
func rewriteFirstLineMeta(firstLine string, mutate func(EntryMeta) EntryMeta) string {
	afterPrefix := stripEntryPrefix(strings.TrimSpace(firstLine))
	pure, meta := splitEntryMeta(afterPrefix)
	meta = mutate(meta)
	body := pure
	if tag := meta.serialize(); tag != "" {
		body = pure + " " + tag
	}
	return rebuildEntryLine(strings.TrimSpace(firstLine), body)
}

// rewriteEntryMetaUnlocked 原地改写指定 ID 条目的内联 meta（调用方已持写锁）。
// 行数不变 → 其它条目的行号 ID 全部保持稳定（供反思批量 apply 安全复用）。
func (fm *FileMemory) rewriteEntryMetaUnlocked(id string, mutate func(EntryMeta) EntryMeta) error {
	dir, filename, lineIdx := fm.resolveEntryLocationUnlocked(id)
	if lineIdx < 0 {
		return fmt.Errorf("记忆条目不存在: %s", id)
	}
	path := filepath.Join(dir, filename)
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取记忆文件失败: %w", err)
	}
	rawStr := string(raw)
	lines := strings.Split(rawStr, "\n")
	blockStart, blockEnd := findBlockByStartLine(rawStr, lineIdx)
	if blockStart < 0 || blockStart >= len(lines) {
		return fmt.Errorf("记忆条目不存在: %s", id)
	}
	mi := metaLineIdx(lines, blockStart, blockEnd) // 多行条目把标签落最后正文行（=parse 读取处），不污染首行
	lines[mi] = rewriteFirstLineMeta(lines[mi], mutate)
	return atomicWriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}

// readEntryMetaUnlocked 读取指定 ID 条目当前的行内 meta（Pinned/Subject/ValidFrom/HitCount…）。调用方已持锁。
// 供 dedup-Update 整合时保留旧 meta（修缺陷I：updateEntryUnlocked 纯覆盖会丢 pin/主语/留史锚）。
func (fm *FileMemory) readEntryMetaUnlocked(id string) EntryMeta {
	dir, filename, lineIdx := fm.resolveEntryLocationUnlocked(id)
	if lineIdx < 0 {
		return EntryMeta{}
	}
	raw, err := os.ReadFile(filepath.Join(dir, filename))
	if err != nil {
		return EntryMeta{}
	}
	rawStr := string(raw)
	lines := strings.Split(rawStr, "\n")
	blockStart, _ := findBlockByStartLine(rawStr, lineIdx)
	if blockStart < 0 || blockStart >= len(lines) {
		return EntryMeta{}
	}
	_, meta := splitEntryMeta(stripEntryPrefix(strings.TrimSpace(lines[blockStart])))
	return meta
}

// SetPinned 设置/清除指定条目的常驻标记（manage_memory pin/unpin 复用，公开 + 自持写锁）。
// Pinned=true → 强制常驻（逃生口，反思永不自动移动）；行内 meta 原地改写，正文不动。
func (fm *FileMemory) SetPinned(id string, pinned bool) error {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	return fm.rewriteEntryMetaUnlocked(id, func(m EntryMeta) EntryMeta {
		m.Pinned = pinned
		return m
	})
}

// BumpHitCount 批量给一组条目的召回频次（hc=）+1，行内 meta 原地改写、按文件单次原子落盘。
//
// 修缺陷F：召回路径从不自增 HitCount → recallAlpha·RecallCount 恒 0、fact 晋升(≥3)、做梦热点保护(≥3)
// 全是死代码。召回时调用本方法（被注入即算一次召回）→ 行为 importance 反馈环复活。best-effort：
// 召回是增强不是命脉，单条改写失败不阻断、不报错给上层。
func (fm *FileMemory) BumpHitCount(ids []string) {
	if len(ids) == 0 {
		return
	}
	fm.mu.Lock()
	defer fm.mu.Unlock()

	// 按 (dir,filename) 分组：每个文件一次读-改-写（meta 改写不改行数 → 同文件内多 id 行号稳定）。
	type fileKey struct{ dir, file string }
	groups := map[fileKey][]int{}
	for _, id := range ids {
		dir, filename, lineIdx := fm.resolveEntryLocationUnlocked(id)
		if lineIdx < 0 {
			continue
		}
		k := fileKey{dir, filename}
		groups[k] = append(groups[k], lineIdx)
	}
	for k, lineIdxs := range groups {
		path := filepath.Join(k.dir, k.file)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		rawStr := string(raw)
		lines := strings.Split(rawStr, "\n")
		changed := false
		for _, li := range lineIdxs {
			blockStart, blockEnd := findBlockByStartLine(rawStr, li) // rawStr 不变、行数不变 → 多 id 稳定
			if blockStart < 0 || blockStart >= len(lines) {
				continue
			}
			mi := metaLineIdx(lines, blockStart, blockEnd) // 多行条目把 hc 落最后正文行，不污染首行
			lines[mi] = rewriteFirstLineMeta(lines[mi], func(m EntryMeta) EntryMeta {
				m.HitCount++
				return m
			})
			changed = true
		}
		if changed {
			_ = atomicWriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
		}
	}
}

// rewriteEntryContentMetaUnlocked 原地改写一条目的**正文 + 行内 meta**（调用方已持写锁）。
// 块起始行不变 → 行号 ID 稳定；正文折叠为单行（首行内联 meta 标签要求单行）。
// 与 updateEntryUnlocked 的区别：后者经 rebuildEntryLine 会丢弃 meta；此处显式重挂 meta 标签。
func (fm *FileMemory) rewriteEntryContentMetaUnlocked(id, content string, meta EntryMeta) error {
	dir, filename, lineIdx := fm.resolveEntryLocationUnlocked(id)
	if lineIdx < 0 {
		return fmt.Errorf("记忆条目不存在: %s", id)
	}
	path := filepath.Join(dir, filename)
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取记忆文件失败: %w", err)
	}
	rawStr := string(raw)
	lines := strings.Split(rawStr, "\n")
	blockStart, blockEnd := findBlockByStartLine(rawStr, lineIdx)
	if blockStart < 0 {
		return fmt.Errorf("记忆条目不存在: %s", id)
	}
	pureOld, _ := splitEntryMeta(strings.TrimSpace(lines[blockStart]))
	newContent := strings.TrimSpace(content)
	newFirst := rebuildEntryLine(pureOld, newContent)
	// 内联 meta 是单行构造（缺陷H 边界）：仅当新正文单行时挂回标签；多行则退化为无 meta 多行条目，
	// 杜绝标签落到续行——否则后续 BumpHitCount/SetPinned 改首行会造成双标签、正文被存储层标签污染。
	if !strings.Contains(newContent, "\n") {
		if tag := meta.serialize(); tag != "" {
			newFirst += " " + tag
		}
	}
	newLines := append(lines[:blockStart], newFirst)
	newLines = append(newLines, lines[blockEnd:]...)
	return atomicWriteFile(path, []byte(strings.Join(newLines, "\n")), 0644)
}

// metaTimeFormat 是内联 meta 中时序字段（vf/vt）的落盘格式（RFC3339，UTC）。
const metaTimeFormat = time.RFC3339

// supersedeEntryUnlocked 执行 G3 时序取代「留史」写链（方案 §7ter / §4.7 R3，调用方已持写锁）：
//
//	① 旧条**正文不动**，仅标 ValidTo=now → 失效但留史（召回默认只取 currently-valid）。
//	② 新条带 Subject/Supersedes(旧条 ID)/ValidFrom=now 落盘，接续有效窗口。
//
// 先改旧（行数不变，行号稳定）后追加新（仅在末尾增行，不动既有行号）→ oldID 始终有效。
func (fm *FileMemory) supersedeEntryUnlocked(oldID, newContent, memType, source, role, subject string, now time.Time) error {
	vt := now.UTC().Format(metaTimeFormat)
	if err := fm.rewriteEntryMetaUnlocked(oldID, func(m EntryMeta) EntryMeta {
		m.ValidTo = vt
		return m
	}); err != nil {
		return err
	}
	targetDir := fm.roleDir(role)
	if isGlobalMemType(memType) {
		targetDir = fm.roleDir("")
	}
	fm.evictIfNeededUnlocked(targetDir)
	meta := EntryMeta{
		Subject:    strings.TrimSpace(subject),
		Supersedes: oldID,
		ValidFrom:  now.UTC().Format(metaTimeFormat),
	}
	return fm.appendStructuredEntryUnlocked(targetDir, memType, source, newContent, meta)
}
