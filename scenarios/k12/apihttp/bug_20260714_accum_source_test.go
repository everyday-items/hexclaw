package apihttp_test

import (
	"net/http"
	"testing"
)

// BUG-20260714（积累本原型对齐）：记录字段里本来有 source，但列表 DTO 漏传，桌面无法渲染
// 原型规定的“出处：课外阅读 · 主动收藏”元信息列。
func TestAccumulationListReturnsSource(t *testing.T) {
	h := newServer(t)
	rec, out := do(t, h, http.MethodPost, "/accumulation",
		`{"agent":"mingming","subject":"语文","entry_type":"好词好句","content":"时间像海绵里的水","source":"课外阅读 · 主动收藏"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add accumulation status=%d body=%v", rec.Code, out)
	}

	rec, out = do(t, h, http.MethodGet, "/accumulation?agent=mingming", "")
	items, _ := out["items"].([]any)
	if rec.Code != http.StatusOK || len(items) != 1 {
		t.Fatalf("list accumulation status=%d body=%v", rec.Code, out)
	}
	item := items[0].(map[string]any)
	if item["source"] != "课外阅读 · 主动收藏" {
		t.Fatalf("source=%v want 课外阅读 · 主动收藏; item=%v", item["source"], item)
	}
}
