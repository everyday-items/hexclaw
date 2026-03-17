# 安全政策

**中文 | [English](SECURITY.md)**

## 报告漏洞

如果您在 HexClaw 中发现安全漏洞，请负责任地进行报告。

**请勿**在 GitHub 上开启公开 Issue 报告安全漏洞。

请发送邮件至：**security@hexclaw.net**

邮件中请包含：
- 漏洞描述
- 复现步骤
- 潜在影响
- 修复建议（如有）

我们将在 48 小时内确认收到，并在 7 天内提供详细回复。

## 支持版本

| 版本 | 支持状态 |
|------|---------|
| v0.1.0（最新） | ✅ 支持 |

## 安全特性

HexClaw 包含六层安全网关：

1. **认证** — HMAC-SHA256 Token 验证，使用 `crypto/subtle` 常量时间比较
2. **限流** — 基于用户的滑动窗口限流，内存上限 100K 窗口
3. **成本控制** — 用户/全局预算强制执行，数据库异常时 **fail-closed**
4. **输入安全** — Prompt 注入检测 + PII 脱敏，异常时 **fail-closed**
5. **RBAC** — 基于角色的访问控制
6. **审计** — 请求日志记录

## 安全加固（v0.1.0）

### API 认证
- Token 比较使用 `crypto/subtle.ConstantTimeCompare`，防止时序攻击
- 日志 API（`/api/v1/logs*`）无论来源 IP 均要求认证
- `isLogsAPI` 使用精确前缀 `/api/v1/logs`，避免匹配 `/api/v1/login` 等路径

### Shell 技能
- **仅白名单**命令执行模式
- 危险命令（`rm`、`sudo`、`python3`、`perl`、`node` 等）永久封锁
- Git 写操作子命令（`push`、`clone`、`pull`、`commit` 等）封锁
- `curl` 写标志（`-o`、`-O`、`--output`）封锁
- 危险模式封锁：反引号、`$()`、`eval`、重定向（`>`、`>>`）、`&&`、`||`
- 环境变量清理（仅保留 `PATH`、`HOME`、`LANG`）
- 30 秒执行超时，64KB 输出限制

### SSRF 防护（Browser 技能）
- 连接前进行 DNS 解析并验证 IP
- 封锁私有 IP 段：RFC 1918、RFC 6598、回环地址、链路本地
- 封锁云元数据端点：AWS（`169.254.169.254`）、GCP（`metadata.google.internal`）、Azure（`168.63.129.16`）、阿里云（`100.100.100.200`）
- 响应体限制 1MB

### 路径遍历防护
- 所有文件操作通过 `filepath.Base()` + 绝对路径前缀检查进行验证
- 记忆系统：`DeleteFile()` 使用 `filepath.Clean()` + 前缀匹配双重验证
- 记忆条目 ID 在处理层验证（拒绝 `..`、`/`、`\`）
- Skill Hub/Marketplace：安装路径经过技能目录边界验证

### 缓存安全
- **Singleflight** 防止缓存击穿（同一 Key 并发 Miss）
- TTL 抖动（10%）防止缓存雪崩
- 正确淘汰逻辑的有界条目数
- Provider 隔离 Key，防止跨模型缓存污染

### Fail-Closed 设计
- 输入安全层在 Guard Chain 报错时拒绝请求（不静默放行）
- 成本检查层在预算数据库查询失败时拒绝请求（不静默放行）

### CORS
- Origin 经允许列表验证：`http://localhost:{port}`、`tauri://localhost`、`http://tauri.localhost`
- 端口必须为 1–5 位数字；路径、非数字端口、非 http 协议均被拒绝
- OPTIONS 预检返回 204，不触发认证中间件

### WebSocket
- 通过 `OriginPatterns` 进行 Origin 验证（替代 `InsecureSkipVerify`）
- 日志流 WebSocket 要求 Bearer Token 认证

### MCP
- `sync.Once` 保护 `Close()` 防止重复关闭 panic
- 后台重连循环配合正确的停止通道

### 工作流执行
- 异步工作流执行使用 10 分钟超时上下文
- 防止挂起工作流导致无限资源消耗
- 执行历史使用 LRU 淘汰（最多 1000 条）

### 插件注册表
- `Register`/`Unregister` 在释放写锁后再触发事件，防止同一 goroutine 死锁
