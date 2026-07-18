package eval

// 常规门（不走 LLM）：六套件数据文件格式校验 + holdout 封存哈希校验。
// 保证「数据可解析/字段齐/套件头对齐文档/用例负载类型化可解/holdout 未被改动」。

import (
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// TestSuiteRegistryMatchesSpec 六套件登记表以执行计划 §5.7 表格为准（名称/单位/顺序）。
func TestSuiteRegistryMatchesSpec(t *testing.T) {
	want := []struct{ name, unit string }{
		{"OCR 识别", "字段"},
		{"题目对齐", "题"},
		{"逐题判定", "题"},
		{"练习生成与验题", "生成题"},
		{"答案泄露", "产物"},
		{"作品反馈", "反馈条"},
	}
	if len(Suites) != 6 {
		t.Fatalf("§5.7 定义六套 eval，登记 %d 套", len(Suites))
	}
	for i, m := range Suites {
		if m.No != i+1 {
			t.Fatalf("套件 %s 编号 %d，应为 %d", m.Key, m.No, i+1)
		}
		if m.Name != want[i].name || m.Unit != want[i].unit {
			t.Fatalf("套件 %d 名称/单位 (%s,%s) 与文档 (%s,%s) 不符", i+1, m.Name, m.Unit, want[i].name, want[i].unit)
		}
	}
}

// TestAllSuitesParseAndValidate 全部 dev+holdout 文件可解析、字段齐、ID 全局唯一且 dev/holdout 不重叠。
func TestAllSuitesParseAndValidate(t *testing.T) {
	globalIDs := map[string]string{}
	for _, m := range Suites {
		for _, split := range []string{"dev", "holdout"} {
			path := SuitePath(m, split)
			s, err := LoadSuite(path)
			if err != nil {
				t.Fatalf("加载 %s: %v", path, err)
			}
			if err := s.Validate(m, split); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			for _, c := range s.Cases {
				if prev, dup := globalIDs[c.ID]; dup {
					t.Fatalf("case id %s 在 %s 与 %s 重复", c.ID, prev, path)
				}
				globalIDs[c.ID] = path
				validateCasePayload(t, m.Key, c, path)
			}
		}
	}
}

// validateCasePayload 按 kind 解出类型化负载并做最小语义校验。
func validateCasePayload(t *testing.T, suiteKey string, c Case, path string) {
	t.Helper()
	if c.Grade != "" && k12.GradeRank(c.Grade) < 0 {
		t.Fatalf("%s case %s: 非法年级 %q", path, c.ID, c.Grade)
	}
	switch c.Kind {
	case "grade", "oos":
		var in GradeInput
		var exp GradeExpected
		mustDecode(t, path, c, c.Input, &in)
		mustDecode(t, path, c, c.Expected, &exp)
		if in.Problem == "" {
			t.Fatalf("%s case %s: problem 不可空", path, c.ID)
		}
		if c.Kind == "grade" {
			if exp.Correct == nil {
				t.Fatalf("%s case %s: kind=grade 必须给 expected.correct ground truth", path, c.ID)
			}
			if in.StudentAnswer == "" {
				t.Fatalf("%s case %s: kind=grade 必须有学生作答", path, c.ID)
			}
		}
		if c.Kind == "oos" && (!exp.OutOfScope || exp.OutOfScopeKP == "") {
			t.Fatalf("%s case %s: kind=oos 必须期望超纲并给出触发知识点", path, c.ID)
		}
	case "boundary":
		var in BoundaryInput
		var exp BoundaryExpected
		mustDecode(t, path, c, c.Input, &in)
		mustDecode(t, path, c, c.Expected, &exp)
		if in.KnowledgePoint == "" || exp.FirstGrade == "" || c.Grade == "" {
			t.Fatalf("%s case %s: boundary 需要 knowledge_point/first_grade/grade", path, c.ID)
		}
		if k12.GradeRank(exp.FirstGrade) < 0 {
			t.Fatalf("%s case %s: 非法首学年级 %q", path, c.ID, exp.FirstGrade)
		}
	case "product":
		var in ProductInput
		var exp ProductExpected
		mustDecode(t, path, c, c.Input, &in)
		mustDecode(t, path, c, c.Expected, &exp)
		if in.Product == "" || in.Answer == "" || in.ProductKind == "" {
			t.Fatalf("%s case %s: product 用例需要 product_kind/product/answer", path, c.ID)
		}
	case "redline":
		var in RedlineInput
		mustDecode(t, path, c, c.Input, &in)
		if in.Feedback == "" {
			t.Fatalf("%s case %s: redline 用例需要 feedback", path, c.ID)
		}
	case "field_extraction":
		var in FieldInput
		var exp FieldExpected
		mustDecode(t, path, c, c.Input, &in)
		mustDecode(t, path, c, c.Expected, &exp)
		if in.PageDesc == "" || len(exp.Fields) == 0 {
			t.Fatalf("%s case %s: field_extraction 需要 page_desc 与 ground truth fields", path, c.ID)
		}
		if _, ok := exp.Fields["stem"]; !ok {
			t.Fatalf("%s case %s: fields 必须含 stem", path, c.ID)
		}
	case "align":
		var in AlignInput
		var exp AlignExpected
		mustDecode(t, path, c, c.Input, &in)
		mustDecode(t, path, c, c.Expected, &exp)
		if len(in.PhotoQuestions) == 0 || len(in.PaperItems) == 0 || len(exp.Mapping) == 0 {
			t.Fatalf("%s case %s: align 需要照片题/卷内题/mapping", path, c.ID)
		}
		total := len(exp.Mapping) + len(exp.UnmatchedPhoto)
		if total != len(in.PhotoQuestions) {
			t.Fatalf("%s case %s: mapping+unmatched_photo 应覆盖全部照片题（%d != %d）", path, c.ID, total, len(in.PhotoQuestions))
		}
	case "generate", "judge":
		// generate/judge 为 LLM 评价型：只要求 input/expected 可解析为对象。
		var in map[string]any
		var exp map[string]any
		mustDecode(t, path, c, c.Input, &in)
		mustDecode(t, path, c, c.Expected, &exp)
		if len(in) == 0 || len(exp) == 0 {
			t.Fatalf("%s case %s: input/expected 不可为空对象", path, c.ID)
		}
	default:
		t.Fatalf("%s case %s: 未登记 kind %q（套件 %s）", path, c.ID, c.Kind, suiteKey)
	}
}

func mustDecode(t *testing.T, path string, c Case, raw json.RawMessage, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("%s case %s: 负载解析失败: %v", path, c.ID, err)
	}
}

