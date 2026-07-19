package apihttp_test

// 点评发送出口 + 观察练习卡 HTTP 契约（§3.10 / §3.12，RED 先行）：
//   1. POST /creative-works/{id}/send-feedback 走注入的 IMDeliverer：内容含标题+点评正文；
//      practice_card 类别发提炼后的练习卡文本；
//   2. 未接线（Deliver=nil）→ 501 诚实降级（前端复制文本兜底）；未绑定/发送失败 → 409；
//   3. 无点评的作品 → 409；跨 agent → 404；
//   4. 美术版本 DTO 派生 practice_card（服务端提炼，单一事实源）；
//   5. POST /creative-works/{id}/practice-card/done 打卡幂等（保留首次时间）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

// fakeDeliverer 记录 Receipt-first DeliveryTransport 的冻结内容。
type fakeDeliverer struct {
	contents []string
	err      error
}

func (f *fakeDeliverer) PrepareText(_ context.Context, agentName, content string) (usecase.PreparedTextDelivery, error) {
	if f.err != nil {
		return usecase.PreparedTextDelivery{}, f.err
	}
	f.contents = append(f.contents, agentName+"|"+content)
	payload, _ := json.Marshal(map[string]string{"text": content})
	return usecase.PreparedTextDelivery{
		BindingID:   "agent-rule:1",
		Target:      k12.DeliveryTarget{Platform: "dingtalk", ChatID: "staff-1", Label: "钉钉 · 妈妈"},
		PayloadJSON: string(payload), RenderJSON: `{}`,
	}, nil
}

func (f *fakeDeliverer) SendPrepared(_ context.Context, _ k12.DeliveryReceipt) (usecase.DeliveryTransportAck, error) {
	return usecase.DeliveryTransportAck{Status: k12.DeliverySending, ExternalMessageID: "pqk-test"}, nil
}

func (f *fakeDeliverer) QueryPrepared(_ context.Context, _ k12.DeliveryReceipt) (usecase.DeliveryTransportAck, error) {
	return usecase.DeliveryTransportAck{Status: k12.DeliveryDelivered, ExternalMessageID: "pqk-test"}, nil
}

func newServerWithDeliverer(t *testing.T, d usecase.DeliveryTransport) http.Handler {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO agents(name) VALUES('mingming')`)
	k, err := assembly.Wire(db, fakeSolveExec{}, assembly.WithDeliveryTransport(d))
	if err != nil {
		t.Fatal(err)
	}
	return apihttp.NewHandler(apihttp.Runtime{Views: k.Registry.Views, Records: k.Records, Deps: k.Deps})
}

// mkArtWorkWithFeedback 建一件美术作品并附点评，返回 record_id。
func mkArtWorkWithFeedback(t *testing.T, h http.Handler, feedback string) string {
	t.Helper()
	rec, out := do(t, h, "POST", "/creative-works",
		`{"agent":"mingming","work_type":"art","title":"雨后的校园","task":"写生","source_asset_id":"a1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("建作品失败: %d %v", rec.Code, out)
	}
	id := out["record_id"].(string)
	rec, fb := do(t, h, "POST", "/creative-works/"+id+"/feedback",
		fmt.Sprintf(`{"agent":"mingming","feedback":%q}`, feedback))
	if rec.Code != http.StatusOK {
		t.Fatalf("附点评失败: %d %v", rec.Code, fb)
	}
	return id
}

const artFeedback = "我看到画面主体清楚。\n## 建议\n- 试试只用三档明暗再画一张小稿。\n- 比一比哪张更亮。"

