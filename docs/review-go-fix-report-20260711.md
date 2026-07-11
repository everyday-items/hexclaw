# Go review 修复与测试闭环报告（2026-07-11）

## 结论

本轮 review 中确认的代码缺陷均已按 RED 回归测试、最小实现修复、横向扩散检查、普通/竞态/静态/安全验证的顺序闭环。当前分支可构建，全部 70 个有测试的 Go package 在发布工具链 Go 1.25.12 下通过；全仓 race 也通过。

结论包含两个明确边界：完整 staticcheck 仍报告 161 条历史债务；真实模型 K12 评测受本机 Ollama CPU 运行时失败影响未通过。这两项均在下文保留原始事实，没有计入“全绿”。

## 范围与环境

- 分支：`feat/v0.5.0-k12-parent-tutor`
- 基线提交：`71c065b`
- 模块声明：`go 1.25.7`
- 发布工具链：`toolchain go1.25.12`
- 验证方式：`GOWORK=off`，避免仓库外层 `go.work` 固定的 Go 1.25.7 覆盖发布工具链
- 工作树：实现与测试均未暂存、未提交；未执行 destructive git 操作
- 本仓库无 `integration` build-tag 测试文件，因此该矩阵不适用

保留 `go 1.25.7` 是为了兼容仓库外层现有 `go.work`；新增 `toolchain go1.25.12` 是因为 Go 1.25.7 的标准库扫描存在 11 个可达 CVE，而 1.25.12 已修复这些问题。

## 修复内容

### K12、档案与记录

- HTTP 请求体限制、错误码和学科边界收紧，评分失败保持 fail-closed。
- 备份恢复校验版本与 checksum；恢复在同一 SQLite 事务内完成记录 merge 和 K12 profile 替换。
- 恢复语义与原型一致：备份中不存在的当前记录保留，同 ID 时导入记录覆盖，profile 以导入快照为准。
- 修复 grounding 的 agent/subject 范围与来源追踪，以及复习、积累、备课、评测和跨学科闭环。
- 新增并发恢复、过期归档、错误备份、真实评测门禁等回归测试。

### Engine、Session 与路由

- 路由后的 agent 标识、回复元数据和历史 egress 分类可持久化、可回放。
- 修复语义缓存状态污染、采样参数覆盖、solve 校验、ghost/pinned agent 静默降级和 warmup 生命周期竞态。
- Router 使用深快照；注册、更新、注销、默认 agent 和规则删除均改为持久化成功后再更新内存。
- SQLite 默认 agent 注销在单事务中完成删除、级联和默认值重选；不支持原子删除的 store fail-closed。

### Cron、MCP 与并发状态

- Cron 的 claim、状态、历史、投递、自愈和连续 checkpoint 全部按 generation 做 CAS/隔离。
- Starlark state 同样按 generation 隔离；相同 generation 可续跑，replacement 不继承旧任务状态。
- MCP 重启使用连接代际保护；旧连接迟到返回的 EOF 不会断开已经重启成功的新连接。
- SendQueue 停止过程线性化；Telegram、Matrix、Discord、DingTalk、WhatsApp 的重复超时 Stop 共用单个 lifecycle waiter，避免 goroutine 累积。

### 出网、安全与适配器

- 本地端点判断统一按 URL hostname/IP 解析，公网路径或 hostname 中包含 `localhost` 文本不再绕过 egress policy。
- 显式公网 BaseURL 优先于 `ollama` 等 provider 名称；embedder、reranker 与 LLM router 共用同一判断。
- Render 对初始 URL 和每次重定向执行 SSRF 校验，并在连接时重新解析、固定已校验 IP、复核 peer address；自定义 transport 无法绕过保护。
- Skill marketplace 和 builtin skill 写入采用 no-replace、原子 rename、权限收紧与文件/目录 symlink no-follow。
- 适配器补齐签名/secret、MIME、请求体和错误体上限、context 传播、保留字段与生命周期处理。
- Webhook 重名返回 409；CJK 截断、Ollama `num_ctx` warmup、KeepAlive 配置/API/factory 传播已统一。

