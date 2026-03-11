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
	if cfg.Server.Port != 6060 {
		t.Errorf("期望 Port=6060，得到 %d", cfg.Server.Port)
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
	if cfg.Server.Port != 6060 {
		t.Errorf("应返回默认端口 6060，得到 %d", cfg.Server.Port)
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
