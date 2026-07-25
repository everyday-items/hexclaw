package engineadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

const imageTaskClassifierPrompt = `你是图片任务分流器，只依据当前图片中可见事实分类；附带消息只能帮助理解指代，绝不能替代图片证据。四类定义：completed_homework=有学生作答痕迹的作业；blank_worksheet=无学生作答、供家长讲题的空白试卷；writing=语文作文/连续文字作品；artwork=绘画/美术作品。证据冲突时输出 unknown，并给出至少两个 confirmation_candidates。严格只输出 JSON：
{"task_intent":"completed_homework|blank_worksheet|writing|artwork|unknown","intent_evidence":["图片中可复核的短证据"],"confidence":0.0,"confirmation_candidates":[],"work_title_candidate":null,"task_requirement_candidate":null}
标题/任务不是必填；只有图片中确实可见时才输出候选，候选格式 {"value":"...","source":"image_vision","confidence":0.0,"evidence_ref":"可复核位置"}，不得用占位标题补齐。`

const imageTaskWritingOCRPrompt = `逐字转写这张语文作文原稿，并同时报告转写质量。不要润色、纠错、补句、概括或点评；无法辨认、涂改覆盖、多个读法冲突的片段不得猜。严格只输出 JSON：
{"raw":"逐字原稿","canonical_content":"只有清晰一致片段才可规范换行，文字不得改写","confidence":0.0,"risk_segments":[{"segment_id":"稳定位置标识","raw_text":"原片段","reasons":["illegible|overwrite|conflicting_reading"],"alternatives":["候选读法"]}]}
清晰一致且无需家长确认时 risk_segments 必须为空；存在任何风险必须逐段列出。`

type ImageTaskAdapter struct{ vision VisionFunc }

type definitiveImageTaskResponseError struct{ cause error }

func (e *definitiveImageTaskResponseError) Error() string { return e.cause.Error() }
func (e *definitiveImageTaskResponseError) Unwrap() error { return e.cause }
func (e *definitiveImageTaskResponseError) ProviderResponseStatusCode() int {
	return 200
}

func definitiveImageTaskResponse(err error) error {
	if err == nil {
		return nil
	}
	return &definitiveImageTaskResponseError{cause: err}
}

func NewImageTaskAdapter(vision VisionFunc) *ImageTaskAdapter {
	return &ImageTaskAdapter{vision: vision}
}

var _ usecase.ImageTaskClassifier = (*ImageTaskAdapter)(nil)
var _ usecase.ImageTaskWritingOCR = (*ImageTaskAdapter)(nil)

func strictImageTaskJSON(raw string, target any) error {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		if newline := strings.IndexByte(raw, '\n'); newline >= 0 {
			raw = raw[newline+1:]
		}
		if end := strings.LastIndex(raw, "```"); end >= 0 {
			raw = raw[:end]
		}
		raw = strings.TrimSpace(raw)
	}
	left, right := strings.IndexByte(raw, '{'), strings.LastIndexByte(raw, '}')
	if left < 0 || right <= left {
		return fmt.Errorf("模型未返回 JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw[left : right+1])))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("模型返回了多余 JSON 内容")
	}
	return nil
}

