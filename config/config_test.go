package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	// 服务器默认值
	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("期望 Host=127.0.0.1，得到 %s", cfg.Server.Host)
	}
	if cfg.Server.Port != 16060 {
		t.Errorf("期望 Port=16060，得到 %d", cfg.Server.Port)
	}

	// 安全默认值：全部开启
	if !cfg.Security.Auth.Enabled {
		t.Error("Auth 应默认开启")
	}
	if !cfg.Security.InjectionDetection.Enabled {
		t.Error("InjectionDetection 应默认开启")
	}
	if !cfg.Security.PIIRedaction.Enabled {
		t.Error("PIIRedaction 应默认开启")
	}

	// 高风险 Skill 默认关闭
	if cfg.Skill.Builtin.Code {
		t.Error("Code Skill 应默认关闭")
	}
	if cfg.Skill.Builtin.Shell {
		t.Error("Shell Skill 应默认关闭")
	}

	// Web UI 默认开启
	if !cfg.Platforms.Web.Enabled {
		t.Error("Web 平台应默认开启")
	}

	// Knowledge 默认值
	if !cfg.Knowledge.Enabled {
		t.Error("Knowledge 应默认开启")
	}
	if cfg.Knowledge.ChunkSize != 400 {
		t.Errorf("期望 ChunkSize=400，得到 %d", cfg.Knowledge.ChunkSize)
	}
	if cfg.Knowledge.ChunkOverlap != 80 {
		t.Errorf("期望 ChunkOverlap=80，得到 %d", cfg.Knowledge.ChunkOverlap)
	}
	if cfg.Knowledge.TopK != 3 {
		t.Errorf("期望 TopK=3，得到 %d", cfg.Knowledge.TopK)
	}
	if cfg.Knowledge.VectorWeight != 0.7 {
		t.Errorf("期望 VectorWeight=0.7，得到 %f", cfg.Knowledge.VectorWeight)
	}
	if cfg.Knowledge.TextWeight != 0.3 {
		t.Errorf("期望 TextWeight=0.3，得到 %f", cfg.Knowledge.TextWeight)
	}
	if cfg.Knowledge.MMRLambda != 0.7 {
		t.Errorf("期望 MMRLambda=0.7，得到 %f", cfg.Knowledge.MMRLambda)
	}
	if cfg.Knowledge.TimeDecayDays != 30 {
		t.Errorf("期望 TimeDecayDays=30，得到 %d", cfg.Knowledge.TimeDecayDays)
	}

	// Compaction 默认值
	if !cfg.Compaction.Enabled {
		t.Error("Compaction 应默认开启")
	}
	if cfg.Compaction.MaxMessages != 50 {
		t.Errorf("期望 MaxMessages=50，得到 %d", cfg.Compaction.MaxMessages)
	}
	if cfg.Compaction.KeepRecent != 10 {
		t.Errorf("期望 KeepRecent=10，得到 %d", cfg.Compaction.KeepRecent)
	}

	// FileMemory 默认值
	if !cfg.FileMemory.Enabled {
		t.Error("FileMemory 应默认开启")
	}
	if cfg.FileMemory.Dir != "~/.hexclaw/memory/" {
		t.Errorf("期望 Dir=~/.hexclaw/memory/，得到 %s", cfg.FileMemory.Dir)
	}
	if cfg.FileMemory.MaxMemory != 200 {
		t.Errorf("期望 MaxMemory=200，得到 %d", cfg.FileMemory.MaxMemory)
	}
	if cfg.FileMemory.DailyDays != 2 {
		t.Errorf("期望 DailyDays=2，得到 %d", cfg.FileMemory.DailyDays)
	}

	// Skills 默认值
	if !cfg.Skills.Enabled {
		t.Error("Skills 应默认开启")
	}
	if !cfg.Skills.AutoLoad {
		t.Error("Skills.AutoLoad 应默认开启")
	}

	// Heartbeat 默认值
	if cfg.Heartbeat.Enabled {
		t.Error("Heartbeat 应默认关闭")
	}
	if cfg.Heartbeat.IntervalMins != 15 {
		t.Errorf("期望 IntervalMins=15，得到 %d", cfg.Heartbeat.IntervalMins)
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "test.yaml")

	// 写入测试配置
	content := `
server:
  host: "0.0.0.0"
  port: 8080
  mode: "development"
llm:
  default: "openai"
  providers:
    openai:
      api_key: "sk-test"
      model: "gpt-4o"
`
	os.WriteFile(cfgFile, []byte(content), 0600)

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	if cfg.Server.Host != "0.0.0.0" {
		t.Errorf("期望 Host=0.0.0.0，得到 %s", cfg.Server.Host)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("期望 Port=8080，得到 %d", cfg.Server.Port)
	}
	if cfg.LLM.Default != "openai" {
		t.Errorf("期望 Default=openai，得到 %s", cfg.LLM.Default)
	}
}

func TestLoadNonExistentFile(t *testing.T) {
	// 不存在的文件路径应返回默认配置
	cfg, err := Load("/tmp/hexclaw-test-nonexistent-12345.yaml")
	if err != nil {
		t.Fatalf("不应返回错误: %v", err)
	}
	if cfg.Server.Port != 16060 {
		t.Errorf("应返回默认端口 16060，得到 %d", cfg.Server.Port)
	}
}

func TestExpandEnvVars(t *testing.T) {
	t.Setenv("TEST_HEX_KEY", "my-secret-key")

	result := expandEnvVars("api_key: ${TEST_HEX_KEY}")
	if result != "api_key: my-secret-key" {
		t.Errorf("环境变量展开失败: %s", result)
	}
}

func TestApplyEnvProviders(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "sk-deepseek-test")
	t.Setenv("OPENAI_API_KEY", "sk-openai-test")

	cfg := DefaultConfig()
	applyEnvProviders(cfg)

	// 应自动添加 deepseek 和 openai Provider
	if _, ok := cfg.LLM.Providers["deepseek"]; !ok {
		t.Error("应自动添加 deepseek Provider")
	}
	if cfg.LLM.Providers["deepseek"].APIKey != "sk-deepseek-test" {
		t.Error("deepseek API Key 不正确")
	}
	if _, ok := cfg.LLM.Providers["openai"]; !ok {
		t.Error("应自动添加 openai Provider")
	}
}

func TestApplyEnvProvidersExistingProvider(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "sk-from-env")

	cfg := DefaultConfig()
	// 已有 Provider 但 API Key 为空
	cfg.LLM.Providers["deepseek"] = LLMProviderConfig{
		Model: "deepseek-coder",
	}

	applyEnvProviders(cfg)

	// 应从环境变量补充 API Key，但保留已有 Model
	p := cfg.LLM.Providers["deepseek"]
	if p.APIKey != "sk-from-env" {
		t.Errorf("API Key 应从环境变量补充，得到: %s", p.APIKey)
	}
	if p.Model != "deepseek-coder" {
		t.Errorf("Model 应保留已有值，得到: %s", p.Model)
	}
}
