package builtin

// 媒体生成 Skill 测试，验证不变量：
//   - 图片：b64/url 经落盘 → FilePath 非空、B64JSON 被清空（不撑爆下游）。
//   - 视频：Submit → 阻塞轮询到 Done → success；ctx 提前取消 → 明确 timeout，绝不挂死。
//   - 未配置（service nil / !HasProvider）→ 明确报错。
//   - ToolDefinition Required = ["prompt"]。

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	imagegen "github.com/hexagon-codes/ai-core/media/image"
	videogen "github.com/hexagon-codes/ai-core/media/video"
)

// --- fakes ---

type fakeImageGen struct {
	hasProvider bool
	res         *imagegen.Result
	err         error
}

func (f *fakeImageGen) HasProvider() bool { return f.hasProvider }
func (f *fakeImageGen) Generate(_ context.Context, _ string, _ imagegen.Request) (*imagegen.Result, error) {
	return f.res, f.err
}

type fakeVideoGen struct {
	hasProvider bool
	taskID      string
	submitErr   error
	// pollSeq 按调用次序返回；polls 记录实际调用次数。
	pollSeq  []videogen.TaskStatus
	pollErr  error
	polls    int32
	blockTID string // 若非空，Poll 在该 taskID 上永不 Done（用于 ctx-timeout 测试）
}

func (f *fakeVideoGen) HasProvider() bool { return f.hasProvider }
func (f *fakeVideoGen) Submit(_ context.Context, _ string, _ videogen.Request) (string, error) {
	if f.submitErr != nil {
		return "", f.submitErr
	}
	return f.taskID, nil
}
func (f *fakeVideoGen) Poll(_ context.Context, taskID string) (videogen.TaskStatus, error) {
	n := atomic.AddInt32(&f.polls, 1)
	if f.pollErr != nil {
		return videogen.TaskStatus{}, f.pollErr
	}
	if taskID == f.blockTID {
		return videogen.TaskStatus{Status: "running"}, nil
	}
	idx := int(n) - 1
	if idx >= len(f.pollSeq) {
		idx = len(f.pollSeq) - 1
	}
	if idx < 0 {
		return videogen.TaskStatus{Status: "running"}, nil
	}
	return f.pollSeq[idx], nil
}

type fakeStore struct {
	saveBytesCalls int
	saveURLCalls   int
}

func (f *fakeStore) SaveBytes(_ []byte, ext string) (string, error) {
	f.saveBytesCalls++
	return "202606/fakehash." + ext, nil
}
func (f *fakeStore) SaveFromURL(_ context.Context, _ string, ext string) (string, error) {
	f.saveURLCalls++
	return "202606/fromurl." + ext, nil
}

// --- tests ---

