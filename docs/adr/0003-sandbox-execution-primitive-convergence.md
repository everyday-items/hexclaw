# ADR-0003: 沙箱执行原语收敛（code/shell/ProcessSandbox → code_exec）

日期：2026-07-08 · 状态：已采纳（P0/P1 已执行，P2 待遥测窗口）

## 背景

"让 Agent 执行代码/命令"这一件事，仓里长成了 **4 份重叠实现**——典型的执行原语历史增生（accreted primitive），旧的没被回收：

| 执行原语 | 位置 | 隔离 | 文件访问 | 网络 | 能力自省 | 契约 |
|---|---|---|---|---|---|---|
| `code` (CodeSkill) | `skill/builtin/code.go` | ❌ 裸 `exec.CommandContext`（go/py/node in tmpDir） | tmpDir，无 broker | 继承宿主 | ❌ 自造 limitedWriter | 纯文本 |
| `shell` (ShellSkill) | `skill/builtin/shell.go` | ❌ 裸 `sh -c`（宿主 cwd） | 全宿主 | 继承宿主 | ❌ 手搓 | 纯文本 |
| **`code_exec`** (CodeExecSkill) | `skill/builtin/code_exec.go` | ✅ `toolkit/os/sandbox`（linux ns / darwin sbpl / basic 降级） | ✅ **FileAccessBroker**（deny-default + 授权目录 allow-list，只读父目录/可写 workspace） | ✅ deny-default + `DenyLoopback` 防 SSRF | ✅ `LimitReport`（不支持项如实上报为 capability gap） | run_id 隔离 workspace + artifact manifest + bounded output + 4 mode |
| `ProcessSandbox` | `runtime/sandbox.go` | ❌ 裸 exec（名叫 sandbox 却不 sandbox） | — | — | — | flag-gated，**零调用点=死代码** |

底层库评审 M0-1 把 `code`/`shell` 的裸 exec 标为 **P0 安全洞**：`code_exec` 是唯一正确样板，但旧原语仍在生产装配（opt-in `cfg.Code`/`cfg.Shell`），config 三开关并列，让债务显性化。

### 一次被推翻的误判（记录以警后人）

评审初期曾判定"`shell` 是设计上的宿主执行工具，套文件系统沙箱会破坏 git/npm/host 工作流，所以只能保留裸 exec——属产品两难"。**此判断错误**，源于对 `code_exec` 的 `mode=project` + `FileAccessBroker` 理解不完整：

- `code_exec` 4 mode 是**功能超集**：`snippet`/`file`/`module` 覆盖 `code`；`project`（带 `command` argv）覆盖 `shell`。
- `FileAccessBroker` 正是"受控宿主访问"的最佳实践答案——`mode=project` 的 `project_root` 必须落在用户授权目录 allow-list（deny-default，只读父目录、可写 run workspace）。既能跑仓库测试/构建，又不给全盘宿主权限。

所以这不是产品两难，而是**工程收敛**：正确答案已在仓里，缺的是做减法的决心。

## 决策

### 一、收敛到单一执行原语 code_exec

```
┌─ L3 hexclaw ────────────────────────────────────────────────┐
│  code_exec（唯一执行 skill）                                  │
│    ├─ mode: snippet | file | module | project                │
│    ├─ 能力协商: FileAccessBroker(文件) + Network policy +     │
│    │            DenyLoopback + LimitReport(资源自省)          │
│    └─ 契约: run_id / artifact manifest / bounded output       │
└──────────────────────────┬───────────────────────────────────┘
                           │ 只依赖机制，不重复造
┌─ L0 toolkit/os/sandbox ──┴───────────────────────────────────┐
│  Sandbox 接口 (Exec) + 平台后端 + LimitReport                 │
└──────────────────────────────────────────────────────────────┘
     ✗ 删除 code / shell / runtime.ProcessSandbox
```

一个 skill、一个沙箱机制（L0 toolkit）、一个能力协商层。机制在 L0、编排/契约在 L3，严格分层。

### 二、分阶段迁移（已执行 P0/P1）

