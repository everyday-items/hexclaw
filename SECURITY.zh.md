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
| v0.5.0-beta（预发布） | ✅ 支持 |
| v0.4.9（最新稳定版） | ✅ 支持 |
| <= v0.4.8 | 不支持 |

## 安全特性

HexClaw 包含六层安全网关：

1. **认证** — HMAC-SHA256 Token 验证，使用 `crypto/subtle` 常量时间比较
2. **限流** — 基于用户的滑动窗口限流，内存上限 100K 窗口
3. **成本控制** — 用户/全局预算强制执行，数据库异常时 **fail-closed**
4. **输入安全** — Prompt 注入检测 + PII 脱敏，异常时 **fail-closed**
5. **RBAC** — 基于角色的访问控制
6. **审计** — 请求日志记录

## 安全加固（v0.5.0-beta）

### API 认证
- Token 比较使用 `crypto/subtle.ConstantTimeCompare`，防止时序攻击
- 日志 API（`/api/v1/logs*`）无论来源 IP 均要求认证
- `isLogsAPI` 使用精确前缀 `/api/v1/logs`，避免匹配 `/api/v1/login` 等路径
- `/api/k12/*` 等场景包挂载路由从挂载注册表派生鉴权前缀，非 loopback 读写均需认证，避免未来新场景绕过 `/api/v1` 守卫。

### 代码执行
- `code_exec` 是推荐执行原语，支持 snippet/file/module/project，通过 toolkit sandbox 执行，并返回有界输出、运行元数据、资源限制、诊断和产物清单。
- 文件访问由 FileAccessBroker 统一裁决。`mode=file` 入口、`mode=project` 根目录和额外只读路径必须显式授权，否则拒绝触达宿主路径。
- `code_exec` 默认禁止 loopback 网络访问，避免无人值守 Agent 通过本机管理端点自提权。
- `code` 和 `shell` 仍作为兼容工具保留但已弃用；新自动化应迁移到 `code_exec`，运营方仍可用 `PermissionPolicy` 对任意执行工具 deny 或 require approval。

### 出站 HTTP 边界
- Browser/search/weather/Skill Hub 使用带超时和响应体限制的原始 HTTP 客户端；当前代码不应被描述为具备通用私网/元数据 SSRF 封锁。
- Cron Starlark `http_get`/`http_post` 在桌面/单用户语义下故意不设置 SSRF 或 loopback 守卫，脚本可以访问 loopback；需要写知识库时优先用 in-process 的 `kb_ingest`，不要回打本地知识库 HTTP API。
- 对不可信无人值守任务，应依赖 `PermissionPolicy`、`security.autonomy`、非 loopback API 鉴权以及 `code_exec` 的 loopback 禁止，而不是假设所有出站 HTTP 都有 SSRF 过滤。

### 工具权限与无人值守闸
- 统一声明式 `PermissionPolicy` 前置闸所有工具调用（GA）。能力变更类工具——`manage_skill`、`create_skill`、`patch_skill`、`manage_skill_pending`、`manage_mcp_server`——与 consequential 动作（`send_message`、`media_generate`、`publish_*`、`shell`、`code`、`code_exec`、`browser`、`file_edit`）在策略要求时**需用户审批**；未命中规则的工具默认放行。
- **无人值守派发**（cron / webhook / spawn / heartbeat / workflow）没有交互审批人，因此使用 `security.autonomy` 的 Profile + 显式开关矩阵处理 `ActionRequireApproval`。默认 profile 为 `function_first`，不是“一把梭全开”。
- 默认 `function_first` 矩阵：`cron=[read,browser,exec,files,automation,delivery,media]`，`webhook=[read,browser,exec,files,delivery,media]`，`heartbeat=[read,browser,exec,files,delivery]`，`workflow=[read,browser,exec,files,automation,delivery,media,heal]`，`spawn=[read,exec,files]`，`solve=[]`。
- 类别映射：`exec=shell/code/code_exec`，`files=file_edit/file_ops`，`automation=cron_task`，`delivery=send_message`，`media=media_generate`，`heal=app_heal`，`capability=create_skill/manage_skill/patch_skill/manage_skill_pending/manage_mcp_server`，`publish=publish_*`。默认不自动放行 `capability`、`publish` 和伪造的 `source=solve`。
- 显式 `PermissionPolicy` `ActionDeny` 仍会阻断，保证运营配置的硬拒绝优先；需要极限功能最大化时可显式设置 `security.autonomy.profile: full_access`，或只给某个来源配置类别/工具。注意：`system_dispatch.<source>` 是替换该来源的 profile 默认值，不是增量合并。
- cron 派发的 Agent 默认保留工具可见性；最终是否执行由 `PermissionPolicy` + autonomy 矩阵裁决，而不是硬编码剥离。

### 路径遍历防护
- 所有文件操作通过 `filepath.Base()` + 绝对路径前缀检查进行验证
- 记忆系统：`DeleteFile()` 使用 `filepath.Clean()` + 前缀匹配双重验证
- 记忆条目 ID 在处理层验证（拒绝 `..`、`/`、`\`）
- Skill Hub/Marketplace：安装路径经过技能目录边界验证
- `code_exec` 宿主文件入口和项目根目录进入沙箱前先经 FileAccessBroker 授权。

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
- 运行时 MCP 管理支持 stdio、SSE、streamable 传输。
- `sync.Once` 保护 `Close()` 防止重复关闭 panic，后台重连循环配合正确的停止通道。
- Server 定义和密钥复用平台集成的配置/凭据加密路径持久化。

### 桌面模式
- `hexclaw serve --desktop` 是单用户本地 sidecar 模式，会为了桌面、cron、本地 UI 集成放行 loopback 无 Bearer Token 请求。
- 暴露到非 loopback 的服务部署应配置 `server.api_token`，保持日志和场景包路由鉴权，并把 desktop mode 视为本地专用模式。

### 工作流执行
- 异步工作流执行使用 10 分钟超时上下文
- 防止挂起工作流导致无限资源消耗
- 执行历史使用 LRU 淘汰（最多 1000 条）

### 插件注册表
- `Register`/`Unregister` 在释放写锁后再触发事件，防止同一 goroutine 死锁
