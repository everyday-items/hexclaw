package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"gopkg.in/yaml.v3"
)

// Writer 配置文件读-改-写工具
//
// 支持追加/移除 MCP server 配置到 hexclaw.yaml，
// 保留用户手动编辑的注释和格式。
type Writer struct {
	mu   sync.Mutex
	path string
}

// NewWriter 创建配置写入器
func NewWriter(path string) *Writer {
	return &Writer{path: path}
}

// AppendMCPServer 追加 MCP server 到配置文件
func (w *Writer) AppendMCPServer(name, transport, command string, args []string, endpoint string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	cfg, err := w.readConfig()
	if err != nil {
		return err
	}

	// 检查是否已存在
	for _, s := range cfg.MCP.Servers {
		if s.Name == name {
			return fmt.Errorf("MCP server '%s' already exists", name)
		}
	}

	cfg.MCP.Servers = append(cfg.MCP.Servers, MCPServerConfig{
		Name:      name,
		Transport: transport,
		Command:   command,
		Args:      args,
		Endpoint:  endpoint,
		Enabled:   true,
	})

	return w.writeConfig(cfg)
}

// RemoveMCPServer 从配置文件移除 MCP server
func (w *Writer) RemoveMCPServer(name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	cfg, err := w.readConfig()
	if err != nil {
		return err
	}

	found := false
	servers := make([]MCPServerConfig, 0, len(cfg.MCP.Servers))
	for _, s := range cfg.MCP.Servers {
		if s.Name == name {
			found = true
			continue
		}
		servers = append(servers, s)
	}
	if !found {
		return fmt.Errorf("MCP server '%s' not found", name)
	}

	cfg.MCP.Servers = servers
	return w.writeConfig(cfg)
}

func (w *Writer) readConfig() (*Config, error) {
	data, err := os.ReadFile(w.path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

func (w *Writer) writeConfig(cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return atomicWriteFile(w.path, data, 0644)
}

// atomicWriteFile 原子写入文件：先写 temp 再 rename，防止 crash 导致配置损坏
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".hexclaw-cfg-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod temp file: %w", err)
	}
	// Windows: os.Rename fails if target exists; remove first
	if runtime.GOOS == "windows" {
		os.Remove(path)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp to target: %w", err)
	}
	return nil
}
