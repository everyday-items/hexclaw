# Security Policy

**[中文](SECURITY.zh.md) | English**

## Reporting a Vulnerability

If you discover a security vulnerability in HexClaw, please report it responsibly.

**DO NOT** open a public GitHub issue for security vulnerabilities.

Instead, please email: **security@hexclaw.net**

Include:
- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

We will acknowledge receipt within 48 hours and provide a detailed response within 7 days.

## Supported Versions

| Version | Supported |
|---------|-----------|
| v0.4.4 (latest) | ✅ Yes |

## Security Features

HexClaw includes a 6-layer security gateway:

1. **Authentication** - HMAC-SHA256 token validation with constant-time comparison (`crypto/subtle`)
2. **Rate Limiting** - Per-user sliding window with memory upper bound (100K windows)
3. **Cost Control** - Budget enforcement per user/global, **fail-closed** on DB errors
4. **Input Safety** - Prompt injection detection + PII redaction, **fail-closed** on errors
5. **RBAC** - Role-based access control
6. **Audit** - Request logging

## Security Hardening (v0.4.4)

### API Authentication
- Token comparison uses `crypto/subtle.ConstantTimeCompare` to prevent timing attacks
- Logs API (`/api/v1/logs*`) always requires authentication regardless of source IP
- `isLogsAPI` uses exact prefix `/api/v1/logs` to avoid matching `/api/v1/login` etc.

### Shell Skill
- **White-list only** command execution model
- Dangerous commands (`rm`, `sudo`, `python3`, `perl`, `node`, etc.) permanently blocked
- Git write subcommands (`push`, `clone`, `pull`, `commit`, etc.) blocked
- `curl` write flags (`-o`, `-O`, `--output`) blocked
- Dangerous patterns blocked: backticks, `$()`, `eval`, redirects (`>`, `>>`), `&&`, `||`
- Environment variables sanitized (only `PATH`, `HOME`, `LANG`)
- 30-second execution timeout, 64KB output limit

### SSRF Protection (Browser Skill & Cron Scripts)
- DNS resolution **before** connection with IP validation; the check runs in the dialer `Control` hook on the resolved IP, defeating DNS-rebinding and internal redirects
- Private/reserved IP ranges blocked: RFC 1918, RFC 6598 CGNAT (`100.64.0.0/10`), RFC 6890 (`192.0.0.0/24`), RFC 2544 (`198.18.0.0/15`), loopback, link-local
- Cloud metadata endpoints blocked: AWS (`169.254.169.254`), GCP (`metadata.google.internal`), Azure (`168.63.129.16`), Alibaba Cloud (`100.100.100.200`)
- Applies to both the browser skill and cron Starlark `http_get`/`http_post`
- 1MB response body limit

### Tool Permission & Unattended Gate
- A single declarative `PermissionPolicy` gates every tool call (GA). Capability-mutating tools — `manage_skill`, `create_skill`, `patch_skill`, `manage_skill_pending`, `manage_mcp_server` — and consequential actions (`send_message`, `media_generate`, `publish_*`, `shell`, `code`, `browser`, `file_edit`) **require approval**; unmatched tools default to allow.
- **Unattended dispatches** (cron / webhook / spawn / heartbeat) have no interactive approver. Resolution order:
  1. A read/collect allowlist (`search`, `web_search`, `knowledge_ingest`, `browser`; plus `cron_task` for cron only) auto-runs.
  2. **Exec + capability/host-mutation tools hard-deny** — `shell`/`code`/`code_exec` and `create_skill`/`manage_skill`/`patch_skill`/`manage_skill_pending`/`manage_mcp_server`/`file_edit` can NEVER auto-run unattended; the LLM risk reviewer **cannot** override them (they require a real interactive approver).
  3. The remaining delivery actions (`send_message`/`media_generate`/`publish_*`) are routed to a one-shot LLM risk reviewer and **allowed only on a `low` verdict — `medium`/`high`/no-reviewer/parse-error all fail-closed (deny)**.
- Cron-dispatched agents additionally have self-escalation tools (`cron_task`, `create_skill`, `manage_skill`, `manage_mcp_server`) stripped from their tool set (recursion guard).

### Path Traversal Prevention
- All file operations validate with `filepath.Base()` + absolute path prefix check
- Memory system: `DeleteFile()` double-validates with `filepath.Clean()` + prefix match
- Memory item ID validated at handler layer (rejects `..`, `/`, `\`)
- Skill Hub/Marketplace: install paths verified against skill directory boundary

### Cache Security
- **Singleflight** prevents cache stampede (concurrent miss on same key)
- TTL jitter (10%) prevents cache avalanche
- Bounded entries with correct eviction logic
- Provider-isolated keys prevent cross-model cache pollution

### Fail-Closed Design
- Input Safety layer rejects requests when guard chain errors (not silently passes)
- Cost Check layer rejects requests when budget DB query fails (not silently passes)

### CORS
- Origin validated against allowlist: `http://localhost:{port}`, `tauri://localhost`, `http://tauri.localhost`
- Port must be 1–5 digits numeric; paths, non-numeric ports, and non-http schemes rejected
- OPTIONS preflight returns 204 without invoking auth middleware

### WebSocket
- Origin validation via `OriginPatterns` (replaced `InsecureSkipVerify`)
- Log stream WebSocket requires Bearer token authentication

### MCP
- `sync.Once` protected `Close()` prevents double-close panics
- Background reconnect loop with proper stop channel

### Workflow Execution
- 10-minute timeout context for async workflow execution
- Prevents unbounded resource consumption from hanging workflows
- Run history bounded with LRU eviction (max 1000 entries)

### Plugin Registry
- `Register`/`Unregister` release write lock before emitting events to prevent same-goroutine deadlock
