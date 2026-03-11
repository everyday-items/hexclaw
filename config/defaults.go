package config

// DefaultConfig 返回安全的默认配置
//
// 所有安全选项默认开启，高风险 Skill 默认关闭。
// 用户只需设置 LLM API Key 即可运行。
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host: "127.0.0.1",
			Port: 6060,
			Mode: "production",
		},
		LLM: LLMConfig{
			Default:   "deepseek",
			Providers: map[string]LLMProviderConfig{},
			Routing: LLMRoutingConfig{
				Enabled:  true,
				Strategy: "cost-aware",
			},
			Cache: LLMCacheConfig{
				Enabled:    true,
				Similarity: 0.92,
				TTL:        "24h",
				MaxEntries: 10000,
			},
		},
		Platforms: PlatformsConfig{
			Web: WebConfig{Enabled: true},
		},
		Security: SecurityConfig{
			Auth: AuthConfig{
				Enabled:        true,
				Method:         "token",
				AllowAnonymous: false,
			},
			InjectionDetection: InjectionConfig{
				Enabled:     true,
				Sensitivity: "medium",
			},
			PIIRedaction: PIIRedactionConfig{
				Enabled: true,
				Types:   []string{"phone", "email", "id_card", "bank_card"},
			},
			ContentFilter: ContentFilterConfig{
				Enabled:         true,
				BlockCategories: []string{"harmful", "illegal"},
			},
			Cost: CostConfig{
				BudgetPerUser:  10.0,
				BudgetGlobal:   1000.0,
				AlertThreshold: 0.8,
			},
			RateLimit: RateLimitConfig{
				RequestsPerMinute: 20,
				RequestsPerHour:   200,
			},
		},
		Skill: SkillConfig{
			Sandbox: SandboxConfig{
				Enabled:   true,
				Timeout:   "30s",
				MaxMemory: "256MB",
			},
			Verification: VerificationConfig{
				Required: true,
			},
			Builtin: BuiltinConfig{
				Search:    true,
				Weather:   true,
				Translate: true,
				Summary:   true,
				Code:      false, // 高风险，默认关闭
				Shell:     false, // 高风险，默认关闭
			},
		},
		Storage: StorageConfig{
			Driver: "sqlite",
			SQLite: SQLiteConfig{
				Path: "~/.hexclaw/data.db",
			},
		},
		Memory: MemoryConfig{
			Conversation: ConversationMemoryConfig{
				MaxTurns:     50,
				SummaryAfter: 20,
			},
			LongTerm: LongTermMemoryConfig{
				Enabled: true,
				Backend: "sqlite",
			},
		},
		Observe: ObserveConfig{
			LogLevel: "info",
			Metrics: MetricsConfig{
				Enabled:  true,
				Endpoint: "/metrics",
			},
			Tracing: TracingConfig{
				Enabled:  false,
				Exporter: "otlp",
			},
		},
	}
}
