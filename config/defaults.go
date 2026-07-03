package config

// DefaultConfig 返回安全的默认配置
//
// 默认配置采用功能优先：主要内置能力默认开启，安全/审批能力作为可配置治理层保留。
// 用户只需设置 LLM API Key 即可运行。
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host:    "127.0.0.1",
			Port:    16060,
			MCPPort: 16070, // 预留: MCP Server 模式端口
			Mode:    "production",
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
			Autonomy: AutonomyConfig{
				Profile: "function_first",
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
				Browser:   true,
				Code:      true, // 功能优先：默认开启代码能力
				Shell:     true, // 功能优先：默认开启 shell 能力
				CodeExec:  true, // 沙箱代码执行（Python/JS/Go），支持抓取网页、数据处理等
				FileOps:   true, // 受限于 workspace，默认开启
				CodeExecPolicy: CodeExecPolicyConfig{
					RequireApproval: boolPtr(false), // 功能优先：默认无需审批
					Network:         boolPtr(true),  // 允许网络访问：抓取网页、调用 API
				},
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
			Vector: VectorMemoryConfig{
				Enabled:  true,     // ★默认开：向量语义记忆层（仅 embedding 已配时真激活，否则 main.go 守卫不启=安全降级）
				Backend:  "memory", // 内存向量库（桌面单机，无需外部 milvus/weaviate）
				TopK:     5,
				MinScore: 0.7,
				AutoSave: true,
			},
		},
		Knowledge: KnowledgeConfig{
			Enabled:           true,
			ChunkSize:         400,
			ChunkOverlap:      80,
			TopK:              3,
			VectorWeight:      0.7,
			TextWeight:        0.3,
			MMRLambda:         0.7,
			TimeDecayDays:     30,
			Rerank:            true,
			QueryExpand:       true,
			Contextual:        true,
			MinScore:          0.55,
			CandidateK:        50,
			SnapshotRetention: 100,
		},
		Compaction: CompactionConfig{
			Enabled:     true,
			MaxMessages: 50,
			KeepRecent:  10,
		},
		FileMemory: FileMemoryConfig{
			Enabled:              true,
			Dir:                  "~/.hexclaw/memory/",
			MaxMemory:            200,
			DailyDays:            2,
			Reflect:              true,          // ★默认开：周期反思整合（轻相机械零 LLM：去重/时序取代留史/晋升降级/归档）
			ReflectIntervalMins:  1440,          // 24h（方案 §4.4.2「cron 0 3」低频后台）
			Profile:              true,          // ★默认开：周期画像蒸馏（对标 ChatGPT Dreaming V3 跨时综合画像）
			ProfileIntervalMins:  1440,          // 24h（方案 §4.7 R5，deep 相低频）
			Dreaming:             true,          // ★默认开：多阶段 dreaming（light 机械 + deep LLM 整合留史，对标 Claude/OpenClaw dreaming）
			DreamingIntervalMins: 10080,         // 每周（深相低频，对标 OpenClaw REM dreaming）
			AutoMemory:           "inline",      // Claude Code 式：主模型随手判断、顺手调 manage_memory（默认；零额外 LLM 调用）
			RecallMinScore:       0.3,           // 召回相关性地板（仅 embedding 在时生效，砍低相关噪音；eval 可调）
			ActiveRecall:         boolPtr(true), // 回复前主动会话深召回默认开（仅 DM/交互式，FTS-fast 零 LLM；§7bis R13）
		},
		Skills: SkillsConfig{
			Enabled:  true,
			Dir:      "~/.hexclaw/skills/",
			AutoLoad: true,
			Hub: SkillsHubConfig{
				RepoURL: "https://github.com/hexagon-codes/hexclaw-hub",
				Branch:  "v0.0.5",
			},
		},
		Heartbeat: HeartbeatConfig{
			Enabled:      true,
			IntervalMins: 15,
		},
		Router: RouterConfig{
			Enabled: true,
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
		MCP: MCPConfig{
			Enabled: true,
		},
		Budget: BudgetConfig{
			MaxTokens:   500000,
			MaxDuration: "30m",
			MaxCost:     5.0,
		},
	}
}

// boolPtr returns a pointer to the given bool value.
func boolPtr(b bool) *bool {
	return &b
}
