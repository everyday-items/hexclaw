package config

import (
	"fmt"
	"os"
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
	return os.WriteFile(w.path, data, 0644)
}
