package builtin

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/toolkit/lang/stringx"
)

// TranslateSkill 翻译 Skill。
//
// 当前实现为本地规则翻译，覆盖常见中英翻译命令和高频词汇，
// 不依赖外部 API，可作为快速路径的确定性兜底。
type TranslateSkill struct{}

// NewTranslateSkill 创建翻译 Skill
func NewTranslateSkill() *TranslateSkill {
	return &TranslateSkill{}
}

func (s *TranslateSkill) Name() string        { return "translate" }
func (s *TranslateSkill) Description() string { return "翻译文本内容，支持中英互译" }

// Match 匹配翻译关键词
func (s *TranslateSkill) Match(content string) bool {
	lower := strings.ToLower(strings.TrimSpace(content))
	keywords := []string{"翻译", "translate", "英译中", "中译英", "翻成中文", "翻成英文", "译成中文", "译成英文"}
	for _, kw := range keywords {
		if strings.HasPrefix(lower, kw) {
			return true
		}
	}
	return false
}

// Execute 执行翻译
func (s *TranslateSkill) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	query := firstStringArg(args, "query", "text", "content")
	if query == "" {
		return &skill.Result{Content: "请提供需要翻译的文本，例如：翻译 hello world 或 中译英 你好世界"}, nil
	}

	req := parseTranslationRequest(query)
	if req.text == "" {
		return &skill.Result{Content: "请提供需要翻译的文本"}, nil
	}

	translated := translateText(req.text, req.direction)
	return &skill.Result{
		Content: fmt.Sprintf("原文：%s\n译文：%s", req.text, translated),
		Metadata: map[string]string{
			"direction": string(req.direction),
		},
	}, nil
}

type translationDirection string

const (
	translateAuto translationDirection = "auto"
	translateENZH translationDirection = "en-zh"
	translateZHEN translationDirection = "zh-en"
)

type translationRequest struct {
	text      string
	direction translationDirection
}

func parseTranslationRequest(query string) translationRequest {
	trimmed := strings.TrimSpace(query)
	lower := strings.ToLower(trimmed)

	switch {
	case strings.HasPrefix(lower, "英译中"):
		return translationRequest{text: strings.TrimSpace(trimmed[len("英译中"):]), direction: translateENZH}
	case strings.HasPrefix(lower, "中译英"):
		return translationRequest{text: strings.TrimSpace(trimmed[len("中译英"):]), direction: translateZHEN}
	case strings.HasPrefix(lower, "translate"):
		return translationRequest{text: strings.TrimSpace(trimmed[len("translate"):]), direction: translateAuto}
	case strings.HasPrefix(lower, "翻译"):
		return translationRequest{text: strings.TrimSpace(trimmed[len("翻译"):]), direction: translateAuto}
	}

	return translationRequest{text: trimmed, direction: translateAuto}
}

func translateText(text string, direction translationDirection) string {
	cleaned := normalizeWhitespace(text)
	if cleaned == "" {
		return ""
	}
	if direction == translateAuto {
		direction = detectTranslationDirection(cleaned)
	}
	switch direction {
	case translateZHEN:
		return translateChineseToEnglish(cleaned)
	default:
		return translateEnglishToChinese(cleaned)
	}
}

func detectTranslationDirection(text string) translationDirection {
	hasASCIIWord := false
	hasHan := false
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
			hasHan = true
		case r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			hasASCIIWord = true
		}
	}
	switch {
	case hasHan && !hasASCIIWord:
		return translateZHEN
	case hasASCIIWord && !hasHan:
		return translateENZH
	default:
		return translateENZH
	}
}

var enToZhPhrases = map[string]string{
	"hello world":             "你好世界",
	"hello":                   "你好",
	"world":                   "世界",
	"good morning":            "早上好",
	"good night":              "晚安",
	"thank you":               "谢谢你",
	"how are you":             "你好吗",
	"go language":             "Go 语言",
	"http server":             "HTTP 服务器",
	"artificial intelligence": "人工智能",
}

var zhToEnPhrases = map[string]string{
	"你好世界":     "hello world",
	"你好":       "hello",
	"世界":       "world",
	"早上好":      "good morning",
	"晚安":       "good night",
	"谢谢你":      "thank you",
	"你好吗":      "how are you",
	"Go 语言":    "go language",
	"HTTP 服务器": "http server",
	"人工智能":     "artificial intelligence",
}

var enToZhWords = map[string]string{
	"hello":        "你好",
	"world":        "世界",
	"good":         "好",
	"morning":      "早上",
	"night":        "夜晚",
	"thanks":       "谢谢",
	"thank":        "谢谢",
	"you":          "你",
	"how":          "如何",
	"are":          "是",
	"go":           "Go",
	"language":     "语言",
	"http":         "HTTP",
	"server":       "服务器",
	"artificial":   "人工",
	"intelligence": "智能",
}

var zhToEnWords = map[string]string{
	"你好":  "hello",
	"世界":  "world",
	"早上":  "morning",
	"好":   "good",
	"晚安":  "good night",
	"谢谢":  "thanks",
	"你":   "you",
	"如何":  "how",
	"Go":  "go",
	"语言":  "language",
	"服务器": "server",
	"人工":  "artificial",
	"智能":  "intelligence",
}

var zhPhraseOrder = sortedPhraseKeys(zhToEnPhrases)

func sortedPhraseKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return len([]rune(keys[i])) > len([]rune(keys[j]))
	})
	return keys
}

func translateEnglishToChinese(text string) string {
	lower := strings.ToLower(text)
	if translated, ok := enToZhPhrases[lower]; ok {
		return translated
	}

	tokens := strings.FieldsFunc(lower, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r))
	})
	if len(tokens) == 0 {
		return text
	}

	translated := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if word, ok := enToZhWords[token]; ok {
			translated = append(translated, word)
			continue
		}
		translated = append(translated, token)
	}
	return stringx.Truncate(strings.Join(translated, " "), 200)
}

func translateChineseToEnglish(text string) string {
	if translated, ok := zhToEnPhrases[text]; ok {
		return translated
	}

	result := text
	for _, phrase := range zhPhraseOrder {
		result = strings.ReplaceAll(result, phrase, " "+zhToEnPhrases[phrase]+" ")
	}

	tokens := strings.Fields(result)
	if len(tokens) == 0 {
		return text
	}

	translated := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if word, ok := zhToEnWords[token]; ok {
			translated = append(translated, word)
			continue
		}
		translated = append(translated, token)
	}
	return stringx.Truncate(strings.Join(translated, " "), 200)
}
