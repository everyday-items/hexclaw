package engineadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// VisionFunc 把图片 + 提示词发给视觉模型返回文本。
//
// 由 composition root 用 llmrouter 实现（mirror knowledge.Captioner）；engineadapter 不 import
// hexagon/llm/router，保持轻量可测。真实现见 cmd/hexclaw/main.go 的注入闭包。
type VisionFunc func(ctx context.Context, image []byte, prompt string) (string, error)

// RecognizerAdapter 用视觉模型把作业照片识别成结构化题目。
type RecognizerAdapter struct{ vision VisionFunc }

// NewRecognizerAdapter 创建 adapter。
func NewRecognizerAdapter(v VisionFunc) *RecognizerAdapter { return &RecognizerAdapter{vision: v} }

var _ usecase.Recognizer = (*RecognizerAdapter)(nil)

const recognizePrompt = `识别这张作业图片里的所有题目，并逐题回收孩子的手写作答内容、判定题目学科。严格输出 JSON 数组，每个元素形如：
{"question": "完整题干", "subject": "数学", "knowledge_points": ["知识点1"], "student_answer": "孩子写在题目上的作答（含算式/答案/涂改）"}
关键规则：
- subject 逐题判定题目学科，只能取以下之一：数学 / 语文 / 英语 / 物理 / 化学；确实判不出学科时才留空字符串 ""。
- student_answer 只如实誊录图中孩子**已经写下**的手写作答；这道题是空白/未作答的，student_answer 必须留空字符串 ""，绝不替孩子编造答案。
- 只输出 JSON，不要任何解释文字。`

// recognizedDTO 解析视觉模型 JSON 用（带 json tag）。
type recognizedDTO struct {
	Question        string   `json:"question"`
	Subject         string   `json:"subject"`
	KnowledgePoints []string `json:"knowledge_points"`
	StudentAnswer   string   `json:"student_answer"`
}

// invalidJSONEscape 匹配 JSON 字符串中的非法转义（\x 且 x ∉ "\/bfnrtu）——视觉模型在题干里
// 输出 LaTeX（\div 等）时 \d 会让 json.Unmarshal 直接失败（BUG-20260712-U 真机取证）。
var invalidJSONEscape = regexp.MustCompile(`\\([^"\\/bfnrtu])`)

// sanitizeModelJSON 让模型产出的 JSON 可安全解析：
//  1. 数学命令降级（复用 adapter.NormalizeMathText：\times→× 等，题干顺带变家长可读）；
//  2. 剩余非法转义加倍（\x → \\x），未知反斜杠永不再炸解析。
func sanitizeModelJSON(s string) string {
	s = adapter.NormalizeMathText(s)
	return invalidJSONEscape.ReplaceAllString(s, `\\$1`)
}

// recognizedSubjects 是识题允许回填的学科白名单——视觉模型判定越界（返回未知词/编造）时归零，
// 避免脏学科流入 solve/批改路由（与 usecase.normalizeSubject 的领域约束对齐）。
var recognizedSubjects = map[string]struct{}{
	"数学": {}, "语文": {}, "英语": {}, "物理": {}, "化学": {},
}

// normalizeRecognizedSubject 只放行白名单内学科，其余（含空/未知）归一为空字符串。
func normalizeRecognizedSubject(s string) string {
	s = strings.TrimSpace(s)
	if _, ok := recognizedSubjects[s]; ok {
		return s
	}
	return ""
}

// Recognize 识题：调视觉模型 → 解析 JSON → 结构化题目值对象。

func (a *RecognizerAdapter) Recognize(ctx context.Context, image []byte) ([]usecase.RecognizedQuestion, error) {
	if a.vision == nil {
		return nil, fmt.Errorf("recognizer: 未配置视觉模型")
	}
	if len(image) == 0 {
		return nil, fmt.Errorf("recognizer: 空图片")
	}
	raw, err := a.vision(ctx, image, recognizePrompt)
	if err != nil {
		return nil, fmt.Errorf("recognizer: 视觉模型调用失败: %w", err)
	}
	var dtos []recognizedDTO
	if err := json.Unmarshal([]byte(sanitizeModelJSON(extractJSON(raw))), &dtos); err != nil {
		return nil, fmt.Errorf("recognizer: 解析识题结果失败: %w（原始: %.120s）", err, raw)
	}
	out := make([]usecase.RecognizedQuestion, 0, len(dtos))
	for _, d := range dtos {
		if strings.TrimSpace(d.Question) == "" {
			continue
		}
		out = append(out, usecase.RecognizedQuestion{
			Question:        d.Question,
			KnowledgePoints: d.KnowledgePoints,
			StudentAnswer:   strings.TrimSpace(d.StudentAnswer),
			Subject:         normalizeRecognizedSubject(d.Subject),
		})
	}
	return out, nil
}

// extractJSON 从模型输出里抠出 JSON 数组（容忍 ```json 围栏 / 前后噪声）。
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	// 去 markdown 代码围栏
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		if j := strings.IndexByte(s, '\n'); j >= 0 { // 去掉 ```json 那行的语言标记
			s = s[j+1:]
		}
		if k := strings.LastIndex(s, "```"); k >= 0 {
			s = s[:k]
		}
		s = strings.TrimSpace(s)
	}
	// 截取首个 '[' 到末个 ']'（数组）
	l, r := strings.IndexByte(s, '['), strings.LastIndexByte(s, ']')
	if l >= 0 && r > l {
		return s[l : r+1]
	}
	return s
}
