package config

import (
	"errors"
	"fmt"

	"github.com/hexagon-codes/hexclaw/secret"
)

// 连接器凭据由 Sidecar 的同一把 secret.Box 负责静态加密。
// env 的历史格式保持兼容；args 只有被 ArgsSecretRefs 明确标记的下标才允许进入密文路径。

const mcpSecretBoxUnavailable = "MCP secret.Box unavailable; refusing plaintext secret persistence"

func validateMCPSecretRefs(server *MCPServerConfig) error {
	if server == nil {
		return fmt.Errorf("MCP server is nil")
	}
	for index, ref := range server.ArgsSecretRefs {
		if index < 0 || index >= len(server.Args) {
			return fmt.Errorf("MCP secret arg index %d is out of range", index)
		}
		if ref == "" {
			return fmt.Errorf("MCP secret arg ref %d is empty", index)
		}
	}
	for key, ref := range server.EnvSecretRefs {
		if key == "" || ref == "" {
			return fmt.Errorf("MCP secret env ref is invalid")
		}
		if _, ok := server.Env[key]; !ok {
			return fmt.Errorf("MCP secret env key %q is missing", key)
		}
	}
	return nil
}

func hasEncryptedMCPValue(server *MCPServerConfig) bool {
	for _, value := range server.Args {
		if secret.IsEncrypted(value) {
			return true
		}
	}
	for _, value := range server.Env {
		if secret.IsEncrypted(value) {
			return true
		}
	}
	return false
}

func openMCPValue(value string, box *secret.Box) (string, error) {
	if !secret.IsEncrypted(value) {
		return value, nil
	}
	if box == nil {
		return "", errors.New(mcpSecretBoxUnavailable)
	}
	plain, err := box.Open(value)
	if err != nil {
		return "", fmt.Errorf("open MCP secret: %w", err)
	}
	return string(plain), nil
}

// DecryptMCPSecrets 在 Sidecar 启动/读改写前解密 env 与带元数据的 args。
// 没有 Box 时，任何密文或 secret metadata 都 fail-closed，绝不把密文交给子进程。
func DecryptMCPSecrets(servers []MCPServerConfig, box *secret.Box) error {
	for i := range servers {
		server := &servers[i]
		if err := validateMCPSecretRefs(server); err != nil {
			return err
		}
		if box == nil && (len(server.ArgsSecretRefs) > 0 || len(server.EnvSecretRefs) > 0 || hasEncryptedMCPValue(server)) {
			return fmt.Errorf("%w for server %q", errors.New(mcpSecretBoxUnavailable), server.Name)
		}
		for index, value := range server.Args {
			plain, err := openMCPValue(value, box)
			if err != nil {
				return fmt.Errorf("server %q arg %d: %w", server.Name, index, err)
			}
			server.Args[index] = plain
		}
		for key, value := range server.Env {
			plain, err := openMCPValue(value, box)
			if err != nil {
				return fmt.Errorf("server %q env %q: %w", server.Name, key, err)
			}
			server.Env[key] = plain
		}
	}
	return nil
}

func sealMCPValue(value string, box *secret.Box) (string, error) {
	if value == "" || secret.IsEncrypted(value) {
		return value, nil
	}
	if box == nil {
		return "", errors.New(mcpSecretBoxUnavailable)
	}
	sealed, err := box.Seal([]byte(value))
	if err != nil {
		return "", fmt.Errorf("seal MCP secret: %w", err)
	}
	return sealed, nil
}

// EncryptMCPSecrets 在 Sidecar 配置写盘前加密所有 env，以及显式标记的 args。
// args 未被 metadata 标记时保持普通参数，避免把路径/包名误当凭据。
func EncryptMCPSecrets(servers []MCPServerConfig, box *secret.Box) error {
	for i := range servers {
		server := &servers[i]
		if err := validateMCPSecretRefs(server); err != nil {
			return err
		}
		if len(server.ArgsSecretRefs) > 0 || len(server.EnvSecretRefs) > 0 {
			if box == nil {
				return fmt.Errorf("%w for server %q", errors.New(mcpSecretBoxUnavailable), server.Name)
			}
		}
		for index := range server.ArgsSecretRefs {
			sealed, err := sealMCPValue(server.Args[index], box)
			if err != nil {
				return fmt.Errorf("server %q arg %d: %w", server.Name, index, err)
			}
			server.Args[index] = sealed
		}
		// Legacy MCP entries without secret metadata may still be authored in a
		// plaintext config when no Box is available. New secret metadata never
		// takes this path; it failed closed above.
		if box == nil {
			continue
		}
		for key, value := range server.Env {
			sealed, err := sealMCPValue(value, box)
			if err != nil {
				return fmt.Errorf("server %q env %q: %w", server.Name, key, err)
			}
			server.Env[key] = sealed
		}
	}
	return nil
}

// EncryptMCPEnv 保留给旧调用方；新读改写路径必须使用 EncryptMCPSecrets 获取错误。
func EncryptMCPEnv(servers []MCPServerConfig, box *secret.Box) {
	if box == nil {
		return
	}
	for i := range servers {
		for key, value := range servers[i].Env {
			if value == "" || secret.IsEncrypted(value) {
				continue
			}
			if sealed, err := box.Seal([]byte(value)); err == nil {
				servers[i].Env[key] = sealed
			}
		}
	}
}

// DecryptMCPEnv 保留给旧调用方；启动与 Writer 读路径必须使用 DecryptMCPSecrets 获取错误。
func DecryptMCPEnv(servers []MCPServerConfig, box *secret.Box) {
	if box == nil {
		return
	}
	for i := range servers {
		for key, value := range servers[i].Env {
			if !secret.IsEncrypted(value) {
				continue
			}
			if plain, err := box.Open(value); err == nil {
				servers[i].Env[key] = string(plain)
			}
		}
	}
}
