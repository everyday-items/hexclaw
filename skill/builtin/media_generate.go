package builtin

// 媒体生成 Skill —— text→image / text→video。
//
// 行为：
//   - Skill 直调子系统（imagegen/videogen + blobstore），不经过 LLM。
//   - 图片：Generate → persistImages 落盘（b64 走 SaveBytes / url 走 SaveFromURL）→ 回填 FilePath，
//     返回稳定相对路径（前端拼 /api/v1/files/generated/{path}），避免 Provider URL 过期 / b64 撑爆。
//   - 视频：Submit → pollUntilDone（ctx 截止 = cron runBudget 兜底，绝不无限等）→ SaveFromURL 落盘。
//   - 未配置（service nil / !HasProvider）→ 明确报错，不静默。

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	imagegen "github.com/hexagon-codes/ai-core/media/image"
	videogen "github.com/hexagon-codes/ai-core/media/video"
	"github.com/hexagon-codes/hexclaw/skill"
	genstore "github.com/hexagon-codes/toolkit/blobstore"
	"github.com/hexagon-codes/toolkit/util/logger"
)

// videoPollInterval 是视频任务轮询间隔。Submit 后阻塞轮询到 Done，靠 ctx 截止兜底。
const videoPollInterval = 3 * time.Second

// imageGenerator 是 *imagegen.Service 的窄注入面（便于测试 + 防 typed-nil）。
type imageGenerator interface {
	Generate(ctx context.Context, providerName string, req imagegen.Request) (*imagegen.Result, error)
	HasProvider() bool
}

// videoGenerator 是 *videogen.Service 的窄注入面。
type videoGenerator interface {
	Submit(ctx context.Context, providerName string, req videogen.Request) (string, error)
	Poll(ctx context.Context, taskID string) (videogen.TaskStatus, error)
	HasProvider() bool
}

// blobStore 是 *blobstore.Store 的窄注入面（落盘拿稳定相对路径）。
type blobStore interface {
	SaveBytes(data []byte, ext string) (string, error)
	SaveFromURL(ctx context.Context, url, ext string) (string, error)
}

// MediaGenerateSkill 从文本提示词生成图片（默认）或视频，返回落盘后的稳定文件路径。
type MediaGenerateSkill struct {
	img   imageGenerator
	vid   videoGenerator
	store blobStore

	// testFastPoll 仅供测试：把视频轮询间隔压到极短，避免单测等真实 3s/次。
	testFastPoll bool
}

// NewMediaGenerateSkill 构造媒体生成 Skill。
//
// 接收具体类型，内部存为窄接口。注意 typed-nil 陷阱：
// 一个 nil 的 *imagegen.Service 装进非空接口后 != nil，故只在非空时装箱，使下游 s.img == nil 判定成立。
func NewMediaGenerateSkill(img *imagegen.Service, vid *videogen.Service, store *genstore.Store) *MediaGenerateSkill {
	s := &MediaGenerateSkill{}
	if img != nil {
		s.img = img
	}
	if vid != nil {
		s.vid = vid
	}
	if store != nil {
		s.store = store
	}
	return s
}

// newMediaGenerateSkillWith 是测试用构造（直接注入窄接口的 fake）。
func newMediaGenerateSkillWith(img imageGenerator, vid videoGenerator, store blobStore) *MediaGenerateSkill {
	return &MediaGenerateSkill{img: img, vid: vid, store: store}
}

func (s *MediaGenerateSkill) Name() string { return "media_generate" }
func (s *MediaGenerateSkill) Description() string {
	return "Generate an image or video from a text prompt"
}

// Match 只走 LLM tool-call 主路径，无关键词快速路径。
func (s *MediaGenerateSkill) Match(string) bool { return false }

func (s *MediaGenerateSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("media_generate",
		"Generate an image (default) or video from a text prompt; returns a stable file path "+
			"(persisted under generated/) usable by export/send/ingest. You MUST call this to actually "+
			"produce media — describing it in text creates nothing.",
		&llm.Schema{Type: "object", Properties: map[string]*llm.Schema{
			"kind":       {Type: "string", Description: `"image" (default) or "video"`},
			"prompt":     {Type: "string", Description: "what to generate (required)"},
			"model":      {Type: "string", Description: "optional model id"},
			"provider":   {Type: "string", Description: "optional provider name"},
			"size":       {Type: "string", Description: `e.g. "1024x1024"`},
			"n":          {Type: "integer", Description: "image count, default 1"},
			"style":      {Type: "string", Description: `"vivid"/"natural" (image)`},
			"quality":    {Type: "string", Description: `"standard"/"hd" (image)`},
			"with_audio": {Type: "boolean", Description: "video only"},
		}, Required: []string{"prompt"}})
}

