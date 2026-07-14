package engineadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"math"
	"regexp"
	"strings"
	"sync"

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

const recognizePrompt = `识别这张作业图片里的所有题目，并逐题回收孩子的手写作答内容、判定题目学科、定位作答区域。严格输出 JSON 数组，每个元素形如：
{"question": "完整题干", "subject": "数学", "knowledge_points": ["知识点1"], "student_answer": "孩子写在题目上的作答（含算式/答案/涂改）", "bbox": {"x": 0.12, "y": 0.34, "w": 0.18, "h": 0.05}}
关键规则：
- subject 逐题判定题目学科，只能取以下之一：数学 / 语文 / 英语 / 物理 / 化学；确实判不出学科时才留空字符串 ""。
- question 题干只抄印刷体/原题内容，绝不能把铅笔、黑笔等手写墨迹拼进题干；student_answer 只如实誊录图中孩子**已经写下**的手写作答（包括紧跟在印刷等号后的数字）。例如印刷题是“4÷0.5=”且等号后手写“8”，必须让 question 写 "4÷0.5="、student_answer 写 "8"，不能把 question 写成“4÷0.5=8”。
- 只有确实完全没有手写作答的题，student_answer 才留空字符串 ""；绝不替孩子编造答案。印刷题干中本身带数字、选项或等号，不算 student_answer。
- bbox 是这道题**学生作答区域**在整张图里的归一化边界框，用于在原图上标注对/错：x,y 是框左上角、w,h 是框宽高，四个值都必须是 0 到 1 之间的小数（相对整图宽/高的比例），且 x+w、y+h 不超过 1。定位不准或看不清作答位置时，把 bbox 整个字段省略（宁可不给，绝不给错位的框）。
- 只输出 JSON，不要任何解释文字。`

// bboxDTO 解析视觉模型返回的归一化边界框。指针字段——视觉模型省略 bbox 时为 nil（降级不叠加）。
type bboxDTO struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// recognizedDTO 解析视觉模型 JSON 用（带 json tag）。
type recognizedDTO struct {
	Question        string   `json:"question"`
	Subject         string   `json:"subject"`
	KnowledgePoints []string `json:"knowledge_points"`
	StudentAnswer   string   `json:"student_answer"`
	BBox            *bboxDTO `json:"bbox"`
}

// invalidJSONEscape 匹配 JSON 字符串中的非法转义（\x 且 x ∉ "\/bfnrtu）——视觉模型在题干里
// 输出 LaTeX（\div 等）时 \d 会让 json.Unmarshal 直接失败（BUG-20260712-U 真机取证）。
var invalidJSONEscape = regexp.MustCompile(`\\([^"\\/bfnrtu])`)
var sectionHeading = regexp.MustCompile(`^[一二三四五六七八九十]+[、.．]\s*[^?？=]{0,20}(?:题|得数|计算|解方程|简算)$`)
var leadingChineseQuestionNumber = regexp.MustCompile(`^\s*\d+\s*、\s*`)
var leadingDottedQuestionNumber = regexp.MustCompile(`^\s*\d+\s*[.．]\s+`)

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

// bboxEpsilon 容忍视觉模型归一化时的极小浮点误差（右/下边界略超 1 视为贴边，不算越界）。
const bboxEpsilon = 0.005

// normalizeBBox 是原图批改的**硬性诚实门**（设计文档 §6）：只放行合法归一化框，其余一律 nil。
//
// 合法 = 四值皆非负、x/y 在 [0,1]、w/h 严格 >0、右下角 x+w、y+h 不越界（含极小误差容忍）。
// 缺失（模型省略）、零框（{0,0,0,0}）、负值、越界、NaN/Inf 全部降级为 nil——
// 该题走纯文字批改，前端绝不叠加错位红叉（错位比不标更糟）。
func normalizeBBox(b *bboxDTO) *usecase.BBox {
	if b == nil {
		return nil
	}
	// NaN/Inf 防护：NaN 的任何比较都为 false，会漏过下面的区间校验，显式拦掉。
	if math.IsNaN(b.X) || math.IsNaN(b.Y) || math.IsNaN(b.W) || math.IsNaN(b.H) ||
		math.IsInf(b.X, 0) || math.IsInf(b.Y, 0) || math.IsInf(b.W, 0) || math.IsInf(b.H, 0) {
		return nil
	}
	if b.W <= 0 || b.H <= 0 { // 零/负宽高 → 不是可叠加的框
		return nil
	}
	if b.X < 0 || b.Y < 0 || b.X > 1 || b.Y > 1 { // 左上角出界
		return nil
	}
	if b.X+b.W > 1+bboxEpsilon || b.Y+b.H > 1+bboxEpsilon { // 右下角越界
		return nil
	}
	return &usecase.BBox{X: b.X, Y: b.Y, W: b.W, H: b.H}
}

