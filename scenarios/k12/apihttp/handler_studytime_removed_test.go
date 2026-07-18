package apihttp_test

import "testing"

// TestStudyTimeRemoved 反向契约：GET /study-time 端点必须不存在（404）。
//
// 依据架构设计 v0.5.0《明确不做》#6：不做学习时长与无证据投入指标；
// §5.7 派生指标口径表内亦无「辅导次数」指标。此前该端点用错题/积累记录活跃
// 近似估算「学习时长/本月辅导次数」，属无证据投入指标，已整链删除
// （usecase/studytime.go + 本路由 + 前端学情瓷片换「证据已掌握」口径）。
// 本测试钉死不回潮：任何人重新挂 /study-time 路由都会在此变红。
func TestStudyTimeRemoved(t *testing.T) {
	h := newServer(t)
	rec, _ := do(t, h, "GET", "/study-time?agent=mingming", "")
	if rec.Code != 404 {
		t.Fatalf("GET /study-time 应 404（《明确不做》#6 已删除），got %d", rec.Code)
	}
}
