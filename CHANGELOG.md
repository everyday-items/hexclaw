# Changelog

本文件记录 hexclaw 的用户可见变更，遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

## [Unreleased]

### Added
- **场景包扩展框架**：新增 `scenario` 六缝注册表（记录集 / 约束 / 视图槽 / Agent mode / 按钮 / eval suite），平台层不再硬编码具体业务包。
- **通用记录本**：新增 `records.agent_records`，以 Agent 为隔离键，支持 schema 校验、去重键、到期复习队列、状态机和乐观锁。
- **K12 家长辅导场景包**：内置 `/api/k12/*`、`k12_grade`/`k12_review`、错题本、积累本、年级约束、备课卡、默认 cron 投递和 K12 专项评测 workflow。
- **无人值守治理 API**：新增 autonomy profile、preflight、summary、decision audit、task grant 端点，并补齐 purpose/data-class 出网策略。

### Changed
- 升级框架依赖：hexagon v0.5.7 → **v0.5.8**、ai-core v0.1.11 → **v0.2.1**、toolkit v0.2.3 → **v0.2.6**；`go.mod` 同步到 Go **1.25.7**。`GOWORK=off go test ./... -run '^$'` 已通过，发版/CI 模式下全仓编译不再依赖本地 `go.work` 的隐式下层源码。
- **执行原语收敛**：`code_exec` 成为推荐执行入口，支持 snippet/file/module/project 与 artifact metadata；`code`/`shell` 保留兼容但标记弃用，顶层 `runtime/` 包删除，沙箱能力收敛到 toolkit + `skill/sandbox`。
- **默认 Hub 标签**：技能市场默认目录对齐 `hexagon-codes/hexclaw-hub` `v0.0.6`。
- 更新 CI/CD 文档口径：补充 `sandbox-code-exec.yml` 专项门禁、Linux CI 等价命令，以及 runner 完整性探针不得进入默认 `go test ./...` 的规则。

### Fixed
- 修复 release 构建不可复现的依赖锁定问题：`skill/builtin/code_exec.go` 依赖的 sandbox 限额字段已由 `toolkit v0.2.6` 提供，不再需要本地 workspace 才能编译。
- 将 runner 完整性故意失败探针改为 `HEXCLAW_RUNNER_PROBE=1` 手工门控，默认 `go test ./...` 不再被取证用例打红。

## [0.4.7] - 2026-06-26
> 技能/MCP 市场离线优先 + 应用自省自愈 skill（app_query/app_heal）+ 文档上传抽取预览 + 会话附件持久化；一组 CJK 截断 / 上游脱敏 / API 契约修复；框架依赖升级 hexagon v0.5.5 / ai-core v0.1.8。

### Added
- **技能/MCP 市场离线优先**：出厂嵌入快照 + 磁盘缓存 + 带 TTL/退避的后台异步刷新；`EnsureCatalog` 保证目录非阻塞可用，网络与缓存均不可用时回落嵌入种子。`McpHub` 收敛为 `Hub` 门面，目录抓取/离线/缓存统一委托 `Hub`。新增 `scripts/sync-hub-embed.sh`，发版时从锚定分支同步嵌入快照。
- **应用自省与自愈 skill**：`app_query`（P0 只读自省——连接/MCP/cron/webhook/agent/config/logs 脱敏查询，凭据红线打码 + `<app-data>` 围栏）与 `app_heal`（P1 自愈——cron retry/resume/pause、workflow_run，含限频与审计）；`react` 能力上下文注入系统卡片（版本/计数），交互式会话仍可走确认门，无人值守自动化按功能优先执行。
- **文档上传/抽取/预览**：`POST /documents/extract`（PDF/DOC/PPTX → 纯文本，供对话上下文注入；PDF 走 poppler、.doc 走 textutil、.pptx 走 hexagon PPTXLoader）、`POST/GET /documents/preview/{token}`（原文件内存环形缓存预览）；知识库放开 .pdf/.doc/.pptx 格式。
- **会话附件持久化**：图片 base64 单列存储（migration v7 `messages.attachments`，8MB 上限）规避 64KB metadata 截断导致重载丢图；`documents` 元数据落库（`json.RawMessage`）；`message-id` 优先用 `request_id` 作存储主键，修复删除返回 404。

