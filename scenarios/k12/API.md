# K12 后端联调契约（前端对接指南）

> 后端 = `scenarios/k12` 场景包，全部端点挂在 **`/api/k12/*`**（平台 `srv.Mount` + `http.StripPrefix`）。
> 启动时 storage V8 迁移自动建 `agent_records` 表。响应均为 JSON；错误统一 `{"error":"..."}` + HTTP 状态码。
> DTO 定义见 `scenarios/k12/apihttp/handler.go`。

---

## 1. 端点清单（27 个）

### 视图 / 识题 / 批改

**`GET /api/k12/view-descriptor?slot=tutor`** — 驱动前端 chat shell 渲染
```json
{
  "header_tabs": ["辅导", "错题本"],
  "message_badges": ["verify", "record-chip"],
  "composer_placeholder": "发消息，或 ⌘V 粘贴作业照片",
  "composer_chips": ["🧮 数学讲解", "💡 渐进提示", "📷 识题校验"],
  "record_collections": ["错题本"],
  "side_panels": ["prep-card"],
  "actions": ["prep-card"],
  "i18n_keys": ["k12.tab.tutor", "k12.tab.mistakes"],
  "schema_version": 1
}
```

**`POST /api/k12/recognize` / `POST /api/k12/recognize/anchors` — 已删除（§6.14 一次切换 · 2026-07-18）**
拍照识题→锚点→批改统一走 `POST /grading-jobs*`（创建→轮询→`awaiting_confirmation` 停点产物含
识别清单+整卷学科+锚点 bbox→confirm→completed 取逐题结果）。两直连端点现回 404（反向契约
`cutover_20260718_old_links_removed_test.go`）。

**`POST /api/k12/grade`** — 批改一道题完整闭环（**需 LLM 密钥**）
- 请求：`{"agent","grade","source_session","problem","student_answer","knowledge_points":[]}`
- 响应：
```json
{
  "solution": "…教学解题…",
  "verdict": "agree|disagree|unverifiable|out_of_scope",
  "evidence_type": "numeric_exec|symbolic_exec|heterogeneous_model|heuristic|none",
  "badge": "verified-strong|verified-weak|disagree|out-of-scope|unverifiable",
  "correct": false,
  "wrong_step": "3.8×3 误算为 10.4",
  "error_cause": "小数点错位",
  "out_of_scope": false,
  "out_of_scope_kp": "",
  "record_created": true,
  "record_id": "…"
}
```

### 错题本 / 复习

**`GET /api/k12/mistakes?agent=X&status=`** — 错题本列表（status 可空=全部）
`{"items":[{"record_id","question","knowledge_point","error_cause","status","version","due_at"}]}`

**`GET /api/k12/review-queue?agent=X`** — 到期该练队列
`{"items":[{"record_id","question","knowledge_point","error_cause","status","version","due_at","subject","review_kind"}]}`
- `review_kind=verify`：数理化走验算链变式；`review_kind=verbatim`：语英积累本原词重现/字符比对。

**`POST /api/k12/mark-mastered`** — 「他会了」（乐观锁）
- 请求：`{"record_id","version"}` → 响应：`{"ok":true}`；**version 陈旧 → 409**

**`POST /api/k12/review/retry`** — 「再练一道」（按错题出同知识点相似题，过 solve 验算链）
- 请求：`{"agent","record_id","grade"}`（grade 可空，后端可按档案解析）
- 响应：`{"solution","verdict","badge"}`

### 学情 / 备课 / 学习时长

**`GET /api/k12/insight-report?agent=X`** — 学情报告
```json
{
  "trend": {"mastered":1,"reviewing":3,"retried":1,"archived":0,"total":5},
  "weak_top3": [{"knowledge_point":"小数乘法","count":3}],
  "month_new_mistakes": 5,
  "review_completion_rate": 0.4,
  "consecutive_fail_kps": ["小数乘法"],
  "suggestion": "「小数乘法」连续受挫，建议本周集中复习这个知识点。"
}
```