// TestHoldoutSealed holdout 数据与封存清单逐文件哈希一致（防提示词过拟合的物理防线之一：
// 改了数据必然过不了常规门，除非显式重封存并留痕）。
func TestHoldoutSealed(t *testing.T) {
	if err := VerifyHoldoutSealed(); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if m.Policy == "" || m.SealedAt == "" {
		t.Fatal("manifest 必须携带封存时间与流程契约 policy")
	}
}

// TestReportIDDeterministic 报告 ID 内容寻址：同内容同 ID，改动任一字段 ID 变化。
func TestReportIDDeterministic(t *testing.T) {
	r := Report{Split: "dev", Provider: "openai", Model: "gpt-5.6-sol", GeneratedAt: "2026-07-18T00:00:00Z",
		Suites: []SuiteResult{{Suite: "judgment", SuiteNo: 3, Mode: "real_llm", Total: 5, Passed: 5, PassRate: 1}}}
	id1, err := ComputeReportID(r)
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := ComputeReportID(r)
	if id1 != id2 {
		t.Fatalf("同内容报告 ID 不稳定: %s vs %s", id1, id2)
	}
	r.Suites[0].Passed = 4
	id3, _ := ComputeReportID(r)
	if id3 == id1 {
		t.Fatal("内容变化后报告 ID 未变化")
	}
	if len(id1) != len(ReportIDPrefix)+16 {
		t.Fatalf("报告 ID 格式应为 %s+16 hex，got %s", ReportIDPrefix, id1)
	}
}