func (s *MediaGenerateSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	prompt := firstStringArg(args, "prompt")
	if prompt == "" {
		return nil, fmt.Errorf("prompt must not be empty")
	}
	kind := firstStringArg(args, "kind")
	if kind == "" {
		kind = "image"
	}
	switch kind {
	case "image":
		return s.genImage(ctx, args, prompt)
	case "video":
		return s.genVideo(ctx, args, prompt)
	default:
		return nil, fmt.Errorf("unknown kind %q (use image|video)", kind)
	}
}

func (s *MediaGenerateSkill) genImage(ctx context.Context, args map[string]any, prompt string) (*skill.Result, error) {
	if s.img == nil || !s.img.HasProvider() {
		return nil, fmt.Errorf("image generation is not configured (enable an imagegen Provider)")
	}
	requestRef := skill.SystemDispatchTaskRef(ctx)
	providerName := firstStringArg(args, "provider")
	modelName := firstStringArg(args, "model")
	req := imagegen.Request{
		Prompt:  prompt,
		Model:   modelName,
		Size:    firstStringArg(args, "size"),
		N:       intArg(args, "n", 1),
		Style:   firstStringArg(args, "style"),
		Quality: firstStringArg(args, "quality"),
	}
	providerStarted := time.Now()
	logger.InfoContext(ctx, "[media] stage",
		"media_kind", "image", "request", requestRef,
		"provider", providerName, "model", modelName,
		"stage", "provider_wait", "status", "started",
		"elapsed_ms", int64(0), "result_count", 0)
	providerHeartbeatDone := make(chan struct{})
	var providerHeartbeatWG sync.WaitGroup
	providerHeartbeatWG.Add(1)
	go func() {
		defer providerHeartbeatWG.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				logger.InfoContext(ctx, "[media] stage",
					"media_kind", "image", "request", requestRef,
					"provider", providerName, "model", modelName,
					"stage", "provider_wait", "status", "heartbeat",
					"elapsed_ms", time.Since(providerStarted).Milliseconds(), "result_count", 0)
			case <-providerHeartbeatDone:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	res, err := s.img.Generate(ctx, providerName, req)
	close(providerHeartbeatDone)
	providerHeartbeatWG.Wait()
	if err != nil {
		waitStatus := "failed"
		if ctx.Err() != nil {
			waitStatus = "cancelled"
		}
		logger.WarnContext(ctx, "[media] stage",
			"media_kind", "image", "request", requestRef,
			"provider", providerName, "model", modelName,
			"stage", "provider_wait", "status", waitStatus,
			"elapsed_ms", time.Since(providerStarted).Milliseconds(), "result_count", 0)
		return nil, fmt.Errorf("image generation failed: %w", err)
	}
	resultCount := 0
	if res != nil {
		if res.Provider != "" {
			providerName = res.Provider
		}
		if res.Model != "" {
			modelName = res.Model
		}
		if requestRef == "" && res.RequestID != "" {
			requestRef = res.RequestID
		}
		resultCount = len(res.Images)
	}
	logger.InfoContext(ctx, "[media] stage",
		"media_kind", "image", "request", requestRef,
		"provider", providerName, "model", modelName,
		"stage", "provider_wait", "status", "completed",
		"elapsed_ms", time.Since(providerStarted).Milliseconds(), "result_count", resultCount)

	if s.store != nil && res != nil {
		materializeStarted := time.Now()
		logger.InfoContext(ctx, "[media] stage",
			"media_kind", "image", "request", requestRef,
			"provider", providerName, "model", modelName,
			"stage", "materialize", "status", "started",
			"elapsed_ms", int64(0), "result_count", 0)
		logger.InfoContext(ctx, "[media] stage",
			"media_kind", "image", "request", requestRef,
			"provider", providerName, "model", modelName,
			"stage", "persist", "status", "started",
			"elapsed_ms", int64(0), "result_count", 0)
		materializeHeartbeatDone := make(chan struct{})
		var materializeHeartbeatWG sync.WaitGroup
		materializeHeartbeatWG.Add(1)
		go func() {
			defer materializeHeartbeatWG.Done()
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					logger.InfoContext(ctx, "[media] stage",
						"media_kind", "image", "request", requestRef,
						"provider", providerName, "model", modelName,
						"stage", "materialize", "status", "heartbeat",
						"elapsed_ms", time.Since(materializeStarted).Milliseconds(), "result_count", 0)
				case <-materializeHeartbeatDone:
					return
				case <-ctx.Done():
					return
				}
			}
		}()
		materializedCount, persistedCount, materializeFailed, persistFailed := persistImages(ctx, s.store, res)
		close(materializeHeartbeatDone)
		materializeHeartbeatWG.Wait()
		materializeStatus := "completed"
		if materializeFailed {
			materializeStatus = "failed"
		}
		persistStatus := "completed"
		if persistFailed {
			persistStatus = "failed"
		}
		elapsedMS := time.Since(materializeStarted).Milliseconds()
		logMaterialize := logger.InfoContext
		if materializeFailed {
			logMaterialize = logger.WarnContext
		}
		logMaterialize(ctx, "[media] stage",
			"media_kind", "image", "request", requestRef,
			"provider", providerName, "model", modelName,
			"stage", "materialize", "status", materializeStatus,
			"elapsed_ms", elapsedMS, "result_count", materializedCount)
		logPersist := logger.InfoContext
		if persistFailed {
			logPersist = logger.WarnContext
		}
		logPersist(ctx, "[media] stage",
			"media_kind", "image", "request", requestRef,
			"provider", providerName, "model", modelName,
			"stage", "persist", "status", persistStatus,
			"elapsed_ms", elapsedMS, "result_count", persistedCount)
	}
	paths := collectImagePaths(res) // FilePath 优先，空则回落 URL
	return &skill.Result{
		Content:  fmt.Sprintf("Generated %d image(s): %s", len(paths), strings.Join(paths, ", ")),
		Data:     res,
		Metadata: map[string]string{"kind": "image", "count": strconv.Itoa(len(paths))},
	}, nil
}

