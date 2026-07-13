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
- **ReAct Agent 引擎** — 推理 + 行动循环，支持多轮工具调用、流式输出、结构化交互消息，以及 `plan-execute` / `reflection` / `tot` 等 Agent 模式
- **六层安全网关** — 认证、限流、成本控制、注入检测、权限校验、审计日志
- **LLM 智能路由** — 多 Provider 自动切换，故障降级，成本优化，模型 tool_call 能力探测
- **Skill 系统** — 内置搜索/天气/翻译/摘要/媒体生成/送达/文档导出等，7 阶段流水线，`.pending` 审批闭环，TrustLevel 与 TOCTOU 校验
- **语义缓存** — Singleflight 防击穿 + TTL 抖动防雪崩 + 空值缓存防穿透
- **知识库** — FTS5 + 向量混合检索，RAG 5 阶段 Pipeline，Ollama 嵌入模型自动发现/安装与有证据召回
- **场景包扩展** — `scenario` 六缝注入（记录集、约束、视图槽、Agent mode、按钮、eval），平台不硬编码具体业务
- **通用记录本** — `records.agent_records` 以 Agent 为隔离键，支持状态机、去重键、到期队列、乐观锁和场景包字段校验

### 内置技能

开箱即用、无需安装的内置 Skill（通过 LLM tool_call 调用）：

| 技能 | 功能 |
|------|------|
| `search` | 网络搜索，查找互联网上的信息 |
| `weather` | 查询城市天气信息 |
| `translate` | 翻译文本内容，支持中英互译 |
| `summary` | 对文本内容进行摘要概括 |
| `browser` | 网页获取、内容提取和表单提交 |
| `code_exec` | 推荐执行原语：在 HexClaw 沙箱内执行 snippet/file/module/project（Go/Python/JavaScript/项目命令），返回 `run_id`、限额、诊断和产物清单 |
| `code` / `shell` | 兼容保留的裸宿主执行工具，已弃用；新任务应迁移到 `code_exec` |
| `file_ops` / `file_edit` | 在工作区内读写、编辑文件 |
| `list_directory` / `read_file` / `list_allowed_directories` | 通过 FileAccessBroker 读取用户授权目录，供 `code_exec` 与连接器共享文件访问边界 |
| `grep` / `glob` | 按文本/正则搜索文件内容，按名称模式查找文件 |
| `knowledge_ingest` | 把文本内容写入本地知识库供后续检索 |
| `knowledge_ingest_path` | 读取路径（目录或 glob）下每个文件的内容，逐个入库（沙箱内防 `..`/软链逃逸，单次上限 200 文件 / 2 MiB·文件） |
| `knowledge_search` | 搜索本地知识库，返回结构化分片、来源和分数 |
| `manage_memory` / `session_search` | 管理文件记忆、检索历史会话 |
| `media_generate` | 从文本提示词生成图片（默认）或视频，落盘后返回稳定文件路径，可供导出/送达/入库复用 |
| `export_document` | 把 Markdown 渲染成可下载文档（md/html/docx/pdf/epub/odt/rtf/txt）并返回文件路径 |
| `send_message` | 把消息发送到已配置渠道（飞书/Discord/微信/邮件/Slack 等）；交互式会话默认经确认门，无人值守自动化由 `security.autonomy` 矩阵决定 |
| `cron_task` | 创建/列出/暂停/恢复/移除应用托管的定时任务 |
| `manage_skill` / `manage_mcp` | 从 HexClaw Hub 搜索、安装、移除技能 / MCP Server；无人值守默认不自动执行，需显式打开 `capability` |
| `app_query` / `app_heal` | 脱敏查询应用状态，或对 cron/workflow 做受控自愈 |
| `transfer_to_agent` / `list_agents` / `orchestrate` / `spawn_agent` / `solve` | 多 Agent 分派、编排、派生运行和带独立 `code_exec` 验算的解题工具 |
| `k12_grade` / `k12_review` | K12 场景包技能：批改入错题本、生成复习变式题（启用 K12 场景包时注册） |

