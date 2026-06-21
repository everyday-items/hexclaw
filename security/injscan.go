package security

// prompt-injection 注入扫描（与 skill_scanner 同包同范式）。
//
// 纵深防御的一层，非主防御：注入正则高漏报（改写/翻译/混淆即绕）、高误报。主防御是
// 架构 —— PolicyGate 兜动作审批 + stripTools 按 source 收窄不可信来源的工具供给。
// 本扫描只做"明显恶意"的快速拦截。两级入口：ScanUserPrompt（创建期）/ ScanAssembled（执行期）。

import (
	"fmt"
	"regexp"
	"strings"
)

// 指令覆盖族（prompt injection）：exec 期有合法 skills/注入数据时放宽（避免误杀讲注入的安全文档）。
var injectionOverridePatterns = []*dangerPattern{
	{regexp.MustCompile(`(?i)ignore\s+(all\s+)?(previous|above)\s+instructions`), "prompt-injection", "instruction override"},
	{regexp.MustCompile(`(忽略|无视|不要理会)(以上|之前|前面|上述).{0,6}(指令|提示|要求|规则)`), "prompt-injection", "指令覆盖(中)"},
	{regexp.MustCompile(`(?i)(you are now|from now on you are|disregard your)\b`), "prompt-injection", "persona override"},
	{regexp.MustCompile(`(?i)(system\s*prompt|开发者消息|developer message)\s*[:：]`), "prompt-injection", "fake system turn"},
}

// 凭证外泄族（env 变量拼进 URL / 外发命令）：任何场景都严格。
var exfiltrationPatterns = []*dangerPattern{
	{regexp.MustCompile(`(?i)(curl|wget|fetch)\b.{0,80}\$\{?[A-Z_]*(KEY|TOKEN|SECRET|PASSWORD)`), "exfiltration", "env-secret in outbound cmd"},
	{regexp.MustCompile(`https?://[^\s"']*\$\{?[A-Z_]*(KEY|TOKEN|SECRET|PASSWORD)`), "exfiltration", "secret embedded in URL"},
}

// 混淆族（零宽 / 不可见字符）：用转义而非字面零宽字符；任何场景都严格。
var obfuscationPatterns = []*dangerPattern{
	{regexp.MustCompile(`[\x{200B}-\x{200D}\x{FEFF}\x{2060}]`), "obfuscation", "zero-width / invisible chars"},
}

// ScanUserPrompt is the STRICT scan for prompt creation (e.g. cron create): all
// families, including instruction-override — a user authoring a task has no
// legitimate reason to embed an override phrase.
func ScanUserPrompt(s string) error {
	return scanInjectionFamilies(s, injectionOverridePatterns, exfiltrationPatterns, obfuscationPatterns)
}

// ScanAssembled is the EXEC-time scan of an assembled prompt. When skills or
// injected (RAG/webhook) data are present, the instruction-override family is
// relaxed (a legitimate security tutorial that quotes "ignore previous
// instructions" must not be killed) — but exfiltration + obfuscation stay strict.
//
// Hard gate: an event-triggered (webhook/external) run that skips
// ScanAssembled must not ship.
func ScanAssembled(prompt string, hasSkills, hasInjectedData bool) error {
	families := [][]*dangerPattern{exfiltrationPatterns, obfuscationPatterns}
	if !hasSkills && !hasInjectedData {
		families = append(families, injectionOverridePatterns)
	}
	return scanInjectionFamilies(prompt, families...)
}

// scanInjectionFamilies matches against the ORIGINAL string (no lowercasing —
// the exfiltration patterns rely on uppercase KEY/TOKEN; case-insensitivity is
// expressed per-pattern via (?i)).
func scanInjectionFamilies(s string, families ...[]*dangerPattern) error {
	var violations []string
	for _, fam := range families {
		for _, p := range fam {
			if p.regex.MatchString(s) {
				violations = append(violations, fmt.Sprintf("[%s] %s", p.category, p.message))
			}
		}
	}
	if len(violations) > 0 {
		return fmt.Errorf("prompt injection scan blocked (%d): %s",
			len(violations), strings.Join(violations, "; "))
	}
	return nil
}
