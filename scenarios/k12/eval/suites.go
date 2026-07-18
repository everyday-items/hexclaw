// Package eval 是 K12 §5.7 产品质量门的六套 eval 数据组织 + 指标计算 + 报告封存库
// （执行计划-v0.5.0 §5.7，2026-07-18 重写版）。
//
// 六套 eval（各自独立集合、独立阈值、独立报告）：
//
//	1 OCR 识别        单位=字段    字段级准确率、字符错误率
//	2 题目对齐        单位=题      照片题→卷内题匹配准确率（含题号识别与顺序兜底）
//	3 逐题判定        单位=题      分学科 confusion matrix + coverage–risk + weighted_risk
//	4 练习生成与验题  单位=生成题  同考点保持率、答案正确率、超纲率
//	5 答案泄露        单位=产物    题目卷/IM/孩子可见文案中出现答案的比率（目标 0）
//	6 作品反馈        单位=反馈条  证据锚定率、禁则违反率（打分/排名/代写，目标 0）
//
// 数据纪律（§5.7）：开发集（data/<套件>/dev.json，可反复调参）与不可变 blind holdout
// （data/holdout/<套件>.json，只在发布评审时跑，任何人不得据其调参）严格分离；
// holdout 以 data/holdout/manifest.json 的 SHA-256 封存，常规门校验哈希一致（防篡改/防漂移）。
// 评测报告内容寻址（ReportID = "k12eval-" + SHA-256 前 16 位），holdout 分割的报告 ID
// 即 SubjectVerifierGate 翻门必须携带的「holdout 报告 ID」（契约 TestVerifierGateGovernance）。
package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SuiteMeta 套件登记表（编号/目录/键名以执行计划 §5.7 六套 eval 表为准）。
type SuiteMeta struct {
	No   int
	Dir  string // data/ 下的目录名（dev）与 holdout 文件名前缀
	Key  string // JSON suite 字段
	Name string // 文档中文名
	Unit string // 评价单位
}

// Suites 六套件登记（顺序 = 文档表格顺序）。
var Suites = []SuiteMeta{
	{1, "01-ocr", "ocr", "OCR 识别", "字段"},
	{2, "02-alignment", "alignment", "题目对齐", "题"},
	{3, "03-judgment", "judgment", "逐题判定", "题"},
	{4, "04-exercise-gen", "exercise-gen", "练习生成与验题", "生成题"},
	{5, "05-answer-leak", "answer-leak", "答案泄露", "产物"},
	{6, "06-work-feedback", "work-feedback", "作品反馈", "反馈条"},
}

// Suite 一个套件的一个分割（dev / holdout）。
type Suite struct {
	Suite   string `json:"suite"`
	SuiteNo int    `json:"suite_no"`
	Name    string `json:"name"`
	Unit    string `json:"unit"`
	Split   string `json:"split"` // dev | holdout
	Version string `json:"version"`
	Notes   string `json:"notes,omitempty"`
	Cases   []Case `json:"cases"`
}

// Case 统一用例信封：输入/期望按 kind 携带类型化负载，scoring 声明评分维度。
type Case struct {
	ID       string          `json:"id"`
	Kind     string          `json:"kind"`
	Subject  string          `json:"subject,omitempty"`
	Grade    string          `json:"grade,omitempty"`
	Input    json.RawMessage `json:"input"`
	Expected json.RawMessage `json:"expected"`
	Scoring  []string        `json:"scoring,omitempty"`
	Source   string          `json:"source,omitempty"` // self | hexclaw-hub@skills/eval/k12/<file>#<idx>
	Notes    string          `json:"notes,omitempty"`
}

// 各 kind 的类型化负载。kind 允许集合见 allowedKinds（format 测试钉死）。

// GradeInput / GradeExpected：套件 3 逐题判定（kind=grade / oos）。
type GradeInput struct {
	Problem         string   `json:"problem"`
	StudentAnswer   string   `json:"student_answer,omitempty"`
	KnowledgePoints []string `json:"knowledge_points,omitempty"`
}

type GradeExpected struct {
	Correct      *bool  `json:"correct,omitempty"`         // ground truth：学生答案是否正确
	OutOfScope   bool   `json:"out_of_scope,omitempty"`    // kind=oos：期望超纲拦截
	OutOfScopeKP string `json:"out_of_scope_kp,omitempty"` // 触发超纲的知识点
}

// BoundaryInput / BoundaryExpected：套件 4 确定性年级边界（kind=boundary）。
type BoundaryInput struct {
	KnowledgePoint string `json:"knowledge_point"`
}