> 无人值守自动化（cron/webhook/spawn/heartbeat/workflow）采用“功能优先 Profile + 显式开关矩阵”：默认 `function_first` 放行 exec 类（推荐 `code_exec`，兼容 `code`/`shell`）、文件编辑、浏览、知识入库、送达等核心任务；Skill/MCP 管理、发布、伪造 `solve` 来源等高后果能力默认不自动放行，需要在 `security.autonomy.system_dispatch` 或 `full_access` profile 中显式打开。显式 `PermissionPolicy` deny 仍是最高优先级。
> `system_dispatch.<source>` 是替换该来源的 profile 默认值，不是增量合并；只想全局放开时直接使用 `profile: full_access`。

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
- **MCP 原生支持** — 兼容 3200+ MCP Server（stdio + SSE + streamable 传输）
- **Markdown 技能市场** — 兼容 OpenClaw 技能格式，按需延迟加载
- **多 Agent 路由** — 一个实例托管多个 Agent，按平台/用户/群组路由
- **K12 家长辅导场景包** — 出厂内置作业图片识题/批改、错题本、变式复习、备课卡、年级约束和默认 cron 投递
- **Canvas / A2UI** — Agent 生成交互式 UI（图表、表单、看板等 8 种组件）
- **安全审计 CLI** — `hexclaw security audit` 一键安全检查 + 修复建议
- **语音交互** — STT/TTS 转写与合成，支持 MiniMax / Edge / OpenAI / Azure TTS 串联 fallback
- **桌面集成** — 系统通知、剪贴板交互（Tauri 桌面端）
- **实时日志** — WebSocket 日志流 + 统计分析

### 多平台接入（13 种）

| 平台 | 方式 | 状态 |
|------|------|:----:|
| Web UI | WebSocket | ✅ |
| 飞书 | SDK WebSocket + HTTP Webhook | ✅ |
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
  autonomy:
    # function_first(default) / balanced / strict / full_access
    profile: function_first
    # 可选显式覆盖；值支持类别、精确工具名、glob 或 "*"。
    # 类别：read,browser,exec,files,automation,delivery,media,heal,capability,publish
    # system_dispatch:
    #   webhook: [read, browser, exec, files, delivery, media, capability]
    #   workflow: [read, browser, exec, files, automation, delivery, media, heal]

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
    branch: v0.0.6

heartbeat:
  enabled: true
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

features:
  # 产品级能力按功能优先默认开启；仅在需要回退/灰度时显式关闭。
  model.gateway.v1: true
  skill.pipeline.v1: true
  tool.lifecycle.v2: true
  tool.policy.engine: true
  config.tx.hotload.v1: true
  rag.pipeline.v1: true
  plugin.extension.v1: true
  agent.factory.real: true
  pricing.layered.v1: true
  mcp.lifecycle.v2: true
  eval.framework.v1: false

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

### Feature Flags

v0.4 新增能力统一通过 `features:` 段启用。未注册的 flag 永远返回关闭，`alpha`
阶段即使代码默认值写 true 也会强制关闭，避免实验能力意外进入生产路径。

