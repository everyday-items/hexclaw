<div align="center">
  <img src=".github/assets/logo.jpg" alt="HexClaw Logo" width="180" />
  <h1>HexClaw 河蟹</h1>
  <p><strong>企业级安全的个人 AI Agent</strong> — 安全 · 开源 · 自托管 · 易用 · 功能全面</p>

  [![CI](https://github.com/hexagon-codes/hexclaw/workflows/CI/badge.svg)](https://github.com/hexagon-codes/hexclaw/actions)
  [![Release](https://img.shields.io/github/v/release/hexagon-codes/hexclaw?include_prereleases)](https://github.com/hexagon-codes/hexclaw/releases)
  [![License](https://img.shields.io/github/license/hexagon-codes/hexclaw)](https://github.com/hexagon-codes/hexclaw/blob/main/LICENSE)
  [![Go Report Card](https://goreportcard.com/badge/github.com/hexagon-codes/hexclaw)](https://goreportcard.com/report/github.com/hexagon-codes/hexclaw)

  **[English](README.en.md) | 中文**

  > 基于 [Hexagon](https://github.com/hexagon-codes/hexagon) AI Agent 全能型框架构建
</div>

## 特性

### 核心能力
- **ReAct Agent 引擎** — 推理 + 行动循环，支持多轮工具调用与流式输出
- **六层安全网关** — 认证、限流、成本控制、注入检测、权限校验、审计日志
- **LLM 智能路由** — 多 Provider 自动切换，故障降级，成本优化
- **Skill 系统** — 内置搜索/天气/翻译/摘要，沙箱安全执行，Shell 白名单防护
- **语义缓存** — Singleflight 防击穿 + TTL 抖动防雪崩 + 空值缓存防穿透
- **知识库** — FTS5 + 向量混合检索，RAG 上下文增强

### 会话与数据
- **会话管理** — 创建/查询/删除会话，消息历史，会话分支 (fork)
- **全文搜索** — FTS5 驱动的消息搜索
- **上下文压缩** — LLM 驱动的旧消息摘要，防止 token 爆炸
- **文件驱动记忆** — MEMORY.md 长期记忆 + 每日日记，可审查可版本控制

### 自主行为
- **Heartbeat 主动巡查** — Agent 定期自主检查待办事项并通知
- **Cron 定时任务** — 定时报告、提醒、巡检（cron 表达式 + @every/@daily/@weekly）
- **Webhooks** — GitHub/GitLab/通用 JSON，HMAC-SHA256 签名验证
- **工作流引擎** — 可视化编排多步骤 Agent 工作流（Canvas Workflow）

### 生态扩展
- **MCP 原生支持** — 兼容 3200+ MCP Server（stdio + SSE 传输）
- **Markdown 技能市场** — 兼容 OpenClaw 技能格式，按需延迟加载
- **多 Agent 路由** — 一个实例托管多个 Agent，按平台/用户/群组路由
- **Canvas / A2UI** — Agent 生成交互式 UI（图表、表单、看板等 8 种组件）
- **安全审计 CLI** — `hexclaw security audit` 一键安全检查 + 修复建议
- **语音交互** — STT/TTS 转写与合成，支持多 Provider 接入
- **桌面集成** — 系统通知、剪贴板交互（Tauri 桌面端）
- **实时日志** — WebSocket 日志流 + 统计分析

### 多平台接入（13 种）

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
| WhatsApp | Cloud API Webhook | ✅ |
| LINE | Messaging API Webhook | ✅ |
| Matrix | Client-Server API | ✅ |
| Email | IMAP/SMTP | ✅ |
| REST API | HTTP | ✅ |

> **WebSocket 安全**：Web WebSocket 连接启用了 Origin 校验，仅允许 localhost 和 Tauri（`tauri://localhost`）来源，不再使用 `InsecureSkipVerify`。

## 快速开始

### 安装

```bash
# 从源码安装
go install github.com/hexagon-codes/hexclaw/cmd/hexclaw@latest

# 或使用预编译二进制（从 Releases 下载）
curl -sSL https://github.com/hexagon-codes/hexclaw/releases/latest/download/hexclaw-$(uname -s)-$(uname -m).tar.gz | tar xz
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

### Docker

```bash
docker run -d \
  --name hexclaw \
  -p 16060:16060 \
  -e DEEPSEEK_API_KEY="sk-xxx" \
  -v hexclaw-data:/data/.hexclaw \
  ghcr.io/hexagon-codes/hexclaw:latest
```

服务启动后：
- Web UI: `http://127.0.0.1:16060`
- 健康检查: `GET http://127.0.0.1:16060/health`
- 聊天 API: `POST http://127.0.0.1:16060/api/v1/chat`

### 使用 API

```bash
curl -X POST http://127.0.0.1:16060/api/v1/chat \
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
  port: 16060

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
  hub:
    repo_url: https://github.com/hexagon-codes/hexclaw-hub
    branch: v0.0.1

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

compaction:
  enabled: true
  max_messages: 50
  keep_recent: 10

knowledge:
  enabled: true
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
用户 → 平台适配器(13种) → 安全网关(6层) → Agent 路由 → Agent 引擎 → LLM Provider
         │                    │              │            │            │
   Web/飞书/Telegram    认证→限流→成本    多Agent路由   ReAct 推理    DeepSeek/OpenAI
   Discord/Slack/...    →安全→权限→审计   工作流引擎    Skill/MCP    Claude/Qwen/...
   钉钉/企微/微信/...                                   知识库RAG
   WhatsApp/LINE/...                                    会话分支
   Matrix/Email
```

### 六层安全网关

| 层级 | 名称 | 功能 | 异常策略 |
|:---:|------|------|---------|
| 1 | Auth | 身份认证（Token/API Key，constant-time 比较） | 拒绝 |
| 2 | RateLimit | 滑动窗口限流（每分钟/每小时，100K 窗口上限） | 拒绝 |
| 3 | CostCheck | 用户/全局月度预算检查 | **Fail-closed** |
| 4 | InputSafety | Prompt 注入检测 + PII 脱敏 | **Fail-closed** |
| 5 | Permission | RBAC 权限校验 | 拒绝 |
| 6 | Audit | 请求审计日志 | 放行（仅记录） |

> 第 3/4 层在服务异常时拒绝请求（fail-closed），而非静默放行。详见 [SECURITY.md](SECURITY.md)。

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
│   ├── wechat/              #   微信公众号
│   ├── whatsapp/            #   WhatsApp
│   ├── line/                #   LINE
│   ├── matrix/              #   Matrix
│   └── email/               #   Email (IMAP/SMTP)
├── agents/                  # Agent 角色 (6 种预置角色)
├── api/                     # REST API 服务 (71 个路由)
│   ├── server.go            #   核心服务器 + 聊天 + 路由注册
│   ├── handler_extended.go  #   工作流/配置/版本/统计 API
│   ├── handler_logs.go      #   日志查询/统计/实时流 API
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

## API 端点（常用接口摘录，完整路由按模块启用）

### 核心
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/health` | 健康检查 |
| POST | `/api/v1/chat` | 聊天（支持流式/同步、角色选择） |
| GET | `/api/v1/roles` | 角色列表 |
| GET | `/api/v1/version` | 版本信息 |
| GET | `/api/v1/stats` | 系统统计 |
| GET | `/api/v1/models` | 已配置 LLM 模型列表 |

### 会话管理
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/sessions` | 会话列表 |
| GET | `/api/v1/sessions/{id}` | 会话详情 |
| DELETE | `/api/v1/sessions/{id}` | 删除会话 |
| GET | `/api/v1/sessions/{id}/messages` | 消息历史 |
| GET | `/api/v1/sessions/{id}/branches` | 会话分支列表 |
| POST | `/api/v1/sessions/{id}/fork` | 分支对话 |
| GET | `/api/v1/messages/search` | 全文搜索消息 |

### 配置
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/config` | 获取完整配置（不含 API Key 明文） |
| PUT | `/api/v1/config` | 更新配置 |
| GET | `/api/v1/config/llm` | 获取 LLM 配置 |
| PUT | `/api/v1/config/llm` | 更新 LLM 配置 |
| POST | `/api/v1/config/llm/test` | 测试单个 Provider 连通性（不落盘；本地 Ollama 可无 Key） |

### 知识库
| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/knowledge/documents` | 上传文档 |
| POST | `/api/v1/knowledge/upload` | 上传文件并返回索引结果 |
| GET | `/api/v1/knowledge/documents` | 文档列表 |
| GET | `/api/v1/knowledge/documents/{id}` | 单个文档详情（含完整内容） |
| DELETE | `/api/v1/knowledge/documents/{id}` | 删除文档 |
| POST | `/api/v1/knowledge/documents/{id}/reindex` | 重建/重试单个文档索引 |
| POST | `/api/v1/knowledge/search` | 结构化搜索（`result` 和 `results` 均返回 `[]SearchHit` 数组，包含分片、来源、分数） |

### 定时任务
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/cron/jobs` | 任务列表 |
| POST | `/api/v1/cron/jobs` | 添加任务 |
| DELETE | `/api/v1/cron/jobs/{id}` | 删除任务 |
| POST | `/api/v1/cron/jobs/{id}/pause` | 暂停任务 |
| POST | `/api/v1/cron/jobs/{id}/resume` | 恢复任务 |
| POST | `/api/v1/cron/jobs/{id}/trigger` | 手动触发 |
| GET | `/api/v1/cron/jobs/{id}/history` | 执行历史 |

### Webhook
| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/webhooks/{name}` | 接收 Webhook 事件 |
| GET | `/api/v1/webhooks` | 列表 |
| POST | `/api/v1/webhooks` | 注册 |
| DELETE | `/api/v1/webhooks/{name}` | 删除 |

### 记忆
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/memory` | 获取记忆 |
| POST | `/api/v1/memory` | 创建记忆 |
| PUT | `/api/v1/memory` | 更新记忆（允许清空） |
| DELETE | `/api/v1/memory` | 清空全部记忆 |
| DELETE | `/api/v1/memory/{id}` | 删除指定记忆 |
| GET | `/api/v1/memory/search` | 搜索记忆 |

### MCP
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/mcp/tools` | 工具列表 |
| GET | `/api/v1/mcp/servers` | Server 列表 |
| GET | `/api/v1/mcp/status` | 连接状态快照 |
| POST | `/api/v1/mcp/tools/call` | 调用工具 |

### 技能
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/skills` | 已安装技能 |
| PUT | `/api/v1/skills/{name}/status` | 启用/禁用技能（返回运行态字段） |
| POST | `/api/v1/skills/install` | 安装技能（`clawhub://name` 或本地相对路径） |
| DELETE | `/api/v1/skills/{name}` | 卸载技能 |
| GET | `/api/v1/clawhub/search` | ClawHub 技能搜索（支持 `q` / `category`） |

默认技能目录仓库：`https://github.com/hexagon-codes/hexclaw-hub` 的 `v0.0.1` 标签（`index.json` + `skills/*.md`）。
安装或卸载 Markdown 技能后，会自动同步运行时技能注册表；通常无需重启 sidecar。

### Agent 路由
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/agents` | Agent 列表 |
| POST | `/api/v1/agents` | 注册 Agent |
| PUT | `/api/v1/agents/{name}` | 更新 Agent |
| DELETE | `/api/v1/agents/{name}` | 删除 Agent |
| POST | `/api/v1/agents/default` | 设置默认 Agent |
| GET | `/api/v1/agents/rules` | 路由规则列表 |
| POST | `/api/v1/agents/rules` | 新增路由规则 |
| POST | `/api/v1/agents/rules/test` | 测试路由并返回命中规则 |
| DELETE | `/api/v1/agents/rules/{id}` | 删除路由规则 |

### 平台实例 / IM 通道
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/platforms/instances` | 平台实例列表 |
| GET | `/api/v1/platforms/instances/health` | 全部实例健康状态 |
| POST | `/api/v1/platforms/instances` | 创建实例 |
| PUT | `/api/v1/platforms/instances/{name}` | 更新实例 |
| DELETE | `/api/v1/platforms/instances/{name}` | 删除实例 |
| GET | `/api/v1/platforms/instances/{name}/health` | 单实例健康状态 |
| POST | `/api/v1/platforms/instances/{name}/test` | 测试实例配置 |
| POST | `/api/v1/platforms/instances/{name}/start` | 启动实例 |
| POST | `/api/v1/platforms/instances/{name}/stop` | 停止实例 |
| POST | `/api/v1/im/channels/{provider}/test` | 测试 IM 通道配置 |

### Canvas / 工作流
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/canvas/panels` | 面板列表 |
| GET | `/api/v1/canvas/panels/{id}` | 面板详情 |
| POST | `/api/v1/canvas/events` | 推送事件 |
| GET | `/api/v1/canvas/workflows` | 工作流列表 |
| POST | `/api/v1/canvas/workflows` | 保存工作流 |
| DELETE | `/api/v1/canvas/workflows/{id}` | 删除工作流 |
| POST | `/api/v1/canvas/workflows/{id}/run` | 异步执行工作流 |
| GET | `/api/v1/canvas/runs/{id}` | 查询执行结果 |

### 语音
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/voice/status` | 语音服务状态 |
| POST | `/api/v1/voice/transcribe` | 语音转文字 (STT) |
| POST | `/api/v1/voice/synthesize` | 文字转语音 (TTS) |

### 桌面集成
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/desktop/info` | 桌面环境信息 |
| GET | `/api/v1/desktop/notifications` | 通知列表 |
| POST | `/api/v1/desktop/notifications` | 发送通知 |
| DELETE | `/api/v1/desktop/notifications` | 清空通知 |
| GET | `/api/v1/desktop/clipboard` | 读取剪贴板 |
| POST | `/api/v1/desktop/clipboard` | 写入剪贴板 |

### 团队协作
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/team/agents` | 团队共享 Agent 列表 |
| POST | `/api/v1/team/agents` | 共享 Agent 到团队 |
| DELETE | `/api/v1/team/agents/{id}` | 删除共享 Agent |
| GET | `/api/v1/team/members` | 团队成员列表 |
| POST | `/api/v1/team/members` | 邀请成员 |
| DELETE | `/api/v1/team/members/{id}` | 移除成员 |

### 日志与监控
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/logs` | 查询日志（支持 level/source/domain/keyword 过滤 + 分页） |
| GET | `/api/v1/logs/stats` | 日志统计（按 level/source 分类计数） |
| GET | `/api/v1/logs/stream` | 实时日志流 (WebSocket，需 Token 认证) |

### 与桌面端对齐的响应语义

- `POST /api/v1/config/llm/test` 返回 `ok`、`message`、`provider`、`model`、`latency_ms`；当 `provider.type=ollama` 时可省略 `api_key`，便于测试本地 OpenAI 兼容端点。
- `GET /api/v1/skills` 稳定返回 `enabled`；`PUT /api/v1/skills/{name}/status` 额外返回 `effective_enabled`、`requires_restart`、`message`。
- `POST /api/v1/skills/install` 支持 `clawhub://skill-name` 和本地相对路径；成功时返回 `requires_restart=false` 与 `runtime_registered=true`，表示已热同步到运行引擎。
- `GET /api/v1/cron/jobs/{id}/history` 的历史项包含 `result`，可直接查看最近一次执行输出摘要。
- `POST /api/v1/knowledge/search` 返回结构化结果数组（`result` 和 `results` 字段均为 `[]SearchHit`），包含文档标题、来源、chunk 位置、内容和相似度分数，适合直接在前端展示引用来源。`result` 不再是拼接后的纯字符串。
- `GET /api/v1/knowledge/documents/{id}` 返回单个文档的完整信息，包含全部内容。
- `GET /api/v1/knowledge/documents` 返回 `status`、`error_message`、`updated_at`、`source_type`；`POST /api/v1/knowledge/upload` 返回 `status`、`source`、`chunk_count`、`warnings`。
- `POST /api/v1/agents/rules/test` 会返回命中规则与分数，便于解释“为什么路由到这个 Agent”。
- `GET /api/v1/logs` 的日志项包含稳定 `domain` 字段，可按 `chat / knowledge / integration / automation / engine` 等功能域过滤。

## 开发

### 前置要求

| 工具 | 版本要求 |
|------|---------|
| Go | >= 1.25 |
| golangci-lint | 最新版（可选） |

### Make 命令

| 命令 | 说明 |
|------|------|
| `make build` | 构建二进制到 `bin/` |
| `make run` | 构建并启动服务 |
| `make test` | 运行所有测试 |
| `make test-cover` | 运行测试（含覆盖率） |
| `make fmt` | 代码格式化 |
| `make vet` | 静态检查 |
| `make lint` | golangci-lint 检查 |
| `make clean` | 清理构建产物 |
| `make init` | 初始化默认配置 |

### 手动命令

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
| Agent 框架 | [Hexagon](https://github.com/hexagon-codes/hexagon) v0.3.2-beta |
| AI 基础库 | [ai-core](https://github.com/hexagon-codes/ai-core) v0.0.4 |
| 工具库 | [toolkit](https://github.com/hexagon-codes/toolkit) v0.0.3 |
| CLI | [Cobra](https://github.com/spf13/cobra) |
| 配置 | YAML + 环境变量 |
| 存储 | SQLite (modernc.org/sqlite) |
| WebSocket | nhooyr.io/websocket + gorilla/websocket |
| MCP | modelcontextprotocol/go-sdk v1.4.1 |
| 安全 | Hexagon Guard Chain |

## 贡献指南

### 工作流程

1. Fork 本仓库
2. 创建功能分支: `git checkout -b feat/your-feature`
3. 提交更改: `git commit -m "feat: 添加新功能"`
4. 推送分支: `git push origin feat/your-feature`
5. 创建 Pull Request

### Commit Message 格式

遵循 [Conventional Commits](https://www.conventionalcommits.org/) 规范：

```
feat: 添加新功能
fix: 修复问题
docs: 文档更新
refactor: 重构
test: 测试相关
chore: 构建/工具链
```

### 代码规范

- 格式化: `make fmt`
- 静态检查: `make vet`
- Lint: `make lint`
- 提交前请确保 `make test` 全部通过

## 相关项目

| 项目 | 说明 | 仓库 |
|------|------|------|
| **Hexagon** | Go AI Agent 框架 (核心引擎) v0.3.2-beta | [hexagon](https://github.com/hexagon-codes/hexagon) |
| **ai-core** | AI 基础能力库 (LLM/Tool/Memory) v0.0.4 | [ai-core](https://github.com/hexagon-codes/ai-core) |
| **toolkit** | Go 通用工具库 v0.0.3 | [toolkit](https://github.com/hexagon-codes/toolkit) |
| **hexagon-ui** | Hexagon Dev UI 观测面板 (Vue 3) | [hexagon-ui](https://github.com/hexagon-codes/hexagon-ui) |
| **hexclaw-desktop** | HexClaw 桌面客户端 (Tauri + Vue 3) | [hexclaw-desktop](https://github.com/hexagon-codes/hexclaw-desktop) |
| **hexclaw-ui** | HexClaw Web 前端 (Vue 3) | [hexclaw-ui](https://github.com/hexagon-codes/hexclaw-ui) |

## 联系我们

- 河蟹 AI: ai@hexclaw.net
- 河蟹支持: support@hexclaw.net
- Issues: [GitHub Issues](https://github.com/hexagon-codes/hexclaw/issues)
- 安全漏洞: 请参阅 [SECURITY.md](SECURITY.md)

## 许可证

[Apache License 2.0](LICENSE)
