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
- **ReAct Agent Engine** — Reasoning + Action loop with multi-turn tool calls and streaming output
- **6-Layer Security Gateway** — Auth, rate limiting, cost control, injection detection, permission check, audit logging
- **LLM Smart Router** — Multi-provider auto-switching, failover, and cost optimization
- **Skill System** — Built-in search/weather/translation/summary, sandboxed execution, shell allowlist protection
- **Semantic Cache** — Singleflight anti-stampede + TTL jitter anti-avalanche + empty-value anti-penetration
- **Knowledge Base** — FTS5 + vector hybrid retrieval, RAG context augmentation

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
- **Voice Interaction** — STT/TTS transcription and synthesis, multi-provider support
- **Desktop Integration** — System notifications, clipboard interaction (Tauri desktop client)
- **Real-Time Logs** — WebSocket log streaming + analytics

### Multi-Platform Support (13 platforms)

| Platform | Method | Status |
|----------|--------|:------:|
| Web UI | WebSocket | ✅ |
| Feishu | HTTP Webhook | ✅ |
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
    branch: main

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

All config values support environment variable substitution (`${VAR_NAME}`).

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
├── cmd/hexclaw/             # CLI entry (serve/init/security audit/skill)
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
│   ├── line/                #   LINE
│   ├── matrix/              #   Matrix
│   └── email/               #   Email (IMAP/SMTP)
├── agents/                  # Agent roles (6 preset roles)
├── api/                     # REST API server (71 routes)
│   ├── server.go            #   Core server + chat + route registration
│   ├── handler_extended.go  #   Workflow/config/version/stats API
│   ├── handler_logs.go      #   Log query/stats/stream API
│   ├── handler_knowledge.go #   Knowledge base API
│   ├── handler_webhook.go   #   Webhook API
│   ├── handler_cron.go      #   Cron job API
│   └── handler_misc.go      #   Memory/MCP/skill/router/canvas/voice API
├── audit/                   # Security audit (7 check categories)
├── cache/                   # LLM response semantic cache
├── canvas/                  # Canvas/A2UI (8 component types)
├── config/                  # Configuration management (YAML + env vars)
├── cron/                    # Cron job scheduler
├── desktop/                 # Desktop integration (notifications/clipboard)
├── engine/                  # Agent engine (ReAct loop)
├── gateway/                 # 6-layer security gateway
├── heartbeat/               # Heartbeat patrol
├── knowledge/               # Knowledge base (FTS5 + vector hybrid)
├── llmrouter/               # LLM smart router
├── mcp/                     # MCP client (stdio + SSE)
├── memory/                  # File memory (MEMORY.md + journal)
├── router/                  # Multi-agent router
├── session/                 # Session management + context compaction
├── skill/                   # Skill system
│   ├── builtin/             #   Built-in skills (search/weather/translate/summary)
│   ├── marketplace/         #   Markdown skill marketplace
│   └── sandbox/             #   Sandboxed execution
├── storage/                 # Data storage
│   └── sqlite/              #   SQLite driver
├── voice/                   # Voice interaction (STT/TTS)
├── webhook/                 # Webhook receiver
├── go.mod
└── Makefile
```

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
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/cron/jobs` | Job list |
| POST | `/api/v1/cron/jobs` | Add job |
| DELETE | `/api/v1/cron/jobs/{id}` | Delete job |
| POST | `/api/v1/cron/jobs/{id}/pause` | Pause job |
| POST | `/api/v1/cron/jobs/{id}/resume` | Resume job |
| POST | `/api/v1/cron/jobs/{id}/trigger` | Manual trigger |
| GET | `/api/v1/cron/jobs/{id}/history` | Execution history |

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
```

## Tech Stack

| Component | Technology |
|-----------|-----------|
| Language | Go 1.25+ |
| Agent Framework | [Hexagon](https://github.com/hexagon-codes/hexagon) v0.3.1-beta |
| AI Core Library | [ai-core](https://github.com/hexagon-codes/ai-core) v0.0.5 |
| Utility Library | [toolkit](https://github.com/hexagon-codes/toolkit) v0.0.3 |
| CLI | [Cobra](https://github.com/spf13/cobra) |
| Configuration | YAML + environment variables |
| Storage | SQLite (modernc.org/sqlite) |
| WebSocket | nhooyr.io/websocket + gorilla/websocket |
| MCP | modelcontextprotocol/go-sdk v1.3.0 |
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
| **Hexagon** | Go AI Agent framework (core engine) v0.3.1-beta | [hexagon](https://github.com/hexagon-codes/hexagon) |
| **ai-core** | AI core library (LLM/Tool/Memory) v0.0.5 | [ai-core](https://github.com/hexagon-codes/ai-core) |
| **toolkit** | Go utility library v0.0.3 | [toolkit](https://github.com/hexagon-codes/toolkit) |
| **hexagon-ui** | Hexagon Dev UI dashboard (Vue 3) | [hexagon-ui](https://github.com/hexagon-codes/hexagon-ui) |
| **hexclaw-desktop** | HexClaw desktop client (Tauri + Vue 3) | [hexclaw-desktop](https://github.com/hexagon-codes/hexclaw-desktop) |
| **hexclaw-ui** | HexClaw web frontend (Vue 3) | [hexclaw-ui](https://github.com/hexagon-codes/hexclaw-ui) |

## Contact

- HexClaw AI: ai@hexclaw.net
- HexClaw Support: support@hexclaw.net
- Issues: [GitHub Issues](https://github.com/hexagon-codes/hexclaw/issues)
- Security vulnerabilities: see [SECURITY.md](SECURITY.md)

## License

[Apache License 2.0](LICENSE)
