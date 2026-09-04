package api

import (
	"container/list"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	videogen "github.com/hexagon-codes/ai-core/media/video"
	"github.com/hexagon-codes/toolkit/util/logger"
	"golang.org/x/sync/singleflight"
)

// videoDownloadCache 是带 LRU 淘汰的任务下载结果缓存：
//   - 线程安全，支持 RLock 快路径 / Lock 写路径
//   - 最大 capacity 条（超过淘汰最旧），防止长期运行时 map 单调膨胀
//   - 进程重启清空（genStore.SaveBytes 内容寻址会去重写入，最坏多下载一次）
//
// singleflight 在 cache miss 时保证同一 task_id 的下载只执行一次，并发 poll 共享结果；
// 避免"两个标签轮询同一任务 → 各自触发 5min 下载"的带宽浪费。
const videoDownloadCacheCap = 1024

type videoDownloadRecord struct {
	VideoFilePath string
	CoverFilePath string
}

var (
	videoDownloadCache   = make(map[string]*list.Element, videoDownloadCacheCap)
	videoDownloadLRU     = list.New() // front=最新，back=最旧
	videoDownloadCacheMu sync.Mutex
	videoDownloadFlight  singleflight.Group
)

type videoDownloadEntry struct {
	taskID string
	rec    videoDownloadRecord
}

// videoDownloadLookup 线程安全读取缓存；hit=true 时同时 LRU touch 到 front。
func videoDownloadLookup(taskID string) (videoDownloadRecord, bool) {
	videoDownloadCacheMu.Lock()
	defer videoDownloadCacheMu.Unlock()
	elem, ok := videoDownloadCache[taskID]
	if !ok {
		return videoDownloadRecord{}, false
	}
	videoDownloadLRU.MoveToFront(elem)
	return elem.Value.(*videoDownloadEntry).rec, true
}

// videoDownloadStore 写入 + 超容淘汰最旧。
func videoDownloadStore(taskID string, rec videoDownloadRecord) {
	videoDownloadCacheMu.Lock()
	defer videoDownloadCacheMu.Unlock()
	if elem, ok := videoDownloadCache[taskID]; ok {
		elem.Value.(*videoDownloadEntry).rec = rec
		videoDownloadLRU.MoveToFront(elem)
		return
	}
	entry := &videoDownloadEntry{taskID: taskID, rec: rec}
	elem := videoDownloadLRU.PushFront(entry)
	videoDownloadCache[taskID] = elem
	// 超容 → 踢最旧
	for videoDownloadLRU.Len() > videoDownloadCacheCap {
		old := videoDownloadLRU.Back()
		if old == nil {
			break
		}
		videoDownloadLRU.Remove(old)
		delete(videoDownloadCache, old.Value.(*videoDownloadEntry).taskID)
	}
}

// --- 视频生成 API ---