type BoundaryExpected struct {
	OutOfScope bool   `json:"out_of_scope"`
	FirstGrade string `json:"first_grade"` // 课标词表首学年级（对照 curriculum.FirstGrade）
}

// ProductInput / ProductExpected：套件 5 答案泄露（kind=product）。
type ProductInput struct {
	ProductKind string `json:"product_kind"` // practice_paper_item | im_message | mistake_sheet_line | child_visible_text
	Product     string `json:"product"`      // 家长/孩子可见的产物文本
	Answer      string `json:"answer"`       // 该产物对应题目的标准答案（不得出现在产物中）
}

type ProductExpected struct {
	Leak bool `json:"leak"`
}

// RedlineInput / RedlineExpected：套件 6 禁则红线（kind=redline，确定性拦截器）。
type RedlineInput struct {
	Feedback string `json:"feedback"`
}

type RedlineExpected struct {
	Violation bool `json:"violation"`
}

// JudgeInput / JudgeExpected：LLM-as-judge 型用例（hub 场景转写；本批不接 judge runner）。
type JudgeInput struct {
	Skills  []string       `json:"skills,omitempty"`
	Query   string         `json:"query"`
	Context map[string]any `json:"context,omitempty"`
}

type JudgeExpected struct {
	Behaviors     []string `json:"behaviors"`
	AntiBehaviors []string `json:"anti_behaviors"` // 红线：任一命中即 FAIL
	AcceptanceRef string   `json:"acceptance_ref,omitempty"`
}

// FieldInput / FieldExpected：套件 1 OCR 字段抽取（kind=field_extraction）。
type FieldInput struct {
	PhotoAsset string `json:"photo_asset"` // 空 = 真实照片待采集（§5.7 每学科 ≥100 张的数据欠账）
	PageDesc   string `json:"page_desc"`
}

type FieldExpected struct {
	Fields map[string]string `json:"fields"` // stem / student_answer / unit
}

// AlignInput / AlignExpected：套件 2 题目对齐（kind=align）。
type AlignInput struct {
	PhotoQuestions []AlignPhotoQ `json:"photo_questions"`
	PaperItems     []AlignPaperQ `json:"paper_items"`
}

type AlignPhotoQ struct {
	Number *int   `json:"number"` // null = 照片上题号不可读（顺序兜底路径）
	Text   string `json:"text"`
}

type AlignPaperQ struct {
	PaperSeq int    `json:"paper_seq"`
	Question string `json:"question"`
}

type AlignExpected struct {
	Mapping        map[string]int `json:"mapping"` // 照片题序(1-based，字符串键) -> paper_seq
	UnmatchedPaper []int          `json:"unmatched_paper,omitempty"`
	UnmatchedPhoto []int          `json:"unmatched_photo,omitempty"`
}

// allowedKinds 每套件允许的 kind 集合（format 测试钉死；judge = hub 场景转写通用型）。
var allowedKinds = map[string][]string{
	"ocr":           {"field_extraction", "judge"},
	"alignment":     {"align"},
	"judgment":      {"grade", "oos", "judge"},
	"exercise-gen":  {"boundary", "generate", "judge"},
	"answer-leak":   {"product", "judge"},
	"work-feedback": {"redline", "judge"},
}

// DataRoot 返回 data 目录（相对包目录；go test 的 cwd 即包目录）。
func DataRoot() string { return "data" }

// DevPath / HoldoutPath 套件数据文件路径。
func DevPath(m SuiteMeta) string     { return filepath.Join(DataRoot(), m.Dir, "dev.json") }
func HoldoutPath(m SuiteMeta) string { return filepath.Join(DataRoot(), "holdout", m.Dir+".json") }

// SuitePath 按分割取路径。
func SuitePath(m SuiteMeta, split string) string {
	if split == "holdout" {
		return HoldoutPath(m)
	}
	return DevPath(m)
}

// LoadSuite 读取并解析一个套件分割。
func LoadSuite(path string) (Suite, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Suite{}, err
	}
	var s Suite
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Suite{}, fmt.Errorf("解析 %s: %w", path, err)
	}
	return s, nil
}