// Recognize 识题：调视觉模型 → 解析 JSON → 结构化题目值对象。

func (a *RecognizerAdapter) Recognize(ctx context.Context, image []byte) ([]usecase.RecognizedQuestion, error) {
	if a.vision == nil {
		return nil, fmt.Errorf("recognizer: 未配置视觉模型")
	}
	if len(image) == 0 {
		return nil, fmt.Errorf("recognizer: 空图片")
	}
	// 密集长作业不能整页只调一次视觉模型：glm-4v-flash 等模型的输出上限会把几十道题的
	// JSON 截断。长图先切成 5 个有重叠的纵向分片，并发识别后把 bbox 映射回整图坐标。
	// 非长图、无法解码的历史/测试载荷仍走原单请求路径，不扩大行为面。
	if segments, ok := splitDenseWorksheetImage(image); ok {
		return a.recognizeSegments(ctx, segments)
	}
	raw, err := a.vision(ctx, image, recognizePrompt)
	if err != nil {
		return nil, fmt.Errorf("recognizer: 视觉模型调用失败: %w", err)
	}
	return parseRecognizedQuestions(raw)
}

func parseRecognizedQuestions(raw string) ([]usecase.RecognizedQuestion, error) {
	var dtos []recognizedDTO
	if err := json.Unmarshal([]byte(sanitizeModelJSON(extractJSON(raw))), &dtos); err != nil {
		return nil, fmt.Errorf("recognizer: 解析识题结果失败: %w（原始: %.120s）", err, raw)
	}
	out := make([]usecase.RecognizedQuestion, 0, len(dtos))
	for _, d := range dtos {
		question := strings.TrimSpace(d.Question)
		if question == "" || sectionHeading.MatchString(question) || likelyCroppedFragment(question) {
			continue
		}
		out = append(out, usecase.RecognizedQuestion{
			Question:        question,
			KnowledgePoints: d.KnowledgePoints,
			StudentAnswer:   strings.TrimSpace(d.StudentAnswer),
			Subject:         normalizeRecognizedSubject(d.Subject),
			BBox:            normalizeBBox(d.BBox),
		})
	}
	return mergeRecognizedQuestions(nil, out), nil
}

type worksheetSegment struct {
	image  []byte
	index  int
	total  int
	startY float64
	endY   float64
}

var denseWorksheetRanges = [][2]float64{
	{0.00, 0.24},
	{0.18, 0.44},
	{0.38, 0.64},
	{0.58, 0.84},
	{0.68, 1.00},
}

var horizontalZoomRanges = [][2]float64{
	{0.00, 0.42},
	{0.29, 0.71},
	{0.58, 1.00},
}

type worksheetHorizontalZoom struct {
	image        []byte
	index        int
	total        int
	startX, endX float64
}

func splitHorizontalZooms(raw []byte) ([]worksheetHorizontalZoom, bool) {
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, false
	}
	bounds := src.Bounds()
	zooms := make([]worksheetHorizontalZoom, 0, len(horizontalZoomRanges))
	for i, r := range horizontalZoomRanges {
		x0 := bounds.Min.X + int(math.Floor(float64(bounds.Dx())*r[0]))
		x1 := bounds.Min.X + int(math.Ceil(float64(bounds.Dx())*r[1]))
		if x1 > bounds.Max.X {
			x1 = bounds.Max.X
		}
		if x1 <= x0 {
			return nil, false
		}
		dst := image.NewRGBA(image.Rect(0, 0, x1-x0, bounds.Dy()))
		draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
		draw.Draw(dst, dst.Bounds(), src, image.Pt(x0, bounds.Min.Y), draw.Over)
		var encoded bytes.Buffer
		if err := jpeg.Encode(&encoded, dst, &jpeg.Options{Quality: 90}); err != nil {
			return nil, false
		}
		zooms = append(zooms, worksheetHorizontalZoom{
			image: encoded.Bytes(), index: i + 1, total: len(horizontalZoomRanges),
			startX: r[0], endX: r[1],
		})
	}
	return zooms, true
}

