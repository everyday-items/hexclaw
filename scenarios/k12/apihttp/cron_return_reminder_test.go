package apihttp_test

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

// GET /cron/return-reminder 契约（§3.13 回传提醒）：昨日固化未回传 → 有文案（含
// paper_no）；重复调用（cron 重触发）→ 第二次空 body（每卷最多一次）；缺 agent → 400。
func TestCronReturnReminder_Endpoint(t *testing.T) {
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	finalizeAt := time.Date(2026, 7, 16, 15, 0, 0, 0, loc)
	remindAt := time.Date(2026, 7, 17, 20, 0, 0, 0, loc)

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO agents(name) VALUES('mingming')`)
	k, err := assembly.Wire(db, fakeSolveExec{})
	if err != nil {
		t.Fatal(err)
	}
	now := finalizeAt.Unix() // 可变时钟：先固化（昨日），再提醒（今日）
	k.Deps.Now = func() int64 { return now }

	ctx := context.Background()
	f := k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceWeekly, Title: "端点契约卷",
		Items: []k12.PracticeItem{{
			ItemID: "q1", QuestionMarkdown: "2.8 × 0.65 = ?", ExpectedAnswerMarkdown: "1.82",
			VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算",
		}},
	}
	id, _, err := k.Deps.CreatePracticeSet(ctx, "mingming", "s1", f)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := k.Deps.FinalizeBasket(ctx, "mingming", id, "print", "")
	if err != nil {
		t.Fatal(err)
	}

	h := apihttp.NewHandler(apihttp.Runtime{Views: k.Registry.Views, Records: k.Records, Deps: k.Deps})
	now = remindAt.Unix()

	code, body := getText(t, h, "/cron/return-reminder?agent=mingming")
	if code != 200 {
		t.Fatalf("return-reminder 状态 %d", code)
	}
	if !strings.Contains(body, v.Fields.PaperNo) {
		t.Errorf("提醒应含卷面号 %s, got %q", v.Fields.PaperNo, body)
	}
	// cron 重触发：每卷最多提醒一次 → 第二次空 body（脚本静默跳过）。
	code, body = getText(t, h, "/cron/return-reminder?agent=mingming")
	if code != 200 || strings.TrimSpace(body) != "" {
		t.Errorf("第二次调用应空 body, got code=%d body=%q", code, body)
	}

	code, _ = getText(t, h, "/cron/return-reminder")
	if code != http.StatusBadRequest {
		t.Errorf("缺 agent 应 400, got %d", code)
	}
}