## 验证结果

| 门禁 | 结果 | 证据 |
| --- | --- | --- |
| Workspace 全量测试 | PASS，70/70 package，294s | `/tmp/hexclaw-test-workspace-final.log` |
| Module 全量测试 | PASS，70/70 package，297s | `/tmp/hexclaw-test-module-final.log` |
| 全仓 race | PASS，70/70 package，644s | `/tmp/hexclaw-test-race-final.log` |
| Go 1.25.12 发布态全量测试 | PASS，70/70 package，407s | `/tmp/hexclaw-test-module-go1.25.12.log` |
| 关键修复包覆盖测试 | PASS，38 package，综合 statements 69.6%，391s | `/tmp/hexclaw-key-cover.log`、`/tmp/hexclaw-key.cover` |
| `go build ./...` | PASS，8s | `/tmp/hexclaw-build-final.log` |
| `go vet ./...` | PASS，0 告警 | `/tmp/hexclaw-vet-final-current.log` |
| `go mod verify` | PASS | `all modules verified` |
| `go mod tidy -diff` | PASS，无差异 | 最终复核命令输出 |
| `govulncheck ./...` | PASS，0 个可达漏洞 | `/tmp/hexclaw-govulncheck-go1.25.12.log` |
| staticcheck 正确性规则集 | PASS，0 告警 | `/tmp/hexclaw-staticcheck-correctness-current.log` |
| `gofmt` / `git diff --check` | PASS，无输出 | 最终复核命令输出 |
| 修改文件 secret scan | PASS，未发现真实凭证 | 私钥、AWS key、强 token、Bearer/JWT 均 0 命中 |

关键包覆盖率包括：Cron 90.2%、egress 86.1%、K12 usecase 84.1%、LLM router 83.2%、session 80.8%、router 78.7%、engine 76.1%、MCP 75.2%、Render 71.8%。命令行入口包含大量启动和外部交互分支，覆盖率为 18.2%。

全仓 race 完成后仅有两处历史测试空分支被改成明确断言；这两处随后分别通过 focused normal、focused race 和 SA9003 staticcheck。生产代码没有在全仓 race 之后发生语义修改。

## 未计入通过的外部项

### 真实模型 K12 评测

启用 `HEXCLAW_REAL_LLM_EVAL=1` 后执行 `TestK12EvalGate_RealModel`，51 个 case 能进入真实调用，但本机 Ollama 的 `qwen3.5:9b` CPU-only 运行时退出，且未产出要求的 grading metadata，命令在 236s 后失败。评测门禁保持 fail-closed，没有因为外部故障而误判通过。当前环境也没有可替代的云模型凭据。

该项需要在具备稳定 Ollama/GPU 或授权云模型的环境中重跑；它是外部运行环境缺口，不应被表述为已经通过。

### 完整 staticcheck 历史债务

完整 `staticcheck ./...` 当前返回 161 条：122 条 SA1019 弃用引用、21 条 U1000 历史未使用代码，其余为少量 style/simplification 和既有测试告警。本轮新增的可执行正确性告警已清零；排除已确认的历史 SA1019、SA4006、SA1012、SA9009 后，SA 正确性规则集为 0 告警。

完整输出保存在 `/tmp/hexclaw-staticcheck-current.log`。迁移 Hexagon 根包和 nhooyr websocket、清理遗留死代码应单独立项，避免和本轮行为修复混在同一变更中。

## 交付状态

- 修复代码、回归测试和本报告均留在工作树中。
- 没有 stage、commit 或 push。
- 没有发现冲突标记、误纳入的生成二进制、格式错误或依赖漂移。
- 生产监控、配额和线上 KPI 不属于本地 SQLite/代码审查范围，本轮不作虚构结论。