// splitDenseWorksheetImage 仅处理高像素、明显纵向的作业图。5 段间保留 4%~6% 重叠，
// 让跨分界线的题至少在一个分片内完整出现；合并阶段按题干去重。
func splitDenseWorksheetImage(raw []byte) ([]worksheetSegment, bool) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Height < 1600 || cfg.Height*5 < cfg.Width*6 {
		return nil, false
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, false
	}
	bounds := src.Bounds()
	segments := make([]worksheetSegment, 0, len(denseWorksheetRanges))
	for i, r := range denseWorksheetRanges {
		y0 := bounds.Min.Y + int(math.Floor(float64(bounds.Dy())*r[0]))
		y1 := bounds.Min.Y + int(math.Ceil(float64(bounds.Dy())*r[1]))
		if y1 > bounds.Max.Y {
			y1 = bounds.Max.Y
		}
		if y1 <= y0 {
			return nil, false
		}
		dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), y1-y0))
		draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
		draw.Draw(dst, dst.Bounds(), src, image.Pt(bounds.Min.X, y0), draw.Over)
		var encoded bytes.Buffer
		if err := jpeg.Encode(&encoded, dst, &jpeg.Options{Quality: 88}); err != nil {
			return nil, false
		}
		segments = append(segments, worksheetSegment{
			image: encoded.Bytes(), index: i + 1, total: len(denseWorksheetRanges),
			startY: r[0], endY: r[1],
		})
	}
	return segments, true
}

func (a *RecognizerAdapter) recognizeSegments(ctx context.Context, segments []worksheetSegment) ([]usecase.RecognizedQuestion, error) {
	type segmentResult struct {
		questions []usecase.RecognizedQuestion
		err       error
	}
	results := make([]segmentResult, len(segments))
	sem := make(chan struct{}, 3) // 控制云端并发，避免 5 路瞬时触发 provider 限流。
	var wg sync.WaitGroup
	for i, segment := range segments {
		i, segment := i, segment
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i].err = ctx.Err()
				return
			}
			prompt := fmt.Sprintf(`%s

这是原作业图片的纵向分片 %d/%d。只识别在本分片内题干完整可见的题目；紧贴上/下边缘且被截断的残题必须忽略，重叠区域的完整题照常输出。bbox 坐标仍以当前分片为 0~1。JSON 必须紧凑输出，不要缩进。`, recognizePrompt, segment.index, segment.total)
			raw, err := a.vision(ctx, segment.image, prompt)
			if err != nil {
				results[i].err = fmt.Errorf("分片 %d/%d 视觉模型调用失败: %w", segment.index, segment.total, err)
				return
			}
			questions, err := parseRecognizedQuestions(raw)
			if err != nil {
				results[i].err = fmt.Errorf("分片 %d/%d: %w", segment.index, segment.total, err)
				return
			}
			// 横排小口算在整宽分片里字号很小，视觉模型偶发只抄到印刷题干、整片漏掉手写。
			// “识出题但 0 作答”既可能真空白，也可能漏识；仅追加一次聚焦复核并合并，
			// 不直接把首轮结果当成空白卷。复核失败保留首轮，避免可选增强拖垮整张识别。
			if segmentNeedsHandwritingReview(questions) {
				focusPrompt := fmt.Sprintf(`%s

这是原作业图片的纵向分片 %d/%d，需要复核手写答案。首轮已识出印刷题，但没有回收到任何 student_answer。请放大辨认铅笔/黑笔等手写墨迹，尤其是印刷等号后的数字、涂改和演算；题干只保留印刷体。逐题重做紧凑 JSON，确实没有手写才留空。紧贴上/下边缘且题干不完整的残题仍忽略。bbox 坐标以当前分片为 0~1。`, recognizePrompt, segment.index, segment.total)
				if retryRaw, retryErr := a.vision(ctx, segment.image, focusPrompt); retryErr == nil {
					if retryQuestions, parseErr := parseRecognizedQuestions(retryRaw); parseErr == nil {
						questions = mergeRecognizedQuestions(questions, retryQuestions)
					}
				}
			}
			// 少量横排短算式通常意味着整宽分片把小字缩得过小。实际裁成 3 个重叠横向放大块，
			// 让模型看清被漏掉的题/手写答案，再把局部 x 坐标映射回整宽分片。
			if segment.index == 1 && segmentNeedsHorizontalZoom(questions) {
				if zooms, ok := splitHorizontalZooms(segment.image); ok {
					for _, zoom := range zooms {
						zoomPrompt := fmt.Sprintf(`%s

这是原作业图片的纵向分片 %d/%d、横向放大 %d/%d。只输出在当前放大块中题干完整可见的题，重点分离印刷题干与手写答案；左右边缘被截断的题忽略。bbox 坐标以当前裁剪块为 0~1。JSON 紧凑输出。`, recognizePrompt, segment.index, segment.total, zoom.index, zoom.total)
						rawZoom, zoomErr := a.vision(ctx, zoom.image, zoomPrompt)
						if zoomErr != nil {
							continue
						}
						zoomQuestions, parseErr := parseRecognizedQuestions(rawZoom)
						if parseErr != nil {
							continue
						}
						if segmentNeedsHandwritingReview(zoomQuestions) {
							focusZoomPrompt := zoomPrompt + "\n这是手写答案二次复核：已有题目的 bbox/作答区可见但答案未誊录时，请逐字读出手写内容放入 student_answer；不要把手写答案拼回 question。"
							if focusRaw, focusErr := a.vision(ctx, zoom.image, focusZoomPrompt); focusErr == nil {
								if focusQuestions, focusParseErr := parseRecognizedQuestions(focusRaw); focusParseErr == nil {
									zoomQuestions = mergeRecognizedQuestions(zoomQuestions, focusQuestions)
								}
							}
						}
						spanX := zoom.endX - zoom.startX
						for q := range zoomQuestions {
							if zoomQuestions[q].BBox == nil {
								continue
							}
							mapped := *zoomQuestions[q].BBox
							mapped.X = zoom.startX + mapped.X*spanX
							mapped.W *= spanX
							zoomQuestions[q].BBox = &mapped
						}
						questions = mergeRecognizedQuestions(questions, zoomQuestions)
					}
				}
			}
			span := segment.endY - segment.startY
			for q := range questions {
				if questions[q].BBox == nil {
					continue
				}
				mapped := *questions[q].BBox
				mapped.Y = segment.startY + mapped.Y*span
				mapped.H *= span
				questions[q].BBox = &mapped
			}
			results[i].questions = questions
		}()
	}
	wg.Wait()

	merged := make([]usecase.RecognizedQuestion, 0)
	for _, result := range results {
		if result.err != nil {
			return nil, fmt.Errorf("recognizer: %w", result.err)
		}
		// 跨纵向分片也必须复用同一套 exact/equation/containment 合并规则。否则重叠区会同时
		// 留下“如果每平方米……”残题和包含周长条件的完整题，后续批改把它们当成两道题。
		merged = mergeRecognizedQuestions(merged, result.questions)
	}
	return merged, nil
}

