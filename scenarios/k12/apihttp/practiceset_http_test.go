package apihttp_test

import (
	"net/http"
	"testing"
)

// TestPracticeSetHTTPLifecycle 通过真实 mux 跑购物车命令流（2026-07-18 裁决）：
// 装篮 → 移除 → 固化（打印/发送即确认，跳过阻断题）→ 回传 → 复批 → 关闭。
func TestPracticeSetHTTPLifecycle(t *testing.T) {
	h := newServer(t)
	// 装篮两题（幂等去重：第三次重复装同题 added=false）。
	rec, out := do(t, h, "POST", "/practice-sets/basket/items", `{"agent":"mingming","source_session":"s1",
		"item":{"subject":"数学","added_via":"weekly","question_markdown":"2.8×0.65=?","expected_answer_markdown":"1.82","verification_status":"verified","verification_evidence":"独立验算"}}`)
	if rec.Code != http.StatusOK || out["added"] != true {
		t.Fatalf("装篮 HTTP %d: %v", rec.Code, out)
	}
	id, _ := out["record_id"].(string)
	rec, out = do(t, h, "POST", "/practice-sets/basket/items", `{"agent":"mingming",
		"item":{"subject":"科学","added_via":"single_variant","question_markdown":"闭合电路判断","verification_status":"pending"}}`)
	if rec.Code != http.StatusOK || out["added"] != true || out["record_id"] != id {
		t.Fatalf("第二题应装入同一篮: %v", out)
	}
	rec, out = do(t, h, "POST", "/practice-sets/basket/items", `{"agent":"mingming",
		"item":{"subject":"数学","added_via":"weekly","question_markdown":"2.8×0.65=?","verification_status":"pending"}}`)
	if rec.Code != http.StatusOK || out["added"] != false {
		t.Fatalf("重复装篮应幂等去重: %v", out)
	}

	rec, got := do(t, h, "GET", "/practice-sets/"+id+"?agent=mingming", "")
	if rec.Code != http.StatusOK || got["status"] != "draft" || got["status_label"] != "草稿" {
		t.Fatalf("GET 篮异常: code=%d %v", rec.Code, got)
	}

	// 固化：打印即确认，一步到 assigned，科学题（pending）被跳过。
	rec, fin := do(t, h, "POST", "/practice-sets/"+id+"/finalize", `{"agent":"mingming","via":"send","target":"钉钉私聊"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("固化 HTTP %d: %v", rec.Code, fin)
	}
	if fin["skipped_blocked_count"] != float64(1) {
		t.Fatalf("应跳过 1 道未验证题: %v", fin["skipped_blocked_count"])
	}
	set, _ := fin["set"].(map[string]any)
	if set["status"] != "assigned" {
		t.Fatalf("固化后应一步到 assigned: %v", set["status"])
	}
	if set["question_artifact_id"] == "" || set["answer_artifact_id"] == "" {
		t.Fatalf("固化后应生成题目卷+答案卷: %v", set)
	}

	for _, step := range []struct{ path, want string }{
		{"/submit", "submitted"}, {"/grade", "graded"}, {"/close", "closed"},
	} {
		rec, r := do(t, h, "POST", "/practice-sets/"+id+step.path, `{"agent":"mingming"}`)
		if rec.Code != http.StatusOK || r["status"] != step.want {
			t.Fatalf("步骤 %s 应到 %s: code=%d %v", step.path, step.want, rec.Code, r)
		}
	}
}

// TestPracticeSetHTTPNoStandaloneConfirm 独立 confirm/assign 路由已删除（打印/发送即确认）。
func TestPracticeSetHTTPNoStandaloneConfirm(t *testing.T) {
	h := newServer(t)
	_, out := do(t, h, "POST", "/practice-sets/basket/items", `{"agent":"mingming",
		"item":{"subject":"数学","added_via":"weekly","question_markdown":"1+1=?","expected_answer_markdown":"2","verification_status":"verified","verification_evidence":"验算"}}`)
	id := out["record_id"].(string)
	for _, dead := range []string{"/confirm", "/assign"} {
		rec, _ := do(t, h, "POST", "/practice-sets/"+id+dead, `{"agent":"mingming"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("独立 %s 命令应已死（404），got %d——存在绕过打印即确认的后门", dead, rec.Code)
		}
	}
}

// TestPracticeSetHTTPRemoveAndZeroVerified 篮内移除；全阻断篮不能出卷。
func TestPracticeSetHTTPRemoveAndZeroVerified(t *testing.T) {
	h := newServer(t)
	_, out := do(t, h, "POST", "/practice-sets/basket/items", `{"agent":"mingming",
		"item":{"subject":"数学","added_via":"custom","question_markdown":"3.8×3=?","expected_answer_markdown":"11.4","verification_status":"verified","verification_evidence":"验算"}}`)
	id := out["record_id"].(string)
	rec, got := do(t, h, "GET", "/practice-sets/"+id+"?agent=mingming", "")
	items := got["items"].([]any)
	itemID := items[0].(map[string]any)["item_id"].(string)

	rec, got = do(t, h, "POST", "/practice-sets/"+id+"/items/remove", `{"agent":"mingming","item_id":"`+itemID+`"}`)
	if rec.Code != http.StatusOK || len(got["items"].([]any)) != 0 {
		t.Fatalf("移除后篮应为空: code=%d %v", rec.Code, got["items"])
	}
	rec, r := do(t, h, "POST", "/practice-sets/"+id+"/finalize", `{"agent":"mingming","via":"print"}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("空篮却固化成功: %v", r)
	}
}

// TestPracticeSetHTTPOwnerIsolation 跨实例 GET 返回非 200。
// （2026-07-18 一次切换：整卷直建端点已删，创建改走唯一路径——装篮命令。）
func TestPracticeSetHTTPOwnerIsolation(t *testing.T) {
	h := newServer(t)
	body := `{"agent":"mingming",
		"item":{"subject":"数学","added_via":"weekly","question_markdown":"题","expected_answer_markdown":"答","verification_status":"verified","verification_evidence":"验算"}}`
	_, out := do(t, h, "POST", "/practice-sets/basket/items", body)
	id := out["record_id"].(string)
	rec, _ := do(t, h, "GET", "/practice-sets/"+id+"?agent=otherkid", "")
	if rec.Code == http.StatusOK {
		t.Fatal("跨实例读取练习集应被拒")
	}
}