常见 flag：
- `agent.factory.real`：允许按 `dispatch_role` 分派到真实 `hexagon.Agent`
- `skill.pipeline.v1`：启用 Skill 7 阶段执行流水线
- `tool.lifecycle.v2`：启用工具生命周期、Hook 优先级、panic 隔离和耗时指标
- `tool.policy.engine`：启用声明式 PermissionPolicy 权限策略
- `interactive.render.v1`：交互消息走平台原生 renderer；关闭时使用文本 fallback
- `config.tx.hotload.v1`：LLM 配置保存走事务热加载
- `model.gateway.v1`：启用 Provider middleware 链路
- `rag.pipeline.v1`：启用知识库 5 阶段 RAG Pipeline
- `pricing.layered.v1`：启用用户覆盖 / 缓存 / 远端 / 内置兜底的分层定价查询
- `mcp.lifecycle.v2`：启用 MCP Server 上下线生命周期 Hook
- `plugin.extension.v1`：启用插件 Manifest 与 capability 校验
- `eval.framework.v1`：评测框架，alpha 阶段默认关闭，发版工具会显式打开
- `voice.tts.chain.v1`：启用多 TTS Provider 串联 fallback

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
├── cmd/
│   ├── hexclaw/             # CLI 入口 (serve/init/version/security audit/skill)
│   └── verify-release/      # 发版门禁/Eval/canary dry-run 校验器
├── acp/                     # Agent Client Protocol 桥接
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
│   ├── whauth/              #   WhatsApp 验签辅助
│   ├── line/                #   LINE
│   ├── matrix/              #   Matrix
│   └── email/               #   Email (IMAP/SMTP)
├── agents/                  # Agent 角色 (6 种预置角色) + 分派/工厂/团队
├── api/                     # REST API 服务
│   ├── server.go            #   核心服务器 + 聊天 + 路由注册
│   ├── handler_config.go    #   LLM 配置查询/更新/测试/模型发现 API
│   ├── handler_capabilities.go # 模型 tool_call 能力探测 API
│   ├── handler_extended.go  #   工作流/配置/版本/统计 API
│   ├── handler_logs.go      #   日志查询/统计/实时流 API
│   ├── handler_knowledge.go #   知识库 API
│   ├── handler_webhook.go   #   Webhook API
│   ├── handler_cron.go      #   定时任务 API
│   ├── handler_cronjob_unified.go # 定时任务统一入口 (POST /cronjob)
│   ├── handler_voicechat.go #   语音 STT/TTS + voicechat API
│   └── handler_misc.go      #   记忆/MCP/技能/路由/Canvas API
├── audit/                   # 安全审计 (7 类检查)
├── autonomy/                # 无人值守权限治理（决策审计 / 任务级授权 / 预检）
├── canvas/                  # Canvas/A2UI (8 种组件)
├── config/                  # 配置管理 (YAML + 环境变量)
├── connector/               # 数据连接器（GitHub/Notion 等只读资源）
├── cron/                    # 定时任务调度
├── desktop/                 # 桌面集成 (通知/剪贴板)
├── egress/                  # 分级隐私出网策略（用途 × 数据敏感类）
├── engine/                  # Agent 引擎（ReAct 推理循环）
├── eval/                    # 发版前评测套件
├── featureflag/             # Feature flag 注册与运行时查询
├── gateway/                 # 六层安全网关
│   └── llmcall/             #   LLM 调用 gateway (中间件链路)
├── heartbeat/               # 心跳巡查
├── httpua/                  # 出站 HTTP User-Agent 统一注入
├── instances/               # 平台实例生命周期管理
├── internal/                # 内部工具 (sqliteutil / upstreamerr / testutil)
├── knowledge/               # 知识库 (FTS5 + 向量混合检索)
├── library/                 # Prompt 库 / 运营下发条目
├── llmrouter/               # LLM 智能路由
├── mcp/                     # MCP Client (stdio + SSE + streamable)
├── memory/                  # 文件记忆 (MEMORY.md + 日记)
├── plugin/                  # 插件 Manifest / Capability 扩展
├── records/                 # 通用记录本原语 agent_records
├── release/                 # 发版门禁与 canary 状态机
├── render/                  # Markdown/文档渲染 (pandoc + LRU 缓存)
├── router/                  # 多 Agent 路由
├── scenario/                # 场景包六缝注册表
├── scenarios/
│   └── k12/                 # K12 家长辅导场景包
├── secret/                  # 静态凭据加密主密钥与信封
├── security/                # 注入扫描 / 内容净化 / 技能扫描
├── session/                 # 会话管理 + 上下文压缩
├── skill/                   # Skill 系统
│   ├── builtin/             #   内置 Skill (搜索/天气/翻译/摘要/媒体生成/送达/文档导出 等)
│   ├── chain/               #   Skill Pipeline 链
│   ├── hub/                 #   在线技能目录 (hexclaw-hub)
│   ├── marketplace/         #   Markdown 技能市场
│   └── sandbox/             #   Skill 沙箱执行
├── storage/                 # 数据存储
│   ├── migrate/             #   迁移
│   └── sqlite/              #   SQLite 驱动
├── streamstate/             # 流式状态注册表
├── webhook/                 # Webhook 接收
├── go.mod
└── Makefile
```

> 媒体生成/genstore/缓存/trace/events 与底层 HTTP 工具等基础能力已下沉至 ai-core / toolkit / hexagon，hexclaw 不再保留本地等价实现；语音 STT/TTS 由 `api/` 与 `gateway/` 内联处理，无独立 `voice/` 包。

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
| POST | `/api/v1/sessions` | 创建会话 |
| GET | `/api/v1/sessions` | 会话列表 |
| GET | `/api/v1/sessions/{id}` | 会话详情 |
| PATCH | `/api/v1/sessions/{id}` | 更新会话元信息 |
| POST | `/api/v1/sessions/{id}/suggest-title` | 自动生成会话标题 |
| DELETE | `/api/v1/sessions/{id}` | 删除会话 |
| GET | `/api/v1/sessions/{id}/messages` | 消息历史 |
| POST | `/api/v1/sessions/{id}/messages` | 追加单条消息 |
| POST | `/api/v1/sessions/{id}/messages/batch` | 批量追加消息 |
| GET | `/api/v1/sessions/{id}/branches` | 会话分支列表 |
| POST | `/api/v1/sessions/{id}/fork` | 分支对话 |
| GET | `/api/v1/messages/search` | 全文搜索消息 |
| DELETE | `/api/v1/messages/{id}` | 删除单条消息 |
| PUT | `/api/v1/messages/{id}/feedback` | 写入消息反馈 |
| GET | `/api/v1/streams/active` | 当前活跃流式请求 |
| GET | `/api/v1/streams/{request_id}` | 流式请求快照/恢复 |
| GET | `/api/v1/sessions/{id}/checkpoints` | 会话检查点列表（启用 checkpoint 时） |

### 配置
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/config` | 获取完整配置（不含 API Key 明文） |
| PUT | `/api/v1/config` | 更新配置 |
| GET | `/api/v1/config/llm` | 获取 LLM 配置 |
| PUT | `/api/v1/config/llm` | 更新 LLM 配置 |
| POST | `/api/v1/config/llm/test` | 测试单个 Provider 连通性（不落盘；本地 Ollama 可无 Key） |
| POST | `/api/v1/config/llm/models` | 动态获取 Provider 可用模型列表（代理到 Provider `/models` API） |
| GET | `/api/v1/config/memory` | 获取自动记忆、主动召回、画像蒸馏等记忆行为配置 |
| PUT | `/api/v1/config/memory` | 字段级更新记忆行为配置 |
| GET | `/api/v1/llm/capabilities` | 列出已缓存的模型 tool_call 能力探测结果 |
| POST | `/api/v1/llm/capabilities/probe` | 立即探测指定 `provider` + `model` 的 tool_call 可靠度 |

