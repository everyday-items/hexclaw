package mcp

import (
	"testing"
)

// TestNewManager 测试创建管理器
func TestNewManager(t *testing.T) {
	mgr := NewManager()
	if mgr == nil {
		t.Fatal("管理器不应为 nil")
	}

	// 初始状态无工具
	tools := mgr.Tools()
	if len(tools) != 0 {
		t.Errorf("初始应无工具，实际 %d", len(tools))
	}

	// 初始状态无服务器
	names := mgr.ServerNames()
	if len(names) != 0 {
		t.Errorf("初始应无服务器，实际 %d", len(names))
	}
}

// TestServerConfig_Validation 测试配置验证
func TestServerConfig_Validation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     ServerConfig
		wantErr bool
	}{
		{
			name: "stdio 无 command",
			cfg: ServerConfig{
				Name:      "test",
				Transport: "stdio",
				Enabled:   true,
			},
			wantErr: true,
		},
		{
			name: "sse 无 endpoint",
			cfg: ServerConfig{
				Name:      "test",
				Transport: "sse",
				Enabled:   true,
			},
			wantErr: true,
		},
		{
			name: "不支持的传输",
			cfg: ServerConfig{
				Name:      "test",
				Transport: "grpc",
				Enabled:   true,
			},
			wantErr: true,
		},
	}

	mgr := NewManager()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := mgr.connectServer(t.Context(), tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("wantErr=%v, got err=%v", tt.wantErr, err)
			}
		})
	}
}

// TestManager_DisabledServer 测试禁用的 Server 不会连接
func TestManager_DisabledServer(t *testing.T) {
	mgr := NewManager()

	configs := []ServerConfig{
		{
			Name:      "disabled-server",
			Transport: "stdio",
			Command:   "echo",
			Enabled:   false, // 禁用
		},
	}

	total, err := mgr.Connect(t.Context(), configs)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if total != 0 {
		t.Errorf("禁用的 Server 不应贡献工具，实际 %d", total)
	}
}

// TestManager_Close 测试关闭管理器
func TestManager_Close(t *testing.T) {
	mgr := NewManager()

	// 关闭空管理器不应 panic
	mgr.Close()

	// 关闭后应为空
	if len(mgr.ServerNames()) != 0 {
		t.Error("关闭后应无服务器")
	}
}

// TestToolInfos 测试工具信息列表
func TestToolInfos(t *testing.T) {
	mgr := NewManager()

	// 空列表
	infos := mgr.ToolInfos()
	if len(infos) != 0 {
		t.Errorf("初始应无工具信息，实际 %d", len(infos))
	}
}