func (a *ImageTaskAdapter) ClassifyImageTask(
	ctx context.Context,
	input usecase.ImageTaskClassificationInput,
) (usecase.ImageTaskClassification, error) {
	if a == nil || a.vision == nil {
		return usecase.ImageTaskClassification{}, fmt.Errorf("image task classifier: 未配置视觉模型")
	}
	if len(input.Images) != 1 || len(input.Images[0]) == 0 {
		return usecase.ImageTaskClassification{}, fmt.Errorf("image task classifier: 当前分流调用必须有且仅有一张真实图片")
	}
	prompt := imageTaskClassifierPrompt
	if message := strings.TrimSpace(input.MessageIntent); message != "" {
		prompt += "\n附带消息（仅作指代上下文）：" + message
	}
	raw, err := a.vision(ctx, input.Images[0], prompt)
	if err != nil {
		return usecase.ImageTaskClassification{}, fmt.Errorf(
			"image task classifier: 视觉模型调用失败: %w", providerResponseError(err),
		)
	}
	var envelope struct {
		TaskIntent             k12.ImageTaskIntent   `json:"task_intent"`
		IntentEvidence         []string              `json:"intent_evidence"`
		Confidence             float64               `json:"confidence"`
		ConfirmationCandidates []k12.ImageTaskIntent `json:"confirmation_candidates"`
		WorkTitleCandidate     *k12.FactCandidate    `json:"work_title_candidate"`
		TaskRequirement        *k12.FactCandidate    `json:"task_requirement_candidate"`
	}
	if err := strictImageTaskJSON(raw, &envelope); err != nil {
		return usecase.ImageTaskClassification{}, definitiveImageTaskResponse(
			fmt.Errorf("image task classifier: 解析失败: %w", err),
		)
	}
	result := usecase.ImageTaskClassification{
		Intent: envelope.TaskIntent, IntentEvidence: envelope.IntentEvidence,
		Confidence:               envelope.Confidence,
		ConfirmationCandidates:   envelope.ConfirmationCandidates,
		WorkTitleCandidate:       envelope.WorkTitleCandidate,
		TaskRequirementCandidate: envelope.TaskRequirement,
	}
	bindImageTaskCandidate := func(candidate *k12.FactCandidate) {
		if candidate == nil {
			return
		}
		if candidate.Source == k12.FactCandidateSourceImageVision ||
			candidate.Source == k12.FactCandidateSourceImageOCR {
			candidate.EvidenceRef = "asset_index:0#" +
				strings.TrimSpace(candidate.EvidenceRef)
		}
	}
	bindImageTaskCandidate(result.WorkTitleCandidate)
	bindImageTaskCandidate(result.TaskRequirementCandidate)
	if len(result.IntentEvidence) == 0 || result.Confidence < 0 || result.Confidence > 1 {
		return result, definitiveImageTaskResponse(
			fmt.Errorf("image task classifier: 缺少可复核证据或 confidence 非法"),
		)
	}
	if result.Intent == k12.ImageTaskIntentUnknown {
		if len(result.ConfirmationCandidates) < 2 {
			return result, definitiveImageTaskResponse(
				fmt.Errorf("image task classifier: unknown 缺少最小候选 exact-set"),
			)
		}
	} else if len(result.ConfirmationCandidates) != 0 {
		return result, definitiveImageTaskResponse(
			fmt.Errorf("image task classifier: 已确定 intent 不得携带确认候选"),
		)
	}
	if result.WorkTitleCandidate != nil {
		if err := result.WorkTitleCandidate.Validate(); err != nil {
			return result, definitiveImageTaskResponse(err)
		}
	}
	if result.TaskRequirementCandidate != nil {
		if err := result.TaskRequirementCandidate.Validate(); err != nil {
			return result, definitiveImageTaskResponse(err)
		}
	}
	return result, nil
}

func (a *ImageTaskAdapter) RecognizeImageTaskWriting(
	ctx context.Context,
	image []byte,
) (usecase.ImageTaskWritingOCRResult, error) {
	if a == nil || a.vision == nil || len(image) == 0 {
		return usecase.ImageTaskWritingOCRResult{}, fmt.Errorf("image task writing OCR: 图片或视觉模型缺失")
	}
	raw, err := a.vision(ctx, image, imageTaskWritingOCRPrompt)
	if err != nil {
		return usecase.ImageTaskWritingOCRResult{}, fmt.Errorf(
			"image task writing OCR: 视觉模型调用失败: %w", providerResponseError(err),
		)
	}
	var envelope struct {
		Raw              string                          `json:"raw"`
		CanonicalContent string                          `json:"canonical_content"`
		Confidence       float64                         `json:"confidence"`
		RiskSegments     []k12.CreativeWorkIntakeOCRRisk `json:"risk_segments"`
	}
	if err := strictImageTaskJSON(raw, &envelope); err != nil {
		return usecase.ImageTaskWritingOCRResult{}, definitiveImageTaskResponse(
			fmt.Errorf("image task writing OCR: 解析失败: %w", err),
		)
	}
	if strings.TrimSpace(envelope.Raw) == "" || strings.TrimSpace(envelope.CanonicalContent) == "" ||
		envelope.Confidence < 0 || envelope.Confidence > 1 {
		return usecase.ImageTaskWritingOCRResult{}, definitiveImageTaskResponse(
			fmt.Errorf("image task writing OCR: 证据不完整"),
		)
	}
	for _, risk := range envelope.RiskSegments {
		if strings.TrimSpace(risk.SegmentID) == "" || len(risk.Reasons) == 0 {
			return usecase.ImageTaskWritingOCRResult{}, definitiveImageTaskResponse(
				fmt.Errorf("image task writing OCR: 风险片段证据不完整"),
			)
		}
	}
	return usecase.ImageTaskWritingOCRResult{
		Raw: envelope.Raw, CanonicalContent: envelope.CanonicalContent,
		Confidence: envelope.Confidence, RiskSegments: envelope.RiskSegments,
	}, nil
}