func segmentNeedsHandwritingReview(questions []usecase.RecognizedQuestion) bool {
	if len(questions) == 0 {
		return false
	}
	allBlank := true
	for _, q := range questions {
		if strings.TrimSpace(q.StudentAnswer) != "" {
			allBlank = false
			continue
		}
		// 模型声称定位到了“作答区域”却没抄出内容，是强烈的漏识信号。
		if q.BBox != nil {
			return true
		}
	}
	return allBlank
}

func segmentNeedsHorizontalZoom(questions []usecase.RecognizedQuestion) bool {
	if len(questions) < 2 || len(questions) > 6 {
		return false
	}
	for _, q := range questions {
		if len([]rune(q.Question)) > 24 || !strings.ContainsAny(q.Question, "0123456789+-×÷*/=") {
			return false
		}
	}
	return true
}

func mergeRecognizedQuestions(primary, recovery []usecase.RecognizedQuestion) []usecase.RecognizedQuestion {
	merged := append([]usecase.RecognizedQuestion(nil), primary...)
	seen := make(map[string]int, len(merged))
	for i, q := range merged {
		seen[recognizedQuestionKey(q.Question)] = i
	}
	for _, q := range recovery {
		key := recognizedQuestionKey(q.Question)
		if existing, ok := seen[key]; ok && key != "" {
			if questionInformationScore(q) > questionInformationScore(merged[existing]) {
				merged[existing] = q
			}
			continue
		}
		// 同一视觉块的多次识别可能一份正确分离为 question="4.7+2.3"，另一份把
		// 手写 RHS 拼成 question="4.7+2.3=7"。只有两份互相印证且 RHS 简短时，
		// 才确定性地把 RHS 回收到 student_answer，不对单份完整方程擅自拆分。
		variantMerged := false
		for existingKey, existing := range seen {
			if answer, ok := equationVariantAnswer(key, existingKey); ok {
				combined := merged[existing]
				if combined.StudentAnswer == "" {
					combined.StudentAnswer = answer
				}
				if combined.BBox == nil {
					combined.BBox = q.BBox
				}
				merged[existing] = combined
				variantMerged = true
				break
			}
			if answer, ok := equationVariantAnswer(existingKey, key); ok {
				combined := q
				if combined.StudentAnswer == "" {
					combined.StudentAnswer = answer
				}
				if combined.BBox == nil {
					combined.BBox = merged[existing].BBox
				}
				merged[existing] = combined
				delete(seen, existingKey)
				seen[key] = existing
				variantMerged = true
				break
			}
		}
		if variantMerged {
			continue
		}
		containmentMerged := false
		for existingKey, existing := range seen {
			if !overlappingWordProblemDuplicate(merged[existing], q, existingKey, key) {
				continue
			}
			combined := merged[existing]
			if len([]rune(q.Question)) > len([]rune(combined.Question)) {
				combined.Question = q.Question
			}
			if len([]rune(q.StudentAnswer)) > len([]rune(combined.StudentAnswer)) {
				combined.StudentAnswer = q.StudentAnswer
				if q.BBox != nil {
					combined.BBox = q.BBox
				}
			} else if combined.BBox == nil {
				combined.BBox = q.BBox
			}
			if combined.Subject == "" {
				combined.Subject = q.Subject
			}
			if len(q.KnowledgePoints) > len(combined.KnowledgePoints) {
				combined.KnowledgePoints = q.KnowledgePoints
			}
			merged[existing] = combined
			newKey := recognizedQuestionKey(combined.Question)
			if newKey != existingKey {
				delete(seen, existingKey)
				seen[newKey] = existing
			}
			containmentMerged = true
			break
		}
		if containmentMerged {
			continue
		}
		seen[key] = len(merged)
		merged = append(merged, q)
	}
	return merged
}