func (s *MediaGenerateSkill) genVideo(ctx context.Context, args map[string]any, prompt string) (*skill.Result, error) {
	if s.vid == nil || !s.vid.HasProvider() {
		return nil, fmt.Errorf("video generation is not configured")
	}
	requestRef := skill.SystemDispatchTaskRef(ctx)
	providerName := firstStringArg(args, "provider")
	modelName := firstStringArg(args, "model")
	req := videogen.Request{
		Model:     modelName,
		Prompt:    prompt,
		WithAudio: argBool(args, "with_audio"),
		Size:      firstStringArg(args, "size"),
	}
	submitStarted := time.Now()
	logger.InfoContext(ctx, "[media] stage",
		"media_kind", "video", "request", requestRef,
		"provider", providerName, "model", modelName,
		"stage", "submit", "status", "started",
		"elapsed_ms", int64(0), "result_count", 0)
	taskID, err := s.vid.Submit(ctx, providerName, req)
	if err != nil {
		submitStatus := "failed"
		if ctx.Err() != nil {
			submitStatus = "cancelled"
		}
		logger.WarnContext(ctx, "[media] stage",
			"media_kind", "video", "request", requestRef,
			"provider", providerName, "model", modelName,
			"stage", "submit", "status", submitStatus,
			"elapsed_ms", time.Since(submitStarted).Milliseconds(), "result_count", 0)
		return nil, fmt.Errorf("video submit failed: %w", err)
	}
	if providerName == "" {
		if provider, _, ok := strings.Cut(taskID, "::"); ok {
			providerName = provider
		}
	}
	logger.InfoContext(ctx, "[media] stage",
		"media_kind", "video", "task", taskID,
		"provider", providerName, "model", modelName,
		"stage", "submit", "status", "completed",
		"elapsed_ms", time.Since(submitStarted).Milliseconds(), "result_count", 1)

	providerStarted := time.Now()
	logger.InfoContext(ctx, "[media] stage",
		"media_kind", "video", "task", taskID,
		"provider", providerName, "model", modelName,
		"stage", "provider_wait", "status", "started",
		"elapsed_ms", int64(0), "result_count", 0)
	providerHeartbeatDone := make(chan struct{})
	var providerHeartbeatWG sync.WaitGroup
	providerHeartbeatWG.Add(1)
	go func() {
		defer providerHeartbeatWG.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				logger.InfoContext(ctx, "[media] stage",
					"media_kind", "video", "task", taskID,
					"provider", providerName, "model", modelName,
					"stage", "provider_wait", "status", "heartbeat",
					"elapsed_ms", time.Since(providerStarted).Milliseconds(), "result_count", 0)
			case <-providerHeartbeatDone:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	st, err := s.pollUntilDone(ctx, taskID, providerName, modelName) // 阻塞到 Done / ctx 截止（明确 timeout，不挂死）
	close(providerHeartbeatDone)
	providerHeartbeatWG.Wait()
	if err != nil {
		waitStatus := "failed"
		if ctx.Err() != nil {
			waitStatus = "cancelled"
		}
		logger.WarnContext(ctx, "[media] stage",
			"media_kind", "video", "task", taskID,
			"provider", providerName, "model", modelName,
			"stage", "provider_wait", "status", waitStatus,
			"elapsed_ms", time.Since(providerStarted).Milliseconds(), "result_count", 0)
		return nil, err
	}
	if st.Provider != "" {
		providerName = st.Provider
	}
	if st.Model != "" {
		modelName = st.Model
	}
	if st.Status != "success" {
		logger.WarnContext(ctx, "[media] stage",
			"media_kind", "video", "task", taskID,
			"provider", providerName, "model", modelName,
			"stage", "provider_wait", "status", "failed",
			"elapsed_ms", time.Since(providerStarted).Milliseconds(), "result_count", 0)
		return nil, fmt.Errorf("video generation failed: %s", st.Error)
	}
	providerResultCount := 0
	if st.VideoURL != "" || st.VideoFilePath != "" {
		providerResultCount = 1
	}
	logger.InfoContext(ctx, "[media] stage",
		"media_kind", "video", "task", taskID,
		"provider", providerName, "model", modelName,
		"stage", "provider_wait", "status", "completed",
		"elapsed_ms", time.Since(providerStarted).Milliseconds(), "result_count", providerResultCount)

	path := st.VideoURL
	if s.store != nil && st.VideoURL != "" {
		materializeStarted := time.Now()
		logger.InfoContext(ctx, "[media] stage",
			"media_kind", "video", "task", taskID,
			"provider", providerName, "model", modelName,
			"stage", "materialize", "status", "started",
			"elapsed_ms", int64(0), "result_count", 0)
		logger.InfoContext(ctx, "[media] stage",
			"media_kind", "video", "task", taskID,
			"provider", providerName, "model", modelName,
			"stage", "persist", "status", "started",
			"elapsed_ms", int64(0), "result_count", 0)
		materializeHeartbeatDone := make(chan struct{})
		var materializeHeartbeatWG sync.WaitGroup
		materializeHeartbeatWG.Add(1)
		go func() {
			defer materializeHeartbeatWG.Done()
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					logger.InfoContext(ctx, "[media] stage",
						"media_kind", "video", "task", taskID,
						"provider", providerName, "model", modelName,
						"stage", "materialize", "status", "heartbeat",
						"elapsed_ms", time.Since(materializeStarted).Milliseconds(), "result_count", 0)
				case <-materializeHeartbeatDone:
					return
				case <-ctx.Done():
					return
				}
			}
		}()
		p, persistErr := s.store.SaveFromURL(ctx, st.VideoURL, "mp4")
		close(materializeHeartbeatDone)
		materializeHeartbeatWG.Wait()
		resultCount := 0
		stageStatus := "failed"
		if persistErr == nil {
			path = p
			resultCount = 1
			stageStatus = "completed"
		}
		elapsedMS := time.Since(materializeStarted).Milliseconds()
		logStage := logger.InfoContext
		if persistErr != nil {
			logStage = logger.WarnContext
		}
		logStage(ctx, "[media] stage",
			"media_kind", "video", "task", taskID,
			"provider", providerName, "model", modelName,
			"stage", "materialize", "status", stageStatus,
			"elapsed_ms", elapsedMS, "result_count", resultCount)
		logStage(ctx, "[media] stage",
			"media_kind", "video", "task", taskID,
			"provider", providerName, "model", modelName,
			"stage", "persist", "status", stageStatus,
			"elapsed_ms", elapsedMS, "result_count", resultCount)
	}
	return &skill.Result{
		Content:  "Generated video: " + path,
		Data:     st,
		Metadata: map[string]string{"kind": "video", "path": path},
	}, nil
}

