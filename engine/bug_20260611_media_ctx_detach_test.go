package engine

// BUG-20260611 (review High): image/video generation goroutines used the
// request ctx for both the upstream generation call and the DB save. When the
// client disconnected mid-generation (ProcessStream caller cancels), the
// expensive result was aborted and the save failed — the generated media was
// lost even though the user paid for it.
//
// The same file already detaches the ctx for background work elsewhere
// (trace.Detach(ctx) + WithTimeout, used 5×); the media goroutines were missed.
//
// Contract: once a media-generation goroutine starts, cancelling the request
// ctx must NOT cancel the generation/save ctx.

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// imageProviderStub implements hexagon.Provider + llm.ImageProvider. Its
// GenerateImage blocks until released, then records whether its ctx is still
// alive — proving whether the generation ctx was detached from the caller's.
type imageProviderStub struct {
	*mockllm.LLMProvider
	release  chan struct{}
	started  chan struct{}
	ctxErr   atomic.Value // error
	imageURL string
}

func (s *imageProviderStub) GenerateImage(ctx context.Context, _ llm.ImageRequest) (*llm.ImageResponse, error) {
	close(s.started)
	<-s.release // wait until the test cancels the request ctx
	if err := ctx.Err(); err != nil {
		s.ctxErr.Store(err.Error())
	} else {
		s.ctxErr.Store("")
	}
	return &llm.ImageResponse{Data: []llm.ImageData{{URL: s.imageURL}}}, nil
}

func TestBug20260611_ImageGenerationDetachesFromRequestCtx(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}

	stub := &imageProviderStub{
		LLMProvider: mockllm.NewLLMProvider("test"),
		release:     make(chan struct{}),
		started:     make(chan struct{}),
		// 1x1 transparent PNG data URL so downloadAsDataURI short-circuits.
		imageURL: "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M8AAAMBAQDJ/pLvAAAAAElFTkSuQmCC",
	}

	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.LLM.Default = "test"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{"test": {Model: "mock-model"}}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"test": stub})

	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := eng.ProcessStream(ctx, &adapter.Message{
		ID:       "msg-img",
		Platform: adapter.PlatformAPI,
		UserID:   "user-1",
		Content:  "画一只猫",
		Metadata: map[string]string{"image_generation": "true"},
	})
	if err != nil {
		t.Fatalf("ProcessStream: %v", err)
	}

	// Cancel the request ctx while generation is in flight.
	select {
	case <-stub.started:
	case <-time.After(5 * time.Second):
		t.Fatal("GenerateImage never started")
	}
	cancel()
	close(stub.release)

	// Drain the reply channel so the goroutine completes.
	for range ch {
	}

	got, _ := stub.ctxErr.Load().(string)
	if got != "" {
		t.Errorf("[BUG-20260611] image generation ctx was cancelled with the request ctx: %v", got)
	}
}

// videoProviderStub implements hexagon.Provider + llm.VideoProvider. Its
// CreateVideoTask blocks until released, then records whether its ctx is
// still alive and returns an already-completed task (no polling needed).
type videoProviderStub struct {
	*mockllm.LLMProvider
	release  chan struct{}
	started  chan struct{}
	ctxErr   atomic.Value // string
	videoURL string
	coverURL string
}

func (s *videoProviderStub) CreateVideoTask(ctx context.Context, _ llm.VideoRequest) (*llm.VideoTask, error) {
	close(s.started)
	<-s.release // wait until the test cancels the request ctx
	if err := ctx.Err(); err != nil {
		s.ctxErr.Store(err.Error())
	} else {
		s.ctxErr.Store("")
	}
	return &llm.VideoTask{
		ID:       "vt-1",
		Status:   llm.VideoTaskCompleted,
		VideoURL: s.videoURL,
		CoverURL: s.coverURL,
	}, nil
}

func (s *videoProviderStub) QueryVideoTask(_ context.Context, taskID string) (*llm.VideoTask, error) {
	return &llm.VideoTask{ID: taskID, Status: llm.VideoTaskCompleted, VideoURL: s.videoURL}, nil
}