### 助手 / Prompt 库 / 连接器
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/assistant/soul` | 获取助手人格 / soul 文本 |
| PUT | `/api/v1/assistant/soul` | 更新助手人格 / soul 文本 |
| GET | `/api/v1/connections` | 列出可配置连接类型与状态 |
| POST | `/api/v1/connections/test` | 无状态测试平台/Provider 凭据 |
| GET | `/api/v1/connectors` | 连接器脱敏列表（启用 connector store 时） |
| POST | `/api/v1/connectors` | 创建并加密保存连接器 |
| DELETE | `/api/v1/connectors/{id}` | 删除连接器 |
| POST | `/api/v1/connectors/test` | 无状态测试连接器凭据 |
| GET | `/api/v1/connectors/{id}/resources` | 读取连接器只读资源列表 |
| GET | `/api/v1/prompts` | 列出启用 Prompt 库条目 |
| GET | `/api/v1/prompts/all` | 列出全部 Prompt 库条目 |
| POST | `/api/v1/prompts` | 创建或更新 Prompt 条目 |
| DELETE | `/api/v1/prompts/{id}` | 删除 Prompt 条目 |

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
| GET | `/api/v1/knowledge/config` | 获取知识库检索配置 |
| PUT | `/api/v1/knowledge/config` | 更新知识库检索配置 |

### 文档解析 / 渲染
| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/documents/extract` | 上传 PDF/DOC/PPTX 等并抽取纯文本 |
| POST | `/api/v1/documents/preview` | 暂存原文件并返回预览 token |
| GET | `/api/v1/documents/preview/{token}` | 预览/下载暂存原文件 |
| POST | `/api/v1/render` | Markdown 渲染为 md/html/docx/pdf/epub/odt/rtf/txt（启用 render 服务时） |

### 定时任务
统一入口 `POST /api/v1/cronjob` 以请求体中的 `action` 字段分发（`create` / `update` / `remove` / `pause` / `resume` / `run` / `list` / `history`），支持 `idempotency_key` 幂等重放。

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/cronjob` | 定时任务统一入口（按 `action` 分发增删改/暂停恢复/手动触发/列表/历史） |
| POST | `/api/v1/cron/jobs/stream` | 创建任务（SSE 流式编译，实时推送 progress/done/error） |
| POST | `/api/v1/cron/parse` | 解析/校验 cron 表达式并返回下次触发时间 |
| GET | `/api/v1/cron/jobs/{id}/history` | 执行历史（历史项含 `result` 输出摘要） |

### Webhook
| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/v1/webhooks/{name}` | 接收 Webhook 事件 |
| GET | `/api/v1/webhooks` | 列表 |
| POST | `/api/v1/webhooks` | 注册 |
| PATCH | `/api/v1/webhooks/{name}` | 更新 Webhook 启用状态 |
| DELETE | `/api/v1/webhooks/{name}` | 删除 |

