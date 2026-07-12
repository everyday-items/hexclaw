package apihttp_test

import (
	"testing"
)

// TestDeleteMistakeEndpoint（UX-3 治本 · 契约）：DELETE /mistakes/{record_id}?agent=X
// 校验 agent 归属后删除对应错题记录，删完列表清零。用于家长「数据纠错」——移除记错/重复条目。
func TestDeleteMistakeEndpoint(t *testing.T) {
	h := newFaithfulServer(t)
	// 先记一条错题
	_, out := do(t, h, "POST", "/record-mistake",
		`{"agent":"mingming","subject":"数学","grade":"五年级上","problem":"解方程 2x+15=43","error_cause":"移项符号错"}`)
	recordID, _ := out["record_id"].(string)
	if recordID == "" {
		t.Fatalf("记一条错题应回 record_id, got %v", out)
	}
	// 删除
	rec, _ := do(t, h, "DELETE", "/mistakes/"+recordID+"?agent=mingming", "")
	if rec.Code != 200 {
		t.Fatalf("删除应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	// 列表清零
	_, mist := do(t, h, "GET", "/mistakes?agent=mingming", "")
	if items, _ := mist["items"].([]any); len(items) != 0 {
		t.Fatalf("删除后错题本应清零, got %d", len(items))
	}
}

// TestDeleteMistake_Scope 删除他人（非本 agent）记录 → 404（不越权删）。
func TestDeleteMistake_Scope(t *testing.T) {
	h := newFaithfulServer(t)
	_, out := do(t, h, "POST", "/record-mistake",
		`{"agent":"mingming","subject":"数学","grade":"五年级上","problem":"题 A"}`)
	recordID, _ := out["record_id"].(string)
	// 用别的 agent 删同一条 → 归属校验失败按不存在返回 404。
	rec, _ := do(t, h, "DELETE", "/mistakes/"+recordID+"?agent=stranger", "")
	if rec.Code != 404 {
		t.Fatalf("越权删应 404, got %d", rec.Code)
	}
	// 记录仍在。
	_, mist := do(t, h, "GET", "/mistakes?agent=mingming", "")
	if items, _ := mist["items"].([]any); len(items) != 1 {
		t.Fatalf("越权删不应改动记录, got %d", len(items))
	}
}

// TestDeleteMistake_MissingAgent agent 缺失 → 400。
func TestDeleteMistake_MissingAgent(t *testing.T) {
	h := newFaithfulServer(t)
	rec, _ := do(t, h, "DELETE", "/mistakes/whatever", "")
	if rec.Code != 400 {
		t.Fatalf("缺 agent 应 400, got %d", rec.Code)
	}
}