// Mirror of the image test for the video goroutine (review M9): cancelling
// the request ctx mid-generation must not cancel the generation/save ctx,
// and the assistant reply must still land in session history (DB save
// survives the caller-ctx cancel).
func TestBug20260611_VideoGenerationDetachesFromRequestCtx(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}

	stub := &videoProviderStub{
		LLMProvider: mockllm.NewLLMProvider("test"),
		release:     make(chan struct{}),
		started:     make(chan struct{}),
		videoURL:    "https://example.com/video.mp4",
		// data URL cover so downloadAsDataURI short-circuits without network.
		coverURL: "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M8AAAMBAQDJ/pLvAAAAAElFTkSuQmCC",
	}

	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.LLM.Default = "test"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{"test": {Model: "mock-model"}}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"test": stub})

	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	ctx, cancel := context.WithCancel(context.Background())
	msg := &adapter.Message{
		ID:       "msg-vid",
		Platform: adapter.PlatformAPI,
		UserID:   "user-1",
		Content:  "生成一段猫咪视频",
		Metadata: map[string]string{"video_generation": "true"},
	}
	ch, err := eng.ProcessStream(ctx, msg)
	if err != nil {
		t.Fatalf("ProcessStream: %v", err)
	}

	select {
	case <-stub.started:
	case <-time.After(5 * time.Second):
		t.Fatal("CreateVideoTask never started")
	}
	cancel()
	close(stub.release)

	var final *adapter.ReplyChunk
	for chunk := range ch {
		final = chunk
	}

	got, _ := stub.ctxErr.Load().(string)
	if got != "" {
		t.Errorf("[BUG-20260611] video generation ctx was cancelled with the request ctx: %v", got)
	}
	if final == nil || final.Error != nil {
		t.Fatalf("expected a successful final chunk, got %+v", final)
	}

	// The assistant reply must be persisted even though the request ctx was
	// cancelled before the save ran.
	msgs, err := store.ListMessages(context.Background(), msg.SessionID, 10, 0)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	foundAssistant := false
	for _, m := range msgs {
		if m.Role == "assistant" && strings.Contains(m.Content, stub.videoURL) {
			foundAssistant = true
		}
	}
	if !foundAssistant {
		t.Errorf("[BUG-20260611] video reply was not saved to session history after caller-ctx cancel; got %d messages", len(msgs))
	}
}

// failingImageProvider always fails generation after recording the call.
type failingImageProvider struct {
	*mockllm.LLMProvider
}

func (s *failingImageProvider) GenerateImage(_ context.Context, _ llm.ImageRequest) (*llm.ImageResponse, error) {
	return nil, context.DeadlineExceeded
}

// Review M9a: when generation fails and the client is gone, the only output
// used to be a buffered error chunk that got dropped. The failure must also
// be persisted as an assistant message so it is visible in session history.
func TestBug20260611_MediaGenerationErrorPersistedToHistory(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}

	stub := &failingImageProvider{LLMProvider: mockllm.NewLLMProvider("test")}

	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.LLM.Default = "test"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{"test": {Model: "mock-model"}}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"test": stub})

	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	msg := &adapter.Message{
		ID:       "msg-img-fail",
		Platform: adapter.PlatformAPI,
		UserID:   "user-1",
		Content:  "画一只狗",
		Metadata: map[string]string{"image_generation": "true"},
	}
	ch, err := eng.ProcessStream(context.Background(), msg)
	if err != nil {
		t.Fatalf("ProcessStream: %v", err)
	}
	for range ch {
	}

	msgs, err := store.ListMessages(context.Background(), msg.SessionID, 10, 0)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	foundFailure := false
	for _, m := range msgs {
		if m.Role == "assistant" && strings.Contains(m.Content, "图片生成失败") {
			foundFailure = true
		}
	}
	if !foundFailure {
		t.Errorf("[BUG-20260611 M9a] image-generation failure was not persisted to session history; got %d messages", len(msgs))
	}
}
