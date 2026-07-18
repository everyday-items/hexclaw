package apihttp_test

// 一次切换终局批（架构设计 §6.14 / 执行计划 §3.4 · 2026-07-18）：
// 批改链路旧「入口自编排」HTTP 面删除后的反向契约。
//
//  1. POST /recognize、POST /recognize/anchors —— 桌面/钉钉批改已全走统一
//     GradingJob（创建→轮询→停点确认→completed 取逐题结果，停点产物含识别
//     清单+整卷学科+锚点 bbox），两阶段直连编排端点被 Job 停点产物完全取代，
//     切换日起 404。
//  2. POST /practice-sets（整卷直建）—— 执行计划 §3.4 端点冻结「切换日死刑
//     名单」：装篮命令（basket/items → finalize）是唯一创建路径，前端从未对接
//     整卷直建，切换日起 404。
//
// 保留申报（非删除对象，甄别依据）：POST /grade（单题补批，RecognizeGuardPanel
// 停点后逐题补批/手工录错题共用验算管道）、POST /solve（空白题求解）仍被 Job 外
// 合法路径消费，本批不删。

import (
	"encoding/base64"
	"net/http"
	"testing"
)

func TestCutover20260718_RecognizeDirectEndpointsRemoved(t *testing.T) {
	h := newServer(t)
	img := base64.StdEncoding.EncodeToString([]byte("fake-image-bytes"))
	cases := []struct {
		path string
		body string
	}{
		{"/recognize", `{"image_base64":"` + img + `"}`},
		{"/recognize/anchors", `{"image_base64":"` + img + `","questions":[{"question":"3.8×3=?","knowledge_points":["小数乘法"],"answer_state":"present","student_answer":"11.4"}]}`},
	}
	for _, c := range cases {
		rec, _ := do(t, h, http.MethodPost, c.path, c.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("旧直连端点 %s 应已随一次切换删除（404），got %d——存在绕过统一 GradingJob 的旧编排后门", c.path, rec.Code)
		}
	}
}

func TestCutover20260718_DirectCreatePracticeSetRemoved(t *testing.T) {
	h := newServer(t)
	// 死刑名单端点：整卷直建 404。
	body := `{"agent":"mingming","title":"直建卷","source_kind":"weekly",
		"items":[{"item_id":"q1","question_markdown":"题","expected_answer_markdown":"答","verification_status":"verified","verification_evidence":"验算"}]}`
	// 同路径仍有 GET /practice-sets（列表），Go mux 对未注册方法回 405（含 Allow 头）——
	// 与 404 同为「端点已死」的诚实回答；唯独不能是 2xx/4xx 业务码。
	rec, _ := do(t, h, http.MethodPost, "/practice-sets", body)
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /practice-sets（整卷直建）应已随切换日死刑名单删除（404/405），got %d", rec.Code)
	}
	// 防过删：唯一创建路径（装篮命令）必须仍然可用。
	rec, out := do(t, h, http.MethodPost, "/practice-sets/basket/items", `{"agent":"mingming",
		"item":{"subject":"数学","added_via":"custom","question_markdown":"3.8×3=?","expected_answer_markdown":"11.4","verification_status":"verified","verification_evidence":"验算"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("装篮命令是唯一创建路径，必须存活: %d %v", rec.Code, out)
	}
	// 防过删：单题补批 /grade 与空白题 /solve 是甄别保留项，路由必须仍在
	//（400/502 都行，唯独不能 404）。
	for _, kept := range []string{"/grade", "/solve"} {
		rec, _ := do(t, h, http.MethodPost, kept, `{}`)
		if rec.Code == http.StatusNotFound {
			t.Errorf("保留端点 %s 被误删（404）——它仍被 Job 外合法路径（单题补批/空白题求解）消费", kept)
		}
	}
}