| 阶段 | 动作 | 状态 |
|---|---|---|
| **P0.1** | 删除整个死 `runtime/` 包（`hexclaw/runtime` 零 importer；ProcessSandbox/DockerSandbox/Sandbox 接口/CheckpointStore 全只被自身 test 引用） | ✅ 已执行 |
| **P0.2** | 单一沙箱路径回归锁 `skill/builtin/sandbox_single_path_test.go`：`skill/builtin` 下除 grandfather 白名单（`code.go`/`shell.go`）外禁止裸 `exec.Command`/`exec.CommandContext`，新增即 FAIL | ✅ 已执行（实测有牙） |
| **P1** | `code`/`shell` 标 deprecated：`builtin.go` security warning 升级为 deprecation warning 指向 `code_exec`；`config.go` `Code`/`Shell` 标弃用；遥测复用既有 `tool.call.completed` 事件（`tool=code`/`shell`），数据驱动删除时机 | ✅ 已执行 |
| **P2** | 遥测确认无残留依赖后（约两个 minor 版本）移除 `code`/`shell` skill + config 开关 + 从锁白名单移除（届时锁自动收紧到零裸 exec） | ⏳ 待遥测窗口 |

**不做"大爆炸"替换**：先加锁防新增、标弃用、上遥测，用数据驱动删除。

### 三、hermeticity 修复（顺带挖出的真 bug）

`code_exec` 原对 `GOWORK` 做 `unset`（`codeExecUnsetEnvKeys`），意图是"沙箱内 go 命令不加载开发者 repo workspace"。但 **`unset` ≠ `GOWORK=off`**：unset 后 go 仍沿目录树**自动发现**宿主 go.work 进 workspace 模式，进而想写宿主 `go.work.sum`（沙箱只读拒绝 → 执行失败）。改为 **`export GOWORK=off`**（base exports）才真正兑现 hermeticity 目标。

### 四、唯一真正的产品决策：逃生舱

若确有"可信容器内完全不隔离宿主执行"诉求：**不**保留独立裸 exec skill，而是做成 `code_exec` 的显式能力位 `unsafe_host`（默认关 + 需显式配置 + 每次调用审计日志 + 醒目警告）。逃生舱收进统一原语并强审计，而非散落一个绕过所有护栏的旁路。

## 后果

- **安全**：死抽象（假沙箱 `ProcessSandbox`）清除；所有代码/命令执行走 `toolkit/os/sandbox`（OS 级隔离 + 能力自省 + deny-default 文件/网络/loopback）。
- **防复发**：单一沙箱路径锁机械挡死"手搓裸 exec 绕过沙箱"整类问题——新增裸 exec 即 FAIL（举一反三）。
- **hermeticity**：`code_exec` 在任何 go.work 环境下都真正隔离，不再越界写宿主 `go.work.sum`（配套消除了 `TestCodeExecSkill_Execute_ProjectGoCommand` 的环境失败）。
- **迁移可控**：`code`/`shell` 仍可用但打弃用警告；遥测就绪，P2 数据驱动删除；届时锁收紧。
- **分层干净**：沙箱机制归 L0 toolkit，编排/契约归 L3 code_exec；不再有 `runtime/` 重复机制。
- **待办（P1 打磨，非阻塞）**：把 `LimitReport` 的 `unsupported` 限制项作为结构化 capability gap 上浮到 `code_exec` 返回（"no silent cap" 契约化）；`FileAccessBroker` 未授权时返回结构化 `needs_authorization{path}` 信号驱动前端一次性授权 UX；网络/文件/loopback 收敛成不可变 `SandboxPolicy` 值对象消除策略漂移。

## 关联

- 底层库架构评审 M0-1（沙箱统一）——本 ADR 是其在 hexclaw 层的落地目标态。
- ADR-0002 讨论的是 **cron 脚本档**的沙箱边界（python3 子进程，单用户桌面场景可接受未隔离），与本 ADR 的 **skill 执行原语**是不同关注点；两者的长期方向一致——需 OS 级隔离时都应收敛到 `toolkit/os/sandbox`。