### 自动化权限治理
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/autonomy/profile` | 获取无人值守权限 Profile |
| PUT | `/api/v1/autonomy/profile` | 更新无人值守权限 Profile |
| POST | `/api/v1/autonomy/preflight` | 对创建中的自动化任务做权限预检 |
| GET | `/api/v1/autonomy/summary` | 权限治理总览与阻断摘要 |
| GET | `/api/v1/autonomy/decisions` | 权限决策审计日志 |
| GET | `/api/v1/autonomy/grants` | 任务级授权列表 |
| POST | `/api/v1/autonomy/grants` | 创建任务级授权 |
| DELETE | `/api/v1/autonomy/grants/{id}` | 撤销任务级授权 |

### 记忆
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/memory` | 获取记忆 |
| POST | `/api/v1/memory` | 创建记忆 |
| PUT | `/api/v1/memory` | 更新记忆（允许清空） |
| PUT | `/api/v1/memory/{id}` | 更新单条记忆 |
| POST | `/api/v1/memory/{id}/archive` | 归档单条记忆 |
| POST | `/api/v1/memory/{id}/restore` | 恢复单条记忆 |
| POST | `/api/v1/memory/{id}/pin` | 置顶单条记忆 |
| POST | `/api/v1/memory/{id}/unpin` | 取消置顶单条记忆 |
| DELETE | `/api/v1/memory` | 清空全部记忆 |
| DELETE | `/api/v1/memory/{id}` | 删除指定记忆 |
| GET | `/api/v1/memory/search` | 搜索记忆 |

### MCP
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/mcp/tools` | 工具列表 |
| GET | `/api/v1/mcp/servers` | Server 列表 |
| POST | `/api/v1/mcp/servers` | 运行时添加并持久化 MCP Server |
| DELETE | `/api/v1/mcp/servers/{name}` | 移除 MCP Server |
| GET | `/api/v1/mcp/status` | 连接状态快照 |
| POST | `/api/v1/mcp/tools/call` | 调用工具 |

### 技能
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/skills` | 已安装技能 |
| GET | `/api/v1/skills/{name}/content` | 查看已安装技能内容 |
| PUT | `/api/v1/skills/{name}/status` | 启用/禁用技能（返回运行态字段） |
| POST | `/api/v1/skills/install` | 安装技能（`clawhub://name` 或本地相对路径） |
| POST | `/api/v1/skills/generate` | 对话式生成 Skill 草稿并安装 |
| DELETE | `/api/v1/skills/{name}` | 卸载技能 |
| GET | `/api/v1/clawhub/search` | ClawHub 技能搜索（支持 `q` / `category`） |
| GET | `/api/v1/clawhub/skills/{name}/content` | 安装前预览 ClawHub 技能内容 |

默认技能目录仓库：`https://github.com/hexagon-codes/hexclaw-hub` 的 `v0.0.6` 标签（`index.json` + `skills/*.md`）。
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
| PUT | `/api/v1/platforms/instances/by-id/{id}` | 按稳定 ID 更新实例 |
| DELETE | `/api/v1/platforms/instances/by-id/{id}` | 按稳定 ID 删除实例 |
| POST | `/api/v1/platforms/instances/by-id/{id}/test` | 按稳定 ID 测试实例配置 |
| POST | `/api/v1/platforms/instances/by-id/{id}/send-test` | 按稳定 ID 发送测试消息 |
| PUT | `/api/v1/platforms/instances/{name}` | 更新实例 |
| DELETE | `/api/v1/platforms/instances/{name}` | 删除实例 |
| GET | `/api/v1/platforms/instances/{name}/health` | 单实例健康状态 |
| POST | `/api/v1/platforms/instances/{name}/test` | 测试实例配置 |
| POST | `/api/v1/platforms/instances/{name}/start` | 启动实例 |
| POST | `/api/v1/platforms/instances/{name}/stop` | 停止实例 |
| POST | `/api/v1/im/channels/{provider}/test` | 测试 IM 通道配置 |
| GET | `/api/v1/channels/wecom/guide` | 获取企业微信配置指引 |
| GET | `/api/v1/platforms/hooks/{provider}/{name}` | 平台回调验证 / GET hook |
| POST | `/api/v1/platforms/hooks/{provider}/{name}` | 平台回调事件入口 |

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
| POST | `/api/v1/canvas/runs/{id}/resume` | 从失败/中断节点续跑工作流 |
| GET | `/api/v1/subagents/runs` | 查询子 Agent 运行记录 |

