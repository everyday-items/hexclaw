package engineadapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// TestRealBBoxRecognition is an opt-in diagnostic/release gate for the real worksheet used by
// the desktop photo-grading E2E. It intentionally logs every raw vision response so a zero-bbox
// result can be attributed to recognition geometry or to the semantic honesty gate.
func TestRealBBoxRecognition(t *testing.T) {
	if os.Getenv("HEXCLAW_REAL_BBOX_EVAL") != "1" {
		t.Skip("set HEXCLAW_REAL_BBOX_EVAL=1 to run the real vision bbox gate")
	}
	imagePath := strings.TrimSpace(os.Getenv("HEXCLAW_REAL_BBOX_IMAGE"))
	if imagePath == "" {
		imagePath = "/Users/guoyanjun/Downloads/作业测试做好的需批改.jpg"
	}
	imageBytes, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	appCfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	selector, err := llmrouter.New(appCfg.LLM)
	if err != nil {
		t.Fatal(err)
	}
	provider := selector.Default()
	if provider == nil {
		t.Fatal("default provider is nil")
	}
	model := selector.ProviderModel(selector.DefaultName())
	t.Logf("provider=%s model=%s image=%s bytes=%d", provider.Name(), model, imagePath, len(imageBytes))

	var call atomic.Int32
	startedAt := time.Now()
	vision := func(ctx context.Context, image []byte, prompt string) (string, error) {
		n := call.Add(1)
		semantic := strings.Contains(prompt, "bbox 二次语义核验")
		if semantic {
			path := fmt.Sprintf("/tmp/hexclaw-bbox-crop-%02d.png", n)
			if writeErr := os.WriteFile(path, image, 0o600); writeErr != nil {
				t.Logf("write semantic contact sheet: %v", writeErr)
			} else {
				t.Logf("call=%d semantic contact sheet=%s bytes=%d", n, path, len(image))
			}
		}
		mime := http.DetectContentType(image)
		dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(image)
		callCtx, cancel := context.WithTimeout(ctx, 150*time.Second)
		defer cancel()
		resp, completeErr := provider.Complete(callCtx, llm.CompletionRequest{
			Model: model,
			Messages: []llm.Message{{
				Role: llm.RoleUser,
				MultiContent: []llm.ContentPart{
					llm.NewTextPart(prompt),
					llm.NewImageURLPart(dataURL, "auto"),
				},
			}},
		})
		if completeErr != nil {
			t.Logf("call=%d semantic=%t error=%v", n, semantic, completeErr)
			return "", completeErr
		}
		t.Logf("call=%d semantic=%t raw=%s", n, semantic, resp.Content)
		return resp.Content, nil
	}

	questions, err := NewRecognizerAdapter(vision).Recognize(context.Background(), imageBytes)
	if err != nil {
		t.Fatal(err)
	}
	boxed := 0
	targets := make([]semanticBBoxExpectation, 0, len(questions))
	for i, question := range questions {
		if question.BBox != nil {
			boxed++
		}
		t.Logf("result[%d] question=%q answer=%q bbox=%+v", i+1, question.Question, question.StudentAnswer, question.BBox)
		if strings.TrimSpace(question.StudentAnswer) != "" {
			targets = append(targets, semanticBBoxExpectation{Index: i + 1, Question: question.Question, StudentAnswer: question.StudentAnswer})
		}
	}
	t.Logf("vision_calls=%d elapsed=%s retained=%d/%d", call.Load(), time.Since(startedAt).Round(time.Millisecond), boxed, len(questions))
	if os.Getenv("HEXCLAW_REAL_BBOX_PROBE") == "1" {
		targetJSON, marshalErr := json.Marshal(targets)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		probe, probeErr := vision(context.Background(), imageBytes, fmt.Sprintf(`这不是识题，不要解题，只在原图上定位已知学生手写答案的像素区域。
原图尺寸是 1280x1707。下列 JSON 给出已另行识别的手写 student_answer：
%s
对每项在原图寻找与 student_answer 真实一致的手写墨迹，返回 bbox_1000=[left,top,right,bottom]，坐标范围 0..1000，紧贴包围该题的学生手写答案。印刷题干、标题、别题答案都不能算。看不清或找不到完全一致的手写答案时 bbox_1000 必须为 null。严格输出 JSON 数组，不要解释：
[{"index":1,"bbox_1000":[120,150,240,200]}]`, targetJSON))
		t.Logf("focused localization err=%v raw=%s", probeErr, probe)
	}
	if boxed == 0 {
		t.Fatalf("real answered worksheet retained 0/%d semantic bboxes", len(questions))
	}
}

func TestRealBBoxKnownCropOCR(t *testing.T) {
	if os.Getenv("HEXCLAW_REAL_BBOX_EVAL") != "1" {
		t.Skip("set HEXCLAW_REAL_BBOX_EVAL=1 to run the real vision bbox gate")
	}
	imagePath := strings.TrimSpace(os.Getenv("HEXCLAW_REAL_BBOX_IMAGE"))
	if imagePath == "" {
		imagePath = "/Users/guoyanjun/Downloads/作业测试做好的需批改.jpg"
	}
	imageBytes, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	src, _, err := image.Decode(bytes.NewReader(imageBytes))
	if err != nil {
		t.Fatal(err)
	}
	appCfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	selector, err := llmrouter.New(appCfg.LLM)
	if err != nil {
		t.Fatal(err)
	}
	provider := selector.Default()
	model := selector.ProviderModel(selector.DefaultName())
	vision := func(ctx context.Context, imageBytes []byte, prompt string) (string, error) {
		mime := http.DetectContentType(imageBytes)
		dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(imageBytes)
		resp, callErr := provider.Complete(ctx, llm.CompletionRequest{
			Model: model,
			Messages: []llm.Message{{Role: llm.RoleUser, MultiContent: []llm.ContentPart{
				llm.NewTextPart(prompt), llm.NewImageURLPart(dataURL, "auto"),
			}}},
		})
		if callErr != nil {
			return "", callErr
		}
		t.Logf("known crop raw=%s", resp.Content)
		return resp.Content, nil
	}

	tests := []usecase.RecognizedQuestion{
		{Question: "4÷0.5=", StudentAnswer: "8", BBox: &usecase.BBox{X: 0.10, Y: 0.14, W: 0.15, H: 0.07}},
		{Question: "15.02-6.8-1.02", StudentAnswer: "=14-6.8=7.2", BBox: &usecase.BBox{X: 0.37, Y: 0.28, W: 0.22, H: 0.14}},
	}
	for _, question := range tests {
		sheet, candidates, buildErr := buildSemanticBBoxContactSheet(src, []usecase.RecognizedQuestion{question})
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		accepted, ok := NewRecognizerAdapter(vision).requestSemanticBBoxVerdicts(context.Background(), sheet, []usecase.RecognizedQuestion{question}, candidates)
		if !ok || !accepted[0] {
			t.Errorf("known correct crop was not independently OCR-verified: question=%q accepted=%v response_ok=%t", question.Question, accepted, ok)
		}
	}
}
