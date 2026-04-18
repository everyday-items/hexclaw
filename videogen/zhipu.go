package videogen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 智谱视频生成 endpoint：
//   - POST {base}/videos/generations           — 异步提交，返回 task ID
//   - GET  {base}/async-result/{task_id}       — 查询任务状态
//
// 文档：https://bigmodel.cn/dev/api/videomodel/cogvideox
const (
	defaultZhipuVideoBase = "https://open.bigmodel.cn/api/paas/v4"
)

type zhipuCogVideoX struct {
	apiKey  string
	baseURL string
	models  []string
	httpc   *http.Client
	timeout time.Duration
}

// NewZhipuCogVideoX 创建智谱 CogVideoX Provider。
func NewZhipuCogVideoX(apiKey, baseURL string) Provider {
	if baseURL == "" {
		baseURL = defaultZhipuVideoBase
	}
	return &zhipuCogVideoX{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		// CogVideoX 在售模型；后续新模型可追加
		models:  []string{"cogvideox-2", "cogvideox-flash", "cogvideox"},
		httpc:   http.DefaultClient,
		timeout: 60 * time.Second, // 提交快，轮询另算
	}
}

func (z *zhipuCogVideoX) Name() string              { return "zhipu-cogvideox" }
func (z *zhipuCogVideoX) SupportedModels() []string { return z.models }

// 提交请求体匹配智谱 /videos/generations 协议。
type zhipuSubmitReq struct {
	Model     string `json:"model"`
	Prompt    string `json:"prompt,omitempty"`
	ImageURL  string `json:"image_url,omitempty"`
	Quality   string `json:"quality,omitempty"`
	WithAudio bool   `json:"with_audio,omitempty"`
	Size      string `json:"size,omitempty"`
	FPS       int    `json:"fps,omitempty"`
	Duration  int    `json:"duration,omitempty"`
	UserID    string `json:"user_id,omitempty"`
}

type zhipuSubmitResp struct {
	ID         string `json:"id"`
	Model      string `json:"model"`
	RequestID  string `json:"request_id"`
	TaskStatus string `json:"task_status"`
	Error      *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type zhipuPollResp struct {
	ID          string `json:"id"`
	Model       string `json:"model"`
	TaskStatus  string `json:"task_status"` // PROCESSING / SUCCESS / FAIL
	VideoResult []struct {
		URL      string `json:"url"`
		CoverURL string `json:"cover_image_url"`
	} `json:"video_result,omitempty"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (z *zhipuCogVideoX) Submit(ctx context.Context, req Request) (string, error) {
	if z.apiKey == "" {
		return "", fmt.Errorf("zhipu-cogvideox: 未配置 API Key")
	}
	body := zhipuSubmitReq{
		Model:     req.Model,
		Prompt:    req.Prompt,
		ImageURL:  req.ImageURL,
		Quality:   req.Quality,
		WithAudio: req.WithAudio,
		Size:      req.Size,
		FPS:       req.FPS,
		Duration:  req.Duration,
		UserID:    req.UserID,
	}
	if body.Model == "" && len(z.models) > 0 {
		body.Model = z.models[0]
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}

	rctx, cancel := context.WithTimeout(ctx, z.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(rctx, http.MethodPost,
		z.baseURL+"/videos/generations", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+z.apiKey)

	resp, err := z.httpc.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("submit: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode >= 400 {
		var er zhipuSubmitResp
		if json.Unmarshal(respBody, &er) == nil && er.Error != nil {
			return "", fmt.Errorf("zhipu HTTP %d: %s", resp.StatusCode, er.Error.Message)
		}
		return "", fmt.Errorf("zhipu HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var parsed zhipuSubmitResp
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("decode submit response: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("zhipu: %s", parsed.Error.Message)
	}
	if parsed.ID == "" {
		return "", fmt.Errorf("zhipu: 提交成功但未返回 task ID")
	}
	return parsed.ID, nil
}

func (z *zhipuCogVideoX) Poll(ctx context.Context, taskID string) (TaskStatus, error) {
	if z.apiKey == "" {
		return TaskStatus{}, fmt.Errorf("zhipu-cogvideox: 未配置 API Key")
	}
	rctx, cancel := context.WithTimeout(ctx, z.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(rctx, http.MethodGet,
		z.baseURL+"/async-result/"+taskID, nil)
	if err != nil {
		return TaskStatus{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+z.apiKey)

	resp, err := z.httpc.Do(httpReq)
	if err != nil {
		return TaskStatus{}, fmt.Errorf("poll: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if resp.StatusCode >= 400 {
		var er zhipuPollResp
		if json.Unmarshal(respBody, &er) == nil && er.Error != nil {
			return TaskStatus{}, fmt.Errorf("zhipu HTTP %d: %s", resp.StatusCode, er.Error.Message)
		}
		return TaskStatus{}, fmt.Errorf("zhipu HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var parsed zhipuPollResp
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return TaskStatus{}, fmt.Errorf("decode poll response: %w", err)
	}

	st := TaskStatus{
		Provider: z.Name(),
		Model:    parsed.Model,
		Status:   strings.ToLower(parsed.TaskStatus),
	}
	switch strings.ToUpper(parsed.TaskStatus) {
	case "SUCCESS":
		st.Done = true
		st.Status = "success"
		if len(parsed.VideoResult) > 0 {
			st.VideoURL = parsed.VideoResult[0].URL
			st.CoverURL = parsed.VideoResult[0].CoverURL
		}
	case "FAIL", "FAILED":
		st.Done = true
		st.Status = "failed"
		if parsed.Error != nil {
			st.Error = parsed.Error.Message
		} else {
			st.Error = "任务失败"
		}
	}
	return st, nil
}
