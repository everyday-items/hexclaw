package recall

import (
	"sort"
	"strings"
)

// 闭环重点二「query 改写/扩展」（§7-核心）：确定性、零 LLM 的真实现，替代 IdentityExpander 占位。
// 缓解漏召——用户原话与记忆措辞不一致时，把近义词并入 query 提高词面重叠。

// SynonymExpander 用近义词典扩展 query：命中 key 即把其近义词并入。确定性（按 key 排序，
// 不依赖 map 迭代序），可由 eval 度量与调词典。
type SynonymExpander struct {
	Synonyms map[string][]string
}

func (e SynonymExpander) Expand(q string) string {
	q = strings.TrimSpace(q)
	if q == "" || len(e.Synonyms) == 0 {
		return q
	}
	lc := strings.ToLower(q)

	keys := make([]string, 0, len(e.Synonyms))
	for k := range e.Synonyms {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 确定性：不受 map 随机迭代序影响

	var add []string
	seen := map[string]bool{}
	for _, key := range keys {
		if !strings.Contains(lc, strings.ToLower(key)) {
			continue
		}
		for _, s := range e.Synonyms[key] {
			if s != "" && !seen[s] {
				seen[s] = true
				add = append(add, s)
			}
		}
	}
	if len(add) == 0 {
		return q
	}
	return q + " " + strings.Join(add, " ")
}

// DefaultSynonyms 返回 hexclaw 场景常见近义词典（可由 eval 调/扩充）。
func DefaultSynonyms() map[string][]string {
	return map[string][]string{
		"主题":  {"外观", "皮肤", "风格"},
		"深色":  {"暗色", "夜间"},
		"偏好":  {"喜欢", "习惯"},
		"时区":  {"时间", "时差"},
		"项目":  {"工程", "代码库"},
		"数据库": {"db", "存储"},
	}
}
