package api

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/hexagon-codes/hexclaw/config"
)

const sidecarConnectionSecretRefPrefix = "sidecar-connection:v1:"

type mcpSecretArgMutation struct {
	Index         int    `json:"index"`
	Mode          string `json:"mode"`
	CredentialRef string `json:"credential_ref"`
}

type mcpSecretEnvMutation struct {
	Key           string `json:"key"`
	Mode          string `json:"mode"`
	CredentialRef string `json:"credential_ref"`
}

func validSidecarConnectionSecretRef(ref string) bool {
	return strings.HasPrefix(ref, sidecarConnectionSecretRefPrefix) &&
		len(ref) > len(sidecarConnectionSecretRefPrefix) &&
		!strings.ContainsAny(ref, " \t\r\n")
}

func cloneMCPArgs(args []string) []string {
	if args == nil {
		return nil
	}
	return append([]string(nil), args...)
}

func cloneMCPStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func cloneMCPArgRefs(values map[int]string) map[int]string {
	if values == nil {
		return nil
	}
	clone := make(map[int]string, len(values))
	for index, ref := range values {
		clone[index] = ref
	}
	return clone
}

func preserveMCPSecretValue(current, next string) string {
	currentURL, currentErr := url.Parse(current)
	nextURL, nextErr := url.Parse(next)
	if currentErr != nil || nextErr != nil || currentURL.User == nil || nextURL.Scheme == "" {
		return current
	}
	password, hasPassword := currentURL.User.Password()
	if !hasPassword {
		return current
	}
	// 用户名被显式清空时，不把旧认证重新注入；Redis 的无用户名 URL 则仍需恢复密码。
	if nextURL.User == nil && currentURL.User.Username() != "" {
		return next
	}
	username := currentURL.User.Username()
	if nextURL.User != nil {
		username = nextURL.User.Username()
	}
	nextURL.User = url.UserPassword(username, password)
	return nextURL.String()
}