func TestSendWorkFeedback_DeliversViaSeam(t *testing.T) {
	fd := &fakeDeliverer{}
	h := newServerWithDeliverer(t, fd)
	id := mkArtWorkWithFeedback(t, h, artFeedback)

	rec, out := do(t, h, "POST", "/creative-works/"+id+"/send-feedback", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || out["status"] != "sending" {
		t.Fatalf("发送点评应 200: %d %v", rec.Code, out)
	}
	target, _ := out["target"].(map[string]any)
	if target["label"] != "钉钉 · 妈妈" {
		t.Fatalf("应回显投递目标, got %v", out["target"])
	}
	if len(fd.contents) != 1 || !strings.Contains(fd.contents[0], "《雨后的校园》点评要点") ||
		!strings.Contains(fd.contents[0], "三档明暗") {
		t.Fatalf("投递内容应含标题+点评正文, got %v", fd.contents)
	}

	// 练习卡类别：发提炼后的建议段。
	rec, _ = do(t, h, "POST", "/creative-works/"+id+"/send-feedback", `{"agent":"mingming","kind":"practice_card"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("发送练习卡应 200: %d", rec.Code)
	}
	last := fd.contents[len(fd.contents)-1]
	if !strings.Contains(last, "观察小练习") || !strings.Contains(last, "试试只用三档明暗") {
		t.Fatalf("练习卡内容应为提炼建议段, got %q", last)
	}
	if strings.Contains(last, "画面主体清楚") {
		t.Fatalf("练习卡不应混入非建议段, got %q", last)
	}
}

func TestSendWorkFeedback_HonestDegrade(t *testing.T) {
	// 未接线 → 501。
	h := newServer(t)
	id := mkArtWorkWithFeedback(t, h, artFeedback)
	rec, _ := do(t, h, "POST", "/creative-works/"+id+"/send-feedback", `{"agent":"mingming"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("未接线应 501 诚实降级, got %d", rec.Code)
	}

	// 已接线但未绑定 → 409（deliverer 报错透传家长向文案）。
	h2 := newServerWithDeliverer(t, &fakeDeliverer{err: fmt.Errorf("这个辅导助手还没绑定手机私聊")})
	id2 := mkArtWorkWithFeedback(t, h2, artFeedback)
	rec2, out2 := do(t, h2, "POST", "/creative-works/"+id2+"/send-feedback", `{"agent":"mingming"}`)
	if rec2.Code != http.StatusConflict || !strings.Contains(out2["error"].(string), "还没绑定") {
		t.Fatalf("未绑定应 409+家长向文案, got %d %v", rec2.Code, out2)
	}
}

func TestSendWorkFeedback_RequiresFeedbackAndOwnership(t *testing.T) {
	fd := &fakeDeliverer{}
	h := newServerWithDeliverer(t, fd)
	rec, out := do(t, h, "POST", "/creative-works",
		`{"agent":"mingming","work_type":"art","title":"无点评","task":"t","source_asset_id":"a1"}`)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	id := out["record_id"].(string)
	if rec, _ := do(t, h, "POST", "/creative-works/"+id+"/send-feedback", `{"agent":"mingming"}`); rec.Code != http.StatusConflict {
		t.Fatalf("无点评应 409, got %d", rec.Code)
	}
	if rec, _ := do(t, h, "POST", "/creative-works/"+id+"/send-feedback", `{"agent":"other"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("跨 agent 应 404, got %d", rec.Code)
	}
}

func TestPracticeCard_DTODerivedAndDoneIdempotent(t *testing.T) {
	h := newServer(t)
	id := mkArtWorkWithFeedback(t, h, artFeedback)

	_, got := do(t, h, "GET", "/creative-works/"+id+"?agent=mingming", "")
	vers := got["versions"].([]any)
	v0 := vers[0].(map[string]any)
	card, _ := v0["practice_card"].(string)
	if !strings.Contains(card, "试试只用三档明暗") {
		t.Fatalf("美术版本 DTO 应派生 practice_card, got %v", v0)
	}
	if _, has := v0["practice_card_done_at"]; has {
		t.Fatalf("未打卡不应有 practice_card_done_at, got %v", v0)
	}

	rec, done := do(t, h, "POST", "/creative-works/"+id+"/practice-card/done", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("打卡应 200: %d %v", rec.Code, done)
	}
	dv := done["versions"].([]any)[0].(map[string]any)
	first, _ := dv["practice_card_done_at"].(float64)
	if first <= 0 {
		t.Fatalf("打卡应写完成时间, got %v", dv)
	}
	// 幂等：重复打卡保留首次时间。
	_, done2 := do(t, h, "POST", "/creative-works/"+id+"/practice-card/done", `{"agent":"mingming"}`)
	dv2 := done2["versions"].([]any)[0].(map[string]any)
	if second, _ := dv2["practice_card_done_at"].(float64); second != first {
		t.Fatalf("重复打卡应保留首次时间: %v vs %v", second, first)
	}

	// 写作作品无练习卡：打卡应被拒。
	_, w := do(t, h, "POST", "/creative-works", `{"agent":"mingming","work_type":"writing","title":"作文","task":"t","content_markdown":"x"}`)
	wid := w["record_id"].(string)
	if rec, _ := do(t, h, "POST", "/creative-works/"+wid+"/practice-card/done", `{"agent":"mingming"}`); rec.Code == http.StatusOK {
		t.Fatal("写作作品不应有观察练习卡打卡")
	}
}