**`POST /api/k12/prep-card`** — 备课卡（只读，**热身题需 LLM 密钥**）
- 请求：`{"agent","grade","subject","knowledge_points":[]}`（`subject` 可选：当前题目学科，①段优先检索该学科教材，无则回退通用；空 = 不分科旧语义）
- 响应：`{"knowledge_points":[], "sections":[{"title","content","source_label"}]}`
- `source_label`：`📖 依据课本` / `🤖 AI 归纳·供参考（未校验）` / `🗂 本地记录` / `✅ 已程序验算` / `🧠 学情信号`

### 积累本 / 档案 / 导出 / 备份

**`POST /api/k12/accumulation`** `{"agent","source_session","subject","entry_type","content","source"}` → `{"record_id","created"}`
- `subject` 只允许 `语文`/`英语`；`entry_type`：默写错/错词/好词好句/古诗/语法点

**`GET /api/k12/accumulation?agent=X&subject=`** → `{"items":[{"record_id","subject","entry_type","content","status"}]}`

**`GET /api/k12/profile?agent=X`** → `{"child_name","grade_term","textbook_edition"}`

**`PUT /api/k12/profile`** — 建档 / 改档（升学改年级）
- 请求：`{"agent","child_name","grade_term","textbook_edition"}`（只改传的非空字段）→ 响应同 GET

**`POST /api/k12/cold-start`** — 首拍作业冷启动建档（按知识点倒推年级，已有档案不覆盖）
- 请求：`{"agent","child_name","knowledge_points":[],"fallback_grade","textbook"}`
- 响应：`{"child_name","grade_term","textbook_edition","inferred":true,"created":true}`

**`GET /api/k12/export?agent=X&format=md|pdf|docx`** — 错题本导出
- `md`（或 render 服务未启用）→ `{"format":"markdown","content":"# 错题本…"}`
- `pdf`/`docx` → **二进制流**（`Content-Type` + `Content-Disposition: attachment`）
- 渲染失败 → 降级 `{"format":"markdown","content":…,"render_error":…}`

**`GET /api/k12/mistake-sheet?agent=X`** — 本周错题卷（Markdown，只出题）→ `{"format":"markdown","content"}`

**`GET /api/k12/backup?agent=X`** → `{"version","agent_name","exported_at","records":[…],"checksum"}`（.hexbak）
**`POST /api/k12/restore`** — body=上面的 hexbak → `{"restored":N,"snapshot":{...}}`；**checksum 不符 → 400**

### 深度对话（渐进提示 + 情绪守门）

**`POST /api/k12/tutor-turn`** `{"agent","prior_stage":0|1|2|3,"parent_message","student_answer","problem","grade"}` → `{"stage":1|2|3,"comfort":bool,"emotion_cue","escalated":bool,"prompt_hint","solution","badge"}`
- 渐进提示三阶段编排（PRD §3.3.4）：`prior_stage` 传上一轮的 `stage`（首轮传 0）。后端按 `parent_message`（"不会"/"直接讲"）+ `student_answer` 推进阶段。
- `prompt_hint` 是**给上游 LLM 的行为指令**（本轮该给方向提示/具体提示/完整讲解），前端把它连同题目发给会话模型生成家长话术。
- `comfort=true` = 情绪守门命中（家长消息含"哭了/生气/急哭"等）→ 本轮切安抚、**不推进阶段、不给解**。
- `stage=3` 且有 `problem` 时 `solution` 带**经 solve 验算链**的完整解 + `badge`（**需 LLM 密钥**）；阶段一二 `solution` 为空（不给未验证答案）。

### 自动化沉淀投递（cron · §3.6）

投递内容端点返回**纯文本**，**空 body = 本期无内容（静默跳过，不投递空内容）**。供平台 cron 的 Starlark 脚本 http_get 抓取后投递到 IM 群/桌面：

