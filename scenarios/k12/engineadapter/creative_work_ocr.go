package engineadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

const creativeWorkOCRPrompt = `请逐字转写这张语文作文原稿照片。只识别孩子实际写下的正文和可见标题，保留原段落、标点、错别字与原本用词；不要润色、纠错、补句、概括或点评。无法辨认的位置写「〔字迹不清〕」，不要猜。严格只输出 JSON 对象：{"text":"逐字原稿"}。`

// CreativeWorkOCRAdapter reuses the production VisionFunc primitive while
// keeping writing OCR separate from homework question recognition.
type CreativeWorkOCRAdapter struct{ vision VisionFunc }

func NewCreativeWorkOCRAdapter(vision VisionFunc) *CreativeWorkOCRAdapter {
	return &CreativeWorkOCRAdapter{vision: vision}
}

var _ usecase.CreativeWorkOCRRecognizer = (*CreativeWorkOCRAdapter)(nil)

func (a *CreativeWorkOCRAdapter) RecognizeWriting(ctx context.Context, image []byte) (string, error) {
	if a == nil || a.vision == nil {
		return "", fmt.Errorf("creative work OCR: 未配置视觉模型")
	}
	if len(image) == 0 {
		return "", fmt.Errorf("creative work OCR: 空图片")
	}
	raw, err := a.vision(ctx, image, creativeWorkOCRPrompt)
	if err != nil {
		return "", fmt.Errorf("creative work OCR: 视觉模型调用失败: %w", err)
	}
	var envelope struct {
		Text string `json:"text"`
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(extractJSON(raw))))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&envelope); err != nil {
		return "", fmt.Errorf("creative work OCR: 解析逐字转写失败: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return "", fmt.Errorf("creative work OCR: 模型返回了多余 JSON 内容")
	}
	if strings.TrimSpace(envelope.Text) == "" {
		return "", fmt.Errorf("creative work OCR: 未识别到可确认的原稿文字")
	}
	return envelope.Text, nil
}