### 媒体生成 / 产物文件
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/images/status` | 图片生成 Provider 状态 |
| POST | `/api/v1/images/generate` | 生成图片 |
| GET | `/api/v1/videos/status` | 视频生成 Provider 状态 |
| POST | `/api/v1/videos/generate` | 提交异步视频生成任务 |
| GET | `/api/v1/videos/tasks/{id}` | 轮询视频生成任务 |
| GET | `/api/v1/voicechat/status` | 语音对话 Provider 状态 |
| POST | `/api/v1/voicechat/chat` | 语音对话 |
| GET | `/api/v1/files/generated/{path...}` | 访问图片/视频/文档等生成产物 |

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

### Ollama 本地模型
| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/v1/ollama/status` | 探测本地 Ollama 服务与模型 |
| POST | `/api/v1/ollama/pull` | 拉取模型 |
| GET | `/api/v1/ollama/running` | 列出运行中模型 |
| POST | `/api/v1/ollama/load` | 加载模型 |
| POST | `/api/v1/ollama/unload` | 卸载模型 |
| DELETE | `/api/v1/ollama/models/{name}` | 删除模型 |
| POST | `/api/v1/ollama/restart` | 重启 Ollama 服务 |

### 场景包
场景包通过 `srv.Mount` 挂载在 `/api/<scenario>` 前缀下，并继承远程访问鉴权。当前内置 K12 家长辅导场景包挂在 `/api/k12/*`，端点契约见 [scenarios/k12/API.md](scenarios/k12/API.md)。

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
- 平台实例推荐在前端用 `by-id` 路由做更新/删除/测试，避免显示名重命名后误操作；`GET/POST /api/v1/platforms/hooks/{provider}/{name}` 由平台适配器复用为回调入口。
- 图片/视频生成优先返回 `file_path`，前端拼接 `/api/v1/files/generated/{path}` 访问，避免把大 base64 写入 SQLite。
- `GET /api/v1/logs` 的日志项包含稳定 `domain` 字段，可按 `chat / knowledge / integration / automation / engine` 等功能域过滤。
- `POST /api/v1/config/llm/models` 向 Provider 的 `/models` 端点发起代理请求，返回标准化的模型列表（`{ models: [{ id, name }] }`）；支持 OpenAI 标准格式和替代格式的自动适配。
- `GET /api/v1/llm/capabilities` 返回 `{ provider_name, model_name, tool_call, tool_call_text, last_probe, probe_error }`；`POST /api/v1/llm/capabilities/probe?provider=X&model=Y` 会实时重测并写入 SQLite 缓存。

## 开发

### 前置要求

| 工具 | 版本要求 |
|------|---------|
| Go | >= 1.25.7 |
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
# release/CI 模式编译校验（避免本地 go.work 掩盖未发布依赖 API）
GOWORK=off go test ./... -run '^$'

# 构建
go build ./...

# 运行测试（runner 完整性探针默认跳过；需取证时设 HEXCLAW_RUNNER_PROBE=1）
go test ./...

# 运行指定测试
go test -run TestName ./package/

# 代码检查
go vet ./...
golangci-lint run

# 发版前门禁 + Eval + canary dry-run
go run ./cmd/verify-release -repo . -version 0.5.0-beta \
  -version-files hexclaw.go,cmd/hexclaw/main.go,api/openapi.yaml,README.md,README.en.md,SECURITY.md,SECURITY.zh.md