func overlappingWordProblemDuplicate(a, b usecase.RecognizedQuestion, aKey, bKey string) bool {
	if len([]rune(aKey)) < 12 || len([]rune(bKey)) < 12 ||
		(!strings.Contains(aKey, bKey) && !strings.Contains(bKey, aKey)) {
		return false
	}
	aAnswer := recognizedQuestionKey(a.StudentAnswer)
	bAnswer := recognizedQuestionKey(b.StudentAnswer)
	if len([]rune(aAnswer)) < 4 || len([]rune(bAnswer)) < 4 {
		return false
	}
	return strings.Contains(aAnswer, bAnswer) || strings.Contains(bAnswer, aAnswer)
}

func equationVariantAnswer(longer, base string) (string, bool) {
	if base == "" || !strings.HasPrefix(longer, base+"=") {
		return "", false
	}
	answer := strings.TrimSpace(strings.TrimPrefix(longer, base+"="))
	if answer == "" || len([]rune(answer)) > 16 || strings.ContainsAny(answer, "?？") {
		return "", false
	}
	return answer, true
}

func likelyCroppedFragment(question string) bool {
	trimmed := strings.TrimSpace(question)
	if trimmed == "" {
		return true
	}
	first := []rune(trimmed)[0]
	if strings.ContainsRune("×÷*/+=", first) {
		return true
	}
	return first == '-' && len([]rune(trimmed)) <= 3
}

func recognizedQuestionKey(question string) string {
	question = leadingChineseQuestionNumber.ReplaceAllString(question, "")
	question = leadingDottedQuestionNumber.ReplaceAllString(question, "")
	replacer := strings.NewReplacer(" ", "", "\t", "", "\n", "", "\r", "", "，", ",", "？", "", "?", "", "＝", "=")
	key := strings.ToLower(replacer.Replace(strings.TrimSpace(question)))
	return strings.TrimSuffix(key, "=")
}

func questionInformationScore(q usecase.RecognizedQuestion) int {
	score := len([]rune(q.Question)) + len([]rune(q.StudentAnswer))*2 + len(q.KnowledgePoints)
	if q.BBox != nil {
		score += 4
	}
	if q.Subject != "" {
		score++
	}
	return score
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