### Changed
- 升级框架依赖：hexagon v0.5.3 → **v0.5.5**、ai-core v0.1.7 → **v0.1.8**（API 兼容，本仓无源码适配改动）。
- 对齐版本常量：`hexclaw.Version` 0.4.4 → **0.4.7**（历次发版漏更，本次补齐）。

### Fixed
- **CJK 截断乱码（AP-049）**：`skill_judge`/`eval`/`llmrouter.capabilities`/`risk_reviewer`/`react` 的工具与描述截断统一改 rune-safe `stringx.SubString`，杜绝字节边界切断 CJK 产出非法 UTF-8 序列。
- **上游错误脱敏 + 数字 error.code**：`upstreamerr` 兼容 JSON number 形式的 `error.code`（如 Nvidia 返回数字 `400`）；WS 流式分片错误统一过 `PublicMessage` 脱敏，避免上游原始 JSON 漏进聊天气泡。
- **API 契约**：绑定到禁用 provider 的 agent 在注册校验期即拒绝（不再延后到运行时）；email 能力仅 `["receive"]`（send 未接入 instances.Manager）；Webhook 在 jobID 绑定时 `prompt` 可选、空 jobID 时仍必填。
- **记忆闸门对称（bug#3-3）**：`memory=off` 同时阻断 `memProvider.Inject`，杜绝 standing/fact 注入泄漏。

## [0.4.6] - 2026-06-23
> 连接中心 MCP 收敛（env keystone + 密钥箱静态加密）+ max-turns 优雅降级（对齐 hexagon StopReason）+ 一组用户反馈修复；框架依赖与技能市场版本升级。

### Added
- **MCP env keystone + 密钥箱（连接中心）**：新增 `config/mcp_secret`——MCP Server 的 `env` 凭据（DB 密码、API Key 等）经主密钥（`~/.hexclaw/master.key`，load-or-create）静态加密落盘（`enc:v1:`），启动时 `LoadBox` + `DecryptMCPEnv` 解密后注入 `mcpMgr` 连接；加载失败降级明文并仅告警，绝不记录密钥或明文凭据。MCP Server 连接透传 `Env` 字段。API 响应一律脱敏。
- **MCP 参数校验 + AP-031/032/034 修复**：`api/handler_misc`/`handler_extended` 补 env 读写脱敏与参数校验，配套 env 持久化 / 校验 / mysql MCP e2e 审计回归测试。

### Changed
- 升级框架依赖：hexagon v0.5.2 → **v0.5.3**（`Result.StopReason` 提升为一等字段、移除 `ErrMaxTurns`/`KindMaxTurns`，达到轮次上限不再是错误，对齐 `stop_reason` 语义）。
- **技能市场默认分支 v0.0.4 → v0.0.5**：`HubConfig` / `SkillsHubConfig` 默认 `Branch` 同步 hexclaw-hub v0.0.5 发布。
- **agent 工具轮次上限默认 5 → 25**：budget 模式仍以 `hardMaxTurns=50` 兜底；上下文压缩在本地模型场景跳过，避免无谓调用。
- **功能优先默认值**：本地/云端模型在 `tools.enabled=auto` 下都默认开启工具；代码执行、Shell、MCP/Skill 管理、RAG Pipeline、Runtime Sandbox、Plugin Extension、真实 Agent Dispatcher 等产品级能力默认可用，显式配置可回退。

### Fixed
- **max-turns 优雅降级**：达到轮次上限按 `result.StopReason==max_turns` 优雅返回部分结果（非错误），尾部追加「继续」提示，不再向用户抛错。
- **文件记忆挂载闸门（bug#3a）**：只要文件记忆系统创建成功即挂到引擎，不再用「启动时记忆是否为空」当闸门——否则首次启动新增的记忆要等重启才注入，问答答不上。
- **用户反馈修复**：记忆注入闸门、挂载 skill 人设透传、人设 prompt、mounted persona skill 一组回归取证；`api/handler_webhook` 契约对齐。
- **桌面通知来源标识**：`desktop.Notification` 新增 `Source`（`cron`/`im`），供前端映射图标与深链；`Notify` 保持旧行为（`NotifySource` 空来源）。

