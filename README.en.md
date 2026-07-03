<div align="center">
  <img src=".github/assets/logo.jpg" alt="HexClaw Logo" width="180" />
  <h1>HexClaw</h1>
  <p><strong>Enterprise-Grade Personal AI Agent</strong> — Secure · Open Source · Self-Hosted · Easy to Use · Feature-Rich</p>

  [![CI](https://github.com/hexagon-codes/hexclaw/workflows/CI/badge.svg)](https://github.com/hexagon-codes/hexclaw/actions)
  [![Release](https://img.shields.io/github/v/release/hexagon-codes/hexclaw?include_prereleases)](https://github.com/hexagon-codes/hexclaw/releases)
  [![License](https://img.shields.io/github/license/hexagon-codes/hexclaw)](https://github.com/hexagon-codes/hexclaw/blob/main/LICENSE)
  [![Go Report Card](https://goreportcard.com/badge/github.com/hexagon-codes/hexclaw)](https://goreportcard.com/report/github.com/hexagon-codes/hexclaw)

  **English | [中文](README.md)**

  > Built on [Hexagon](https://github.com/hexagon-codes/hexagon) — the all-in-one AI Agent framework
</div>

## Features

### Core Capabilities
- **ReAct Agent Engine** — Reasoning + Action loop with multi-turn tool calls, streaming output, structured interactive replies, and Agent modes such as `plan-execute`, `reflection`, and `tot`
- **6-Layer Security Gateway** — Auth, rate limiting, cost control, injection detection, permission check, audit logging
- **LLM Smart Router** — Multi-provider auto-switching, failover, cost optimization, and model tool-call capability probing
- **Skill System** — Built-in search/weather/translation/summary/media-generation/messaging/document-export and more, 7-phase pipeline, `.pending` approval flow, TrustLevel filtering, and TOCTOU checks
- **Semantic Cache** — Singleflight anti-stampede + TTL jitter anti-avalanche + empty-value anti-penetration
- **Knowledge Base** — FTS5 + vector hybrid retrieval, 5-stage RAG pipeline, and context augmentation

### Built-in Skills

Ready-to-use built-in skills (no install required, invoked via LLM tool_call):

| Skill | Function |
|-------|----------|
| `search` | Web search to find information on the internet |
| `weather` | Look up city weather |
| `translate` | Translate text (Chinese ↔ English) |
| `summary` | Summarize text content |
| `browser` | Fetch web pages, extract content, and submit forms |
| `code` / `code_exec` | Execute code snippets (Go/Python/JavaScript) inside the HexClaw Sandbox and return output |
| `shell` | Run shell commands and return output |
| `file_ops` / `file_edit` | Read, write, and edit files in the workspace |
| `grep` / `glob` | Search file contents by text/regex; find files by name pattern |
| `knowledge_ingest` | Write text content into the local knowledge base for later retrieval |
| `knowledge_ingest_path` | Read each file under a path (directory or glob) and ingest its content into the knowledge base (sandboxed against `..`/symlink escape; capped at 200 files / 2 MiB per file) |
| `media_generate` | Generate an image (default) or video from a text prompt, persist it, and return a stable file path reusable by export/send/ingest |
| `export_document` | Render Markdown into a downloadable document (md/html/docx/pdf/epub/odt/rtf/txt) and return its file path |
| `send_message` | Send a message to a configured channel (feishu/Discord/WeChat/email/Slack/...); interactive sessions use confirmation, while unattended automation follows the `security.autonomy` matrix |
| `cron_task` | Create/list/pause/resume/remove app-managed scheduled tasks |
| `manage_skill` / `manage_mcp` | Search, install, or remove skills / MCP servers from HexClaw Hub; unattended dispatch does not auto-run these by default unless `capability` is explicitly enabled |

> Unattended automation (cron/webhook/spawn/heartbeat/workflow) uses a function-first profile plus an explicit switch matrix. The default `function_first` profile auto-approves core work such as `code_exec`, shell, file edits, browsing, knowledge ingest, and delivery. Skill/MCP management, publishing, and forgeable `solve` sources are not auto-approved by default; enable them through `security.autonomy.system_dispatch` or the explicit `full_access` profile. Explicit `PermissionPolicy` deny rules remain authoritative.
> `system_dispatch.<source>` replaces that source's profile default; it does not merge with it. Use `profile: full_access` when you want a global explicit open mode.

### Session & Data
- **Session Management** — Create/query/delete sessions, message history, session forking
- **Full-Text Search** — FTS5-powered message search
- **Context Compaction** — LLM-driven summarization of old messages to prevent token overflow
- **File-Driven Memory** — MEMORY.md long-term memory + daily journal, auditable and version-controlled

### Autonomous Behavior
- **Heartbeat Patrol** — Agent periodically checks todos and sends notifications autonomously
- **Cron Jobs** — Scheduled reports, reminders, inspections (cron expressions + @every/@daily/@weekly)
- **Webhooks** — GitHub/GitLab/generic JSON with HMAC-SHA256 signature verification
- **Workflow Engine** — Visual orchestration of multi-step Agent workflows (Canvas Workflow)

### Ecosystem & Extensions
- **Native MCP Support** — Compatible with 3200+ MCP Servers (stdio + SSE transport)
- **Markdown Skill Marketplace** — Compatible with OpenClaw skill format, lazy-loaded on demand
- **Multi-Agent Routing** — Host multiple agents in one instance, route by platform/user/group
- **Canvas / A2UI** — Agent-generated interactive UIs (charts, forms, kanban, and 8+ component types)
- **Security Audit CLI** — `hexclaw security audit` one-click security check + remediation suggestions
- **Voice Interaction** — STT/TTS transcription and synthesis with chained MiniMax / Edge / OpenAI / Azure TTS fallback
- **Desktop Integration** — System notifications, clipboard interaction (Tauri desktop client)
- **Real-Time Logs** — WebSocket log streaming + analytics

### Multi-Platform Support (13 platforms)

| Platform | Method | Status |
|----------|--------|:------:|
| Web UI | WebSocket | ✅ |
| Feishu | SDK WebSocket + HTTP Webhook | ✅ |
| Telegram | Long polling | ✅ |
| DingTalk | HTTP Webhook | ✅ |
| Discord | Gateway WebSocket | ✅ |
| Slack | Events API | ✅ |
| WeCom | HTTP Callback + AES | ✅ |
| WeChat Official Account | XML + passive/customer reply | ✅ |
| WhatsApp | Cloud API Webhook | ✅ |
| LINE | Messaging API Webhook | ✅ |
| Matrix | Client-Server API | ✅ |
| Email | IMAP/SMTP | ✅ |
| REST API | HTTP | ✅ |

> **WebSocket Security**: The Web WebSocket endpoint now validates the Origin header, allowing only localhost and Tauri (`tauri://localhost`) origins. `InsecureSkipVerify` is no longer used.

## Quick Start

### Installation

```bash
# Install from source
go install github.com/hexagon-codes/hexclaw/cmd/hexclaw@latest

# Or use a pre-built binary (download from Releases)
curl -sSL https://github.com/hexagon-codes/hexclaw/releases/latest/download/hexclaw-$(uname -s)-$(uname -m).tar.gz | tar xz
sudo mv hexclaw /usr/local/bin/
```

### Start the Service

```bash
# Set LLM API Key (choose one)
export DEEPSEEK_API_KEY="sk-xxx"
# export OPENAI_API_KEY="sk-xxx"
# export ANTHROPIC_API_KEY="sk-xxx"

# Start the service
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

After startup:
- Web UI: `http://127.0.0.1:16060`
- Health check: `GET http://127.0.0.1:16060/health`
- Chat API: `POST http://127.0.0.1:16060/api/v1/chat`

### Use the API

```bash
curl -X POST http://127.0.0.1:16060/api/v1/chat \
  -H "Content-Type: application/json" \
  -d '{"message": "Hello", "user_id": "test-user"}'
```

### Security Audit

```bash
hexclaw security audit
hexclaw security audit --config hexclaw.yaml
```

### Configuration File

```bash
# Generate default config
hexclaw init

# Start with custom config
hexclaw serve --config ~/.hexclaw/hexclaw.yaml
```

> For detailed installation and deployment guide, see [docs/install.en.md](docs/install.en.md)

## Configuration

Configuration file `~/.hexclaw/hexclaw.yaml`:

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
    # Optional explicit overrides. Values support categories, exact tool names,
    # glob patterns, or "*". Categories:
    # read,browser,exec,files,automation,delivery,media,heal,capability,publish
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
    branch: v0.0.2

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

features:
  # Product capabilities are function-first and enabled by default; override only for rollback/rollout.
  model.gateway.v1: true
  skill.pipeline.v1: true
  config.tx.hotload.v1: true
  rag.pipeline.v1: true
  runtime.sandbox.v1: true
  plugin.extension.v1: true
  agent.factory.real: true

skill:
  sandbox:
    enabled: true
    timeout: 30s

storage:
  driver: sqlite
  sqlite:
    path: ~/.hexclaw/data.db
```

All config values support environment variable substitution (`${VAR_NAME}`).

### Feature Flags

v0.4 capabilities are enabled through the `features:` section. Unknown flags always
resolve to disabled, and `alpha` flags are forced off by default even when their
registered default is true.

Common flags:
- `agent.factory.real`: allow `dispatch_role` to invoke a real `hexagon.Agent`
- `skill.pipeline.v1`: enable the 7-phase Skill execution pipeline
- `interactive.render.v1`: use native platform renderers for interactive replies; disabled uses text fallback
- `config.tx.hotload.v1`: update LLM config through transactional hot reload
- `model.gateway.v1`: enable the Provider middleware chain
- `events.transport.v1`: enable structured event sink delivery
- `rag.pipeline.v1`: enable the 5-stage knowledge RAG pipeline
- `voice.tts.chain.v1`: enable chained TTS provider fallback

## Architecture

```
User → Platform Adapters (13) → Security Gateway (6 layers) → Agent Router → Agent Engine → LLM Provider
        │                          │                              │              │               │
  Web/Feishu/Telegram         Auth→RateLimit→Cost          Multi-Agent       ReAct Loop     DeepSeek/OpenAI
  Discord/Slack/...           →Safety→RBAC→Audit           Workflow Engine   Skill/MCP      Claude/Qwen/...
  DingTalk/WeCom/WeChat                                                       Knowledge RAG
  WhatsApp/LINE/...                                                            Session Fork
  Matrix/Email
```

### 6-Layer Security Gateway

| Layer | Name | Function | Failure Policy |
|:-----:|------|----------|---------------|
| 1 | Auth | Token/API Key auth with constant-time comparison | Reject |
| 2 | RateLimit | Sliding window per user/hour (100K window max) | Reject |
| 3 | CostCheck | User/global monthly budget enforcement | **Fail-closed** |
| 4 | InputSafety | Prompt injection detection + PII redaction | **Fail-closed** |
| 5 | Permission | RBAC access control | Reject |
| 6 | Audit | Request audit logging | Pass (log only) |

> Layers 3/4 reject requests on service errors (fail-closed), rather than silently allowing through. See [SECURITY.md](SECURITY.md).

### Directory Structure

```
hexclaw/
├── hexclaw.go               # Root package (version info + package docs)
├── cmd/
│   ├── hexclaw/             # CLI entry (serve/init/version/security audit/skill)
│   └── verify-release/      # Release gate / Eval / canary dry-run verifier
├── acp/                     # Agent Client Protocol bridge
├── adapter/                 # Platform adapters
│   ├── web/                 #   Web WebSocket
│   ├── feishu/              #   Feishu Bot
│   ├── telegram/            #   Telegram Bot
│   ├── dingtalk/            #   DingTalk Bot
│   ├── discord/             #   Discord Bot
│   ├── slack/               #   Slack Bot
│   ├── wecom/               #   WeCom (Enterprise WeChat)
│   ├── wechat/              #   WeChat Official Account
│   ├── whatsapp/            #   WhatsApp
│   ├── whauth/              #   WhatsApp signature helper
│   ├── line/                #   LINE
│   ├── matrix/              #   Matrix
│   └── email/               #   Email (IMAP/SMTP)
├── agents/                  # Agent roles (6 preset roles) + dispatcher/factory/team
├── api/                     # REST API server
│   ├── server.go            #   Core server + chat + route registration
│   ├── handler_config.go    #   LLM config query/update/test/model discovery API
│   ├── handler_capabilities.go # Model tool-call capability probe API
│   ├── handler_extended.go  #   Workflow/config/version/stats API
│   ├── handler_logs.go      #   Log query/stats/stream API
│   ├── handler_knowledge.go #   Knowledge base API
│   ├── handler_webhook.go   #   Webhook API
│   ├── handler_cron.go      #   Cron job API
│   ├── handler_cronjob_unified.go # Unified cron entrypoint (POST /cronjob)
│   ├── handler_voicechat.go #   Voice STT/TTS + voicechat API
│   └── handler_misc.go      #   Memory/MCP/skill/router/canvas API
├── audit/                   # Security audit (7 check categories)
├── canvas/                  # Canvas/A2UI (8 component types)
├── config/                  # Configuration management (YAML + env vars)
├── cron/                    # Cron job scheduler
├── desktop/                 # Desktop integration (notifications/clipboard)
├── engine/                  # Agent engine (ReAct loop)
├── eval/                    # Pre-release eval suites
├── featureflag/             # Feature flag registry and runtime lookup
├── gateway/                 # 6-layer security gateway
│   └── llmcall/             #   LLM call gateway (middleware chain)
├── heartbeat/               # Heartbeat patrol
├── instances/               # Platform instance lifecycle manager
├── internal/                # Internal utils (sqliteutil / upstreamerr / testutil)
├── knowledge/               # Knowledge base (FTS5 + vector hybrid)
├── llmrouter/               # LLM smart router
├── mcp/                     # MCP client (stdio + SSE)
├── memory/                  # File memory (MEMORY.md + journal)
├── plugin/                  # Plugin Manifest / Capability extensions
├── release/                 # Release gates and canary state machine
├── render/                  # Markdown/document rendering (pandoc + LRU cache)
├── router/                  # Multi-agent router
├── runtime/                 # Runtime sandbox and checkpoint rollback
├── security/                # Injection scan / content sanitize / skill scanner
├── session/                 # Session management + context compaction
├── skill/                   # Skill system
│   ├── builtin/             #   Built-in skills (search/weather/translate/summary/media-gen/messaging/export, ...)
│   ├── chain/               #   Skill pipeline chain
│   ├── hub/                 #   Online skill catalog (hexclaw-hub)
│   ├── marketplace/         #   Markdown skill marketplace
│   └── sandbox/             #   Skill sandboxed execution
├── storage/                 # Data storage
│   ├── migrate/             #   Migrations
│   └── sqlite/              #   SQLite driver
├── streamstate/             # Streaming state registry
├── webhook/                 # Webhook receiver
├── go.mod
└── Makefile
```

> Base capabilities such as media generation/genstore/SSRF/cache/trace/events have been pushed down into ai-core / toolkit / hexagon; hexclaw no longer keeps local equivalents. Voice STT/TTS is handled inline by `api/` and `gateway/`, with no standalone `voice/` package.

## API Endpoints (selected endpoints; full routing is module-dependent)

### Core
| Method | Path | Description |
|--------|------|-------------|
| GET | `/health` | Health check |
| POST | `/api/v1/chat` | Chat (streaming/sync, role selection) |
| GET | `/api/v1/roles` | Role list |
| GET | `/api/v1/version` | Version info |
| GET | `/api/v1/stats` | System statistics |
| GET | `/api/v1/models` | Configured LLM models |

### Session Management
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/sessions` | Session list |
| GET | `/api/v1/sessions/{id}` | Session details |
| DELETE | `/api/v1/sessions/{id}` | Delete session |
| GET | `/api/v1/sessions/{id}/messages` | Message history |
| GET | `/api/v1/sessions/{id}/branches` | Session branch list |
| POST | `/api/v1/sessions/{id}/fork` | Fork conversation |
| GET | `/api/v1/messages/search` | Full-text message search |

### Configuration
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/config` | Get full config (API keys masked) |
| PUT | `/api/v1/config` | Update config |
| GET | `/api/v1/config/llm` | Get LLM config |
| PUT | `/api/v1/config/llm` | Update LLM config |
| POST | `/api/v1/config/llm/test` | Test one provider config without persisting it; local Ollama may omit the key |
| POST | `/api/v1/config/llm/models` | Dynamically fetch available models from a provider (proxies to provider `/models` API) |
| GET | `/api/v1/llm/capabilities` | List cached model tool-call capability probe results |
| POST | `/api/v1/llm/capabilities/probe` | Probe the tool-call reliability for a given `provider` + `model` |

### Knowledge Base
| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/v1/knowledge/documents` | Upload document |
| POST | `/api/v1/knowledge/upload` | Upload file and return indexing result |
| GET | `/api/v1/knowledge/documents` | Document list |
| GET | `/api/v1/knowledge/documents/{id}` | Single document detail with full content |
| DELETE | `/api/v1/knowledge/documents/{id}` | Delete document |
| POST | `/api/v1/knowledge/documents/{id}/reindex` | Reindex/retry one document |
| POST | `/api/v1/knowledge/search` | Structured search — both `result` and `results` return `[]SearchHit` with chunks, sources, and scores |

### Cron Jobs
The unified entrypoint `POST /api/v1/cronjob` dispatches on the request body's `action` field (`create` / `update` / `remove` / `pause` / `resume` / `run` / `list` / `history`) and supports `idempotency_key` replay.

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/v1/cronjob` | Unified cron entrypoint (dispatches CRUD / pause-resume / manual trigger / list / history by `action`) |
| POST | `/api/v1/cron/jobs/stream` | Create job (SSE streaming compile, pushes progress/done/error) |
| POST | `/api/v1/cron/parse` | Parse/validate a cron expression and return the next run time |
| GET | `/api/v1/cron/jobs/{id}/history` | Execution history (each entry includes a `result` summary) |

### Webhooks
| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/v1/webhooks/{name}` | Receive webhook event |
| GET | `/api/v1/webhooks` | List webhooks |
| POST | `/api/v1/webhooks` | Register webhook |
| DELETE | `/api/v1/webhooks/{name}` | Delete webhook |

### Memory
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/memory` | Get memory |
| POST | `/api/v1/memory` | Create memory |
| PUT | `/api/v1/memory` | Update memory (allows clearing) |
| DELETE | `/api/v1/memory` | Clear all memory |
| DELETE | `/api/v1/memory/{id}` | Delete specific memory |
| GET | `/api/v1/memory/search` | Search memory |

### MCP
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/mcp/tools` | Tool list |
| GET | `/api/v1/mcp/servers` | Server list |
| GET | `/api/v1/mcp/status` | Connection status snapshot |
| POST | `/api/v1/mcp/tools/call` | Call tool |

### Skills
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/skills` | Installed skills |
| PUT | `/api/v1/skills/{name}/status` | Enable/disable a skill with runtime status fields |
| POST | `/api/v1/skills/install` | Install skill from `clawhub://name` or a local relative path |
| DELETE | `/api/v1/skills/{name}` | Uninstall skill |
| GET | `/api/v1/clawhub/search` | ClawHub skill search with `q` / `category` filters |

Default skill catalog repo: `https://github.com/hexagon-codes/hexclaw-hub` (`index.json` + `skills/*.md`).
Installing or uninstalling Markdown skills automatically syncs the runtime skill registry; a sidecar restart is usually unnecessary.

### Agent Routing
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/agents` | Agent list |
| POST | `/api/v1/agents` | Register agent |
| PUT | `/api/v1/agents/{name}` | Update agent |
| DELETE | `/api/v1/agents/{name}` | Delete agent |
| POST | `/api/v1/agents/default` | Set default agent |
| GET | `/api/v1/agents/rules` | List routing rules |
| POST | `/api/v1/agents/rules` | Create routing rule |
| POST | `/api/v1/agents/rules/test` | Test routing and return matched rules |
| DELETE | `/api/v1/agents/rules/{id}` | Delete routing rule |

### Platform Instances / IM Channels
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/platforms/instances` | Platform instance list |
| GET | `/api/v1/platforms/instances/health` | Health of all instances |
| POST | `/api/v1/platforms/instances` | Create instance |
| PUT | `/api/v1/platforms/instances/{name}` | Update instance |
| DELETE | `/api/v1/platforms/instances/{name}` | Delete instance |
| GET | `/api/v1/platforms/instances/{name}/health` | Health of one instance |
| POST | `/api/v1/platforms/instances/{name}/test` | Test instance config |
| POST | `/api/v1/platforms/instances/{name}/start` | Start instance |
| POST | `/api/v1/platforms/instances/{name}/stop` | Stop instance |
| POST | `/api/v1/im/channels/{provider}/test` | Test IM channel config |

### Canvas / Workflow
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/canvas/panels` | Panel list |
| GET | `/api/v1/canvas/panels/{id}` | Panel details |
| POST | `/api/v1/canvas/events` | Push event |
| GET | `/api/v1/canvas/workflows` | Workflow list |
| POST | `/api/v1/canvas/workflows` | Save workflow |
| DELETE | `/api/v1/canvas/workflows/{id}` | Delete workflow |
| POST | `/api/v1/canvas/workflows/{id}/run` | Run workflow async |
| GET | `/api/v1/canvas/runs/{id}` | Query run result |

### Voice
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/voice/status` | Voice service status |
| POST | `/api/v1/voice/transcribe` | Speech-to-text (STT) |
| POST | `/api/v1/voice/synthesize` | Text-to-speech (TTS) |

### Desktop Integration
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/desktop/info` | Desktop environment info |
| GET | `/api/v1/desktop/notifications` | Notification list |
| POST | `/api/v1/desktop/notifications` | Send notification |
| DELETE | `/api/v1/desktop/notifications` | Clear notifications |
| GET | `/api/v1/desktop/clipboard` | Read clipboard |
| POST | `/api/v1/desktop/clipboard` | Write clipboard |

### Team Collaboration
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/team/agents` | List shared team agents |
| POST | `/api/v1/team/agents` | Share an agent with the team |
| DELETE | `/api/v1/team/agents/{id}` | Delete shared agent |
| GET | `/api/v1/team/members` | Team member list |
| POST | `/api/v1/team/members` | Invite member |
| DELETE | `/api/v1/team/members/{id}` | Remove member |

### Logs & Monitoring
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/logs` | Query logs (level/source/domain/keyword filter + pagination) |
| GET | `/api/v1/logs/stats` | Log statistics (count by level/source) |
| GET | `/api/v1/logs/stream` | Real-time log stream (WebSocket, requires token auth) |

### Desktop-Aligned Response Semantics

- `POST /api/v1/config/llm/test` returns `ok`, `message`, `provider`, `model`, and `latency_ms`; when `provider.type=ollama`, `api_key` may be empty for local OpenAI-compatible connectivity checks.
- `GET /api/v1/skills` always returns `enabled`; `PUT /api/v1/skills/{name}/status` additionally returns `effective_enabled`, `requires_restart`, and `message`.
- `POST /api/v1/skills/install` accepts `clawhub://skill-name` and local relative paths; on success it returns `requires_restart=false` and `runtime_registered=true`, meaning the runtime engine has already hot-synced.
- `GET /api/v1/cron/jobs/{id}/history` includes `result` in each history entry so the latest execution output summary can be shown directly.
- `POST /api/v1/knowledge/search` returns structured chunk results (both `result` and `results` fields are now `[]SearchHit`) with document title, source, chunk position, content, and similarity score so the UI can show citations directly. `result` is no longer a concatenated plain string.
- `GET /api/v1/knowledge/documents/{id}` returns a single document with its full content.
- `GET /api/v1/knowledge/documents` includes `status`, `error_message`, `updated_at`, and `source_type`; `POST /api/v1/knowledge/upload` returns `status`, `source`, `chunk_count`, and `warnings`.
- `POST /api/v1/agents/rules/test` returns matched rules and scores so the UI can explain why a request was routed to a given agent.
- Log entries returned by `GET /api/v1/logs` include a stable `domain` field for filtering by functional area such as `chat`, `knowledge`, `integration`, `automation`, or `engine`.
- `POST /api/v1/config/llm/models` proxies to a provider's `/models` endpoint and returns a normalized model list (`{ models: [{ id, name }] }`); auto-adapts between OpenAI standard format and alternative formats.
- `GET /api/v1/llm/capabilities` returns `{ provider_name, model_name, tool_call, tool_call_text, last_probe, probe_error }`; `POST /api/v1/llm/capabilities/probe?provider=X&model=Y` probes immediately and writes the SQLite cache.

## Development

### Prerequisites

| Tool | Version |
|------|---------|
| Go | >= 1.25 |
| golangci-lint | Latest (optional) |

### Make Commands

| Command | Description |
|---------|-------------|
| `make build` | Build binary to `bin/` |
| `make run` | Build and start service |
| `make test` | Run all tests |
| `make test-cover` | Run tests with coverage |
| `make fmt` | Format code |
| `make vet` | Static analysis |
| `make lint` | golangci-lint check |
| `make clean` | Clean build artifacts |
| `make init` | Initialize default config |

### Manual Commands

```bash
# Build
go build ./...

# Run tests
go test ./...

# Run specific test
go test -run TestName ./package/

# Code check
go vet ./...
golangci-lint run

# Release gate + Eval + canary dry-run
go run ./cmd/verify-release -repo . -version 0.4.4 -version-files package.json
```

## Tech Stack

| Component | Technology |
|-----------|-----------|
| Language | Go 1.25+ |
| Agent Framework | [Hexagon](https://github.com/hexagon-codes/hexagon) v0.5.0 |
| AI Core Library | [ai-core](https://github.com/hexagon-codes/ai-core) v0.1.4 |
| Utility Library | [toolkit](https://github.com/hexagon-codes/toolkit) v0.1.0 |
| CLI | [Cobra](https://github.com/spf13/cobra) |
| Configuration | YAML + environment variables |
| Storage | SQLite (modernc.org/sqlite) |
| WebSocket | nhooyr.io/websocket + gorilla/websocket |
| MCP | modelcontextprotocol/go-sdk v1.5.0 |
| Security | Hexagon Guard Chain |

## Contributing

### Workflow

1. Fork this repository
2. Create a feature branch: `git checkout -b feat/your-feature`
3. Commit your changes: `git commit -m "feat: add new feature"`
4. Push the branch: `git push origin feat/your-feature`
5. Create a Pull Request

### Commit Message Format

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat: add new feature
fix: fix a bug
docs: update documentation
refactor: code refactoring
test: add tests
chore: build/toolchain updates
```

### Code Standards

- Format: `make fmt`
- Static check: `make vet`
- Lint: `make lint`
- Ensure `make test` passes before committing

## Related Projects

| Project | Description | Repository |
|---------|-------------|------------|
| **Hexagon** | Go AI Agent framework (core engine) v0.5.0 | [hexagon](https://github.com/hexagon-codes/hexagon) |
| **ai-core** | AI core library (LLM/Tool/Memory) v0.1.4 | [ai-core](https://github.com/hexagon-codes/ai-core) |
| **toolkit** | Go utility library v0.1.0 | [toolkit](https://github.com/hexagon-codes/toolkit) |
| **hexagon-ui** | Hexagon Dev UI dashboard (Vue 3) | [hexagon-ui](https://github.com/hexagon-codes/hexagon-ui) |
| **hexclaw-desktop** | HexClaw desktop client (Tauri + Vue 3) | [hexclaw-desktop](https://github.com/hexagon-codes/hexclaw-desktop) |
| **hexclaw-ui** | HexClaw web frontend (Vue 3) | [hexclaw-ui](https://github.com/hexagon-codes/hexclaw-ui) |

## Changelog

### v0.4.4

**New Features**
- **At-rest credential encryption** — Platform credentials are sealed on disk with AES-256-GCM (`enc:v1:` envelope, 0600 master key); legacy plaintext rows read back transparently and are backfilled on next write
- **Prompt-injection scan** — Defense-in-depth scanner at cron-create (strict) and exec assembly time; exfiltration/obfuscation families always strict, instruction-override relaxed only when skills/RAG data are present
- **Unified permission gate (GA)** — Declarative `PermissionPolicy` is now the single tool-authorization gate; unattended dispatch uses the `security.autonomy` profile + explicit matrix
- **Skill tool palette** — New `export_document` / `knowledge_ingest` / `media_generate` / `send_message` builtin skills
- **Library memory** — Lightweight prompt/memory store injected per turn

**Dependencies & Architecture**
- **Framework upgrade** — Upgraded to hexagon v0.5.1 / ai-core v0.1.6 / toolkit v0.2.0 (go.mod toolchain line removed, Go 1.25.5); upstream bug fixes (lossless `streamx` timeout, `runtime/runner` tool-call pairing, `failover` classification). The toolkit `crypto/sign` `APISigner` wire-format BREAKING change does not affect hexclaw (only `HMACSHA256` primitives are used)
- **Capability push-down** — Media generation/genstore/SSRF/cache/trace/events migrated into ai-core/toolkit/hexagon; gateway HMAC now uses `toolkit/crypto/sign`
- **Failover push-down** — LLM failover logic moved into ai-core/llm; hexclaw removed its local equivalent and call sites now use `llm.*`
- **Sandbox migration** — The Skill sandbox package moved from the top-level `sandbox/` to `skill/sandbox/`

**Bug Fixes**
- **Matrix adapter** — Idempotent Stop, eliminating the double close(closed channel) panic
- **Knowledge time decay** — Zero-value CreatedAt is no longer decayed to zero (fixes chunks without timestamps never being recalled)
- **Cron multi-replica** — Atomic DB claim + fencing prevents double-running jobs across replicas; fail-open preserves pure in-memory behavior
- **Security hardening** — SSRF allows loopback only (blocks metadata and intranet addresses); file-op symlink boundary protection; WhatsApp webhook signature verification + constant-time comparison for WeChat/WeCom; shell now follows the function-first execution model
- **Function-first unattended automation matrix** — Default `function_first` auto-approves core automation such as `code_exec`, shell, file edits, browsing, knowledge ingest, and delivery. Skill/MCP management, publishing, and forgeable `solve` sources are not auto-approved by default; enable them explicitly through `security.autonomy` or `full_access`. Explicit `PermissionPolicy` deny rules still enforce operator hard limits
- **SSRF reserved ranges (BUG-F4)** — cron Starlark `http_*` now also blocks RFC6598 CGNAT `100.64.0.0/10`, `192.0.0.0/24`, `198.18.0.0/15` (incl. IPv4-mapped IPv6 forms)

### v0.4.0

**New Features**
- **Feature flag foundation** — The `features:` config section controls rollout-capable features; product features default on, while unknown flags are treated as config mistakes and stay off
- **Model capability probing** — New `/api/v1/llm/capabilities` and `/probe` endpoints cache model tool-call reliability
- **Skill lifecycle loop** — Adds the 7-phase Pipeline, `skill_view` progressive disclosure, `.pending` approvals, TrustLevel filtering, and TOCTOU protection
- **Interactive replies** — `Reply.Interactive` supports buttons/select/approval/card with IM text fallback
- **Runtime governance** — Adds Provider middleware, structured events, permission policies, MCP lifecycle hooks, RAG Pipeline, Runtime Sandbox, and release gates
- **Voice improvements** — Adds MiniMax TTS and chained multi-provider TTS fallback

### v0.3.0

**New Features**
- **Dynamic Model Discovery** — New `POST /api/v1/config/llm/models` endpoint that proxies to a provider's `/models` API for dynamic model listing. Supports both OpenAI format (`{ data: [...] }`) and alternative format (`{ models: [...] }`)
- **MCP `~` Path Expansion** — `~` and `~/subpath` in MCP server args are now automatically expanded to the user's home directory, cross-platform via `os.UserHomeDir()` (macOS/Linux/Windows)

**Bug Fixes**
- **Feishu Thinking Placeholder** — The Feishu adapter now sends a thinking placeholder message (e.g., "🤔 Thinking...") immediately upon receiving a message, then replaces it with the final reply via `patchMessage`. Both SDK (WebSocket) and Webhook paths are covered
- **Streaming Tool Execution Fix** — `ProcessStream` with tools previously used `pipeStreamWithTools` which did not execute tools. Fixed to use `processStreamToolLoop` which performs full tool execution → feed results → continue LLM reasoning loop
- **Reasoning Content Persistence** — `pipeStream` and `pipeStreamWithTools` streamed reasoning/thinking content to the frontend but did not collect it for persistence. Added `fullReasoning` collection and new `SaveAssistantMessageWithMeta()` method that saves reasoning to message metadata JSON

## Contact

- HexClaw AI: ai@hexclaw.net
- HexClaw Support: support@hexclaw.net
- Issues: [GitHub Issues](https://github.com/hexagon-codes/hexclaw/issues)
- Security vulnerabilities: see [SECURITY.md](SECURITY.md)

## License

[Apache License 2.0](LICENSE)
