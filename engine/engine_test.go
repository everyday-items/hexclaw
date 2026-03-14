package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/everyday-items/hexagon"
	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/config"
	"github.com/everyday-items/hexclaw/llmrouter"
	"github.com/everyday-items/hexclaw/skill"
	sqlitestore "github.com/everyday-items/hexclaw/storage/sqlite"
)

func TestReActEngine_Lifecycle(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	defer store.Close()
	store.Init(context.Background())

	cfg := config.DefaultConfig()
	skills := skill.NewRegistry()

	// 没有 LLM Provider 时无法创建路由器，但可以测试引擎生命周期
	// 使用一个假的 API Key 创建路由器（不实际调用）
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"test": {APIKey: "sk-test", Model: "test-model"},
	}
	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Fatalf("创建路由器失败: %v", err)
	}

	eng := NewReActEngine(cfg, router, store, skills)

	// 启动前健康检查应失败
	ctx := context.Background()
	if err := eng.Health(ctx); err == nil {
		t.Error("启动前健康检查应失败")
	}

	// 启动
	if err := eng.Start(ctx); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	// 启动后健康检查应通过
	if err := eng.Health(ctx); err != nil {
		t.Errorf("启动后健康检查应通过: %v", err)
	}

	// 停止
	if err := eng.Stop(ctx); err != nil {
		t.Fatalf("停止失败: %v", err)
	}

	// 停止后健康检查应失败
	if err := eng.Health(ctx); err == nil {
		t.Error("停止后健康检查应失败")
	}
}

func TestReActEngine_SkillFastPath(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	defer store.Close()
	store.Init(context.Background())

	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"test": {APIKey: "sk-test", Model: "test-model"},
	}
	router, _ := llmrouter.New(cfg.LLM)

	// 注册一个模拟 Skill
	skills := skill.NewRegistry()
	skills.Register(&echoSkill{})

	eng := NewReActEngine(cfg, router, store, skills)
	eng.Start(context.Background())
	defer eng.Stop(context.Background())

	// 触发快速路径
	msg := &adapter.Message{
		ID:       "msg-001",
		Platform: adapter.PlatformAPI,
		UserID:   "user-001",
		Content:  "/echo hello world",
	}

	reply, err := eng.Process(context.Background(), msg)
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	if reply.Content != "echo: hello world" {
		t.Errorf("期望 'echo: hello world'，得到 %q", reply.Content)
	}
}

func TestReActEngine_ProcessStream_SkillFastPath(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	defer store.Close()
	store.Init(context.Background())

	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"test": {APIKey: "sk-test", Model: "test-model"},
	}
	router, _ := llmrouter.New(cfg.LLM)

	skills := skill.NewRegistry()
	skills.Register(&echoSkill{})

	eng := NewReActEngine(cfg, router, store, skills)
	eng.Start(context.Background())
	defer eng.Stop(context.Background())

	msg := &adapter.Message{
		ID:       "msg-002",
		Platform: adapter.PlatformAPI,
		UserID:   "user-001",
		Content:  "/echo streaming test",
	}

	ch, err := eng.ProcessStream(context.Background(), msg)
	if err != nil {
		t.Fatalf("ProcessStream 失败: %v", err)
	}

	var chunks []adapter.ReplyChunk
	for chunk := range ch {
		chunks = append(chunks, *chunk)
	}

	if len(chunks) != 1 {
		t.Fatalf("期望 1 个 chunk（快速路径），得到 %d", len(chunks))
	}
	if !chunks[0].Done {
		t.Error("快速路径 chunk 应标记 Done=true")
	}
	if chunks[0].Content != "echo: streaming test" {
		t.Errorf("期望 'echo: streaming test'，得到 %q", chunks[0].Content)
	}
}

func TestSingleChunk(t *testing.T) {
	ch := singleChunk("hello")
	var got []adapter.ReplyChunk
	for c := range ch {
		got = append(got, *c)
	}
	if len(got) != 1 || got[0].Content != "hello" || !got[0].Done {
		t.Errorf("singleChunk 结果不符合预期: %+v", got)
	}
}

func TestBuildStreamMessages(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"test": {APIKey: "sk-test", Model: "test-model"},
	}
	router, _ := llmrouter.New(cfg.LLM)
	dir := t.TempDir()
	store, _ := sqlitestore.New(filepath.Join(dir, "test.db"))
	defer store.Close()
	store.Init(context.Background())
	skills := skill.NewRegistry()
	eng := NewReActEngine(cfg, router, store, skills)

	// 无历史、无知识库、无角色
	msgs := eng.buildStreamMessages("", nil, "", "你好")
	if len(msgs) != 2 {
		t.Fatalf("期望 2 条消息（system+user），得到 %d", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Errorf("第一条消息应为 system，得到 %q", msgs[0].Role)
	}
	if msgs[1].Content != "你好" {
		t.Errorf("用户消息内容不匹配: %q", msgs[1].Content)
	}

	// 有知识库上下文
	msgs = eng.buildStreamMessages("", nil, "相关知识内容", "你好")
	if len(msgs) != 2 {
		t.Fatalf("期望 2 条消息，得到 %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Content, "[参考知识]") {
		t.Error("system 消息应包含知识库内容")
	}

	// 有历史消息
	history := []hexagon.Message{
		{Role: "user", Content: "之前的问题"},
		{Role: "assistant", Content: "之前的回答"},
	}
	msgs = eng.buildStreamMessages("", history, "", "新问题")
	if len(msgs) != 4 {
		t.Fatalf("期望 4 条消息（system+2history+user），得到 %d", len(msgs))
	}
	if msgs[3].Content != "新问题" {
		t.Errorf("最后一条应为用户新消息: %q", msgs[3].Content)
	}

	// 有角色
	msgs = eng.buildStreamMessages("coder", nil, "", "写代码")
	if len(msgs) != 2 {
		t.Fatalf("期望 2 条消息，得到 %d", len(msgs))
	}
	// coder 角色的 system prompt 应不同于默认
	if msgs[0].Content == systemPrompt {
		t.Error("指定 coder 角色后 system prompt 应不同于默认")
	}
}

// echoSkill 测试用的 echo Skill
type echoSkill struct{}

func (s *echoSkill) Name() string        { return "echo" }
func (s *echoSkill) Description() string  { return "回显输入" }
func (s *echoSkill) Match(content string) bool {
	return len(content) > 6 && content[:6] == "/echo "
}
func (s *echoSkill) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	query := args["query"].(string)
	return &skill.Result{Content: "echo: " + query[6:]}, nil
}
