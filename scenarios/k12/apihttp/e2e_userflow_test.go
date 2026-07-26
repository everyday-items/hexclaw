package apihttp_test

import (
	"testing"
)

// TestE2E_ParentSession 模拟真实家长会话完整链路（真 HTTP 请求）：
// 批改答错 → 错题进本 → 拿 record_id+version → 「他会了」→ 记录家长确认并安排抽查。
// 这是"真实用户请求"级别的端到端验证（solve/grade 在 Execute 边界用 fake，其余全真）。
func TestE2E_ParentSession(t *testing.T) {
	h := newServer(t)

	// 1) 家长贴一道题、孩子答错 → 批改闭环
	_, out := do(t, h, "POST", "/grade",
		`{"agent":"mingming","grade":"五年级上","source_session":"s-home-1","problem":"3.8×3=?","student_answer":"10.4","knowledge_points":["小数乘法"]}`)
	if out["record_created"] != true {
		t.Fatalf("答错应入库: %v", out)
	}
	recordID, _ := out["record_id"].(string)
	if recordID == "" {
		t.Fatal("应返回有效 record_id")
	}

	// 2) 打开错题本 → 应看到这道题（真实用户查看）
	_, out = do(t, h, "GET", "/mistakes?agent=mingming", "")
	items, _ := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("错题本应 1 条, got %v", out)
	}
	m := items[0].(map[string]any)
	if m["record_id"] != recordID {
		t.Errorf("错题本 record_id 应与批改返回一致: %v vs %s", m["record_id"], recordID)
	}
	if m["status"] != "new" {
		t.Errorf("初始状态应 new, got %v", m["status"])
	}
	version := int(m["version"].(float64))

	// 3) 孩子会了 → 家长点「他会了」（乐观锁带 version）
	rec, out := do(t, h, "POST", "/mark-mastered",
		`{"agent":"mingming","record_id":"`+recordID+`","version":`+itoa(version)+`}`)
	if rec.Code != 200 || out["ok"] != true {
		t.Fatalf("mark-mastered 应成功: code=%d %v", rec.Code, out)
	}

	// 4) 再看错题本 → 学习状态不冒充系统掌握证据，同时可见家长确认与抽查事实。
	_, out = do(t, h, "GET", "/mistakes?agent=mingming", "")
	items, _ = out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("确认后错题仍应保留, got %v", out)
	}
	confirmed := items[0].(map[string]any)
	if confirmed["status"] != "new" {
		t.Fatalf("家长确认不得直接写 mastered, got %v", confirmed["status"])
	}
	if confirmed["parent_confirmed_at"].(float64) <= 0 {
		t.Fatalf("应记录 parent_confirmed_at, got %v", confirmed)
	}
	if confirmed["spot_check_state"] != "scheduled" {
		t.Fatalf("应安排一次抽查, got %v", confirmed["spot_check_state"])
	}
	_, out = do(t, h, "GET", "/mistakes?agent=mingming&status=mastered", "")
	items, _ = out["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("没有复做证据时不得进入 mastered, got %v", out)
	}

	// 5) 陈旧 version 再点 → 乐观锁冲突 409（防并发重复推进）
	rec, _ = do(t, h, "POST", "/mark-mastered",
		`{"agent":"mingming","record_id":"`+recordID+`","version":`+itoa(version)+`}`)
	if rec.Code != 409 {
		t.Errorf("陈旧 version 应 409 冲突, got %d", rec.Code)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
