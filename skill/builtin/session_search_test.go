package builtin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/storage"
)

type fakeSearcher struct {
	results  []*storage.SearchResult
	total    int
	gotQuery string
	gotLimit int
	gotUser  string
}

func (f *fakeSearcher) SearchMessages(_ context.Context, userID, query string, limit, _ int) ([]*storage.SearchResult, int, error) {
	f.gotUser, f.gotQuery, f.gotLimit = userID, query, limit
	return f.results, f.total, nil
}

func TestSessionSearch_FormatsResults(t *testing.T) {
	fs := &fakeSearcher{
		total: 2,
		results: []*storage.SearchResult{
			{SessionTitle: "部署讨论", Rank: 1, Message: &storage.MessageRecord{
				Role: "assistant", Content: "上次的部署方案是用 Kubernetes…", CreatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}},
		},
	}
	s := NewSessionSearchSkill(fs)
	res, err := s.Execute(context.Background(), map[string]any{"query": "部署", "limit": float64(3)})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if fs.gotQuery != "部署" || fs.gotLimit != 3 {
		t.Fatalf("透传错误: query=%q limit=%d", fs.gotQuery, fs.gotLimit)
	}
	if !strings.Contains(res.Content, "部署讨论") || !strings.Contains(res.Content, "助手") || !strings.Contains(res.Content, "Kubernetes") {
		t.Fatalf("结果格式不含会话标题/角色/正文: %q", res.Content)
	}
}

func TestSessionSearch_LimitClampAndEmptyQuery(t *testing.T) {
	fs := &fakeSearcher{}
	s := NewSessionSearchSkill(fs)
	// limit 超上限 → 钳到 20
	if _, err := s.Execute(context.Background(), map[string]any{"query": "x", "limit": float64(999)}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if fs.gotLimit != 20 {
		t.Fatalf("limit 应钳到 20，得 %d", fs.gotLimit)
	}
	// 空 query → 报错
	if _, err := s.Execute(context.Background(), map[string]any{"query": "  "}); err == nil {
		t.Fatal("空 query 应报错")
	}
}

func TestSessionSearch_NoResults(t *testing.T) {
	s := NewSessionSearchSkill(&fakeSearcher{total: 0})
	res, err := s.Execute(context.Background(), map[string]any{"query": "不存在的关键词"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(res.Content, "没有找到") {
		t.Fatalf("无结果应友好提示，得 %q", res.Content)
	}
}
