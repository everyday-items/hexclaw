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

	mediavid "github.com/hexagon-codes/ai-core/media/video"
	"github.com/hexagon-codes/hexagon/observe/trace"
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

// generateVideo 通过 ai-core/media 的视频服务提交任务并轮询到完成。
//
// media 视频服务按 model 路由 Provider，SubmitAndWait 内部以 Submit→Poll
// （media.WaitFor 同步包装）轮询到终态。返回视频 URL、封面 data URI；
// 封面图下载失败时 coverDataURI 为空。
func generateVideo(ctx context.Context, svc *mediavid.Service, model, prompt string) (videoURL, coverDataURI string, err error) {
	started := time.Now()
	trace.L(ctx).Info("video provider stage started", "stage", "provider", "model", model, "prompt", prompt)
	if svc == nil || !svc.HasProvider() {
		err := fmt.Errorf("未配置视频生成服务（model=%s）", model)
		trace.L(ctx).Warn("video provider stage failed", "stage", "provider", "model", model, "reason", "provider_unavailable", "err", err, "elapsed_ms", time.Since(started).Milliseconds())
		return "", "", err
	}

	providerStarted := time.Now()
	st, err := svc.SubmitAndWait(ctx, "", mediavid.Request{Model: model, Prompt: prompt}, videoTaskPollInterval)
	if err != nil {
		trace.L(ctx).Warn("video provider stage failed", "stage", "provider", "model", model, "reason", "generation_error", "err", err, "elapsed_ms", time.Since(providerStarted).Milliseconds(), "total_ms", time.Since(started).Milliseconds())
		return "", "", fmt.Errorf("视频生成失败: %w", err)
	}
	videoURLForLog := st.VideoURL
	if strings.HasPrefix(strings.ToLower(videoURLForLog), "data:") {
		videoURLForLog = "[omitted: base64 media]"
	}
	coverURLForLog := st.CoverURL
	if strings.HasPrefix(strings.ToLower(coverURLForLog), "data:") {
		coverURLForLog = "[omitted: base64 media]"
	}
	if st.Status == "failed" || st.Error != "" {
		errMsg := st.Error
		if errMsg == "" {
			errMsg = "未知错误"
		}
		trace.L(ctx).Warn("video provider stage failed", "stage", "provider", "model", model, "reason", "provider_failed", "status", st.Status, "video_url", videoURLForLog, "cover_url", coverURLForLog, "err", errMsg, "elapsed_ms", time.Since(providerStarted).Milliseconds(), "total_ms", time.Since(started).Milliseconds())
		return "", "", fmt.Errorf("视频生成失败: %s", errMsg)
	}
	if st.VideoURL == "" {
		err := fmt.Errorf("视频任务已完成但未返回视频地址")
		trace.L(ctx).Warn("video provider stage failed", "stage", "provider", "model", model, "reason", "missing_video", "status", st.Status, "cover_url", coverURLForLog, "err", err, "elapsed_ms", time.Since(providerStarted).Milliseconds(), "total_ms", time.Since(started).Milliseconds())
		return "", "", err
	}
	trace.L(ctx).Info("video provider stage completed", "stage", "provider", "model", model, "status", st.Status, "video_url", videoURLForLog, "cover_url", coverURLForLog, "elapsed_ms", time.Since(providerStarted).Milliseconds())

	// 下载封面图为 data URI（失败不阻塞）
	if st.CoverURL != "" {
		coverStarted := time.Now()
		trace.L(ctx).Info("video cover materialization started", "stage", "cover_materialize", "model", model, "cover_url", coverURLForLog)
		if uri, dlErr := downloadAsDataURI(ctx, st.CoverURL); dlErr == nil {
			coverDataURI = uri
			trace.L(ctx).Info("video cover materialization completed", "stage", "cover_materialize", "model", model, "cover_url", coverURLForLog, "elapsed_ms", time.Since(coverStarted).Milliseconds())
		} else {
			trace.L(ctx).Warn("video cover materialization failed", "stage", "cover_materialize", "model", model, "cover_url", coverURLForLog, "err", dlErr, "elapsed_ms", time.Since(coverStarted).Milliseconds())
		}
	}
	return st.VideoURL, coverDataURI, nil
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
