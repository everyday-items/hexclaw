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
| main / v0.5.0-dev | Active development |
| v0.4.9 | ✅ Yes |
| <= v0.4.3 | No |

## Security Features

HexClaw includes a 6-layer security gateway:

1. **Authentication** - HMAC-SHA256 token validation with constant-time comparison (`crypto/subtle`)
2. **Rate Limiting** - Per-user sliding window with memory upper bound (100K windows)
3. **Cost Control** - Budget enforcement per user/global, **fail-closed** on DB errors
4. **Input Safety** - Prompt injection detection + PII redaction, **fail-closed** on errors
5. **RBAC** - Role-based access control
6. **Audit** - Request logging

## Security Hardening (current)

### API Authentication
- Token comparison uses `crypto/subtle.ConstantTimeCompare` to prevent timing attacks
- Logs API (`/api/v1/logs*`) always requires authentication regardless of source IP
- `isLogsAPI` uses exact prefix `/api/v1/logs` to avoid matching `/api/v1/login` etc.
- Mounted scenario-pack routes such as `/api/k12/*` are derived from the mount registry and require auth for non-loopback reads and writes, preventing future scenario mounts from bypassing `/api/v1` guards.

### Code Execution
- `code_exec` is the recommended execution primitive. It runs snippet/file/module/project modes through the toolkit sandbox, returns bounded output, run metadata, resource limits, diagnostics, and an artifact manifest.
- File access is mediated by FileAccessBroker. `mode=file` entrypoints, `mode=project` roots, and extra readable paths must be explicitly authorized; otherwise host paths are rejected.
- Sandbox networking denies loopback by default for `code_exec` to prevent unattended agents from using local management endpoints as a privilege-escalation path.
- `code` and `shell` remain as deprecated compatibility tools. New automation should migrate to `code_exec`; operator `PermissionPolicy` can still deny or require approval for any execution tool.

### Outbound HTTP Boundaries
- Browser/search/weather/Skill Hub use raw outbound HTTP clients with timeouts and response-size limits. They should not be described as SSRF-protected private-network blockers in the current code.
- Cron Starlark `http_get`/`http_post` intentionally has no SSRF or loopback guard in desktop/single-user semantics; scripts can reach loopback. Prefer the in-process `kb_ingest` builtin instead of posting back to local knowledge APIs.
- For untrusted unattended tasks, rely on `PermissionPolicy`, `security.autonomy`, API auth on non-loopback requests, and `code_exec` loopback denial rather than assuming generic outbound HTTP SSRF filtering.

### Tool Permission & Unattended Gate
- A single declarative `PermissionPolicy` gates every tool call (GA). Capability-mutating tools — `manage_skill`, `create_skill`, `patch_skill`, `manage_skill_pending`, `manage_mcp_server` — and consequential actions (`send_message`, `media_generate`, `publish_*`, `shell`, `code`, `code_exec`, `browser`, `file_edit`) **require approval** when policy says so; unmatched tools default to allow.
- **Unattended dispatches** (cron / webhook / spawn / heartbeat / workflow) have no interactive approver, so `ActionRequireApproval` is resolved through the `security.autonomy` profile + explicit switch matrix. The default profile is `function_first`; it is not an implicit approve-all mode.
- Default `function_first` matrix: `cron=[read,browser,exec,files,automation,delivery,media]`, `webhook=[read,browser,exec,files,delivery,media]`, `heartbeat=[read,browser,exec,files,delivery]`, `workflow=[read,browser,exec,files,automation,delivery,media,heal]`, `spawn=[read,exec,files]`, `solve=[]`.
- Category mapping: `exec=shell/code/code_exec`, `files=file_edit/file_ops`, `automation=cron_task`, `delivery=send_message`, `media=media_generate`, `heal=app_heal`, `capability=create_skill/manage_skill/patch_skill/manage_skill_pending/manage_mcp_server`, `publish=publish_*`. `capability`, `publish`, and forgeable `source=solve` are not auto-approved by default.
- Explicit `PermissionPolicy` `ActionDeny` still blocks. Operators who need maximum functionality can explicitly set `security.autonomy.profile: full_access` or configure the exact source categories/tools they need. Note that `system_dispatch.<source>` replaces that source's profile default; it does not merge with it.
- Cron-dispatched agents keep tool visibility; actual execution is decided by `PermissionPolicy` + the autonomy matrix instead of hard-coded tool stripping.

### Path Traversal Prevention
- All file operations validate with `filepath.Base()` + absolute path prefix check
- Memory system: `DeleteFile()` double-validates with `filepath.Clean()` + prefix match
- Memory item ID validated at handler layer (rejects `..`, `/`, `\`)
- Skill Hub/Marketplace: install paths verified against skill directory boundary
- `code_exec` host-file entrypoints and project roots are authorized through FileAccessBroker before entering the sandbox.

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
- Runtime MCP management supports stdio, SSE, and streamable transports.
- `sync.Once` protected `Close()` prevents double-close panics, with a background reconnect loop and proper stop channel.
- Server definitions and secrets are persisted through the same config/secret handling path used by other platform integrations.

### Desktop Mode
- `hexclaw serve --desktop` is a single-user local sidecar mode. It intentionally allows loopback requests without Bearer tokens for desktop, cron, and local UI integration.
- Service deployments exposed beyond loopback should configure `server.api_token`, keep logs/scenario routes authenticated, and treat desktop mode as a local-only profile.

### Workflow Execution
- 10-minute timeout context for async workflow execution
- Prevents unbounded resource consumption from hanging workflows
- Run history bounded with LRU eviction (max 1000 entries)

### Plugin Registry
- `Register`/`Unregister` release write lock before emitting events to prevent same-goroutine deadlock
