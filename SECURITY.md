# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in HexClaw, please report it responsibly.

**DO NOT** open a public GitHub issue for security vulnerabilities.

Instead, please email: **hexclaw.dev@gmail.com**

Include:
- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

We will acknowledge receipt within 48 hours and provide a detailed response within 7 days.

## Supported Versions

| Version | Supported |
|---------|-----------|
| latest  | Yes       |

## Security Features

HexClaw includes a 6-layer security gateway:

1. **Authentication** - HMAC-SHA256 token validation with constant-time comparison
2. **Rate Limiting** - Per-user sliding window with memory upper bound (100K windows)
3. **Cost Control** - Budget enforcement per user/global, **fail-closed** on DB errors
4. **Input Safety** - Prompt injection detection + PII redaction, **fail-closed** on errors
5. **RBAC** - Role-based access control
6. **Audit** - Request logging

## Security Hardening (v0.x)

### Shell Skill
- **White-list only** command execution model
- Dangerous commands (`rm`, `sudo`, `python3`, `perl`, `node`, etc.) permanently blocked
- Git write subcommands (`push`, `clone`, `pull`, `commit`, etc.) blocked
- `curl` write flags (`-o`, `-O`, `--output`) blocked
- Dangerous patterns blocked: backticks, `$()`, `eval`, redirects (`>`, `>>`), `&&`, `||`
- Environment variables sanitized (only `PATH`, `HOME`, `LANG`)
- 30-second execution timeout, 64KB output limit

### SSRF Protection (Browser Skill)
- DNS resolution **before** connection with IP validation
- Private IP ranges blocked: RFC 1918, RFC 6598, loopback, link-local
- Cloud metadata endpoints blocked: AWS (`169.254.169.254`), GCP (`metadata.google.internal`), Azure (`168.63.129.16`), Alibaba Cloud (`100.100.100.200`)
- 1MB response body limit

### Path Traversal Prevention
- All file operations validate with `filepath.Base()` + absolute path prefix check
- Memory system: `DeleteFile()` double-validates with `filepath.Clean()` + prefix match
- Skill Hub/Marketplace: install paths verified against skill directory boundary

### Cache Security
- **Singleflight** prevents cache stampede (concurrent miss on same key)
- TTL jitter (10%) prevents cache avalanche
- Bounded entries with correct eviction logic
- Provider-isolated keys prevent cross-model cache pollution

### Fail-Closed Design
- Input Safety layer rejects requests when guard chain errors (not silently passes)
- Cost Check layer rejects requests when budget DB query fails (not silently passes)

### WebSocket
- Origin validation via `OriginPatterns` (replaced `InsecureSkipVerify`)

### MCP
- `sync.Once` protected `Close()` prevents double-close panics
- Background reconnect loop with proper stop channel

### Workflow Execution
- 10-minute timeout context for async workflow execution
- Prevents unbounded resource consumption from hanging workflows
