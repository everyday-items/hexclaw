# Changelog

本文件记录 hexclaw 的用户可见变更，遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

## [Unreleased]

### Added
- **数据连接器（§15.1）**：新增 `connector/` 包——token（PAT / Integration Token）只读接入 GitHub / Notion，token 经 `secret.Box` 静态加密落盘（`~/.hexclaw/connectors.json`，`enc:v1:`），API 响应一律脱敏。端点 `GET/POST/DELETE /api/v1/connectors`、`POST /api/v1/connectors/test`、`GET /api/v1/connectors/{id}/resources`（真实拉取仓库 / 页面）。
- **默认助理人设（SOUL）端点**：注册 `GET/PUT /api/v1/assistant/soul`（读写 `~/.hexclaw/SOUL.md`，空=恢复内置默认；引擎每轮读取，保存即时生效）。
- **定时任务显式投递目标**：`AddCronJobRequest.Deliver` 透传到 `cron.AddJobRequest`，前端「从连接库下拉选投递目标」即走此字段（§5 一处存处处引）。

### Changed
- **工作流图执行器兼容前端字段**：`workflow_runtime.go` parse() 同时接受节点 `data`/`config`、边 `source/target`/`from/to`，使桌面端线性工作流保存的形状能被图执行器正确读取并链接。

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
- 安全加固：SSRF 仅放行 loopback（封禁元数据与内网地址）；shell `find -exec` 收紧白名单；文件操作 symlink 越界防护；WhatsApp webhook 验签 + 微信/企微常量时间比较。
- **安全（BUG-F1）**：`manage_skill`（安装/卸载市场技能 = 能力注入）补入默认权限策略 `DefaultBaselinePolicy`，归为 require_approval。此前它漏出策略 → 默认 allow，可被 webhook/spawn 无人值守派发免审批自动调用（交互态也无确认）。回归测试 `engine/bug_f1_manage_skill_gate_test.go` 枚举全部能力变更类工具（新增同类工具忘记加规则会自动 FAIL）。
- **安全（BUG-F4）**：cron starlark `http_get/http_post` 的 SSRF 拦截补齐 RFC6598 CGNAT `100.64.0.0/10` 及 `192.0.0.0/24`、`198.18.0.0/15`——此前依赖的 toolkit stdlib 谓词漏判这些保留段，与 SECURITY 文档「封锁 RFC 6598」的承诺不符。回归测试 `cron/bug_f4_ssrf_reserved_test.go`、`cron/ssrf_guard_edge_test.go`（含 IPv4-mapped IPv6 / ULA / 整段 CGNAT）。
- **安全（BUG-F5）**：无人值守派发下，任意代码执行（shell/code/code_exec）+ 能力/宿主变更（create_skill/manage_skill/patch_skill/manage_skill_pending/manage_mcp_server/file_edit）类工具改为**硬拒**——风险顾问的「low」判定不再能放行它们（顾问仅继续兜 send_message/media_generate/publish_* 送达类）。此前这些高危工具一句 LLM「low」即可从 webhook 免审批运行。回归测试 `engine/bug_f5_unattended_reviewer_override_test.go`（含 {工具 × 来源 × 顾问判定} 全量授权矩阵）。
- **测试（BUG-F6）**：修复 feishu `TestStart_LogMessageWellFormed` 在 `-race` 下偶发（3/10）的数据竞争——`captureDefaultLog` 改用 mutex 包裹的 `syncBuffer` 作日志 sink，消除后台 Start 协程写入与测试读取的竞争（仅测试侧，生产代码不变）。

## [基线]
- 与 ai-core v0.1.6 / hexagon v0.5.1 / toolkit v0.2.0 对齐。
