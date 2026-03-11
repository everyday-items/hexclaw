# HexClaw 河蟹 🦀

**企业级安全的个人 AI Agent** — 安全 · 开源 · 自托管 · 易用 · 功能全面

> 基于 [Hexagon](https://github.com/everyday-items/hexagon) AI Agent 全能型框架构建

## 特性

### 核心能力
- **ReAct Agent 引擎** — 推理 + 行动循环，支持多轮工具调用
- **六层安全网关** — 认证、限流、成本控制、注入检测、权限校验、审计日志
- **LLM 智能路由** — 多 Provider 自动切换，故障降级，成本优化
- **Skill 系统** — 内置搜索/天气/翻译/摘要，沙箱安全执行
- **知识库** — FTS5 + 向量混合检索，RAG 上下文增强

### 自主行为
- **Heartbeat 主动巡查** — Agent 定期自主检查待办事项并通知
- **Cron 定时任务** — 定时报告、提醒、巡检（cron 表达式 + @every/@daily）
- **Webhooks** — GitHub/GitLab/通用 JSON，HMAC-SHA256 签名验证
- **上下文压缩** — LLM 驱动的旧消息摘要，防止 token 爆炸
- **文件驱动记忆** — MEMORY.md 长期记忆 + 每日日记，可审查可版本控制

### 生态扩展
- **MCP 原生支持** — 兼容 3200+ MCP Server（stdio + SSE 传输）
- **Markdown 技能市场** — 兼容 OpenClaw 技能格式，按需延迟加载
- **多 Agent 路由** — 一个实例托管多个 Agent，按平台/用户/群组路由
- **Canvas / A2UI** — Agent 生成交互式 UI（图表、表单、看板等 8 种组件）
- **安全审计 CLI** — `hexclaw security audit` 一键安全检查 + 修复建议
- **语音交互** — STT/TTS 接口定义，支持多 Provider 接入

### 多平台接入

| 平台 | 方式 | 状态 |
|------|------|:----:|
| Web UI | WebSocket | ✅ |
| 飞书 | HTTP Webhook | ✅ |
| Telegram | 长轮询 | ✅ |
| 钉钉 | HTTP Webhook | ✅ |
| Discord | Gateway WebSocket | ✅ |
| Slack | Events API | ✅ |
| 企业微信 | HTTP 回调 + AES 加解密 | ✅ |
| 微信公众号 | XML 消息 + 被动/客服回复 | ✅ |
| REST API | HTTP | ✅ |

## 快速开始

### 安装

```bash
# 从源码安装
go install github.com/everyday-items/hexclaw/cmd/hexclaw@latest

# 或使用预编译二进制（从 Releases 下载）
curl -sSL https://github.com/everyday-items/hexclaw/releases/latest/download/hexclaw-$(uname -s)-$(uname -m).tar.gz | tar xz
sudo mv hexclaw /usr/local/bin/
```

### 启动服务

```bash
# 设置 LLM API Key（任选一个）
export DEEPSEEK_API_KEY="sk-xxx"
# export OPENAI_API_KEY="sk-xxx"
# export ANTHROPIC_API_KEY="sk-xxx"

# 启动服务
hexclaw serve
```

服务启动后：
- Web UI: `http://127.0.0.1:6060`
- 健康检查: `GET http://127.0.0.1:6060/health`
- 聊天 API: `POST http://127.0.0.1:6060/api/v1/chat`

### 使用 API

```bash
curl -X POST http://127.0.0.1:6060/api/v1/chat \
  -H "Content-Type: application/json" \
  -d '{"message": "你好", "user_id": "test-user"}'
```

### 安全审计

```bash
hexclaw security audit
hexclaw security audit --config hexclaw.yaml
```

### 配置文件

```bash
# 生成默认配置
hexclaw init

# 使用自定义配置启动
hexclaw serve --config ~/.hexclaw/hexclaw.yaml
```

> 详细的安装和部署指南请参考 [docs/install.md](docs/install.md)

## 配置

配置文件 `~/.hexclaw/hexclaw.yaml`：

```yaml
server:
  host: 127.0.0.1
  port: 6060

llm:
  default: deepseek
  providers:
    deepseek:
      api_key: ${DEEPSEEK_API_KEY}
      model: deepseek-chat
    openai:
      api_key: ${OPENAI_API_KEY}
      model: gpt-4o

security:
  auth:
    enabled: true
  rate_limit:
    requests_per_minute: 20
  cost:
    budget_per_user: 10.0
    budget_global: 1000.0
  injection_detection:
    enabled: true
  pii_redaction:
    enabled: true

platforms:
  web:
    enabled: true
  telegram:
    enabled: false
    token: ${TELEGRAM_BOT_TOKEN}
  discord:
    enabled: false
    token: ${DISCORD_BOT_TOKEN}
  slack:
    enabled: false
    token: ${SLACK_BOT_TOKEN}
    signing_secret: ${SLACK_SIGNING_SECRET}

mcp:
  enabled: false
  servers:
    - name: filesystem
      transport: stdio
      command: npx
      args: ["-y", "@anthropic/mcp-filesystem"]

skills:
  enabled: true
  dir: ~/.hexclaw/skills/
  auto_load: true

heartbeat:
  enabled: false
  interval_mins: 15
  quiet_start: "22:00"
  quiet_end: "08:00"

cron:
  enabled: false

webhook:
  enabled: false

file_memory:
  enabled: true
  dir: ~/.hexclaw/memory/

knowledge:
  enabled: false
  chunk_size: 400
  top_k: 3

skill:
  sandbox:
    enabled: true
    timeout: 30s

storage:
  driver: sqlite
  sqlite:
    path: ~/.hexclaw/data.db
```

所有配置项支持环境变量替换（`${VAR_NAME}`）。

## 架构

```
用户 → 平台适配器(8种) → 安全网关(6层) → Agent 路由 → Agent 引擎 → LLM Provider
         │                    │              │            │            │
   Web/飞书/Telegram    认证→限流→成本    多Agent路由   ReAct 推理    DeepSeek/OpenAI
   Discord/Slack/...    →安全→权限→审计                 Skill/MCP    Claude/Qwen/...
   钉钉/企微/微信                                       知识库RAG
```

### 六层安全网关

| 层级 | 名称 | 功能 |
|:---:|------|------|
| 1 | Auth | 身份认证（Token/API Key） |
| 2 | RateLimit | 滑动窗口限流（每分钟/每小时） |
| 3 | CostCheck | 用户/全局月度预算检查 |
| 4 | InputSafety | Prompt 注入检测 + PII 脱敏 |
| 5 | Permission | RBAC 权限校验 |
| 6 | Audit | 请求审计日志 |

### 目录结构

```
hexclaw/
├── hexclaw.go               # 根包（版本信息 + 包文档）
├── cmd/hexclaw/             # CLI 入口 (serve/init/security audit/skill)
├── adapter/                 # 平台适配器
│   ├── web/                 #   Web WebSocket
│   ├── feishu/              #   飞书 Bot
│   ├── telegram/            #   Telegram Bot
│   ├── dingtalk/            #   钉钉 Bot
│   ├── discord/             #   Discord Bot
│   ├── slack/               #   Slack Bot
│   ├── wecom/               #   企业微信
│   └── wechat/              #   微信公众号
├── agents/                  # Agent 角色 (6 种预置角色)
├── api/                     # REST API 服务 (50+ 端点)
│   ├── server.go            #   核心服务器 + 聊天
│   ├── handler_knowledge.go #   知识库 API
│   ├── handler_webhook.go   #   Webhook API
│   ├── handler_cron.go      #   定时任务 API
│   └── handler_misc.go      #   记忆/MCP/技能/路由/Canvas/语音 API
├── audit/                   # 安全审计 (7 类检查)
├── cache/                   # LLM 响应语义缓存
├── canvas/                  # Canvas/A2UI (8 种组件)
├── config/                  # 配置管理 (YAML + 环境变量)
├── cron/                    # 定时任务调度
├── desktop/                 # 桌面集成 (通知/剪贴板)
├── engine/                  # Agent 引擎（ReAct 推理循环）
├── gateway/                 # 六层安全网关
├── heartbeat/               # 心跳巡查
├── knowledge/               # 知识库 (FTS5 + 向量混合检索)
├── llmrouter/               # LLM 智能路由
├── mcp/                     # MCP Client (stdio + SSE)
├── memory/                  # 文件记忆 (MEMORY.md + 日记)
├── router/                  # 多 Agent 路由
├── session/                 # 会话管理 + 上下文压缩
├── skill/                   # Skill 系统
│   ├── builtin/             #   内置 Skill (搜索/天气/翻译/摘要)
│   ├── marketplace/         #   Markdown 技能市场
│   └── sandbox/             #   沙箱执行
├── storage/                 # 数据存储
│   └── sqlite/              #   SQLite 驱动
├── voice/                   # 语音交互 (STT/TTS)
├── webhook/                 # Webhook 接收
├── go.mod
└── Makefile
```

## API 端点

### 核心
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/health` | 健康检查 |
| POST | `/api/v1/chat` | 聊天（支持角色选择） |
| GET | `/api/v1/roles` | 角色列表 |

### 知识库
| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/knowledge/documents` | 上传文档 |
| GET | `/api/v1/knowledge/documents` | 文档列表 |
| DELETE | `/api/v1/knowledge/documents/{id}` | 删除文档 |
| POST | `/api/v1/knowledge/search` | 搜索 |

### 定时任务
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/cron/jobs` | 任务列表 |
| POST | `/api/v1/cron/jobs` | 添加任务 |
| DELETE | `/api/v1/cron/jobs/{id}` | 删除任务 |
| POST | `/api/v1/cron/jobs/{id}/pause` | 暂停任务 |
| POST | `/api/v1/cron/jobs/{id}/resume` | 恢复任务 |

### Webhook
| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/webhooks/{name}` | 接收 Webhook 事件 |
| GET | `/api/v1/webhooks` | 列表 |
| POST | `/api/v1/webhooks` | 注册 |
| DELETE | `/api/v1/webhooks/{name}` | 删除 |

### 其他端点
| 方法 | 路径 | 说明 |
|------|------|------|
| GET/POST | `/api/v1/memory` | 获取/保存记忆 |
| GET | `/api/v1/memory/search` | 搜索记忆 |
| GET | `/api/v1/mcp/tools` | MCP 工具列表 |
| GET | `/api/v1/mcp/servers` | MCP Server 列表 |
| GET | `/api/v1/skills` | 已安装技能 |
| POST | `/api/v1/skills/install` | 安装技能 |
| GET/POST/DELETE | `/api/v1/agents` | Agent 路由管理 |
| GET | `/api/v1/canvas/panels` | Canvas 面板列表 |
| GET | `/api/v1/voice/status` | 语音服务状态 |

## 开发

```bash
# 构建
go build ./...

# 运行测试
go test ./...

# 运行指定测试
go test -run TestName ./package/

# 代码检查
go vet ./...
golangci-lint run
```

## 技术栈

| 组件 | 技术 |
|------|------|
| 语言 | Go 1.25+ |
| Agent 框架 | [Hexagon](https://github.com/everyday-items/hexagon) |
| CLI | [Cobra](https://github.com/spf13/cobra) |
| 配置 | YAML + 环境变量 |
| 存储 | SQLite (modernc.org/sqlite) |
| WebSocket | nhooyr.io/websocket + gorilla/websocket |
| MCP | modelcontextprotocol/go-sdk |
| 安全 | Hexagon Guard Chain |

## 许可证

MIT License

## 致谢

- [Hexagon](https://github.com/everyday-items/hexagon) — AI Agent 全能型框架
- [ai-core](https://github.com/everyday-items/ai-core) — AI 基础能力库
- [toolkit](https://github.com/everyday-items/toolkit) — Go 通用工具库