// Validate 校验套件分割与登记表一致 + 用例结构完整。
func (s Suite) Validate(m SuiteMeta, split string) error {
	if s.Suite != m.Key || s.SuiteNo != m.No || s.Name != m.Name || s.Unit != m.Unit {
		return fmt.Errorf("套件头与登记表不符: got (%s,%d,%s,%s) want (%s,%d,%s,%s)",
			s.Suite, s.SuiteNo, s.Name, s.Unit, m.Key, m.No, m.Name, m.Unit)
	}
	if s.Split != split {
		return fmt.Errorf("split 字段 %q 与所在位置 %q 不符", s.Split, split)
	}
	if s.Version == "" {
		return fmt.Errorf("version 不可空")
	}
	if len(s.Cases) == 0 {
		return fmt.Errorf("套件 %s/%s 无用例", m.Key, split)
	}
	kinds := allowedKinds[m.Key]
	seen := map[string]bool{}
	for i, c := range s.Cases {
		if c.ID == "" {
			return fmt.Errorf("case[%d] 缺 id", i)
		}
		if seen[c.ID] {
			return fmt.Errorf("case id 重复: %s", c.ID)
		}
		seen[c.ID] = true
		ok := false
		for _, k := range kinds {
			if c.Kind == k {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("case %s: kind %q 不在套件 %s 允许集合 %v", c.ID, c.Kind, m.Key, kinds)
		}
		if len(c.Input) == 0 || len(c.Expected) == 0 {
			return fmt.Errorf("case %s: input/expected 不可空", c.ID)
		}
	}
	return nil
}

// ---------- 指标（§5.7 判定 eval 计分） ----------

// Confusion 逐题判定 confusion matrix（§5.7 第 3 套：对判对/对判错/错判对/错判错/判 needs_review）。
type Confusion struct {
	TrueRightJudgedRight int `json:"true_right_judged_right"` // 对判对
	TrueRightJudgedWrong int `json:"true_right_judged_wrong"` // 对判错（weighted_risk 权重 3）
	TrueWrongJudgedRight int `json:"true_wrong_judged_right"` // 错判对（weighted_risk 权重 2）
	TrueWrongJudgedWrong int `json:"true_wrong_judged_wrong"` // 错判错
	NeedsReview          int `json:"needs_review"`            // 非确定判定（unverifiable 等）
}

// Total 全部评价单位数。
func (c Confusion) Total() int {
	return c.TrueRightJudgedRight + c.TrueRightJudgedWrong + c.TrueWrongJudgedRight + c.TrueWrongJudgedWrong + c.NeedsReview
}

// Definite 给出确定判定的数量（needs_review 之外）。
func (c Confusion) Definite() int { return c.Total() - c.NeedsReview }

// Coverage = 给出确定判定的比率（§5.7：全部拒答 coverage=0 直接不过门；数学/字词门槛 ≥90%）。
func (c Confusion) Coverage() float64 {
	if c.Total() == 0 {
		return 0
	}
	return float64(c.Definite()) / float64(c.Total())
}

// WeightedRisk = (3·N(对判错) + 2·N(错判对)) / N(确定判定)（§5.7 文档公式；分母 0 返回 -1）。
func (c Confusion) WeightedRisk() float64 {
	if c.Definite() == 0 {
		return -1
	}
	return float64(3*c.TrueRightJudgedWrong+2*c.TrueWrongJudgedRight) / float64(c.Definite())
}

// 判定 eval 门槛（§5.7：数学与语英字词 weighted_risk ≤2%、coverage ≥90%，以 holdout 95% 置信上界计；
// 常量为点估门槛，置信上界收紧在报告评审侧执行）。
const (
	MinJudgmentCoverage = 0.90
	MaxWeightedRisk     = 0.02
)

// ---------- 答案泄露确定性检测（套件 5） ----------

// AnswerLeaked 判定产物文本是否泄露标准答案：空白符归一后包含完整答案串（长度 ≥2 时判定；
// 单字符答案无法可靠判包含，交 judge 型用例）。注意「答案」二字本身不构成泄露
// （"答案我先不说" 是合规话术），只有答案值出现才算——holdout 用例钉死该边界。
func AnswerLeaked(product, answer string) bool {
	p := stripWS(product)
	a := stripWS(answer)
	if len([]rune(a)) < 2 {
		return false
	}
	return strings.Contains(p, a)
}

func stripWS(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '　':
			return -1
		}
		return r
	}, s)
}

// ---------- holdout 封存（manifest 哈希） ----------

// Manifest holdout 封存清单：文件 → SHA-256。任何改动 holdout 数据必须重封存并走发布评审，
// 流程契约见 README（不得在提示词调优中查看/使用 holdout）。
type Manifest struct {
	SealedAt string            `json:"sealed_at"`
	Policy   string            `json:"policy"`
	Files    map[string]string `json:"files"` // 相对 data/ 的路径 -> sha256 hex
}

// ManifestPath holdout 清单路径。
func ManifestPath() string { return filepath.Join(DataRoot(), "holdout", "manifest.json") }

// LoadManifest 读取封存清单。
func LoadManifest() (Manifest, error) {
	raw, err := os.ReadFile(ManifestPath())
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("解析 holdout manifest: %w", err)
	}
	return m, nil
}