func TestMediaGen_Image_PersistsFilePath(t *testing.T) {
	img := &fakeImageGen{
		hasProvider: true,
		res: &imagegen.Result{Images: []imagegen.Image{
			{B64JSON: "aGVsbG8="}, // "hello"
		}},
	}
	store := &fakeStore{}
	s := newMediaGenerateSkillWith(img, nil, store)

	res, err := s.Execute(context.Background(), map[string]any{"prompt": "a cat"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	result, ok := res.Data.(*imagegen.Result)
	if !ok {
		t.Fatalf("Data should be *imagegen.Result, got %T", res.Data)
	}
	got := result.Images[0]
	if got.FilePath == "" {
		t.Fatalf("FilePath must be set after persist, got empty")
	}
	if got.B64JSON != "" {
		t.Fatalf("B64JSON must be cleared after persist (avoid blowing up downstream), got %q", got.B64JSON)
	}
	if store.saveBytesCalls != 1 {
		t.Fatalf("b64 image must go through SaveBytes once, got %d", store.saveBytesCalls)
	}
	if !strings.Contains(res.Content, got.FilePath) {
		t.Fatalf("summary should mention the file path, got %q", res.Content)
	}
}

func TestMediaGen_Image_URL_PersistsFilePath(t *testing.T) {
	img := &fakeImageGen{
		hasProvider: true,
		res:         &imagegen.Result{Images: []imagegen.Image{{URL: "http://example.com/x.png"}}},
	}
	store := &fakeStore{}
	s := newMediaGenerateSkillWith(img, nil, store)

	res, err := s.Execute(context.Background(), map[string]any{"prompt": "a dog"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	result := res.Data.(*imagegen.Result)
	if result.Images[0].FilePath == "" {
		t.Fatalf("url image must be persisted to a FilePath")
	}
	if store.saveURLCalls != 1 {
		t.Fatalf("url image must go through SaveFromURL once, got %d", store.saveURLCalls)
	}
}

func TestMediaGen_Video_PollUntilDone(t *testing.T) {
	vid := &fakeVideoGen{
		hasProvider: true,
		taskID:      "task-1",
		pollSeq: []videogen.TaskStatus{
			{Status: "queueing", Done: false},
			{Status: "running", Done: false},
			{Status: "success", Done: true, VideoURL: "http://example.com/v.mp4"},
		},
	}
	store := &fakeStore{}
	s := newMediaGenerateSkillWith(nil, vid, store)
	// 缩短轮询间隔，避免测试等 3s/次。
	s.testFastPoll = true

	res, err := s.Execute(context.Background(), map[string]any{"kind": "video", "prompt": "a sunrise"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if atomic.LoadInt32(&vid.polls) != 3 {
		t.Fatalf("should poll until the 3rd (Done) status, got %d polls", vid.polls)
	}
	if store.saveURLCalls != 1 {
		t.Fatalf("successful video must be persisted via SaveFromURL once, got %d", store.saveURLCalls)
	}
	if !strings.Contains(res.Content, "Generated video") {
		t.Fatalf("unexpected content %q", res.Content)
	}
}

func TestMediaGen_Video_CtxTimeout(t *testing.T) {
	vid := &fakeVideoGen{
		hasProvider: true,
		taskID:      "task-block",
		blockTID:    "task-block", // 永不 Done
	}
	s := newMediaGenerateSkillWith(nil, vid, &fakeStore{})
	s.testFastPoll = true

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消 → pollUntilDone 必须返回 timeout，不挂死

	done := make(chan error, 1)
	go func() {
		_, err := s.Execute(ctx, map[string]any{"kind": "video", "prompt": "x"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("cancelled ctx must surface a timeout error, got nil")
		}
		if !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("error should mention timeout, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("pollUntilDone hung on a cancelled ctx — MUST never wait infinitely")
	}
}

func TestMediaGen_NotConfigured(t *testing.T) {
	// image: nil service
	s := newMediaGenerateSkillWith(nil, nil, &fakeStore{})
	if _, err := s.Execute(context.Background(), map[string]any{"prompt": "x"}); err == nil {
		t.Fatalf("nil image service must error clearly")
	}
	// image: HasProvider=false
	s2 := newMediaGenerateSkillWith(&fakeImageGen{hasProvider: false}, nil, &fakeStore{})
	if _, err := s2.Execute(context.Background(), map[string]any{"prompt": "x"}); err == nil {
		t.Fatalf("image service with no provider must error clearly")
	}
	// video: nil service
	s3 := newMediaGenerateSkillWith(&fakeImageGen{hasProvider: true}, nil, &fakeStore{})
	if _, err := s3.Execute(context.Background(), map[string]any{"kind": "video", "prompt": "x"}); err == nil {
		t.Fatalf("nil video service must error clearly")
	}
}

func TestMediaGen_EmptyPrompt_Errors(t *testing.T) {
	s := newMediaGenerateSkillWith(&fakeImageGen{hasProvider: true}, nil, &fakeStore{})
	if _, err := s.Execute(context.Background(), map[string]any{"prompt": "   "}); err == nil {
		t.Fatalf("empty prompt must error")
	}
}

func TestMediaGen_ToolDef_RequiresPrompt(t *testing.T) {
	s := newMediaGenerateSkillWith(nil, nil, nil)
	def := s.ToolDefinition()
	req := def.Function.Parameters.Required
	if len(req) != 1 || req[0] != "prompt" {
		t.Fatalf("Required must be exactly [prompt], got %v", req)
	}
}