// handleVideoGenStatus GET /api/v1/videos/status
func (s *Server) handleVideoGenStatus(w http.ResponseWriter, r *http.Request) {
	enabled := s.videogenSvc != nil && s.videogenSvc.HasProvider()
	resp := map[string]any{
		"enabled":   enabled,
		"providers": []string{},
		"models":    []string{},
	}
	if enabled {
		resp["providers"] = s.videogenSvc.Providers()
		resp["models"] = s.videogenSvc.Models()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleVideoGenSubmit POST /api/v1/videos/generate
//
// 请求体：
//
//	{
//	  "provider": "zhipu-cogvideox",     // 可选
//	  "model":    "cogvideox-2",
//	  "prompt":   "...",                  // 必填（与 image_url 二选一）
//	  "image_url": "...",                 // 首帧图，可选
//	  "size":      "1280x720",            // 可选
//	  "with_audio": true,                 // 可选
//	  "duration":  6                      // 秒
//	}
//
// 响应：{ "task_id": "..." } — 调用方持有 task_id 后轮询 /api/v1/videos/tasks/{id}
func (s *Server) handleVideoGenSubmit(w http.ResponseWriter, r *http.Request) {
	if s.videogenSvc == nil || !s.videogenSvc.HasProvider() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "视频生成服务未配置",
		})
		return
	}

	const maxBody = 64 << 10
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	var req struct {
		Provider string `json:"provider,omitempty"`
		videogen.Request
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}

	requestRef := req.IdempotencyKey
	if requestRef == "" {
		requestRef = r.Header.Get("Idempotency-Key")
	}
	if requestRef == "" {
		requestRef = r.Header.Get("X-Request-ID")
	}
	requestRef = mediaLogRef(requestRef)
	providerName := req.Provider
	modelName := req.Model
	submitStarted := time.Now()
	logger.InfoContext(r.Context(), "[media] stage",
		"media_kind", "video", "request_ref", requestRef,
		"provider", providerName, "model", modelName,
		"stage", "submit", "status", "started",
		"elapsed_ms", int64(0), "result_count", 0)
	taskID, err := s.videogenSvc.Submit(r.Context(), req.Provider, req.Request)
	if err != nil {
		submitStatus := "failed"
		errorType := "provider_error"
		if r.Context().Err() != nil {
			submitStatus = "cancelled"
			errorType = "context_cancelled"
		}
		logger.WarnContext(r.Context(), "[media] stage",
			"media_kind", "video", "request_ref", requestRef,
			"provider", providerName, "model", modelName,
			"stage", "submit", "status", submitStatus,
			"elapsed_ms", time.Since(submitStarted).Milliseconds(), "result_count", 0,
			"error_type", errorType)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "提交失败: " + err.Error()})
		return
	}
	if providerName == "" {
		if provider, _, ok := strings.Cut(taskID, "::"); ok {
			providerName = provider
		}
	}
	taskRef := mediaLogRef(taskID)
	logger.InfoContext(r.Context(), "[media] stage",
		"media_kind", "video", "task_ref", taskRef,
		"provider", providerName, "model", modelName,
		"stage", "submit", "status", "completed",
		"elapsed_ms", time.Since(submitStarted).Milliseconds(), "result_count", 0)
	writeJSON(w, http.StatusOK, map[string]string{"task_id": taskID})
}

