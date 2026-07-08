package skilladapter_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/skilladapter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
)

// BUG-1 (Medium) RecordChip 前后端 metadata 契约不一致。
//
// 前端 messageRecordChip 读嵌套对象 metadata.record.{collection,fields,status}（据 schema
// 渲染入库徽章），但后端 grade_skill 只发扁平 k12_record_* 键、无 record 生产者 → 徽章恒不显。
// 本测试钉死后端契约：判错入库时 Metadata["record"] 必须是可被前端消费的结构化 JSON
// （collection=错题本、fields 含 question/knowledge_point/error_cause、status=new）。
func TestGradeSkill_BUG1_EmitsConsumableRecordMetadata(t *testing.T) {
	deps := newDeps(t, usecase.GradeOutcome{
		Correct: false, WrongStep: "小数点错位", ErrorCause: "计算失误", KnowledgePoint: "小数乘法",
	})
	sk := skilladapter.NewGradeSkill(deps)
	ctx := skill.WithRoutedAgent(context.Background(), "mingming")

	res, err := sk.Execute(ctx, map[string]any{
		"problem": "3.8×3", "student_answer": "11.6", "grade": "五年级上",
		"knowledge_points": []any{"小数乘法"},
	})
	if err != nil {
		t.Fatal(err)
	}

	raw, ok := res.Metadata["record"]
	if !ok || raw == "" {
		t.Fatalf("判错入库应产出前端可消费的 record 元数据, meta=%v", res.Metadata)
	}

	var rec struct {
		Collection string            `json:"collection"`
		Fields     map[string]string `json:"fields"`
		Status     string            `json:"status"`
	}
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatalf("record 元数据应是合法 JSON: %v (raw=%q)", err, raw)
	}
	if rec.Collection != k12.CollectionMistakes {
		t.Errorf("collection 应为 %q, got %q", k12.CollectionMistakes, rec.Collection)
	}
	if rec.Fields["question"] != "3.8×3" {
		t.Errorf("fields.question 应回填题干, got %q", rec.Fields["question"])
	}
	if rec.Fields["knowledge_point"] != "小数乘法" {
		t.Errorf("fields.knowledge_point 应为小数乘法, got %q", rec.Fields["knowledge_point"])
	}
	if rec.Fields["error_cause"] != "计算失误" {
		t.Errorf("fields.error_cause 应为计算失误, got %q", rec.Fields["error_cause"])
	}
	if rec.Status != k12.StatusNew {
		t.Errorf("status 应为 %q, got %q", k12.StatusNew, rec.Status)
	}
}

// 答对不入库 → 不得产出 record 元数据（避免前端渲染空徽章）。
func TestGradeSkill_BUG1_NoRecordMetadataWhenCorrect(t *testing.T) {
	deps := newDeps(t, usecase.GradeOutcome{Correct: true})
	sk := skilladapter.NewGradeSkill(deps)
	ctx := skill.WithRoutedAgent(context.Background(), "mingming")
	res, err := sk.Execute(ctx, map[string]any{"problem": "1+1", "student_answer": "2", "grade": "一年级上"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Metadata["record"]; ok {
		t.Errorf("答对不入库不应有 record 元数据, meta=%v", res.Metadata)
	}
}
