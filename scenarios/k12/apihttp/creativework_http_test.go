package apihttp_test

import (
	"context"
	"net/http"
	"testing"
)

// TestCreativeWorkHTTPLifecycle 通过真实 mux 跑作品 create→AI feedback→revision→再 AI feedback。
func TestCreativeWorkHTTPLifecycle(t *testing.T) {
	h := newFeedbackServer(t, func(context.Context, string, string, string) (string, error) {
		return "「柳枝像绿色的丝带」比喻贴切；建议补一个听觉细节。", nil
	})
	body := `{"agent":"mingming","source_session":"s","work_type":"writing","title":"《春天的校园》","task":"观察春景","content_markdown":"柳枝像绿色的丝带"}`
	rec, out := do(t, h, "POST", "/creative-works", body)
	if rec.Code != http.StatusOK || out["created"] != true {
		t.Fatalf("创建作品异常: code=%d %v", rec.Code, out)
	}
	id := out["record_id"].(string)

	rec, got := do(t, h, "GET", "/creative-works/"+id+"?agent=mingming", "")
	if rec.Code != http.StatusOK || got["status"] != "draft" || got["status_label"] != "待点评" {
		t.Fatalf("GET 作品异常: %d %v", rec.Code, got)
	}

	rec, fb := do(t, h, "POST", "/creative-works/"+id+"/generate-feedback", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || fb["status"] != "feedback_ready" {
		t.Fatalf("点评异常: %d %v", rec.Code, fb)
	}

	rec, rv := do(t, h, "POST", "/creative-works/"+id+"/revision", `{"agent":"mingming","content_markdown":"柳枝像绿色的丝带，风一吹沙沙响。"}`)
	if rec.Code != http.StatusOK || rv["status"] != "revised" {
		t.Fatalf("修改稿异常: %d %v", rec.Code, rv)
	}
	vers, _ := rv["versions"].([]any)
	if len(vers) != 2 {
		t.Fatalf("应有 2 个版本，got %d", len(vers))
	}

	rec, fb2 := do(t, h, "POST", "/creative-works/"+id+"/generate-feedback", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || fb2["status"] != "feedback_ready" {
		t.Fatalf("二次点评异常: %d %v", rec.Code, fb2)
	}
}

// TestCreativeWorkHTTPTypeFilter 列表按类型过滤。
func TestCreativeWorkHTTPTypeFilter(t *testing.T) {
	h := newServer(t)
	do(t, h, "POST", "/creative-works", `{"agent":"mingming","work_type":"writing","title":"作文A","task":"tA","content_markdown":"x"}`)
	do(t, h, "POST", "/creative-works", `{"agent":"mingming","work_type":"art","title":"画作B","task":"tB","source_asset_id":"a1"}`)

	_, all := do(t, h, "GET", "/creative-works?agent=mingming", "")
	_, arts := do(t, h, "GET", "/creative-works?agent=mingming&type=art", "")
	if len(all["items"].([]any)) != 2 || len(arts["items"].([]any)) != 1 {
		t.Fatalf("类型过滤错误: all=%d art=%d", len(all["items"].([]any)), len(arts["items"].([]any)))
	}
}

// TestCreativeWorkHTTPInvalidType 非法类型 4xx。
func TestCreativeWorkHTTPInvalidType(t *testing.T) {
	h := newServer(t)
	rec, _ := do(t, h, "POST", "/creative-works", `{"agent":"mingming","work_type":"math","title":"错","task":"t","content_markdown":"x"}`)
	if rec.Code == http.StatusOK {
		t.Fatal("非法作品类型应被拒")
	}
}
