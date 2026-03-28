package security

import (
	"regexp"
	"strings"
	"unicode"
)

// SanitizeToolOutput cleans tool execution output before sending to LLM.
//
// Defends against indirect prompt injection where a malicious website or
// API response embeds instructions that the LLM might follow.
//
// Strategy:
//   - Strip invisible Unicode characters (zero-width, RTL overrides)
//   - Strip HTML tags and hidden content
//   - Detect and flag LLM delimiter injection attempts
//   - Truncate to maxLen if needed
func SanitizeToolOutput(output string, maxLen int) string {
	if output == "" {
		return output
	}

	// 1. Strip invisible Unicode characters
	output = stripInvisible(output)

	// 2. Strip HTML tags
	output = stripHTMLTags(output)

	// 3. Detect LLM delimiter injection
	output = neutralizeDelimiters(output)

	// 4. Truncate
	if maxLen > 0 && len(output) > maxLen {
		output = output[:maxLen] + "\n[truncated]"
	}

	return output
}

var htmlTagRe = regexp.MustCompile(`<[^>]{1,500}>`)

func stripHTMLTags(s string) string {
	return htmlTagRe.ReplaceAllString(s, "")
}

// stripInvisible removes zero-width characters, RTL/LTR overrides, and other invisible Unicode
func stripInvisible(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\u200B' || r == '\u200C' || r == '\u200D' || r == '\uFEFF':
			return -1 // zero-width chars
		case r == '\u200E' || r == '\u200F': // LTR/RTL marks
			return -1
		case r >= '\u202A' && r <= '\u202E': // bidi overrides
			return -1
		case r >= '\u2066' && r <= '\u2069': // bidi isolates
			return -1
		case r == '\u00AD': // soft hyphen
			return -1
		case !unicode.IsPrint(r) && r != '\n' && r != '\r' && r != '\t':
			return -1 // other non-printable (except newlines/tabs)
		default:
			return r
		}
	}, s)
}

// Common LLM delimiters that attackers inject to hijack context
var delimiterPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\[/?INST\]`),                          // Llama/Mistral
	regexp.MustCompile(`(?i)<\|?(system|user|assistant)\|?>`),     // ChatML
	regexp.MustCompile(`(?i)<\|im_(start|end)\|?>`),               // GPT tokenizer format
	regexp.MustCompile(`(?i)###\s*(System|User|Assistant|Human)`), // Claude-style
	regexp.MustCompile(`(?i)IGNORE\s+(ALL\s+)?PREVIOUS`),          // Social engineering
	regexp.MustCompile(`(?i)YOU\s+ARE\s+NOW\s+`),                  // Persona hijack
	regexp.MustCompile(`(?i)NEW\s+INSTRUCTION[S]?\s*:`),           // Injection prefix
}

func neutralizeDelimiters(s string) string {
	for _, re := range delimiterPatterns {
		if re.MatchString(s) {
			s = re.ReplaceAllStringFunc(s, func(match string) string {
				return "[SANITIZED:" + strings.ReplaceAll(match, "\n", " ") + "]"
			})
		}
	}
	return s
}