func mergeMCPSecretMutations(
	current *config.MCPServerConfig,
	next config.MCPServerConfig,
	argMutations []mcpSecretArgMutation,
	envMutations []mcpSecretEnvMutation,
) (config.MCPServerConfig, error) {
	next.Args = cloneMCPArgs(next.Args)
	next.Env = cloneMCPStringMap(next.Env)
	next.ArgsSecretRefs = cloneMCPArgRefs(next.ArgsSecretRefs)
	next.EnvSecretRefs = cloneMCPStringMap(next.EnvSecretRefs)
	if next.ArgsSecretRefs == nil {
		next.ArgsSecretRefs = make(map[int]string)
	}
	if next.EnvSecretRefs == nil {
		next.EnvSecretRefs = make(map[string]string)
	}

	argTouched := make(map[int]struct{}, len(argMutations))
	for _, mutation := range argMutations {
		if _, exists := argTouched[mutation.Index]; exists {
			return config.MCPServerConfig{}, fmt.Errorf("duplicate secret arg mutation %d", mutation.Index)
		}
		argTouched[mutation.Index] = struct{}{}
	}
	envTouched := make(map[string]struct{}, len(envMutations))
	for _, mutation := range envMutations {
		if mutation.Key == "" {
			return config.MCPServerConfig{}, fmt.Errorf("secret env key cannot be empty")
		}
		if _, exists := envTouched[mutation.Key]; exists {
			return config.MCPServerConfig{}, fmt.Errorf("duplicate secret env mutation %q", mutation.Key)
		}
		envTouched[mutation.Key] = struct{}{}
	}

	// 未在本次请求中提及的旧 secret 仅在新配置仍保留该位置时继承，避免编辑普通字段时丢凭证。
	if current != nil {
		for index, ref := range current.ArgsSecretRefs {
			if _, touched := argTouched[index]; touched {
				continue
			}
			if index < 0 || index >= len(current.Args) || index >= len(next.Args) {
				continue
			}
			if !validSidecarConnectionSecretRef(ref) {
				return config.MCPServerConfig{}, fmt.Errorf("invalid stored secret arg credential reference")
			}
			next.Args[index] = current.Args[index]
			next.ArgsSecretRefs[index] = ref
		}
		for key, ref := range current.EnvSecretRefs {
			if _, touched := envTouched[key]; touched {
				continue
			}
			if _, exists := current.Env[key]; !exists {
				return config.MCPServerConfig{}, fmt.Errorf("stored secret env key %q is missing", key)
			}
			if _, exists := next.Env[key]; !exists {
				continue
			}
			if !validSidecarConnectionSecretRef(ref) {
				return config.MCPServerConfig{}, fmt.Errorf("invalid stored secret env credential reference")
			}
			next.Env[key] = current.Env[key]
			next.EnvSecretRefs[key] = ref
		}
	}

	for _, mutation := range argMutations {
		if mutation.Index < 0 || mutation.Index >= len(next.Args) {
			return config.MCPServerConfig{}, fmt.Errorf("secret arg index %d is out of range", mutation.Index)
		}
		ref := mutation.CredentialRef
		switch mutation.Mode {
		case "preserve":
			if current == nil || mutation.Index >= len(current.Args) {
				return config.MCPServerConfig{}, fmt.Errorf("cannot preserve missing secret arg %d", mutation.Index)
			}
			if ref == "" {
				ref = current.ArgsSecretRefs[mutation.Index]
			}
			if !validSidecarConnectionSecretRef(ref) {
				return config.MCPServerConfig{}, fmt.Errorf("secret arg %d credential reference is required", mutation.Index)
			}
			next.Args[mutation.Index] = preserveMCPSecretValue(current.Args[mutation.Index], next.Args[mutation.Index])
			next.ArgsSecretRefs[mutation.Index] = ref
		case "replace":
			if !validSidecarConnectionSecretRef(ref) {
				return config.MCPServerConfig{}, fmt.Errorf("secret arg %d credential reference is required", mutation.Index)
			}
			if next.Args[mutation.Index] == "" {
				return config.MCPServerConfig{}, fmt.Errorf("secret arg %d replacement is empty; use clear", mutation.Index)
			}
			next.ArgsSecretRefs[mutation.Index] = ref
		case "clear":
			delete(next.ArgsSecretRefs, mutation.Index)
		default:
			return config.MCPServerConfig{}, fmt.Errorf("secret arg %d mode is invalid", mutation.Index)
		}
	}

	for _, mutation := range envMutations {
		ref := mutation.CredentialRef
		switch mutation.Mode {
		case "preserve":
			if current == nil {
				return config.MCPServerConfig{}, fmt.Errorf("cannot preserve missing secret env %q", mutation.Key)
			}
			value, exists := current.Env[mutation.Key]
			if !exists {
				return config.MCPServerConfig{}, fmt.Errorf("cannot preserve missing secret env %q", mutation.Key)
			}
			if ref == "" {
				ref = current.EnvSecretRefs[mutation.Key]
			}
			if !validSidecarConnectionSecretRef(ref) {
				return config.MCPServerConfig{}, fmt.Errorf("secret env %q credential reference is required", mutation.Key)
			}
			next.Env[mutation.Key] = preserveMCPSecretValue(value, next.Env[mutation.Key])
			next.EnvSecretRefs[mutation.Key] = ref
		case "replace":
			if !validSidecarConnectionSecretRef(ref) {
				return config.MCPServerConfig{}, fmt.Errorf("secret env %q credential reference is required", mutation.Key)
			}
			if next.Env[mutation.Key] == "" {
				return config.MCPServerConfig{}, fmt.Errorf("secret env %q replacement is empty; use clear", mutation.Key)
			}
			next.EnvSecretRefs[mutation.Key] = ref
		case "clear":
			if next.Env[mutation.Key] == "" {
				delete(next.Env, mutation.Key)
			}
			delete(next.EnvSecretRefs, mutation.Key)
		default:
			return config.MCPServerConfig{}, fmt.Errorf("secret env %q mode is invalid", mutation.Key)
		}
	}

	if len(next.ArgsSecretRefs) == 0 {
		next.ArgsSecretRefs = nil
	}
	if len(next.EnvSecretRefs) == 0 {
		next.EnvSecretRefs = nil
	}
	return next, nil
}
