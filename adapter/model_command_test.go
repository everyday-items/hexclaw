package adapter

import "testing"

func TestParseModelCommand(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantKind ModelCommandKind
		wantSpec string
	}{
		{"普通消息非命令", "你好，今天天气如何", ModelCmdNone, ""},
		{"含 model 字样的普通消息不误伤", "这个 model 不错", ModelCmdNone, ""},
		{"裸 /model → List", "/model", ModelCmdList, ""},
		{"前后空白容忍", "   /model   ", ModelCmdList, ""},
		{"中文命令头 /模型 → List", "/模型", ModelCmdList, ""},
		{"/model list", "/model list", ModelCmdList, ""},
		{"/model ls", "/model ls", ModelCmdList, ""},
		{"/model ? 帮助", "/model ?", ModelCmdList, ""},
		{"/model reset", "/model reset", ModelCmdReset, ""},
		{"/model default", "/model default", ModelCmdReset, ""},
		{"中文 /模型 重置", "/模型 重置", ModelCmdReset, ""},
		{"命令头大小写不敏感", "/MODEL reset", ModelCmdReset, ""},
		{"set provider:model", "/model openai:gpt-4o", ModelCmdSet, "openai:gpt-4o"},
		{"set 仅 model", "/model gpt-4o", ModelCmdSet, "gpt-4o"},
		{"set ollama 含冒号 model 原样保留 spec", "/model qwen3:8b", ModelCmdSet, "qwen3:8b"},
		{"多余参数只取首 token", "/model gpt-4o 请用这个", ModelCmdSet, "gpt-4o"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseModelCommand(tt.text)
			if got.Kind != tt.wantKind {
				t.Fatalf("Kind: got %v want %v (text=%q)", got.Kind, tt.wantKind, tt.text)
			}
			if got.Spec != tt.wantSpec {
				t.Fatalf("Spec: got %q want %q (text=%q)", got.Spec, tt.wantSpec, tt.text)
			}
		})
	}
}

func TestResolveModelSpec(t *testing.T) {
	// 已知 provider 集合：openai / anthropic / ollama。
	known := func(p string) bool {
		switch p {
		case "openai", "anthropic", "ollama":
			return true
		}
		return false
	}

	cases := []struct {
		spec      string
		wantProv  string
		wantModel string
	}{
		{"openai:gpt-4o", "openai", "gpt-4o"},                         // 已知 provider → 切分
		{"anthropic:claude-sonnet-4", "anthropic", "claude-sonnet-4"}, // 已知 provider → 切分
		{"gpt-4o", "", "gpt-4o"},                                      // 无冒号 → 整体 model
		{"qwen3:8b", "", "qwen3:8b"},                                  // qwen3 非 provider（ollama tag）→ 不误切
		{"ollama:qwen3:8b", "ollama", "qwen3:8b"},                     // ollama 是 provider，按首冒号切 → model 保留剩余冒号
		{"unknownprov:somemodel", "", "unknownprov:somemodel"},        // 未知 provider → 整体 model
		{"openai:", "", "openai:"},                                    // 右侧空 → 整体（无效 set 由上层当 List 处理）
	}
	for _, c := range cases {
		t.Run(c.spec, func(t *testing.T) {
			p, m := ResolveModelSpec(c.spec, known)
			if p != c.wantProv || m != c.wantModel {
				t.Fatalf("ResolveModelSpec(%q): got (%q,%q) want (%q,%q)", c.spec, p, m, c.wantProv, c.wantModel)
			}
		})
	}

	// isKnownProvider 为 nil 的兜底分支（无注册表时乐观切分）。
	if p, m := ResolveModelSpec("openai:gpt-4o", nil); p != "openai" || m != "gpt-4o" {
		t.Fatalf("nil predicate: got (%q,%q) want (openai,gpt-4o)", p, m)
	}
}