// pollUntilDone 尊重 ctx 截止（= cron runBudget），绝不无限等。
// ctx 超时/取消 → 返回明确 timeout 错误，不挂死。
func (s *MediaGenerateSkill) pollUntilDone(ctx context.Context, taskID, providerName, modelName string) (videogen.TaskStatus, error) {
	interval := videoPollInterval
	if s.testFastPoll {
		interval = time.Millisecond
	}
	pollStarted := time.Now()
	logger.InfoContext(ctx, "[media] stage",
		"media_kind", "video", "task", taskID,
		"provider", providerName, "model", modelName,
		"stage", "poll", "status", "started",
		"elapsed_ms", int64(0), "result_count", 0)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.WarnContext(ctx, "[media] stage",
				"media_kind", "video", "task", taskID,
				"provider", providerName, "model", modelName,
				"stage", "poll", "status", "cancelled",
				"elapsed_ms", time.Since(pollStarted).Milliseconds(), "result_count", 0)
			return videogen.TaskStatus{}, fmt.Errorf("video generation timed out: %w", ctx.Err())
		case <-t.C:
			st, err := s.vid.Poll(ctx, taskID)
			if err != nil {
				pollStatus := "failed"
				if ctx.Err() != nil {
					pollStatus = "cancelled"
				}
				logger.WarnContext(ctx, "[media] stage",
					"media_kind", "video", "task", taskID,
					"provider", providerName, "model", modelName,
					"stage", "poll", "status", pollStatus,
					"elapsed_ms", time.Since(pollStarted).Milliseconds(), "result_count", 0)
				return st, err
			}
			if st.Provider != "" {
				providerName = st.Provider
			}
			if st.Model != "" {
				modelName = st.Model
			}
			if st.Done {
				resultCount := 0
				if st.Status == "success" && (st.VideoURL != "" || st.VideoFilePath != "") {
					resultCount = 1
				}
				if st.Status == "success" {
					logger.InfoContext(ctx, "[media] stage",
						"media_kind", "video", "task", taskID,
						"provider", providerName, "model", modelName,
						"stage", "poll", "status", "completed",
						"elapsed_ms", time.Since(pollStarted).Milliseconds(), "result_count", resultCount)
				} else {
					logger.WarnContext(ctx, "[media] stage",
						"media_kind", "video", "task", taskID,
						"provider", providerName, "model", modelName,
						"stage", "poll", "status", "failed",
						"elapsed_ms", time.Since(pollStarted).Milliseconds(), "result_count", 0)
				}
				return st, nil
			}
		}
	}
}

