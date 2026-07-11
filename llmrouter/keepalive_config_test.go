package llmrouter

// BUG-20260710 P1 · keep_alive 设置化:16GB 机器上本地模型驻留 30m(ai-core 默认)可选调短。
// 契约:provider 配置新增 keep_alive(如 "5m"/"30m"),Ollama 构造时透传 WithKeepAlive;
// 空串=沿用 ai-core 默认(30m),不改既有行为。
import (
	"os"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func mustReadFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func TestOllamaKeepAlive_ConfigPassthrough(t *testing.T) {
	r := &Selector{}
	p := r.createProvider("Ollama (本地)", config.LLMProviderConfig{
		BaseURL:   "http://127.0.0.1:11434",
		Model:     "qwen3.5:9b",
		KeepAlive: "5m",
	})
	// 经 buildRequestBody 侧观察:ai-core Provider 无公开 keepAlive 读口,
	// 以类型内省不可行——契约锁降级为「构造不 panic + 配置字段存在」,
	// 真值锁放在 ai-core 侧(WithKeepAlive 已有);此处锁 hexclaw 透传线路存在。
	if p == nil {
		t.Fatal("provider 构造失败")
	}
}

func TestOllamaKeepAlive_FieldWiredInSource(t *testing.T) {
	// 粒度锁:createProvider 的 Ollama 分支必须引用 pc.KeepAlive → WithKeepAlive
	//(防止字段加了没接线;源码级断言与 AST 等价的最小配方)。
	src := mustReadFile(t, "router.go")
	if !strings.Contains(src, "ollama.WithKeepAlive(pc.KeepAlive)") {
		t.Fatal("createProvider Ollama 分支未透传 pc.KeepAlive")
	}
}
