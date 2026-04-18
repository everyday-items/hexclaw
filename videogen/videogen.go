// Package videogen 提供文本到视频生成能力。
//
// 视频生成与图像生成的关键差异：
//   - 长任务（智谱 CogVideoX 单次 30s-2min，OpenAI Sora 几分钟）
//   - 普遍走"提交 → 轮询 → 结果"两阶段 API（不能像 DALL-E 一样同步返回）
//
// 因此 Provider 接口分两步：Submit → Poll。前端调用方负责轮询节奏。
package videogen

import (
	"context"
	"fmt"
	"strings"
)

// Provider 文生视频 Provider 接口。
type Provider interface {
	// Name 返回 Provider 名称（如 "zhipu-cogvideox"）
	Name() string
	// Submit 提交生成任务，返回任务 ID（异步 Provider）。
	// 同步 Provider 可在此处直接完成生成，返回 ID 供 Poll 立即返回 done=true。
	Submit(ctx context.Context, req Request) (taskID string, err error)
	// Poll 查询任务状态。done=true 时 Result 应有可用结果。
	Poll(ctx context.Context, taskID string) (status TaskStatus, err error)
	// SupportedModels 返回支持的模型 ID 列表
	SupportedModels() []string
}

// Request 视频生成请求。
type Request struct {
	Model     string `json:"model"`                // 模型 ID（如 "cogvideox-2"）
	Prompt    string `json:"prompt"`               // 文本提示词
	ImageURL  string `json:"image_url,omitempty"`  // 首帧图（图生视频）
	Quality   string `json:"quality,omitempty"`    // "speed" / "quality"（CogVideoX）
	WithAudio bool   `json:"with_audio,omitempty"` // 是否生成音轨
	Duration  int    `json:"duration,omitempty"`   // 时长秒（部分模型支持）
	FPS       int    `json:"fps,omitempty"`        // 帧率
	Size      string `json:"size,omitempty"`       // 分辨率 1280x720
	UserID    string `json:"user_id,omitempty"`    // 终端用户标识
}

// TaskStatus 任务状态。
type TaskStatus struct {
	TaskID        string  `json:"task_id"`
	Provider      string  `json:"provider"`
	Model         string  `json:"model"`
	Status        string  `json:"status"`                    // queueing / running / success / failed
	Done          bool    `json:"done"`                      // status==success / failed
	VideoURL      string  `json:"video_url,omitempty"`       // Provider 给的临时 URL（24h 过期）
	VideoFilePath string  `json:"video_file_path,omitempty"` // 持久化路径（成功后由 handler 落盘）
	CoverURL      string  `json:"cover_url,omitempty"`       // 封面图 URL（部分 Provider 提供）
	CoverFilePath string  `json:"cover_file_path,omitempty"` // 封面图持久化路径
	Error         string  `json:"error,omitempty"`
	Progress      float64 `json:"progress,omitempty"`
	UsageMs       int64   `json:"usage_ms,omitempty"`
}

// Service 视频生成服务（多 Provider 路由）。
type Service struct {
	providers       map[string]Provider
	modelIndex      map[string]Provider
	defaultProvider string
}

// NewService 创建视频生成服务。
func NewService(providers map[string]Provider, defaultName string) *Service {
	s := &Service{
		providers:       make(map[string]Provider, len(providers)),
		modelIndex:      make(map[string]Provider),
		defaultProvider: defaultName,
	}
	for k, p := range providers {
		if p == nil {
			continue
		}
		s.providers[k] = p
		for _, m := range p.SupportedModels() {
			if _, exists := s.modelIndex[m]; !exists {
				s.modelIndex[m] = p
			}
		}
	}
	return s
}

func (s *Service) resolveProvider(name, model string) Provider {
	if name != "" {
		if p, ok := s.providers[name]; ok {
			return p
		}
	}
	if model != "" {
		if p, ok := s.modelIndex[model]; ok {
			return p
		}
	}
	if s.defaultProvider != "" {
		return s.providers[s.defaultProvider]
	}
	return nil
}

// resolveByTaskID 反查任务所属 Provider。
//
// 任务 ID 由 Service 在 Submit 后包装为 "{providerName}::{rawTaskID}"，
// Poll 时解析回原 Provider，确保前端不需要记忆 Provider。
func (s *Service) resolveByTaskID(taskID string) (Provider, string, error) {
	name, raw, ok := strings.Cut(taskID, "::")
	if !ok {
		return nil, "", fmt.Errorf("任务 ID 格式错误（缺少 provider 前缀）")
	}
	p, known := s.providers[name]
	if !known {
		return nil, "", fmt.Errorf("未知 Provider %q", name)
	}
	return p, raw, nil
}

// Submit 提交生成任务。返回带 Provider 前缀的任务 ID（"{provider}::{rawID}"）。
func (s *Service) Submit(ctx context.Context, providerName string, req Request) (string, error) {
	if req.Prompt == "" && req.ImageURL == "" {
		return "", fmt.Errorf("prompt 与 image_url 至少需要一个")
	}
	p := s.resolveProvider(providerName, req.Model)
	if p == nil {
		return "", fmt.Errorf("未找到可用的视频生成 Provider（请求 provider=%q model=%q）", providerName, req.Model)
	}
	rawID, err := p.Submit(ctx, req)
	if err != nil {
		return "", err
	}
	return p.Name() + "::" + rawID, nil
}

// Poll 查询任务状态。
func (s *Service) Poll(ctx context.Context, taskID string) (TaskStatus, error) {
	p, raw, err := s.resolveByTaskID(taskID)
	if err != nil {
		return TaskStatus{}, err
	}
	st, err := p.Poll(ctx, raw)
	if err != nil {
		return TaskStatus{}, err
	}
	st.TaskID = taskID // 返回带前缀 ID 给前端透传
	return st, nil
}

// HasProvider 是否注册了任意 Provider。
func (s *Service) HasProvider() bool { return len(s.providers) > 0 }

// Providers 返回 Provider 名列表。
func (s *Service) Providers() []string {
	names := make([]string, 0, len(s.providers))
	for k := range s.providers {
		names = append(names, k)
	}
	return names
}

// Models 返回所有支持的模型 ID。
func (s *Service) Models() []string {
	models := make([]string, 0, len(s.modelIndex))
	for m := range s.modelIndex {
		models = append(models, m)
	}
	return models
}
