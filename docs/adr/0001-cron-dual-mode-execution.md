# ADR-0001: 定时任务双模执行（脚本编译 + Agent 推理 + 自愈桥）

日期：2026-06-11 · 状态：已采纳

## 背景

cron v2 把自然语言一次性编译为 Python 脚本，每次 tick 零 LLM 成本执行。
但它覆盖不了两类任务：

1. **认知类任务**（"每天总结待办挑重点发我"）——任务本身需要推理，脚本写不出来
2. **长尾脆弱**——上游改版后脚本持续失败，只能人工重建

备选方案：(a) 全部任务每次执行跑 Agent；(b) 保持纯脚本；(c) 双模 + 自愈。

## 决策

采用 (c)，三个组成部分：

1. **编译期模式分类**（`cron.ClassifyTaskMode`）：含推理动词（总结/分析/评估…）
   的任务标记 `Runtime=agent`，跳过脚本编译；机械 I/O 类维持脚本编译。
   分类是系统决策，**不暴露模式开关给用户**。
2. **Agent 执行器**（`Scheduler.SetAgentRunner`）：agent 模式任务每次 tick 经
   注入的 runner 跑一轮完整 Agent（main.go 包装 `engine.Process`）。
   护栏：最小间隔 1 小时（创建时拒绝并给出修改建议）。
3. **自愈桥**（`maybeSelfHeal`）：脚本任务连续失败 3 次 → 携带失败上下文
   重新编译一次；24h 窗口内最多 2 次，超额跳过并记录 `heal_failed`。

## 选型依据（外部先例）

| 组成 | 先例 |
|---|---|
| 确定性优先、agent 兜底 | Anthropic《Building Effective Agents》workflows-vs-agents 原则 |
| 编译一次零成本执行 | Airflow/Temporal 确定性调度；Voyager skill library；Zapier/n8n AI builder |
| 系统静默选执行计划 | 数据库 query optimizer（声明式 SQL → planner 选计划，用户不选 join 算法） |
| 失败回退智能路径再优化 | JIT deoptimization（HotSpot/V8 分层编译 + deopt）；Meta SapFix |

## 后果与已知限制

- agent 模式每次执行消耗一轮 LLM —— 由 1 小时频率护栏控制上限
- 分类器当前为确定性关键词表（保守偏向脚本，误判由自愈桥兜底）；
  可升级为编译器 LLM 判定而不改契约
- 自愈配额计数在内存中（桌面单实例场景成立；多实例部署需挪到 DB）
- cron 派发的 agent 执行消息带 `metadata.source=cron`，engine 对其旁路
  cron 意图引导（否则"执行任务"会被引导改写为"创建任务"）