// persistImages 把生成结果中的每张图落盘并回填 FilePath。
//
// b64 走 SaveBytes（落盘后清空 B64JSON，避免再撑爆下游）；url 走 SaveFromURL。
func persistImages(ctx context.Context, store blobStore, res *imagegen.Result) (materializedCount, persistedCount int, materializeFailed, persistFailed bool) {
	if store == nil || res == nil {
		return
	}
	for i := range res.Images {
		img := &res.Images[i]
		switch {
		case img.B64JSON != "":
			data, decodeErr := base64.StdEncoding.DecodeString(img.B64JSON)
			if decodeErr != nil {
				materializeFailed = true
				persistFailed = true
				continue
			}
			materializedCount++
			if p, saveErr := store.SaveBytes(data, "png"); saveErr == nil {
				img.FilePath = p
				img.B64JSON = ""
				persistedCount++
			} else {
				persistFailed = true
			}
		case img.URL != "":
			if p, saveErr := store.SaveFromURL(ctx, img.URL, "png"); saveErr == nil {
				img.FilePath = p
				materializedCount++
				persistedCount++
			} else {
				materializeFailed = true
				persistFailed = true
			}
		}
	}
	return
}

// collectImagePaths 收集每张图的引用：FilePath 优先（落盘稳定路径），空则回落 URL。
func collectImagePaths(res *imagegen.Result) []string {
	if res == nil {
		return nil
	}
	paths := make([]string, 0, len(res.Images))
	for i := range res.Images {
		img := &res.Images[i]
		switch {
		case img.FilePath != "":
			paths = append(paths, img.FilePath)
		case img.URL != "":
			paths = append(paths, img.URL)
		}
	}
	return paths
}
