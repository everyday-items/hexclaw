package memory

import "regexp"

// PII / 凭证守卫（方案 §4.10「凭证红线不进记忆任何层」）。放 memory 包供所有写入路径复用
// （自动抽取 ingestExtractedFacts + manage_memory 自管工具），单一口径。
//
// **确定性兜底**：即便上游已用 prompt 指示不抽敏感信息，写入前仍机械再过一道——
// 账号密码 / API key / 私钥 / 身份证 / 银行卡一律不落。守卫保守、宁漏挡不误挡：
// 只命中「高置信凭证形态」（赋值上下文+值，或明确密钥/证件指纹），不因含「密码」二字就误杀。

var sensitivePatterns = []*regexp.Regexp{
	// 赋值上下文的凭证：`密码: xxx` / `api_key = sk-...`（需分隔符+值，避开「密码管理器」误杀）。
	regexp.MustCompile(`(?i)(密码|口令|密钥|私钥|password|passwd|pwd|secret|api[_\s-]?key|access[_\s-]?token|client[_\s-]?secret|token)\s*[:：=]\s*\S{4,}`),
	regexp.MustCompile(`(密码|口令|密钥|私钥)\s*(是|为)\s*\S{4,}`),
	// 已知密钥指纹形态。
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`),                // OpenAI 风格
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),                     // AWS Access Key
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),           // GitHub token
	regexp.MustCompile(`-----BEGIN (?:[A-Z ]+ )?PRIVATE KEY-----`), // PEM 私钥
	// 中国身份证（18 位，含合法年月日段与校验位）。
	regexp.MustCompile(`\b[1-9]\d{5}(?:18|19|20)\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\d{3}[\dXx]\b`),
	// 银行卡 / 长数字串（13-19 位连续数字，允许空格/连字符分组）。
	regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`),
}

// LooksSensitive 报告内容是否疑似含凭证 / PII（高置信形态命中即真）。
func LooksSensitive(content string) bool {
	for _, re := range sensitivePatterns {
		if re.MatchString(content) {
			return true
		}
	}
	return false
}
