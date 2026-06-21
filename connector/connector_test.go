package connector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/secret"
)

// newGitHubMock 返回一个模拟 GitHub API：/user 校验 token，/user/repos 返回两个仓库。
func newGitHubMock(t *testing.T, validToken string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	auth := func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer "+validToken
	}
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	})
	mux.HandleFunc("/user/repos", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[{"full_name":"octocat/hello","html_url":"https://github.com/octocat/hello","description":"hi","private":false},{"full_name":"octocat/secret","html_url":"https://github.com/octocat/secret","description":"","private":true}]`))
	})
	return httptest.NewServer(mux)
}

func newStoreWithGitHub(t *testing.T, dir string, box *secret.Box, ghBase string) *Store {
	t.Helper()
	s := NewStore(dir, box)
	s.ghBase = ghBase
	return s
}

func TestStore_AddTestList_Success(t *testing.T) {
	gh := newGitHubMock(t, "tok-good")
	defer gh.Close()
	dir := t.TempDir()
	s := newStoreWithGitHub(t, dir, nil, gh.URL)

	summary, err := s.Add(context.Background(), ProviderGitHub, "我的 GitHub", "tok-good")
	if err != nil {
		t.Fatalf("Add 失败: %v", err)
	}
	if summary.Provider != ProviderGitHub || summary.Name != "我的 GitHub" {
		t.Fatalf("摘要不对: %+v", summary)
	}
	// List 脱敏：Summary 结构本身无 token 字段，断言数量
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("期望 1 个连接器，实际 %d", len(list))
	}
}

func TestStore_Add_InvalidToken(t *testing.T) {
	gh := newGitHubMock(t, "tok-good")
	defer gh.Close()
	s := newStoreWithGitHub(t, t.TempDir(), nil, gh.URL)

	_, err := s.Add(context.Background(), ProviderGitHub, "bad", "tok-WRONG")
	if err == nil {
		t.Fatal("期望无效 token 报错，但成功了")
	}
	if !strings.Contains(err.Error(), "鉴权失败") {
		t.Fatalf("错误信息应指明鉴权失败，实际: %v", err)
	}
	// 不应保存
	if len(s.List()) != 0 {
		t.Fatal("无效 token 不应保存连接器")
	}
}

func TestStore_Add_InvalidProvider(t *testing.T) {
	s := NewStore(t.TempDir(), nil)
	_, err := s.Add(context.Background(), Provider("xiaohongshu"), "x", "tok")
	if err == nil || !strings.Contains(err.Error(), "不支持的数据源") {
		t.Fatalf("期望不支持的数据源报错，实际: %v", err)
	}
}

func TestStore_Resources(t *testing.T) {
	gh := newGitHubMock(t, "tok-good")
	defer gh.Close()
	s := newStoreWithGitHub(t, t.TempDir(), nil, gh.URL)

	summary, err := s.Add(context.Background(), ProviderGitHub, "gh", "tok-good")
	if err != nil {
		t.Fatalf("Add 失败: %v", err)
	}
	res, err := s.Resources(context.Background(), summary.ID)
	if err != nil {
		t.Fatalf("Resources 失败: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("期望 2 个仓库，实际 %d", len(res))
	}
	if res[0].Title != "octocat/hello" || res[1].Kind != "repo·private" {
		t.Fatalf("资源解析不对: %+v", res)
	}
}

func TestStore_TokenEncryptedAtRest(t *testing.T) {
	gh := newGitHubMock(t, "super-secret-token")
	defer gh.Close()
	dir := t.TempDir()
	box, err := secret.LoadBox(dir)
	if err != nil {
		t.Fatalf("LoadBox 失败: %v", err)
	}
	s := newStoreWithGitHub(t, dir, box, gh.URL)

	if _, err := s.Add(context.Background(), ProviderGitHub, "gh", "super-secret-token"); err != nil {
		t.Fatalf("Add 失败: %v", err)
	}

	// 落盘文件里 token 必须是密文，绝不含明文
	raw, err := os.ReadFile(filepath.Join(dir, "connectors.json"))
	if err != nil {
		t.Fatalf("读 connectors.json 失败: %v", err)
	}
	content := string(raw)
	if strings.Contains(content, "super-secret-token") {
		t.Fatal("落盘文件含明文 token —— 加密未生效")
	}
	if !strings.Contains(content, "enc:v1:") {
		t.Fatalf("落盘 token 未加密(无 enc:v1: 前缀): %s", content)
	}
}

func TestStore_PersistReloadDecrypts(t *testing.T) {
	gh := newGitHubMock(t, "tok-good")
	defer gh.Close()
	dir := t.TempDir()
	box, err := secret.LoadBox(dir)
	if err != nil {
		t.Fatalf("LoadBox 失败: %v", err)
	}

	s1 := newStoreWithGitHub(t, dir, box, gh.URL)
	summary, err := s1.Add(context.Background(), ProviderGitHub, "gh", "tok-good")
	if err != nil {
		t.Fatalf("Add 失败: %v", err)
	}

	// 新建 Store 从同目录加载 → token 应被解密回内存，Resources 仍可用
	box2, _ := secret.LoadBox(dir)
	s2 := newStoreWithGitHub(t, dir, box2, gh.URL)
	if len(s2.List()) != 1 {
		t.Fatalf("重载后期望 1 个连接器，实际 %d", len(s2.List()))
	}
	if _, err := s2.Resources(context.Background(), summary.ID); err != nil {
		t.Fatalf("重载后用解密 token 拉资源失败: %v", err)
	}
}

func TestStore_Delete(t *testing.T) {
	gh := newGitHubMock(t, "tok-good")
	defer gh.Close()
	s := newStoreWithGitHub(t, t.TempDir(), nil, gh.URL)
	summary, _ := s.Add(context.Background(), ProviderGitHub, "gh", "tok-good")
	if err := s.Delete(summary.ID); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	if len(s.List()) != 0 {
		t.Fatal("删除后仍有连接器")
	}
	if err := s.Delete("nope"); err == nil {
		t.Fatal("删除不存在的连接器应报错")
	}
}