**`GET /api/k12/cron/mistake-sheet?agent=X`** — 周五错题卷（到期该练；无到期→空）
**`GET /api/k12/cron/daily-reminder?agent=X`** — 每日复习提醒一句话（无待复习→空）
**`GET /api/k12/cron/return-reminder?agent=X`** — 回传提醒（§3.13）：昨日固化仍未回传的卷 → 温和提醒（含卷面号与题数，**每卷最多一次**，reminder_sent_at 持久幂等；已回传/家长关闭/非昨日→空）
**`GET /api/k12/cron/monthly-report?agent=X`** — 月度学情报告 Markdown（无记录→空）
**`GET /api/k12/cron/semester-check?agent=X`** — 学期确认提醒（无档案/已最末档→空）
**`GET /api/k12/cron/year-archive?agent=X`** — 学年 6 月底归档建议（无记录→空）

**`POST /api/k12/cron/provision`** `{"agent","platform","chat_id","deliver":["dingtalk"],"user_id","base_url"}` → `{"provisioned":[{"kind","name","schedule","job_id"}]}`
- 显式切换兼容入口：注册 §3.13 四个默认任务，并回收历史默认任务残留。
- 需服务器注入 cron.Scheduler（桌面默认有）；未注入 → **501**。`base_url` 服务器已配时可省。

**`POST /api/k12/cron/reconcile-defaults`** `{"agent","platform","chat_id","deliver":["dingtalk"],"user_id","base_url"}` → `{"provisioned":[{"kind","name","schedule","job_id","created"}]}`
- 档案新建/编辑成功后的作用域修复入口，只补该 `agent` 缺失的 §3.13 默认任务。
- exact `source_key` 已存在时零写入：保留用户的暂停状态、时间表、时区、投递目标、平台/会话和脚本；`source_key` 为空的用户任务永不参与。
- 逐项失败可安全重试，二次成功调用零写；不做全局启动扫描。
- 需服务器注入 cron.Scheduler（桌面默认有）；未注入 → **501**。`base_url` 服务器已配时可省。

### IM 入站路由绑定（§3.1.7）

**`POST /api/k12/bind-im`** `{"agent","platform","instance_id","chat_id"}` → `{"bound":true,...}`
- 把某 IM 群（platform+chat_id）绑到辅导实例，之后该群消息经平台 `agent_rules` 路由到这个 Agent（各绑各的群）。
- 需注入 router（桌面默认有）；未注入 → **501**。

---

## 2. 前端必须遵守的 9 个契约点

1. **`agent` = 实例 `name`（agents.name，不可变隔离键），不是显示名。** 所有端点靠它做多孩隔离。
2. **`grade` = 18 档枚举**（`一年级上`…`初三下`），从 `GET /profile.grade_term` 取，原样传。
3. **徽章按 `badge` 字段渲染，别自己判断**：
   - `verified-strong` = ✅ 已程序验算
   - `verified-weak` = 「AI 自检一致·未程序验算」— **绝不能显示"已程序验算"**
   - `disagree` = ⚠️ 并列双答 + 请复核 / `out-of-scope` = ⛔ / `unverifiable` = 无徽章
4. **`out_of_scope=true` 先判**：超纲错发，无 solution，走"错发反问"UI（按档案年级讲/别的孩子/按题目年级）。
5. **`record_created=false` 是去重命中**，`record_id` 仍有效（指向已存在错题），可直接 mark-mastered。
6. **`mark-mastered` 必带 `version`**（从 mistakes/review-queue 拿）；409 冲突时重取再试。
7. **view-descriptor 驱动 chat shell**：tabs/badges/composer_chips/side_panels 全从 descriptor 渲染；**K12 字面量只允许在 `features/k12`**，通用 chat shell 不硬编码（AP-1 红线）。
8. **`review_completion_rate == -1`** = 当月无错题（分母 0），显示「—」。
9. **`review_kind` 驱动再练 UI**：`verify` 走变式题/验算链，`verbatim` 走原文重现/字符比对，不要只按 collection 名猜。