// VerifyHoldoutSealed 校验 holdout 数据与封存清单逐文件哈希一致，且清单恰好覆盖全部六套件。
func VerifyHoldoutSealed() error {
	m, err := LoadManifest()
	if err != nil {
		return err
	}
	if len(m.Files) != len(Suites) {
		return fmt.Errorf("manifest 覆盖 %d 个文件，应为 %d（六套件各一）", len(m.Files), len(Suites))
	}
	for _, meta := range Suites {
		rel := filepath.ToSlash(filepath.Join("holdout", meta.Dir+".json"))
		want, ok := m.Files[rel]
		if !ok {
			return fmt.Errorf("manifest 缺套件 holdout 文件 %s", rel)
		}
		raw, err := os.ReadFile(filepath.Join(DataRoot(), filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); got != want {
			return fmt.Errorf("holdout %s 哈希不符（数据被改动而未重封存）: got %s want %s", rel, got, want)
		}
	}
	return nil
}

// HoldoutManifestSHA 封存清单文件自身的 SHA-256（写进评测报告，绑定报告↔封存版本）。
func HoldoutManifestSHA() (string, error) {
	raw, err := os.ReadFile(ManifestPath())
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// SealHoldout 重新生成封存清单（仅限扩容 holdout 后的显式封存动作使用；常规流程不调用）。
func SealHoldout(policy string) (Manifest, error) {
	m := Manifest{
		SealedAt: time.Now().UTC().Format(time.RFC3339),
		Policy:   policy,
		Files:    map[string]string{},
	}
	for _, meta := range Suites {
		rel := filepath.ToSlash(filepath.Join("holdout", meta.Dir+".json"))
		raw, err := os.ReadFile(filepath.Join(DataRoot(), filepath.FromSlash(rel)))
		if err != nil {
			return Manifest{}, err
		}
		sum := sha256.Sum256(raw)
		m.Files[rel] = hex.EncodeToString(sum[:])
	}
	return m, nil
}

// ---------- 评测报告（内容寻址落盘） ----------

// SuiteResult 单套件运行结果。
type SuiteResult struct {
	Suite    string     `json:"suite"`
	SuiteNo  int        `json:"suite_no"`
	Mode     string     `json:"mode"` // deterministic | real_llm | not_wired
	Total    int        `json:"total"`
	Passed   int        `json:"passed"`
	PassRate float64    `json:"pass_rate"`
	Coverage *float64   `json:"coverage,omitempty"`      // 套件 3
	Weighted *float64   `json:"weighted_risk,omitempty"` // 套件 3（文档公式）
	Matrix   *Confusion `json:"confusion_matrix,omitempty"`
	Failures []string   `json:"failures,omitempty"`
	Notes    string     `json:"notes,omitempty"`
}

// Report 一次评测运行的落盘报告。ReportID 内容寻址（对除 report_id 外的规范化 JSON 取
// SHA-256 前 16 位，前缀 "k12eval-"）；split=holdout 的 ReportID 即翻门证据 ID。
type Report struct {
	ReportID           string        `json:"report_id"`
	Split              string        `json:"split"` // dev | holdout
	Provider           string        `json:"provider"`
	Model              string        `json:"model"`
	HoldoutManifestSHA string        `json:"holdout_manifest_sha256"`
	GeneratedAt        string        `json:"generated_at"`
	CaseLimitPerSuite  int           `json:"case_limit_per_suite,omitempty"` // 0 = 全量
	Suites             []SuiteResult `json:"suites"`
}

// ReportIDPrefix 报告 ID 前缀（SubjectVerifierGate 翻门证据的格式契约）。
const ReportIDPrefix = "k12eval-"

// ComputeReportID 计算内容寻址报告 ID。
func ComputeReportID(r Report) (string, error) {
	r.ReportID = ""
	sort.Slice(r.Suites, func(i, j int) bool { return r.Suites[i].SuiteNo < r.Suites[j].SuiteNo })
	blob, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(blob)
	return ReportIDPrefix + hex.EncodeToString(sum[:])[:16], nil
}

// WriteReport 计算 ReportID 并把报告写到 dir/<report_id>.json，返回落盘路径。
func WriteReport(dir string, r Report) (string, Report, error) {
	id, err := ComputeReportID(r)
	if err != nil {
		return "", r, err
	}
	r.ReportID = id
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", r, err
	}
	blob, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", r, err
	}
	path := filepath.Join(dir, id+".json")
	if err := os.WriteFile(path, append(blob, '\n'), 0o644); err != nil {
		return "", r, err
	}
	return path, r, nil
}
