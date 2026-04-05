package builtin

import "strings"

// firstStringArg 从 map 中按 key 优先级取第一个非空白 string 值。
//
// 注意：toolkit/lang/stringx.FirstNonEmpty 只接收 ...string 参数，
// 不支持从 map[string]any 中按 key 提取并做类型断言，因此无法替代。
func firstStringArg(args map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := args[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// normalizeWhitespace 将字符串中的连续空白字符归一化为单个空格。
//
// 注意：toolkit/lang/stringx 没有提供等效函数。
func normalizeWhitespace(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

// trimPrefixKeyword 以大小写不敏感方式匹配并移除前缀关键词。
//
// 注意：toolkit/lang/stringx.RemovePrefix 是大小写敏感的，
// 此函数需要大小写不敏感匹配（用于中英文混合的用户输入），因此无法替代。
func trimPrefixKeyword(content string, prefixes []string) string {
	trimmed := strings.TrimSpace(content)
	lower := strings.ToLower(trimmed)
	for _, prefix := range prefixes {
		if strings.HasPrefix(lower, prefix) {
			return strings.TrimSpace(trimmed[len(prefix):])
		}
	}
	return trimmed
}
