package engine

import (
	"context"
	"path/filepath"
	"testing"

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