## [0.4.5] - 2026-06-23
> 框架依赖升级 + 桌面定位下的网络/扫描放开 + 出站 UA 统一 + 一组 R4 契约修复与审计回归测试。

### Added
- **数据连接器（§15.1）**：新增 `connector/` 包——token（PAT / Integration Token）只读接入 GitHub / Notion，token 经 `secret.Box` 静态加密落盘（`~/.hexclaw/connectors.json`，`enc:v1:`），API 响应一律脱敏。端点 `GET/POST/DELETE /api/v1/connectors`、`POST /api/v1/connectors/test`、`GET /api/v1/connectors/{id}/resources`（真实拉取仓库 / 页面）。
- **默认助理人设（SOUL）端点**：注册 `GET/PUT /api/v1/assistant/soul`（读写 `~/.hexclaw/SOUL.md`，空=恢复内置默认；引擎每轮读取，保存即时生效）。
- **定时任务显式投递目标**：`AddCronJobRequest.Deliver` 透传到 `cron.AddJobRequest`，前端「从连接库下拉选投递目标」即走此字段（§5 一处存处处引）。
- **AI Skill 生成端点**：`handleGenerateSkill`（对话式生成 Skill）+ 安装路径，配套生成/安装测试。
- **`httpua` 包**：统一出站 HTTP 默认 User-Agent（image 下载 / render 抓取 / cron starlark http）。裸 `Go-http-client` UA 易被站点反爬返回 HTML 拦截页，致下游 json/图片解码 `invalid character '<'`；`httpua.Set` 在调用方未显式设 UA 时注入真实浏览器 UA（显式 UA 优先）。

### Changed
- **工作流图执行器兼容前端字段**：`workflow_runtime.go` parse() 同时接受节点 `data`/`config`、边 `source/target`/`from/to`，使桌面端线性工作流保存的形状能被图执行器正确读取并链接。
- 升级框架依赖：toolkit v0.2.0 → **v0.2.1**（`net/httpx.RawClient` 遵循 `HTTP(S)_PROXY`/`NO_PROXY`）、ai-core v0.1.6 → **v0.1.7**（`llmcall` 退避委托 `toolkit/util/retry`）、hexagon v0.5.1 → **v0.5.2**（circuit/retry/sse/ssrf 复用 toolkit；`CircuitBreaker` 状态回调改异步——本仓不直接依赖该回调同步性）。
- **adapter.ReplyChunk 补小写 json tag（R4-1）**：SSE 聊天 wire JSON 由 PascalCase（`{"Content":…}`）改为 `content`/`reasoning`/… 小写，修复桌面端读取聊天正文恒空。
- **adapter/dingtalk 重连退避复用 `toolkit/util/retry.ExponentialBackoff`（A-1）**，退避序列保真。
- **api/team 枚举校验（C10）**：`visibility`/`role` 非法值返 400，空值取默认（private/member）。

### Security
- **网络放开（桌面定位）**：移除 cron starlark / skill browser / api 各处应用层 SSRF 私网阻断及相关回归测试（`engine_starlark.go` 瘦身、删 4 个 SSRF 测试）。hexclaw 作为**本地单用户桌面后端**运行，出站受用户机网络策略约束，不再在应用层强制私网名单。⚠️ 若改为多租户/公网部署，需自行恢复 SSRF 防护。
- **`security/desktop_mode` 部署模式开关**：`--desktop` 启动时置位，桌面（单用户自有机）放行内容注入扫描 / 危险模式扫描（`ScanUserPrompt`/`ScanAssembled`/`SkillScanner.Scan`）——单用户无第三方提示注入面，误杀代价 > 收益；**服务端（多租户）默认仍开启**全部扫描。

