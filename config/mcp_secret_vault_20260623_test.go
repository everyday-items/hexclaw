package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/secret"
	"gopkg.in/yaml.v3"
)

// 增量4 保险箱接管 MCP 凭证：MCP server 的 env（DB 凭证 MYSQL_PASS / MDB_MCP_CONNECTION_STRING 等）
// 此前在 hexclaw.yaml 里**明文落盘**，而 IM config_json、connector token 早已经 secret.Box 静态加密。
// 本测试钉死：注入 box 后，env 值在磁盘上是 enc:v1:…（密文），读回经 box 解密还原；
// 未注入 box 时保持明文（向后兼容、不破坏既有 AppendMCPServer 测试与手编 yaml）。

func boxForTest(t *testing.T) *secret.Box {
	t.Helper()
	box, err := secret.LoadBox(t.TempDir())
	if err != nil {
		t.Fatalf("LoadBox: %v", err)
	}
	return box
}

func TestMCPEnvVault_EncryptedAtRest_DecryptOnRead(t *testing.T) {
	box := boxForTest(t)
	path := filepath.Join(t.TempDir(), "hexclaw.yaml")
	w := NewWriter(path)
	w.SetSecretBox(box)

	env := map[string]string{"MYSQL_HOST": "localhost", "MYSQL_PASS": "s3cr3t-pw", "MYSQL_DB": "dev"}
	if err := w.AppendMCPServer("mysql", "stdio", "npx", []string{"-y", "@benborla29/mcp-server-mysql"}, env, ""); err != nil {
		t.Fatalf("AppendMCPServer: %v", err)
	}

	// 1) 磁盘上密码必须是密文，绝不能出现明文 s3cr3t-pw。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	if strings.Contains(string(raw), "s3cr3t-pw") {
		t.Fatal("BUG: MCP env 密码以明文落盘（保险箱未生效）")
	}
	// 2) 解析回来 env 值是 enc:v1: 密文。
	var onDisk Config
	if err := yaml.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := onDisk.MCP.Servers[0].Env
	if !secret.IsEncrypted(got["MYSQL_PASS"]) {
		t.Fatalf("MYSQL_PASS 应为 enc:v1: 密文，实际=%q", got["MYSQL_PASS"])
	}
	// 非机密的 host 也一并加密（统一处置，简单且无害）——但必须能解回。

	// 3) DecryptMCPEnv 用同一 box 解密 → 还原明文。
	DecryptMCPEnv(onDisk.MCP.Servers, box)
	dec := onDisk.MCP.Servers[0].Env
	if dec["MYSQL_PASS"] != "s3cr3t-pw" || dec["MYSQL_HOST"] != "localhost" || dec["MYSQL_DB"] != "dev" {
		t.Fatalf("解密未还原：%v", dec)
	}
}

func TestMCPEnvVault_WriterRoundTripDecryptsOnRead(t *testing.T) {
	box := boxForTest(t)
	path := filepath.Join(t.TempDir(), "hexclaw.yaml")

	w := NewWriter(path)
	w.SetSecretBox(box)
	if err := w.AppendMCPServer("a", "stdio", "npx", []string{"x"}, map[string]string{"K": "v1"}, ""); err != nil {
		t.Fatalf("append a: %v", err)
	}
	// 第二次 append：Writer 读盘(密文)→解密→再写(全部重新加密)，不得双重加密、不得丢首条凭证。
	if err := w.AppendMCPServer("b", "stdio", "npx", []string{"y"}, map[string]string{"K": "v2"}, ""); err != nil {
		t.Fatalf("append b: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var cfg Config
	_ = yaml.Unmarshal(raw, &cfg)
	if len(cfg.MCP.Servers) != 2 {
		t.Fatalf("应有 2 个 server，得 %d", len(cfg.MCP.Servers))
	}
	DecryptMCPEnv(cfg.MCP.Servers, box)
	byName := map[string]string{}
	for _, s := range cfg.MCP.Servers {
		byName[s.Name] = s.Env["K"]
	}
	if byName["a"] != "v1" || byName["b"] != "v2" {
		t.Fatalf("读改写往返凭证错乱（疑双重加密）：%v", byName)
	}
}

func TestMCPEnvVault_NoBoxStaysPlaintext(t *testing.T) {
	// 未注入 box（默认）→ 明文落盘，保持向后兼容（既有 AppendMCPServer 测试 + 手编 yaml）。
	path := filepath.Join(t.TempDir(), "hexclaw.yaml")
	w := NewWriter(path)
	if err := w.AppendMCPServer("mysql", "stdio", "npx", []string{"x"}, map[string]string{"MYSQL_PASS": "plain"}, ""); err != nil {
		t.Fatalf("append: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "plain") {
		t.Fatal("无 box 时应明文落盘（向后兼容）")
	}
}

func TestDecryptMCPEnv_PlaintextPassthrough(t *testing.T) {
	// 手编 yaml 的明文 env（非 enc:v1:）解密时应原样保留，不报错、不清空。
	box := boxForTest(t)
	servers := []MCPServerConfig{{Name: "x", Env: map[string]string{"K": "plain-value"}}}
	DecryptMCPEnv(servers, box)
	if servers[0].Env["K"] != "plain-value" {
		t.Fatalf("明文应原样保留，得 %q", servers[0].Env["K"])
	}
	// box==nil 时全程 no-op。
	servers2 := []MCPServerConfig{{Name: "y", Env: map[string]string{"K": "v"}}}
	DecryptMCPEnv(servers2, nil)
	EncryptMCPEnv(servers2, nil)
	if servers2[0].Env["K"] != "v" {
		t.Fatal("box==nil 应 no-op")
	}
}
