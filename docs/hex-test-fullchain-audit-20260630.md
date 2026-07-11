# hex-test 全链路审计报告（2026-06-30）

> 范围：以后端 `hexclaw` + 前端 `hexclaw-desktop` 为主，基于真实 sidecar、真实浏览器、真实模型、真实 API、MCP、cron、benchmark 的测试结果做审计。  
> 约束：本次运行环境为 macOS / Apple M1；Linux、Windows 只做代码/历史测试证据引用，未做真机 E2E。日志中的 access key / token 已刻意脱敏，不写入本文。

## 结论

当前主链路不是“不可用”，但还没有达到 100% 闭环。核心会话、API、知识库检索、记忆读写、MCP 基础调用、cron 任务创建/执行/清理、桌面真实浏览器路径多数可用；主要短板集中在 IM 适配器稳定性、模型路由一致性、多模态路由、MCP 单 server 健康、真实模型测试抗波动。

总体评级：黄偏红。

- 绿色：基础 API 矩阵 35/35；真实模型 API 链路 23/24；桌面 live E2E 3/3；MCP Manager live 通过；cron 确定性任务 API 全闭环；Go benchmark 可跑。
- 红色：DingTalk SDK 曾触发 `panic: send on closed channel` 并杀死 sidecar；OpenRouter alternate provider 429 导致 provider switch E2E 失败；默认 `qwen3-vl` 在知识/记忆路径持续 404；图片附件可发送但模型路由到非视觉模型后失败，E2E 没抓住。
- 黄色：桌面单测有多处 warning/审计提示；MCP `time` server 持续 EOF 但 `filesystem` 可用；真实模型 P0 sandbox 测试受上游网络/超时影响未 100% 证明。

### 2026-07-04 CI/CD 复验更新

本轮复验基于当前工作区依赖升级：`toolkit v0.2.6`、`ai-core v0.2.4`、`hexagon v0.5.9`，三者均声明 `go=1.25.7`。

- `sandbox-code-exec.yml` 已进入默认分支并注册为专项 workflow，覆盖 toolkit 联调下的 Linux/macOS code_exec 强沙箱路径，并保留 Windows toolkit sandbox 硬门禁；Windows code_exec runtime 集成用例按当前 toolkit 工具链/设备访问能力门控。
- `GOWORK=off go test ./... -run '^$'` 通过，说明发版/CI 模式下全仓编译已恢复；此前 `toolkit v0.2.3` 缺字段导致的 release 构建不可复现问题已由依赖升级修复。
- `GOWORK=off go test ./... -count=1` 通过。`engine/TestProbe_RunnerIntegrity_MustFail` 已改为 `HEXCLAW_RUNNER_PROBE=1` 手工门控，默认 CI 不再被故意失败探针阻断。
- `.github/workflows/{ci,render,release,sandbox-code-exec}.yml` 经 `actionlint v1.7.7` 校验通过。
- 2026-07-04 的 CI 修复补充了四项边界：普通 Linux CI 在无 bubblewrap/usable unshare 时跳过真实沙箱执行型用例；Linux project 模式使用 dedicated scratch workspace，并只读进入项目根执行，避免强沙箱中未绑定的外部 `/tmp` 不可见；Windows code_exec 用临时 `.cmd` wrapper 文件执行，避免 toolkit ADS 校验误判多行脚本参数；默认 `max_memory_bytes` 提升到 2GiB，满足 Go/Node runtime 在强沙箱中的冷启动需求。

## 已执行测试