## [0.4.4] - 2026-06-21
> 接续 v0.4.3（已 tag 于 aa34a45）之后的地基功能 + 安全加固发布。本次新增静态凭据加密、
> 注入扫描、统一权限闸 GA、Skill 工具盘、library 记忆薄版，并修复一组无人值守安全缺口。

### Changed
- 升级框架依赖到 hexagon v0.5.1 / ai-core v0.1.6 / toolkit v0.2.0（go.mod 去除 toolchain 行，Go 1.25.5）。上游均为带回归测试的缺陷修复：ai-core `streamx` 超时改无损语义（不再丢在途元素）、`llm/failover` 错误分类收窄；hexagon `runtime/runner` 工具去重收敛为仅续跑（修复 provider 复用 tool-call ID 时漏配对结果）+ MaxTurns 返回部分结果。toolkit v0.2.0 `crypto/sign` `APISigner` wire 格式为 BREAKING，但本仓仅用 `HMACSHA256` 原语、未引用 `APISigner`，不受影响。
- 媒体/genstore/ssrf/cache/trace/events 迁移到 ai-core/toolkit/hexagon；gateway HMAC 改用 toolkit/crypto/sign。
- LLM failover 逻辑下沉到 ai-core/llm，删除本地等价实现，消费点改用 `llm.*`。
- Skill 沙箱包从顶层 `sandbox/` 迁移到 `skill/sandbox/`。

### Fixed
- matrix 适配器 Stop 幂等（消除二次调用 close(closed channel) panic）。
- knowledge 时间衰减：零值 CreatedAt 不再被衰减清零（修复无时间戳 chunk 永不召回）。
- cron：多副本 job 双跑防护（DB 原子领取 + fencing），fail-open 保纯内存行为。
- 安全加固：SSRF 仅放行 loopback（封禁元数据与内网地址）；文件操作 symlink 越界防护；WhatsApp webhook 验签 + 微信/企微常量时间比较；shell 后续已调整为功能优先执行模型。
- **安全（BUG-F1）**：`manage_skill`（安装/卸载市场技能 = 能力注入）补入默认权限策略 `DefaultBaselinePolicy`，归为 require_approval。此前它漏出策略 → 默认 allow，可被 webhook/spawn 无人值守派发免审批自动调用（交互态也无确认）。回归测试 `engine/bug_f1_manage_skill_gate_test.go` 枚举全部能力变更类工具（新增同类工具忘记加规则会自动 FAIL）。
- **安全（BUG-F4）**：cron starlark `http_get/http_post` 的 SSRF 拦截补齐 RFC6598 CGNAT `100.64.0.0/10` 及 `192.0.0.0/24`、`198.18.0.0/15`——此前依赖的 toolkit stdlib 谓词漏判这些保留段，与 SECURITY 文档「封锁 RFC 6598」的承诺不符。回归测试 `cron/bug_f4_ssrf_reserved_test.go`、`cron/ssrf_guard_edge_test.go`（含 IPv4-mapped IPv6 / ULA / 整段 CGNAT）。
- **无人值守权限口径变更**：早期 BUG-F5 曾将无人值守代码执行/能力变更类工具硬拒；当前功能优先策略已改为 cron/webhook/spawn/heartbeat/workflow 自动放行 `require_approval` 工具，显式 `PermissionPolicy` deny 仍是运营硬限制。回归测试 `engine/bug_f5_unattended_reviewer_override_test.go` 覆盖 {工具 × 来源 × 顾问判定} 授权矩阵。
- **测试（BUG-F6）**：修复 feishu `TestStart_LogMessageWellFormed` 在 `-race` 下偶发（3/10）的数据竞争——`captureDefaultLog` 改用 mutex 包裹的 `syncBuffer` 作日志 sink，消除后台 Start 协程写入与测试读取的竞争（仅测试侧，生产代码不变）。

## [基线]
- 与 ai-core v0.1.6 / hexagon v0.5.1 / toolkit v0.2.0 对齐。