## 3. 关键流程

- **识题回显护栏**（分渠道两种形态，同一信任链目标）：
  - **桌面**：交互门——`POST /grading-jobs`（照片）→ 轮询到 `awaiting_confirmation` 停点（响应携带识别清单+锚点）→ 前端 `RecognizeGuardPanel` 展示让家长确认/纠正 → `confirm` → 轮询到 `completed` 取逐题结果；单题补批仍走 `grade`。
  - **IM（钉钉/微信）**：内联回显——`homework-checker` skill 在解答**同一条消息**开头先列「我读到的题目」抬头再给整页解答，不阻塞等确认（IM 多轮往返代价高）；`[?]` 不确定字符点名请确认、确认前该题不下批改结论。出站经 `NormalizeMathText` 把 LaTeX 降级为 Unicode 数学符号（钉钉 markdown 不渲染 LaTeX）。
- **建档**：实例（agent）先经平台 Agent 创建，再 `PUT /profile` 写 K12 档案（落 `k12.child_name`/`k12.grade_term`/`k12.textbook_edition` metadata 键，不覆盖其他 metadata）。
- **导出 PDF**：`format=pdf` 需服务器装 pandoc；未装则降级 markdown JSON — 前端要判断响应是二进制还是 `{content}`。

## 4. 状态与仍受限项

| 功能 | 状态 |
|---|---|
| 真 LLM | `grade`/`grading-jobs`（识别+批改阶段）/`prep-card 热身题`/`tutor-turn 阶段三 solution` 运行时真调云端模型，**服务器必须配 `cfg.LLM` provider+密钥**，否则报错。其余端点纯本地无需 LLM |
| cron 自动投递（周五错题卷/回传提醒/学期确认×2，§3.13 四任务） | ✅ 已接：档案保存走 `cron/reconcile-defaults` missing-only 补齐，保留用户已改任务；显式切换兼容入口 `cron/provision` 仍负责注册并回收历史 kind 残留。投递内容走 `cron/*` 纯文本端点（空 body 静默跳过），复用平台 cron 调度 + Deliverer（IM/桌面）|
| IM 群绑定（各绑各的群） | ✅ 已接：`bind-im` 写 `agent_rules`，入站群消息路由到对应实例 |
| 渐进三阶段提示 + 情绪守门 | ✅ 已接：`tutor-turn` 输出分阶段指令 + 守门标志；**桌面/HTTP 联调可用** |
| IM 入站作业 → 自动错题入库副作用 | ✅ 结构已通：engine 把已路由 Agent 名 stamp 进 ctx（`skill.RoutedAgentName`），K12 提供通用 `k12_grade` skill 包全闭环（批改+错题入库+学情），实例 scope 从 ctx 取。**辅导 Agent 模板须在 Skills 声明 `k12_grade`**（建档时挂载）。真 IM+LLM 端到端仍需活环境验 |
| 学情注入 / 超纲学段内重解 | 学情信号写入已有（`grade` 触发 WriteWeakness）；超纲已判（`out_of_scope`），学段内自动重解仍走 LLM |
| 学习时长 | 近似值（`note` 说明，基于记录活跃非消息间隔） |
| 多教材版本 / 物化硬边界 | 仅人教数学有超纲硬判定，其他学科软约束（不 block） |

## 5. 最容易踩的第一个坑

联调报"LLM 未配置/解题失败"类错误时，先确认服务器端 `cfg.LLM` 有可用 provider + 密钥（`grade`/`grading-jobs`/`prep-card` 依赖它）。纯数据端点（mistakes/review/report/profile/backup/export-md/accumulation）不依赖 LLM，可先联调这些。