| 层级 | 命令/路径 | 结果 |
| --- | --- | --- |
| 后端全量单测（原审计，2026-06-30） | `go test ./... -count=1` | 通过 |
| 后端 release 构建（原审计，2026-06-30） | `GOWORK=off GOFLAGS=-mod=readonly go build ./...` | 失败：`skill/builtin/code_exec.go` 使用了本地 toolkit 新字段，但 `go.mod` 的 `github.com/hexagon-codes/toolkit v0.2.3` 不包含这些字段 |
| CI/CD 复验（2026-07-04） | `GOWORK=off go test ./... -run '^$'` | 通过；全仓编译与依赖解析在 release 模式下可复现 |
| CI/CD 复验（2026-07-04） | `GOWORK=off go test ./... -count=1` | 通过；runner 完整性探针默认跳过，仅在 `HEXCLAW_RUNNER_PROBE=1` 时手工触发 |
| Workflow lint（2026-07-04） | `actionlint v1.7.7 .github/workflows/*.yml` | 通过 |
| code_exec 强沙箱复验（2026-07-04） | `HEXCLAW_P0_SANDBOX_PROOF=1 go test -v -count=1 -timeout 360s ./skill/builtin -run 'TestCodeExecSkill_\|TestDetectMissingPackages\|TestBuildInstallCommand\|TestSandboxP0_StaticGapMatrix'` | 通过；覆盖 P0 code_exec 执行、产物、限额、超时、运行时诊断矩阵 |
| Windows 编译复验（2026-07-04） | `GOOS=windows GOARCH=amd64 go test -c -o /tmp/hexclaw-builtin-windows.test.exe ./skill/builtin` | 通过；覆盖 Windows wrapper 签名与 toolkit sandbox API 兼容性 |
| 后端 race 全量复验（2026-07-04） | `go test -race -count=1 -coverprofile=/tmp/hexclaw-coverage.out ./...` | 通过 |
| 桌面 Vitest | `pnpm vitest run --no-cache` | 4987 passed，15 todo；有 warning/审计提示 |
| 桌面类型检查 | `pnpm vue-tsc --build` | 通过 |
| WebKit feel | `pnpm test:webkit-feel` | 7/7 通过；sidecar 未启动时有 Vite proxy `ECONNREFUSED` 噪声 |
| API 矩阵 | 35 个真实 sidecar endpoint | 35/35 通过 |
| 真实模型 API E2E | `tests/e2e/api-chain.spec.ts`，硅基流动 + DeepSeek-V4-Pro | 23/24 通过；provider switch 因 alternate 上游 429 失败 |
| 真实浏览器 live E2E | `pnpm test:e2e:live`，硅基流动 + Qwen/Qwen3.6-35B-A3B | 3/3 通过 |
| 浏览器 bugfix E2E | `tests/e2e/browser-bugfixes-real.spec.ts` | 3 passed，1 failed，1 未执行；DingTalk retry 计数未被真实点击路径证明 |
| MCP live | `go test ./mcp -run TestLive_TimeServer` | 通过；`@modelcontextprotocol/server-memory` 发现 9 个工具 |
| MCP sidecar 状态 | `/api/v1/mcp/status` | 200；`filesystem` connected=true/tool_count=14，`time` connected=false/tool_count=0 |
| MCP 工具调用 | `list_allowed_directories` | 200；返回 `/private/tmp` |
| cron API 全链路 | `/api/v1/cronjob` create/list/run/history/remove | 通过；history: `success`, `exit_code=0`, `data=hex-test-cron-ok` |
| Go benchmark | engine/api/knowledge/router 短时基准 | 通过；见下方摘录 |
| P0 sandbox 静态矩阵 | `TestSandboxP0_StaticGapMatrix` | 11/11 通过 |
| P0 sandbox 真实模型矩阵 | `TestSandboxP0_RealModelToolUseMatrix` | 6/11 通过，5/11 因真实上游 timeout/reset 失败，未证明 100% |

## 关键问题

### P0：sidecar 稳定性被 IM SDK 拖垮

证据：此前真实 sidecar 长跑中出现 `panic: send on closed channel`，栈位于 `github.com/open-dingtalk/dingtalk-stream-sdk-go/client.(*StreamClient).processLoop.func6`，导致进程退出。代码上 desktop 模式会无条件 `instanceMgr.StartEnabled(ctx)`，没有明显的 CLI/env 级禁用 IM adapter 开关。

影响：任何一个 IM adapter 的第三方 SDK panic 都可以杀死整个 agent runtime，聊天、cron、MCP、sandbox 全部受影响。

建议：

- IM adapter 必须 supervisor 化：每个 adapter 独立 goroutine + panic recover + 自动熔断 + 重启上限。
- desktop/测试模式增加 `--disable-im` 或 env 开关，且支持按平台禁用。
- SDK 回调边界统一包一层 recover，不允许第三方 SDK panic 穿透主进程。

### P0：模型路由不一致，默认 `qwen3-vl` 持续 404

证据：sidecar 日志反复出现 `model 'qwen3-vl' not found`，触发点包括 knowledge multi-query、HyDE、memory/profile 等路径。即使测试显式选择硅基流动模型，内部增强链路仍会走不可用默认模型。

影响：用户主 chat 可能成功，但知识增强、记忆画像、RAG 查询扩展在后台持续失败，表现为“能聊但智能能力不稳定/变弱”。