```

## 技术栈

| 组件 | 技术 |
|------|------|
| 语言 | Go 1.25.7+ 兼容基线（发版工具链 1.25.12） |
| Agent 框架 | [Hexagon](https://github.com/hexagon-codes/hexagon) v0.5.9 |
| AI 基础库 | [ai-core](https://github.com/hexagon-codes/ai-core) v0.2.4 |
| 工具库 | [toolkit](https://github.com/hexagon-codes/toolkit) v0.2.6 |
| CLI | [Cobra](https://github.com/spf13/cobra) |
| 配置 | YAML + 环境变量 |
| 存储 | SQLite (modernc.org/sqlite) |
| WebSocket | nhooyr.io/websocket + gorilla/websocket |
| MCP | modelcontextprotocol/go-sdk v1.5.0 |
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
- 提交前请确保 `make test` 全部通过；runner 完整性探针这类故意失败用例必须默认跳过或放入手工 workflow

## 相关项目

| 项目 | 说明 | 仓库 |
|------|------|------|
| **Hexagon** | Go AI Agent 框架 (核心引擎) v0.5.9 | [hexagon](https://github.com/hexagon-codes/hexagon) |
| **ai-core** | AI 基础能力库 (LLM/Tool/Memory) v0.2.4 | [ai-core](https://github.com/hexagon-codes/ai-core) |
| **toolkit** | Go 通用工具库 v0.2.6 | [toolkit](https://github.com/hexagon-codes/toolkit) |
| **hexagon-ui** | Hexagon Dev UI 观测面板 (Vue 3) | [hexagon-ui](https://github.com/hexagon-codes/hexagon-ui) |
| **hexclaw-desktop** | HexClaw 桌面客户端 (Tauri + Vue 3) | [hexclaw-desktop](https://github.com/hexagon-codes/hexclaw-desktop) |
| **hexclaw-ui** | HexClaw Web 前端 (Vue 3) | [hexclaw-ui](https://github.com/hexagon-codes/hexclaw-ui) |

## 更新日志

### v0.5.0-beta（2026-07-13）

**场景包与记录系统**
- **场景包六缝扩展** — 新增 `scenario` 注册表，统一注入记录集、约束、视图槽、Agent mode、按钮和 eval suite，平台层不硬编码业务包。
- **通用记录本** — 新增 `records.agent_records`，以 Agent 为隔离键，支持 schema 校验、去重键、到期复习队列、状态机和乐观锁。
- **K12 家长辅导包** — 打通作业图片识题内联回显、学科/题型标注、空白题求解、批改入本、错题纠正、变式复习、备课卡与默认 cron 投递。

**模型、知识与执行**
- **推理与多模态路由** — 解题/批改使用专用 reasoning model；视觉、embedding、rerank 按用途选路，failover 时重建符合目标 locality 的请求。
- **嵌入与召回闭环** — 自动发现 Ollama 嵌入模型，提供状态/安装接口；短输入和场景会话增加注入门控，无证据召回不再伪造命中。
- **执行原语收敛** — `code_exec` 成为推荐执行入口，支持 snippet/file/module/project 与 artifact metadata；`code`/`shell` 保留兼容但标记弃用，沙箱能力收敛到 toolkit + `skill/sandbox`。

**稳定性与送达**
- **视觉图片预算** — 按路由策略限制历史图片，上游报图片数超限时保留当轮图、淘汰最旧图重试，避免多轮作业批改失败或超时。
- **钉钉图片闭环** — `picture` 消息经 `downloadCode` 进入多模态管道；成功、错误和超时都会撤回思考占位并送达终态消息。
- **适配器/工作流韧性** — 加固有界发送队列、webhook 体限额、MCP/IM 生命周期、条件节点和原子持久化；cron 编译改用文本推理模型。

**依赖与 CI/CD**
- **框架依赖升级** — `go.mod` 对齐 hexagon v0.5.9 / ai-core v0.2.4 / toolkit v0.2.6，保持 Go 1.25.7 兼容基线并指定 Go 1.25.12 发版工具链。
- **技能种子升级** — 首启内嵌技能支持版本感知升级，默认目录对齐 `hexagon-codes/hexclaw-hub` `v0.0.6`。
- **CI/CD 复验口径** — `sandbox-code-exec.yml` 专项验证强沙箱；普通 Linux CI 按后端能力门控真实沙箱用例，专项 workflow 使用 `HEXCLAW_P0_SANDBOX_PROOF=1`，runner 完整性探针仅在 `HEXCLAW_RUNNER_PROBE=1` 时手工触发。

> 完整发布历史见 [CHANGELOG.md](CHANGELOG.md)。

### v0.4.4

**新功能**
- **凭据静态加密** — 平台凭据以 AES-256-GCM 落盘加密（`enc:v1:` 信封 + 0600 主密钥）；历史明文透明回读，下次写入自动回填密文
- **注入扫描** — 纵深防御：cron 创建期（严格）+ exec 组装期；外泄/混淆族始终严格，指令覆盖族仅在有 skills/RAG 数据时放宽
- **统一权限闸 GA** — 声明式 `PermissionPolicy` 成为单一工具授权闸，无人值守按 `security.autonomy` profile + 显式矩阵放行
- **Skill 工具盘** — 新增 `export_document`/`knowledge_ingest`/`media_generate`/`send_message` 内置技能
- **library 记忆薄版** — 轻量 prompt/记忆库，每轮注入

**依赖与架构**
- **框架升级** — 升级到 hexagon v0.5.1 / ai-core v0.1.6 / toolkit v0.2.0（go.mod 去除 toolchain 行，Go 1.25.5）；上游均为带回归测试的缺陷修复（`streamx` 超时无损、`runtime/runner` 工具配对、`failover` 分类）。toolkit `crypto/sign` `APISigner` wire 格式 BREAKING 不影响本仓（仅用 `HMACSHA256` 原语）
- **能力下沉** — 媒体生成/genstore/SSRF/缓存/trace/events 迁移到 ai-core/toolkit/hexagon；gateway HMAC 改用 `toolkit/crypto/sign`
- **failover 下沉** — LLM failover 逻辑下沉到 ai-core/llm，hexclaw 删除本地等价实现，消费点改用 `llm.*`
- **sandbox 迁移** — Skill 沙箱包从顶层 `sandbox/` 迁移到 `skill/sandbox/`

**修复**
- **matrix 适配器** — Stop 幂等，消除二次调用 close(closed channel) panic
- **knowledge 时间衰减** — 零值 CreatedAt 不再被衰减清零（修复无时间戳 chunk 永不召回）
- **cron 多副本** — DB 原子领取 + fencing 防止多副本 job 双跑，fail-open 保纯内存行为
- **安全加固** — SSRF 仅放行 loopback（封禁元数据与内网地址）；文件操作 symlink 越界防护；WhatsApp webhook 验签 + 微信/企微常量时间比较；shell 改为功能优先执行模型
- **无人值守功能优先矩阵** — 默认 `function_first` 自动放行 `code_exec`、shell、文件编辑、浏览、知识入库、送达等核心自动化；Skill/MCP 管理、发布、伪造 `solve` 来源默认不自动放行，需显式 `security.autonomy` 开关或 `full_access` profile；显式 `PermissionPolicy` deny 仍可硬限制
- **SSRF 保留段（BUG-F4）** — cron Starlark `http_*` 补封 RFC6598 CGNAT `100.64.0.0/10`、`192.0.0.0/24`、`198.18.0.0/15`（含 IPv4-mapped IPv6 形式）

### v0.4.0

**新功能**
- **Feature flag 基建** — `features:` 配置段统一控制可灰度能力；产品级能力默认开启，未注册 flag 仍视为配置错误并关闭
- **模型能力探测** — 新增 `/api/v1/llm/capabilities` 与 `/probe`，缓存模型 tool_call 可靠度
- **Skill 闭环** — 新增 7 阶段 Pipeline、`skill_view` 渐进披露、`.pending` 审批、TrustLevel 与 TOCTOU 防护
- **交互式回复** — `Reply.Interactive` 支持 buttons/select/approval/card，并在 IM 适配器中提供文本 fallback
- **运行时治理** — 新增 Provider middleware、结构化事件、权限策略、MCP 生命周期 hook、RAG Pipeline、Runtime Sandbox 与发版门禁
- **语音增强** — 新增 MiniMax TTS 与多 Provider TTS 串联 fallback

### v0.3.0

**新功能**
- **动态模型发现** — 新增 `POST /api/v1/config/llm/models` 端点，代理到 Provider 的 `/models` API 获取可用模型列表，支持 OpenAI 格式（`{ data: [...] }`）和替代格式（`{ models: [...] }`）
- **MCP 路径 `~` 展开** — MCP Server 参数中的 `~` 和 `~/subpath` 自动展开为用户主目录，跨平台支持（macOS/Linux/Windows，基于 `os.UserHomeDir()`）

**修复**
- **飞书思考占位消息** — 飞书适配器收到消息后立即发送思考占位消息（如 "🤔 思考中..."），AI 处理完成后通过 `patchMessage` 替换为最终回复，SDK（WebSocket）和 Webhook 两条路径均已覆盖
- **流式工具调用修复** — `ProcessStream` 带工具时原使用 `pipeStreamWithTools`（不执行工具），修复为使用 `processStreamToolLoop`，完整执行工具 → 反馈结果 → 继续 LLM 推理循环
- **Reasoning 内容持久化** — `pipeStream` 和 `pipeStreamWithTools` 将 reasoning/thinking 内容流式推送给前端但未收集用于持久化，新增 `fullReasoning` 收集逻辑和 `SaveAssistantMessageWithMeta()` 方法，将 reasoning 保存到消息元数据 JSON

## 联系我们

- 官网: [hexclaw.net](https://hexclaw.net)
- 河蟹 AI: ai@hexclaw.net
- 河蟹支持: support@hexclaw.net
- Issues: [GitHub Issues](https://github.com/hexagon-codes/hexclaw/issues)
- 安全漏洞: 请参阅 [SECURITY.md](SECURITY.md)

### 微信公众号

关注 HexClaw 微信公众号，获取最新动态、使用教程和版本更新：

<p align="center">
  <img src=".github/assets/wechat-qrcode.jpg" alt="HexClaw 微信公众号" width="200" />
</p>

## 许可证

[Apache License 2.0](LICENSE)
