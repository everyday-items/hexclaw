package usecase

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// minutesPerRecord 每条记录（一道批改题/一条积累）估算的辅导分钟数。
const minutesPerRecord = 15

// maxMinutesPerDay 单日估算上限（防长会话高估）。
const maxMinutesPerDay = 120

// StudyDay 某日学习时长。
type StudyDay struct {
	Date             string `json:"date"` // YYYY-MM-DD
	RecordCount      int    `json:"record_count"`
	EstimatedMinutes int    `json:"estimated_minutes"`
}

// StudyTime 学习时长统计（日粒度 + 汇总）。
type StudyTime struct {
	Days         []StudyDay `json:"days"`
	TotalRecords int        `json:"total_records"`
	TotalMinutes int        `json:"total_minutes"`
	Note         string     `json:"note"`
}

// StudyTime 计算学习时长（**近似**）。
//
// 说明：PRD §5.4.4 精确算法要求"相邻消息间隔<10min 累计"，但消息数据在平台层、与 K12 实例
// 无直查映射、且不带学科（AP-1 隔离）。故本实现走 K12 自有 records 时间戳做 AP-1-clean 近似：
// 按自然日聚合记录活跃，每条记录估 15 分钟、单日封顶 120 分钟。精确消息级时长待平台侧接口就绪。
func (d Deps) StudyTime(ctx context.Context, agentName string) (StudyTime, error) {
	if agentName == "" {
		return StudyTime{}, fmt.Errorf("usecase: agentName 不可空")
	}
	recs, err := d.Records.ExportAgent(ctx, agentName)
	if err != nil {
		return StudyTime{}, fmt.Errorf("usecase: 聚合学习时长: %w", err)
	}
	byDay := map[string]int{}
	for _, r := range recs {
		day := time.Unix(r.CreatedAt, 0).Format("2006-01-02")
		byDay[day]++
	}
	st := StudyTime{
		TotalRecords: len(recs),
		Note:         "近似值：基于错题/积累记录活跃估算（非消息间隔精确算法，见 §5.4.4）",
	}
	for day, cnt := range byDay {
		mins := cnt * minutesPerRecord
		if mins > maxMinutesPerDay {
			mins = maxMinutesPerDay
		}
		st.Days = append(st.Days, StudyDay{Date: day, RecordCount: cnt, EstimatedMinutes: mins})
		st.TotalMinutes += mins
	}
	sort.Slice(st.Days, func(i, j int) bool { return st.Days[i].Date < st.Days[j].Date })
	return st, nil
}