// handleVideoGenPoll GET /api/v1/videos/tasks/{id}
//
// 返回 videogen.TaskStatus；done=true 时含 video_url。
func (s *Server) handleVideoGenPoll(w http.ResponseWriter, r *http.Request) {
	if s.videogenSvc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "视频生成服务未配置"})
		return
	}
	taskID := r.PathValue("id")
	if taskID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 task ID"})
		return
	}
	taskRef := mediaLogRef(taskID)
	providerName := ""
	if provider, _, ok := strings.Cut(taskID, "::"); ok {
		providerName = provider
	}
	pollStarted := time.Now()
	status, err := s.videogenSvc.Poll(r.Context(), taskID)
	if err != nil {
		pollStatus := "failed"
		errorType := "provider_error"
		if r.Context().Err() != nil {
			pollStatus = "cancelled"
			errorType = "context_cancelled"
		}
		logger.WarnContext(r.Context(), "[media] stage",
			"media_kind", "video", "task_ref", taskRef,
			"provider", providerName, "model", "",
			"stage", "poll", "status", pollStatus,
			"elapsed_ms", time.Since(pollStarted).Milliseconds(), "result_count", 0,
			"error_type", errorType)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败: " + err.Error()})
		return
	}
	if status.Provider != "" {
		providerName = status.Provider
	}
	if status.Done {
		pollResultCount := 0
		if status.Status == "success" && (status.VideoURL != "" || status.VideoFilePath != "") {
			pollResultCount = 1
		}
		if status.Status == "success" {
			logger.InfoContext(r.Context(), "[media] stage",
				"media_kind", "video", "task_ref", taskRef,
				"provider", providerName, "model", status.Model,
				"stage", "poll", "status", "completed",
				"elapsed_ms", time.Since(pollStarted).Milliseconds(), "result_count", pollResultCount)
		} else {
			logger.WarnContext(r.Context(), "[media] stage",
				"media_kind", "video", "task_ref", taskRef,
				"provider", providerName, "model", status.Model,
				"stage", "poll", "status", "failed",
				"elapsed_ms", time.Since(pollStarted).Milliseconds(), "result_count", 0,
				"error_type", "provider_error")
		}
	}

	// 任务成功 → 立即下载视频/封面到本地，避免 Provider URL 24h 过期。
	// 幂等：cache 命中直接返回；未命中走 singleflight，并发 poll 共享同一次下载。
	if status.Done && status.Status == "success" && s.genStore != nil {
		persistStarted := time.Now()
		logger.InfoContext(r.Context(), "[media] stage",
			"media_kind", "video", "task_ref", taskRef,
			"provider", providerName, "model", status.Model,
			"stage", "persist", "status", "started",
			"elapsed_ms", int64(0), "result_count", 0)
		var flightErr error
		if cached, hit := videoDownloadLookup(taskID); hit {
			status.VideoFilePath = cached.VideoFilePath
			status.CoverFilePath = cached.CoverFilePath
		} else {
			// singleflight key = taskID；在途期间同一 taskID 的其它 poll 共享此次结果
			result, downloadErr, _ := videoDownloadFlight.Do(taskID, func() (any, error) {
				// 进入 flight 后再查一次 cache，避免在等待期间其它 goroutine 已完成
				if cached, hit := videoDownloadLookup(taskID); hit {
					return cached, nil
				}
				rec := videoDownloadRecord{}
				var videoErr error
				if status.VideoURL != "" {
					saved, dlErr := s.genStore.SaveFromURL(r.Context(), status.VideoURL, "mp4")
					if dlErr != nil {
						videoErr = dlErr
					} else {
						rec.VideoFilePath = saved
					}
				}
				if status.CoverURL != "" {
					saved, dlErr := s.genStore.SaveFromURL(r.Context(), status.CoverURL, "jpg")
					if dlErr == nil {
						rec.CoverFilePath = saved
					}
				}
				// 只要视频成功就入缓存；失败仅返回（不缓存 → 下次 poll 可重试）
				if rec.VideoFilePath != "" {
					videoDownloadStore(taskID, rec)
				} else if videoErr != nil {
					// 视频失败但 Provider 仍返回 video_url，让前端回退到临时 URL（24h 有效）
					return rec, videoErr
				}
				return rec, nil
			})
			flightErr = downloadErr

			if rec, ok := result.(videoDownloadRecord); ok {
				status.VideoFilePath = rec.VideoFilePath
				status.CoverFilePath = rec.CoverFilePath
				if rec.VideoFilePath == "" && status.VideoURL != "" {
					// 下载失败，前端将回退到临时 URL
					status.Error = "视频下载失败（可用临时 URL 播放但 24h 后失效）"
				}
			}
		}
		resultCount := 0
		if status.VideoFilePath != "" {
			resultCount = 1
		}
		persistFailed := flightErr != nil ||
			(status.VideoURL != "" && status.VideoFilePath == "") ||
			(status.CoverURL != "" && status.CoverFilePath == "")
		persistStatus := "completed"
		if persistFailed {
			persistStatus = "failed"
		}
		elapsedMS := time.Since(persistStarted).Milliseconds()
		if persistFailed {
			errorType := "persist_incomplete"
			if flightErr != nil {
				errorType = "store_error"
			}
			logger.WarnContext(r.Context(), "[media] stage",
				"media_kind", "video", "task_ref", taskRef,
				"provider", providerName, "model", status.Model,
				"stage", "persist", "status", persistStatus,
				"elapsed_ms", elapsedMS, "result_count", resultCount,
				"error_type", errorType)
		} else {
			logger.InfoContext(r.Context(), "[media] stage",
				"media_kind", "video", "task_ref", taskRef,
				"provider", providerName, "model", status.Model,
				"stage", "persist", "status", persistStatus,
				"elapsed_ms", elapsedMS, "result_count", resultCount)
		}
	}

	writeJSON(w, http.StatusOK, status)
}
