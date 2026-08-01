package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 外部搜索输入必须在进入 RAG 扩展、向量检索和重排之前受预算约束；否则单一请求可将
// candidate pool 与辅助模型输入放大到不受控的规模。
func TestHandleSearchKnowledge_RejectsUnboundedRequestBudget(t *testing.T) {
	srv, _ := newKBConfigServer(t)

	for name, body := range map[string]string{
		"top_k": `{"query":"algebra","top_k":51}`,
		"query": `{"query":"` + strings.Repeat("代", 4097) + `","top_k":3}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/search", strings.NewReader(body))
			w := httptest.NewRecorder()
			srv.handleSearchKnowledge(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("超出检索预算应返回 400，得 %d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
