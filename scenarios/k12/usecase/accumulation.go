package usecase

import (
	"context"
	"fmt"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// AccumItem 积累本条目（记录 + 领域字段）。
type AccumItem struct {
	Record *records.AgentRecord
	Fields k12.AccumFields
}

// AddAccumulation 往积累本写一条（语文/英语沉淀，幂等去重）。
// **纠错型**（默写错/错词/语法改错）设首次复习到期 → 进统一复习队列（与错题本同飞轮，PRD §3.5.4）；
// 积累/留档型不设到期。
func (d Deps) AddAccumulation(ctx context.Context, agentName, sourceSession string, f k12.AccumFields) (recordID string, created bool, err error) {
	rec, err := k12.NewAccumRecord(agentName, sourceSession, f)
	if err != nil {
		return "", false, err
	}
	if k12.AccumIsCorrective(f.EntryType) {
		due := d.now() + FirstReviewInterval
		rec.DueAt = &due
	}
	created, err = d.Records.Put(ctx, rec)
	if err != nil {
		return "", false, fmt.Errorf("usecase: 积累入库: %w", err)
	}
	return rec.RecordID, created, nil
}

// ListAccumulation 列积累本（可按学科过滤；subject 空 = 全部）。
func (d Deps) ListAccumulation(ctx context.Context, agentName, subject string) ([]AccumItem, error) {
	recs, err := d.Records.ListByScope(ctx, agentName, k12.CollectionAccumulation, "")
	if err != nil {
		return nil, fmt.Errorf("usecase: 列积累本: %w", err)
	}
	out := make([]AccumItem, 0, len(recs))
	for _, r := range recs {
		f, _ := k12.ParseAccumFields(r.Fields)
		if subject != "" && f.Subject != subject {
			continue
		}
		out = append(out, AccumItem{Record: r, Fields: f})
	}
	return out, nil
}
