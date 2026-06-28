package engine

import (
	"context"
	"strings"

	"github.com/hexagon-codes/hexagon/observe/trace"
	"github.com/hexagon-codes/hexclaw/memory"
)

// 增量 3/4：自动记忆写链命脉（重点一 —— 记得准、记得净）。
//
// 抽取 → 原子化拆分 → 类型分类 → 单写原子 dedupUpsert（memory.UpsertEntryForRole）。
// dedupUpsert/相似度阈值/落盘原子性收口在 memory 包（增量 4 单写器），引擎只负责抽取与分类。

// extractedFact 是一条解析后的离散事实：Content 为自包含正文，Subject 为可变属性主语（可空）。
// Subject 非空 → 该事实是「会随时间改变的属性」（居住地/时区/职业…），供 G3 时序取代矛盾消解。
type extractedFact struct {
	Subject string
	Content string
}

// cleanFactLine 归一抽取结果的单行：去 bullet / trim / NONE→空；保守不剥数字前缀（防「3年经验」被截）。
func cleanFactLine(line string) string {
	line = strings.TrimSpace(line)
	for _, p := range []string{"- ", "* ", "• ", "· ", "・ "} {
		line = strings.TrimPrefix(line, p)
	}
	line = strings.TrimSpace(line)
	if strings.EqualFold(line, "none") {
		return ""
	}
	return line
}

// splitAtomicFacts 把 LLM 抽取结果（「每条一行」）拆成离散自包含事实正文（不含主语解析）。
// 去项目符号 / 去空行 / 去 NONE / 同批去重。保留供无需主语的纯正文场景与既有契约。
func splitAtomicFacts(result string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, raw := range strings.Split(result, "\n") {
		line := cleanFactLine(raw)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	return out
}

// parseExtractedFacts 把 LLM 抽取结果拆成 {Subject, Content}（G3-extractor）。
// 约定：可变属性型事实以 `[主语] 正文` 标注（如「[居住地] 用户住在北京」）→ Subject 入库供时序取代；
// 普通/不变事实无前缀 → Subject 空。同批按 Content 去重。
func parseExtractedFacts(result string) []extractedFact {
	var out []extractedFact
	seen := make(map[string]bool)
	for _, raw := range strings.Split(result, "\n") {
		line := cleanFactLine(raw)
		if line == "" {
			continue
		}
		subj, content := splitSubjectPrefix(line)
		if content == "" || seen[content] {
			continue
		}
		seen[content] = true
		out = append(out, extractedFact{Subject: subj, Content: content})
	}
	return out
}

// splitSubjectPrefix 从 `[主语] 正文` 解析主语；无前缀 / 主语空 / 过长(>16 runes) / 正文空 → 兜底为整行正文、主语空。
// 防御 LLM 误格式（把大段内容塞进方括号），不让坏主语污染时序取代键。
func splitSubjectPrefix(line string) (subject, content string) {
	if !strings.HasPrefix(line, "[") {
		return "", line
	}
	idx := strings.Index(line, "]")
	if idx <= 1 {
		return "", line
	}
	subj := strings.TrimSpace(line[1:idx])
	rest := strings.TrimSpace(line[idx+1:])
	if subj == "" || rest == "" || len([]rune(subj)) > 16 {
		return "", line
	}
	return subj, rest
}

// classifyMemoryType 轻量启发式分类（确定性，零 LLM）：决定存储路由与注入分层
// （instruction/identity/preference → 常驻层保证带；fact → 检索层）。
// 优先级：约定/指令 > 身份 > 偏好 > 事实。
func classifyMemoryType(content string) string {
	lc := strings.ToLower(content)
	hit := func(hints ...string) bool {
		for _, h := range hints {
			if strings.Contains(lc, h) {
				return true
			}
		}
		return false
	}
	switch {
	case hit("以后", "从现在", "从今", "记住", "总是", "务必", "每次", "请用", "回答用", "from now", "always", "remember"):
		return "instruction"
	case hit("我是", "我叫", "我的名字", "名字叫", "职业", "工程师", "开发者", "设计师", "developer", "engineer", "my name", "i am", "i'm"):
		return "identity"
	case hit("喜欢", "偏好", "习惯", "讨厌", "不喜欢", "prefer", "like", "favorite"):
		return "preference"
	default:
		return "fact"
	}
}

// ingestExtractedFacts 把抽取的离散事实逐条经**单写原子** UpsertStructuredEntryForRole 落库，返回写入/更新/取代条数。
//
// 每条 upsert 在 FileMemory 写锁内完成「解析→dedup→insert/update/discard/**supersede**」，无 TOCTOU；
// 带 Subject 的属性型事实遇同主语异值 → 时序取代留史（G3）；同批后续事实自动据已落盘前序去重。
func (e *ReActEngine) ingestExtractedFacts(ctx context.Context, facts []extractedFact, role string) int {
	if e.fileMem == nil || len(facts) == 0 {
		return 0
	}
	n := 0
	for _, f := range facts {
		// PII / 凭证守卫（§4.10）：敏感内容一律不落记忆任何层，即便 LLM 误抽出。
		if memory.LooksSensitive(f.Content) {
			trace.L(ctx).Warn("auto-memory: 跳过疑似敏感内容（凭证/PII 不入记忆）")
			continue
		}
		action, err := e.fileMem.UpsertStructuredEntryForRole(f.Content, classifyMemoryType(f.Content), "chat_extract", role, f.Subject)
		if err != nil {
			trace.L(ctx).Error("auto-memory: 写入失败", "err", err)
			continue
		}
		if action != "discard" {
			n++
		}
	}
	return n
}
