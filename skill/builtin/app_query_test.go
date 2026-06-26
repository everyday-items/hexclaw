package builtin

import (
	"context"
	"strings"
	"testing"
)

type fakeQuerier struct{ gotDomain, gotAction, gotID string }

func (f *fakeQuerier) Query(_ context.Context, domain, action, id, _ string) (string, error) {
	f.gotDomain, f.gotAction, f.gotID = domain, action, id
	return "ok", nil
}

// AP-058：action=get 必须带 id（兑现 schema），缺 id 在调 querier 之前就报错；get+id 正常透传。
func TestAppQuery_GetRequiresId(t *testing.T) {
	q := &fakeQuerier{}
	s := NewAppQuerySkill(q)

	if _, err := s.Execute(context.Background(), map[string]any{"domain": "webhooks", "action": "get"}); err == nil {
		t.Fatal("action=get 缺 id 应报错（否则 get 退化成 list = over-promise）")
	}
	if q.gotDomain != "" {
		t.Fatalf("get 缺 id 的校验应在调用 querier 之前，querier 不应被触达: %+v", q)
	}

	if _, err := s.Execute(context.Background(), map[string]any{"domain": "webhooks", "action": "get", "id": "wh1"}); err != nil {
		t.Fatalf("get+id 应成功: %v", err)
	}
	if q.gotAction != "get" || q.gotID != "wh1" {
		t.Fatalf("action/id 未如实透传给 querier: %+v", q)
	}
}

// F2：domain 缺失的错误文案由单一事实源 AppQueryDomains 派生，必须列出**全部** 8 个 domain（含 logs）。
func TestAppQuery_DomainRequiredError_ListsAllDomains(t *testing.T) {
	s := NewAppQuerySkill(&fakeQuerier{})
	_, err := s.Execute(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("缺 domain 应报错")
	}
	for _, d := range AppQueryDomains {
		if !strings.Contains(err.Error(), d) {
			t.Fatalf("domain-required 错误应列出全部 domain（缺 %q）: %v", d, err)
		}
	}
	// 防回归：logs/workflows 曾被漏列
	if !strings.Contains(err.Error(), "logs") || !strings.Contains(err.Error(), "workflows") {
		t.Fatalf("错误文案漏列 logs/workflows: %v", err)
	}
}

// F2：schema Enum 由 AppQueryDomains 单一源派生（domainEnumAny），二者必须等集等序。
func TestAppQuery_SchemaEnumDerivedFromSource(t *testing.T) {
	got := domainEnumAny()
	if len(got) != len(AppQueryDomains) {
		t.Fatalf("Enum 长度 %d ≠ 源 %d", len(got), len(AppQueryDomains))
	}
	for i, d := range AppQueryDomains {
		if got[i] != any(d) {
			t.Fatalf("Enum[%d]=%v ≠ 源 %q（单一事实源漂移）", i, got[i], d)
		}
	}
}
