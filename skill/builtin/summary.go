package builtin

import (
	"context"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/toolkit/lang/stringx"
)

// SummarySkill 文本摘要 Skill。
//
// 使用本地抽取式摘要算法，适合快速概括中短文本，
// 不依赖外部 LLM，可作为快速路径和离线兜底。
type SummarySkill struct{}

// NewSummarySkill 创建摘要 Skill
func NewSummarySkill() *SummarySkill {
	return &SummarySkill{}
}

func (s *SummarySkill) Name() string        { return "summary" }
func (s *SummarySkill) Description() string { return "对文本内容进行摘要概括" }

// ToolDefinition 返回摘要工具的 LLM 定义
func (s *SummarySkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("summary", "对文本内容进行摘要概括", &llm.Schema{
		Type: "object",
		Properties: map[string]*llm.Schema{
			"text":       {Type: "string", Description: "要摘要的文本内容"},
			"max_length": {Type: "integer", Description: "摘要最大长度（字符数）"},
		},
		Required: []string{"text"},
	})
}

// Match 匹配摘要关键词
func (s *SummarySkill) Match(content string) bool {
	lower := strings.ToLower(strings.TrimSpace(content))
	keywords := []string{"摘要", "总结", "概括", "summary", "summarize", "tldr", "tl;dr"}
	for _, kw := range keywords {
		if strings.HasPrefix(lower, kw) {
			return true
		}
	}
	return false
}

// Execute 执行摘要
func (s *SummarySkill) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	query := firstStringArg(args, "query", "text", "content")
	if query == "" {
		return &skill.Result{Content: "请提供需要摘要的文本，例如：摘要 这篇文章讲了什么"}, nil
	}

	text := extractSummaryText(query)
	if text == "" {
		return &skill.Result{Content: "请提供需要摘要的文本"}, nil
	}

	return &skill.Result{
		Content: summarizeText(text),
	}, nil
}

func extractSummaryText(query string) string {
	prefixes := []string{"摘要一下", "总结一下", "概括一下", "摘要", "总结", "概括", "summary", "summarize", "tl;dr", "tldr"}
	trimmed := trimPrefixKeyword(query, prefixes)
	trimmed = strings.TrimLeft(trimmed, "：:")
	return strings.TrimSpace(trimmed)
}

func summarizeText(text string) string {
	cleaned := normalizeWhitespace(text)
	if cleaned == "" {
		return "摘要："
	}
	if utf8.RuneCountInString(cleaned) <= 80 {
		return "摘要：" + cleaned
	}

	sentences := splitSummarySentences(cleaned)
	if len(sentences) == 0 {
		return "摘要：" + stringx.Truncate(cleaned, 200)
	}
	if len(sentences) == 1 {
		return "摘要：" + stringx.Truncate(sentences[0], 200)
	}

	scores := scoreSummarySentences(sentences)
	selected := pickTopSummarySentences(scores, min(len(sentences), 3))
	parts := make([]string, 0, len(selected))
	for _, idx := range selected {
		parts = append(parts, sentences[idx])
	}
	return "摘要：" + stringx.Truncate(strings.Join(parts, "；"), 240)
}

func splitSummarySentences(text string) []string {
	var (
		parts   []string
		current strings.Builder
	)
	flush := func() {
		s := normalizeWhitespace(current.String())
		if s != "" {
			parts = append(parts, s)
		}
		current.Reset()
	}

	for _, r := range text {
		switch r {
		case '\n', '。', '！', '？', '.', '!', '?', ';', '；':
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return parts
}

type summarySentenceScore struct {
	index int
	score int
}

func scoreSummarySentences(sentences []string) []summarySentenceScore {
	freq := make(map[string]int)
	for _, sentence := range sentences {
		for _, token := range summaryTokens(sentence) {
			freq[token]++
		}
	}

	scores := make([]summarySentenceScore, 0, len(sentences))
	for i, sentence := range sentences {
		score := 0
		for _, token := range summaryTokens(sentence) {
			score += freq[token]
		}
		score += min(utf8.RuneCountInString(sentence), 40)
		scores = append(scores, summarySentenceScore{index: i, score: score})
	}
	return scores
}

func pickTopSummarySentences(scores []summarySentenceScore, n int) []int {
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].score == scores[j].score {
			return scores[i].index < scores[j].index
		}
		return scores[i].score > scores[j].score
	})
	if len(scores) > n {
		scores = scores[:n]
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].index < scores[j].index })
	indexes := make([]int, 0, len(scores))
	for _, item := range scores {
		indexes = append(indexes, item.index)
	}
	return indexes
}

var summaryStopwords = map[string]struct{}{
	"the": {}, "a": {}, "an": {}, "is": {}, "are": {}, "to": {}, "of": {}, "and": {}, "or": {}, "in": {}, "on": {}, "for": {}, "with": {},
	"的": {}, "了": {}, "和": {}, "是": {}, "在": {}, "把": {}, "对": {}, "与": {}, "及": {}, "并": {}, "也": {},
}

func summaryTokens(sentence string) []string {
	var tokens []string
	var ascii strings.Builder
	flushASCII := func() {
		if ascii.Len() == 0 {
			return
		}
		token := ascii.String()
		ascii.Reset()
		if len(token) <= 1 {
			return
		}
		if _, skip := summaryStopwords[token]; !skip {
			tokens = append(tokens, token)
		}
	}

	for _, r := range strings.ToLower(sentence) {
		switch {
		case r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			ascii.WriteRune(r)
		case unicode.Is(unicode.Han, r):
			flushASCII()
			token := string(r)
			if _, skip := summaryStopwords[token]; !skip {
				tokens = append(tokens, token)
			}
		default:
			flushASCII()
		}
	}
	flushASCII()
	return tokens
}