建议：

- 建立统一 ModelResolver：chat、knowledge、memory、cron compile、vision 都通过同一模型选择与可用性校验。
- 启动时检测默认模型是否存在；不存在则降级到已配置可用模型，并在 UI 显示配置问题。
- 对 404 model_not_found 做 fail-fast，不要在每次请求里重复打失败日志。

### P0：provider switch 链路失败

证据：真实 API E2E 24 个用例里唯一失败项为 `Cross-provider switch (default then alternate)`，WebSocket 返回 `Provider returned error (code: 429)`；sidecar 日志显示 alternate 走到 OpenRouter free `google/gemma-4-26b-a4b-it:free`，上游 rate-limited。

影响：用户在 UI 切换 provider/model 时不是稳定能力，而是可能切到不可用/限流模型，破坏多 provider 体验。

建议：

- alternate provider 选择只从“健康且有 key/额度”的 provider 池里取。
- 429/5xx 做分类：上游限流不应等同业务失败，应给出可恢复错误和 fallback 策略。
- E2E 中不要固定依赖 OpenRouter free 模型，优先在硅基流动配置内切换多个可用模型。

### P0：多模态附件路由与测试断言不闭环

证据：真实浏览器 live 的拖拽 PNG 用例通过，但 sidecar 日志显示图片发送实际路由到本地 `qwen3.5:9b` 后返回 400：`Failed to load image or audio file`。测试只验证附件预览/发送按钮/清空，没有断言 assistant 成功或用户可见错误。

影响：UI 看起来“发送成功”，真实模型处理失败，用户会得到空回复或错误体验。

建议：

- 附件消息必须根据 MIME/能力路由到 vision-capable model。
- 非视觉模型时前端发送前阻断，并给出明确错误。
- E2E 必须断言最终 assistant 成功内容或明确错误提示，不能只断言输入框状态。

### P0：DingTalk runtime retry 真实点击路径未被证明

证据：`browser-bugfixes-real.spec.ts` 中 DingTalk transient health test 失败：预期 mock 计数为 3，实际 `undefined`，说明真实点击路径没有命中测试里期望的 bridge/mock 计数路径。后续 failure test 未执行。

影响：DingTalk 重试/失败边界仍未被 E2E 证明，测试夹具和真实 UI 调用链可能脱节。

建议：

- 统一前端 runtime test 调用入口，测试 mock 必须挂在真实 click path 会调用的位置。
- 断言 API 请求、重试次数、最终 UI 状态三者同时成立。
- 对 DingTalk/Feishu/Line 等 IM channel 做同构 E2E 表格测试。

### P0：默认 CI 会纳入故意失败探针（2026-07-04 已修复）

历史证据：`engine/probe_runner_integrity_test.go` 中的 `TestProbe_RunnerIntegrity_MustFail` 用于证明测试 runner 真实执行；修复前代码路径恒定 `t.Fatalf`。本地复现：

```text
GOWORK=off go test -race -count=1 ./engine -run TestProbe_RunnerIntegrity_MustFail -v
--- FAIL: TestProbe_RunnerIntegrity_MustFail
```

影响：GitHub Actions `CI / Test (Linux)` 执行 `go test -race -count=1 -coverprofile=coverage.out ./...`，修复前会将该探针纳入默认全量测试，导致 PR/main push 红灯。它不是产品功能失败，但会阻断 CI/CD。

修复状态：

- runner 完整性探针已默认 `t.Skip`，仅在显式设置 `HEXCLAW_RUNNER_PROBE=1` 时启用。
- CI 全量命令不需要排除普通包，避免用 `-run` 或包列表掩盖真实回归。
- `GOWORK=off go test ./... -run '^$'` 与 `GOWORK=off go test ./... -count=1` 均已通过。

### P0：release 构建不可复现（2026-07-04 已修复）

历史证据：`GOWORK=off GOFLAGS=-mod=readonly go build ./...` 曾失败，原因是 `skill/builtin/code_exec.go` 引用了 `sandbox.Config` / `ExecResult` 的新字段，但 `go.mod` 锁定的 `toolkit v0.2.3` 不包含这些字段。

修复状态：当前依赖已升级到 `toolkit v0.2.6`、`ai-core v0.2.4`、`hexagon v0.5.9`，并统一 `go 1.25.7`。`GOWORK=off go test ./... -run '^$'` 已通过，全仓编译和模块解析不再依赖本地 `go.work` 隐式状态。

