// Package engine 视频生成支持
//
// 通过 ai-core 的 VideoProvider 接口提交视频生成任务，
// 轮询任务状态直到完成，下载封面图转为 data URI，
// 视频 URL 通过 metadata 传递给前端渲染。
package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
)

// videoModelPrefixes 已知的视频生成模型前缀（API 调用方的兼容回退）
var videoModelPrefixes = []string{
	"cogvideox", // 智谱 CogVideoX 系列
	"sora",      // OpenAI Sora
	"kling",     // 快手可灵
	"wan",       // 通义万相
}

// isVideoGeneration 判断本次请求是否为视频生成
//
// 优先检查前端通过 metadata 传递的能力标记，
// 回退到模型名前缀匹配。
func isVideoGeneration(model string, metadata map[string]string) bool {
	if metadata != nil && metadata["video_generation"] == "true" {
		return true
	}
	lower := strings.ToLower(model)
	for _, prefix := range videoModelPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// videoTaskPollInterval 轮询间隔
const videoTaskPollInterval = 10 * time.Second

// generateVideo 通过 VideoProvider 接口提交任务并轮询到完成
//
// 返回视频 URL、封面 data URI。封面图下载失败时 coverDataURI 为空。
func generateVideo(ctx context.Context, provider hexagon.Provider, model, prompt string) (videoURL, coverDataURI string, err error) {
	vidProvider, ok := provider.(llm.VideoProvider)
	if !ok {
		return "", "", fmt.Errorf("provider %s 不支持视频生成", provider.Name())
	}

	// 1. 提交任务
	task, err := vidProvider.CreateVideoTask(ctx, llm.VideoRequest{
		Model:  model,
		Prompt: prompt,
	})
	if err != nil {
		return "", "", fmt.Errorf("提交视频任务失败: %w", err)
	}

	// 2. 轮询直到完成或失败
	//    先检查初始状态（任务可能立即失败），再进入定时轮询。
	ticker := time.NewTicker(videoTaskPollInterval)
	defer ticker.Stop()

	for {
		// 检查当前 task 状态
		switch task.Status {
		case llm.VideoTaskCompleted:
			if task.VideoURL == "" {
				return "", "", fmt.Errorf("视频任务已完成但未返回视频地址")
			}
			// 下载封面图为 data URI（失败不阻塞）
			if task.CoverURL != "" {
				if uri, dlErr := downloadAsDataURI(ctx, task.CoverURL); dlErr == nil {
					coverDataURI = uri
				}
			}
			return task.VideoURL, coverDataURI, nil

		case llm.VideoTaskFailed:
			errMsg := task.Error
			if errMsg == "" {
				errMsg = "未知错误"
			}
			return "", "", fmt.Errorf("视频生成失败: %s", errMsg)
		}

		// queued / processing → 等待下一次轮询
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-ticker.C:
			task, err = vidProvider.QueryVideoTask(ctx, task.ID)
			if err != nil {
				return "", "", fmt.Errorf("查询视频任务失败: %w", err)
			}
		}
	}
}

// formatVideoMarkdown 将视频结果格式化为 Markdown
//
// 封面图使用 data URI 内嵌（永不过期），视频 URL 作为链接。
func formatVideoMarkdown(videoURL, coverDataURI string) string {
	var b strings.Builder
	if coverDataURI != "" {
		b.WriteString(fmt.Sprintf("![视频封面](%s)\n\n", coverDataURI))
	}
	b.WriteString(fmt.Sprintf("[▶ 播放视频](%s)", videoURL))
	return b.String()
}
