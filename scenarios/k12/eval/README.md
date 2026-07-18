# K12 §5.7 产品质量门 · 六套 eval

权威：`hexclaw-docs/dev/k12-prd/执行计划-v0.5.0.md` §5.7（2026-07-18 重写版）。
本目录即 `Pack.EvalSuites` 声明的 `scenarios/k12/eval`：数据组织 + 指标计算 + blind holdout 封存 + runner。

## 六套件与数据量（当前种子集）

| # | 套件 | 目录 | 单位 | dev | holdout | 可评方式 |
|---|---|---|---|---|---|---|
| 1 | OCR 识别 | `data/01-ocr` | 字段 | 8 + 3 judge | 3 | not_wired：字段 ground truth 已备好，**真实照片待采集** |
| 2 | 题目对齐 | `data/02-alignment` | 题 | 6 | 2 | not_wired：数据契约已钉，对齐器待落地 |
| 3 | 逐题判定 | `data/03-judgment` | 题 | 44 | 14 | **真机**（confusion matrix + coverage + weighted_risk） |
| 4 | 练习生成与验题 | `data/04-exercise-gen` | 生成题 | 12 boundary + 1 generate + 6 judge | 6 | boundary=**确定性**（curriculum+IsBeyond）；generate/judge=真机/judge |
| 5 | 答案泄露 | `data/05-answer-leak` | 产物 | 10 product + 3 judge | 4 | product=**确定性**（AnswerLeaked + 真实打印卷接线检查） |
| 6 | 作品反馈 | `data/06-work-feedback` | 反馈条 | 12 redline + 7 judge | 6 | redline=**确定性**（生产 INV-011 拦截器唯一真相） |

- 自造题全部小学范围、逐题独立验算（验算过程写在用例 `notes`）。
- hub 数据引入方式：**转写同步**（非引用）——`hexclaw-hub/skills/eval/k12/*.json` 场景转成统一信封，
  `source` 字段标 `hexclaw-hub@skills/eval/k12/<file>#<idx>` 溯源；仓库自包含，不依赖 hub 路径存在。
  未同步：concept/science/infotech（讲解/学科辅导行为，属 skill 侧行为 eval，不落六套件评价单位）。
- **数据欠账（申报）**：§5.7 要求每学科 ≥100 张真实照片（手写/多题/多页/密集/歪斜/涂改）。
  照片不可捏造，本批为文本形态种子集；照片采集与扩容留专门批。

## 统一用例格式

```json
{ "id": "...", "kind": "...", "subject": "...", "grade": "...",
  "input": { 按 kind 类型化 }, "expected": { 按 kind 类型化 },
  "scoring": ["评分维度"], "source": "self | hexclaw-hub@...", "notes": "验算/边界说明" }
```

kind 与负载定义见 `suites.go`（format 测试钉死每套件允许的 kind 与必填字段）。

## blind holdout 机制（流程契约）

1. **分离**：holdout 在独立目录 `data/holdout/`，与 dev 集语义独立、case id 全局不重叠（format 测试钉死）。
2. **封存**：`data/holdout/manifest.json` 记录每个 holdout 文件的 SHA-256；常规门 `TestHoldoutSealed`
   校验逐文件哈希一致——改 holdout 数据不重封存必红。
3. **契约**：holdout **只在发布评审时运行**（`HEXCLAW_K12_EVAL=1` 且 `HEXCLAW_K12_EVAL_SPLIT=holdout`），
   **任何人不得在提示词/参数调优中查看或使用 holdout 用例**；同一数据不得既调阈值又当验收。
   跑一次记一次报告（内容寻址）。扩容/修订 holdout 须显式重封存（`eval.SealHoldout`）并在执行计划留痕。
4. **报告 ID**：报告内容寻址——`report_id = "k12eval-" + SHA-256(报告规范化 JSON)[:16]`，
   报告内嵌 `holdout_manifest_sha256` 绑定封存版本。**holdout 全量报告的 report_id 即翻门证据 ID**。

## runner 用法

```bash
# 常规门（默认在内，不走 LLM）：格式校验 + holdout 封存校验 + 确定性 eval（套件4/5/6 dev）
GOWORK=off GOTOOLCHAIN=auto go test -count=1 ./scenarios/k12/...

# 真机小规模冒烟（gpt-5.6-sol，每 LLM 套件 5 题，dev 分割，报告落盘 reports/）
HEXCLAW_K12_EVAL=1 GOWORK=off GOTOOLCHAIN=auto \
  go test ./scenarios/k12/eval -run TestK12EvalSuites_RealModel -v -count=1 -timeout 30m

# 发布评审（holdout 全量，执行 §5.7 门槛：coverage≥90%、weighted_risk≤2%、确定性套件全过）
HEXCLAW_K12_EVAL=1 HEXCLAW_K12_EVAL_SPLIT=holdout HEXCLAW_K12_EVAL_LIMIT=0 GOWORK=off GOTOOLCHAIN=auto \
  go test ./scenarios/k12/eval -run TestK12EvalSuites_RealModel -v -count=1 -timeout 60m
```

模型解析：默认取 `~/.hexclaw/hexclaw.yaml` 的 `reasoning_provider/reasoning_model`（gpt-5.6-sol），
凭据不经环境变量明文传递；`HEXCLAW_K12_EVAL_PROVIDER/_MODEL` 可覆盖（显式启用后缺配置即 FAIL，不静默换模型）。

指标（§5.7 文档公式）：`coverage = 确定判定数 / 总数`；
`weighted_risk = (3·N(对判错) + 2·N(错判对)) / N(确定判定)`；holdout 全量运行以 95% 置信上界口径评审。

## SubjectVerifierGate 翻门契约

门治理表在 `scenarios/k12/practiceset.go`（`subjectVerifierGate` + `VerifierGateBaselineEvidence`）：

- 数学/语文/英语：`deterministic-baseline-20260718`（legacy 基线证据，执行计划 §5.7 翻门治理明文允许，
  正式分学科 holdout 报告落库后替换）。
- 科学/信息科技：未达门；**翻门必须携带第 4 套 eval 的 holdout 报告 ID**（`k12eval-` + 16 hex）。
- 契约双钉：`usecase.TestVerifierGateGovernance`（非空 + 与运行时一致）+
  `eval.TestVerifierGateHoldoutEvidence`（证据格式：legacy 白名单仅限三学科，其余必须为 holdout 报告 ID）。
