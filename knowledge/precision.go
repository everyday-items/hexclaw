package knowledge

import (
	"context"
	"fmt"
)

// 检索精确率 / 抗噪评测（KB 深度质量门 #2）。
//
// 既有评测重 recall（相关文档在不在 top-k），轻 precision（top-k 里混进了多少不相关
// 的"近义干扰"文档）。本文件补 precision@k：在含 distractor（话题相邻但答案错误）的
// 语料上量化 top-k 的纯度——干扰泄漏越多 precision 越低。
//
// precision@k = top-k 中相关文档数 / 实际返回数（≤k）；越高越好。
// 配套 DistractorCorpus/PrecisionDataset：每个查询有 ≥k 个真正相关文档 + 若干话题相邻
// 的干扰文档，使 precision@k 有意义且可设阈值护栏。
//
// MinScore 地板对"纯弱向量噪声"的拦截见 precision_test.go 的确定性 scripted-embedder 测试
// （floor on/off 对照），那是模型无关逻辑，确定性可测。

// PrecisionCase 一条精确率样本：查询 + 全部相关文档标题集合（用于判定 top-k 命中纯度）。
type PrecisionCase struct {
	Name     string
	Query    string
	Relevant []string // 该查询的"全部"相关文档标题（其余皆视为干扰）
}

// PrecisionDatasetT 是精确率样本集。
type PrecisionDatasetT []PrecisionCase

// PrecisionCaseResult 单条样本的精确率结果。
type PrecisionCaseResult struct {
	Name       string
	PrecisionK float64  // 本例 precision@k
	Retrieved  int      // 实际返回数（≤k）
	RelevantN  int      // 其中相关数
	Leaked     []string // 混入 top-k 的干扰文档标题（取证）
}

// PrecisionReport 聚合精确率报告。
type PrecisionReport struct {
	K           int
	N           int
	MeanPrecK   float64 // 各样本 precision@k 的均值
	TotalLeaked int     // 全样本 top-k 里干扰文档出现总次数
	Cases       []PrecisionCaseResult
}

// RunPrecisionEval 在数据集上跑检索，计算 precision@k（top-k 纯度）。
func RunPrecisionEval(ctx context.Context, s EvalSearcher, ds PrecisionDatasetT, k int) (PrecisionReport, error) {
	if k <= 0 {
		k = 3
	}
	rep := PrecisionReport{K: k, N: len(ds)}
	var pSum float64
	for _, c := range ds {
		hits, err := s.Search(ctx, c.Query, k)
		if err != nil {
			return rep, fmt.Errorf("precision[%s] search: %w", c.Name, err)
		}
		if len(hits) > k {
			hits = hits[:k]
		}
		relN := 0
		var leaked []string
		for _, h := range hits {
			if titleMatches(c.Relevant, h.DocTitle) {
				relN++
			} else {
				leaked = append(leaked, h.DocTitle)
			}
		}
		var p float64
		if len(hits) > 0 {
			p = float64(relN) / float64(len(hits))
		}
		pSum += p
		rep.TotalLeaked += len(leaked)
		rep.Cases = append(rep.Cases, PrecisionCaseResult{
			Name: c.Name, PrecisionK: p, Retrieved: len(hits), RelevantN: relN, Leaked: leaked,
		})
	}
	n := float64(len(ds))
	if n == 0 {
		n = 1
	}
	rep.MeanPrecK = pSum / n
	return rep, nil
}

// DistractorCorpus 是精确率/抗噪语料：两组话题，每组含若干"相关"文档 + 话题相邻的"干扰"
// 文档（festival 与 holiday 同域、relational DB 与 cache/MQ 同域），逼真考验 top-k 纯度。
func DistractorCorpus() []EvalDoc {
	return []EvalDoc{
		// —— 组 A：中国传统节日（相关）vs 西方节日（话题相邻干扰）——
		{"春节", "春节是中国农历新年，最重要的传统节日，家家贴春联、放鞭炮、吃年夜饭、给压岁钱，象征辞旧迎新、阖家团圆。"},
		{"端午节", "端午节是中国传统节日，农历五月初五，为纪念屈原，有吃粽子、赛龙舟、挂艾草的习俗。"},
		{"中秋节", "中秋节是中国传统节日，农历八月十五，象征团圆，有赏月、吃月饼、提灯笼的习俗。"},
		{"清明节", "清明节是中国传统节日，既是节气也是祭祖扫墓、踏青郊游的日子，表达对先人的追思。"},
		{"圣诞节", "圣诞节是西方基督教节日，每年 12 月 25 日纪念耶稣诞生，有圣诞树、圣诞老人、互赠礼物的习俗。"},
		{"感恩节", "感恩节是源自北美的西方节日，家人团聚吃火鸡、南瓜派，表达对一年收获的感恩。"},
		{"万圣节", "万圣节是西方节日，10 月 31 日，孩子们装扮成鬼怪挨家讨糖，有南瓜灯（杰克灯）的习俗。"},
		// —— 组 B：关系型数据库（相关）vs 缓存/消息队列（话题相邻干扰）——
		{"PostgreSQL", "PostgreSQL 是开源的关系型数据库，支持 ACID 事务、丰富的 SQL 标准、外键约束、MVCC 多版本并发控制与复杂查询。"},
		{"MySQL", "MySQL 是流行的开源关系型数据库，使用 SQL，支持事务（InnoDB 引擎）、主从复制，广泛用于 Web 应用。"},
		{"SQLite", "SQLite 是嵌入式关系型数据库，零配置、单文件、支持标准 SQL 与事务，常用于本地与移动端存储。"},
		{"Redis", "Redis 是内存键值存储，常作缓存与会话存储，支持字符串、哈希、列表等数据结构与发布订阅，不是关系型数据库。"},
		{"Kafka", "Kafka 是分布式消息队列 / 流处理平台，以高吞吐的发布订阅与日志分区著称，用于事件流，不是关系型数据库。"},
	}
}

// PrecisionDataset 是与 DistractorCorpus 配套的查询集：每个查询有 ≥3 个相关文档，
// 干扰文档话题相邻——precision@3 度量 top-3 是否被干扰污染。
func PrecisionDataset() PrecisionDatasetT {
	return PrecisionDatasetT{
		{"中国传统节日", "列举几个中国的传统节日及其习俗", []string{"春节", "端午节", "中秋节", "清明节"}},
		{"关系型数据库", "有哪些常见的关系型数据库（支持 SQL 与事务）？", []string{"PostgreSQL", "MySQL", "SQLite"}},
	}
}