## 次级问题

### MCP：基础能力可用，但配置 server 不健康

证据：

- `go test ./mcp -run TestLive_TimeServer` 通过，MCP Manager 可连 stdio server 并发现工具。
- sidecar `/api/v1/mcp/status` 显示 `filesystem` connected=true/tool_count=14。
- sidecar 同时显示 `time` connected=false/tool_count=0，并持续日志 `calling "initialize": EOF`。
- `list_allowed_directories` 调用成功，但当前只返回 `/private/tmp`。

判断：MCP 底座不是坏的；问题在配置 server 健康、UI 状态提示、默认 allowed directories 与用户项目能力预期。

### 真实模型 sandbox P0 未 100% 证明

证据：静态 P0 sandbox gap matrix 11/11 通过；真实模型矩阵 11 项中 6 项通过，5 项失败为 `context deadline exceeded` / `connection reset by peer` 这类上游网络/模型调用问题。

判断：不能把这 5 项定性为 sandbox 功能失败，但也不能宣称真实模型全闭环。需要把“模型可用性失败”和“sandbox 执行失败”在 harness 中拆开。

### 桌面测试 warning 与质量债

证据：

- Vitest 全量通过，但有 Node `--localstorage-file was provided without a valid path` warning。
- 设计审计提示 API layer 硬编码中文 17 处、`src/stores/settings.ts` 超 500 行。
- i18n 缺失 key，包括 `integration.searchSkills` 等。
- ChatInput 测试出现 `Invalid watch source: undefined`。
- `WorkspaceFlows` mock 缺 `getKnowledgeConfig`。

判断：不是当前阻断，但会持续降低测试可信度和重构安全边界。

## Benchmark 摘录

环境：darwin/arm64，Apple M1，`-benchtime=100ms`。

```text
BenchmarkBudget_Check-8                         54.92 ns/op    0 B/op    0 allocs/op
BenchmarkToolCache_GetHit-8                    583.7 ns/op   464 B/op   11 allocs/op
BenchmarkLogCollector_Query_Unfiltered-8      2382 ns/op
BenchmarkVectorSearch_100docs-8             281704 ns/op
BenchmarkDispatcher_RouteDefault-8             188.1 ns/op    32 B/op    1 allocs/op
```

建议：`ToolCache` 的 9-14 allocs/op 可以作为后续性能优化点；当前不是 P0。

## 建议优先级

P0：

- IM adapter supervisor/recover/disable switch。
- 统一 ModelResolver，消除 `qwen3-vl` 不可用默认值。
- 修 provider switch 健康选择与 fallback。
- 多模态能力路由 + E2E 断言最终结果。
- 修 DingTalk runtime test 的真实 click path。
- 保持 runner 完整性探针默认 skip，仅在 `HEXCLAW_RUNNER_PROBE=1` 下手工触发。
- 保持 `GOWORK=off` release 构建/编译门禁，防止再次依赖本地工作区隐式版本。

P1：

- MCP server 健康 UI、配置校验、失败退避。
- 真实模型 harness 增加 retry、多模型 fallback、失败分类。
- API 状态码语义优化，例如 invalid provider 更适合 4xx/422，而不是 500。
- 清理 Vitest warning、i18n 缺失、ChatInput invalid watch。

P2：

- 把 chat/knowledge/memory/cron/vision 的模型能力矩阵下沉为统一能力注册表。
- IM/MCP/connector 统一生命周期管理：启动、健康、熔断、恢复、可观测。
- 桌面 E2E 从“UI 状态断言”升级到“用户意图完成断言”。

P3：

- Linux/Windows 真机矩阵与 sandbox runtime 统一 conformance suite。
- 长稳压测：sidecar 8h、IM websocket 抖动、MCP server 重启、模型 provider 限流。
- benchmark 纳入 CI 趋势，不只做一次性快照。

## 未覆盖/限制

- 本轮未在 Linux/Windows 真机执行 E2E，不能证明跨平台 100%。
- 未连接生产环境。
- 未执行 MySQL MCP 真机 E2E，因为它要求本机 MySQL `dev` 库和固定凭据；本轮只跑了通用 stdio MCP live。
- 真实模型测试受上游服务波动影响，报告中已按“未证明”而不是“产品必然失败”处理。
- 清理阶段已停止本轮 sidecar/Vite；按名称检索未发现 `E2E DingTalk` 或本轮 cron smoke 任务残留在 YAML/JSON 配置中。
