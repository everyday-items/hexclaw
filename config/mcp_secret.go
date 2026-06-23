package config

import "github.com/hexagon-codes/hexclaw/secret"

// 保险箱接管 MCP 凭证（增量4）。
//
// MCP stdio 连接器的 env 常含数据库凭证（MYSQL_PASS / MDB_MCP_CONNECTION_STRING、
// 连接串里的口令等）。与 IM 平台实例的 config_json、connector 的 token 一致，这些凭证
// 应经 secret.Box 静态加密落盘（hexclaw.yaml），而非明文。
//
// 设计：
//   - 写盘前 EncryptMCPEnv：把每个 env 值 Seal 成 enc:v1:…（已加密的跳过，空值跳过）。
//   - 读盘后 DecryptMCPEnv：把 enc:v1:… 还原为明文；**非加密值原样保留**——兼容用户手编
//     的明文 yaml，也让历史明文凭证在下次写盘时被惰性加密（无需启动期重写整份 yaml）。
//   - box==nil（主密钥加载失败降级）时两者均为 no-op，保持明文可用，绝不阻断启动。

// EncryptMCPEnv 就地加密所有 MCP server 的 env 值（box==nil 时 no-op）。
func EncryptMCPEnv(servers []MCPServerConfig, box *secret.Box) {
	if box == nil {
		return
	}
	for i := range servers {
		for k, v := range servers[i].Env {
			if v == "" || secret.IsEncrypted(v) {
				continue
			}
			if sealed, err := box.Seal([]byte(v)); err == nil {
				servers[i].Env[k] = sealed
			}
		}
	}
}

// DecryptMCPEnv 就地解密所有 MCP server 的 env 值（非加密值原样保留；box==nil 时 no-op）。
func DecryptMCPEnv(servers []MCPServerConfig, box *secret.Box) {
	if box == nil {
		return
	}
	for i := range servers {
		for k, v := range servers[i].Env {
			if !secret.IsEncrypted(v) {
				continue
			}
			if plain, err := box.Open(v); err == nil {
				servers[i].Env[k] = string(plain)
			}
		}
	}
}
