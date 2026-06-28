package knowledge

import (
	"context"
	"fmt"
)

// 知识库检索评测护栏（#评测）：在固定 golden 语料 + 查询集上量化 recall@k / MRR，
// 用于锁死召回质量、防回归。指标计算是确定性的（可纯单测）；真模型跑分见 *_real_test.go。

// EvalDoc 是 golden 语料中的一篇文档。
type EvalDoc struct {
	Title   string
	Content string
}

// EvalCase 一条评测样本：查询 + 期望命中的（任一）相关文档标题（命中任一即算相关）。
type EvalCase struct {
	Name     string
	Query    string
	Relevant []string
}

// EvalDataset 是评测样本集。
type EvalDataset []EvalCase

// EvalSearcher 是被评测的检索器；*Manager 满足它，测试里也可传 fake 以单测指标逻辑。
type EvalSearcher interface {
	Search(ctx context.Context, query string, topK int) ([]SearchHit, error)
}

// EvalCaseResult 单条样本的评测结果。
type EvalCaseResult struct {
	Name     string
	Query    string
	TopTitle string // top-1 命中的文档标题
	Rank     int    // 首个相关文档的 1-based 排名；0 = top-k 内未命中
}

// EvalReport 聚合评测报告。
type EvalReport struct {
	K         int
	N         int
	RecallAt1 float64 // 首位即相关的比例
	RecallAtK float64 // top-k 内含相关文档的比例
	MRR       float64 // 平均倒数排名
	Cases     []EvalCaseResult
}

// RunEval 在数据集上跑检索，计算 recall@1 / recall@k / MRR。
func RunEval(ctx context.Context, s EvalSearcher, ds EvalDataset, k int) (EvalReport, error) {
	if k <= 0 {
		k = 3
	}
	rep := EvalReport{K: k, N: len(ds)}
	var hit1, hitK, mrrSum float64
	for _, c := range ds {
		hits, err := s.Search(ctx, c.Query, k)
		if err != nil {
			return rep, fmt.Errorf("eval[%s] search: %w", c.Name, err)
		}
		top := ""
		if len(hits) > 0 {
			top = hits[0].DocTitle
		}
		rank := 0
		for i, h := range hits {
			if titleMatches(c.Relevant, h.DocTitle) {
				rank = i + 1
				break
			}
		}
		if rank == 1 {
			hit1++
		}
		if rank > 0 && rank <= k {
			hitK++
		}
		if rank > 0 {
			mrrSum += 1.0 / float64(rank)
		}
		rep.Cases = append(rep.Cases, EvalCaseResult{Name: c.Name, Query: c.Query, TopTitle: top, Rank: rank})
	}
	n := float64(len(ds))
	if n == 0 {
		n = 1
	}
	rep.RecallAt1 = hit1 / n
	rep.RecallAtK = hitK / n
	rep.MRR = mrrSum / n
	return rep, nil
}

func titleMatches(set []string, t string) bool {
	for _, x := range set {
		if x == t {
			return true
		}
	}
	return false
}

// IngestGolden 把 golden 语料灌入 Manager（真模型评测前调用）。
func IngestGolden(ctx context.Context, mgr *Manager) error {
	for _, d := range GoldenCorpus() {
		if _, err := mgr.AddDocument(ctx, d.Title, d.Content, "eval"); err != nil {
			return fmt.Errorf("ingest %q: %w", d.Title, err)
		}
	}
	return nil
}

// GoldenCorpus 是评测用的固定语料（主题语义可分，含相近干扰文档与含拉丁 token 的文档）。
func GoldenCorpus() []EvalDoc {
	return []EvalDoc{
		{"光合作用", "光合作用是绿色植物、藻类利用光能，把二氧化碳和水合成富能有机物并释放氧气的总过程，是地球氧气与能量的最终来源。\n\n" +
			"## 光反应\n光反应发生在叶绿体的类囊体膜上，叶绿素吸收光能，将水分子裂解，产生 ATP 和 NADPH，同时把氧气释放到大气中。\n\n" +
			"## 暗反应\n暗反应又称卡尔文循环，在叶绿体基质中进行，它利用光反应产生的 ATP 与 NADPH，把二氧化碳逐步固定、还原为葡萄糖。"},
		{"细胞呼吸", "细胞呼吸是细胞把葡萄糖等有机物在氧气参与下逐步氧化分解，释放能量并生成二氧化碳和水的过程，主要在线粒体中进行。" +
			"它可视为光合作用的逆过程，为生命活动提供 ATP 能量货币。"},
		{"Go 并发模型", "Go 以 CSP（通信顺序进程）为基础设计并发。goroutine 是运行时调度的轻量级执行单元，启动成本极低。" +
			"goroutine 之间通过 channel 传递数据，遵循以通信共享内存的原则；select 可在多个 channel 上多路复用；sync.Mutex 提供互斥。"},
		{"HexClaw 知识库检索配置", "HexClaw 知识库的检索参数：candidate_k 控制重排前的宽召回候选池大小，默认 50；" +
			"min_score 是向量相关度地板，默认 0.55；rerank 开关控制是否启用 LLM 重排；query_expand 控制 HyDE 与 multi-query；" +
			"contextual 控制入库时是否给 chunk 生成文档级上下文。"},
		{"京杭大运河", "京杭大运河始建于春秋，隋朝贯通，北起北京南到杭州，沟通海河、黄河、淮河、长江、钱塘江五大水系，全长约一千八百公里。"},
		{"Vector Database Indexing", "Vector databases use approximate nearest neighbor (ANN) search to retrieve the closest embeddings to a query. " +
			"The HNSW (Hierarchical Navigable Small World) graph index is the most popular choice for low-latency, high-recall retrieval, " +
			"while IVF (inverted file) clustering trades some recall for lower memory. Brute-force flat search gives exact recall and is fine below ~100k vectors."},
		{"Redis 持久化", "Redis 提供两种持久化方式：RDB 在指定时间点生成内存快照，体积小、恢复快，但可能丢失最后一次快照之后的数据；" +
			"AOF 以追加方式记录每一条写命令，数据更安全但文件更大、恢复更慢。生产环境常同时开启 RDB 与 AOF 取得平衡。"},
		{"HTTP 缓存", "HTTP 缓存通过响应头控制：Cache-Control 指定 max-age 等强缓存策略，ETag 提供资源指纹用于条件请求，" +
			"配合请求头 If-None-Match 实现 304 Not Modified 协商缓存，避免重复传输未变更的资源。"},
		{"维生素C", "维生素C又称抗坏血酸，是一种水溶性维生素，参与胶原蛋白合成、抗氧化与免疫调节。人体无法自行合成，" +
			"需从柑橘、猕猴桃、青椒等新鲜果蔬中摄取；长期严重缺乏会导致坏血病，表现为牙龈出血、伤口难愈。"},
		{"二十四节气", "二十四节气是中国古代依据太阳在黄道上的位置划分的时令体系，从立春开始、到大寒结束，反映季节、物候与农事节律，" +
			"指导播种与收获，2016 年被联合国教科文组织列入人类非物质文化遗产代表作名录。"},
	}
}

// GoldenDataset 是与 GoldenCorpus 配套的查询集（含语义改写/干扰区分/精确术语/精确符号/多chunk/跨语种/中文）。
func GoldenDataset() EvalDataset {
	return EvalDataset{
		{"语义改写", "植物是怎么把太阳能转变成养分、并产生氧气的？", []string{"光合作用"}},
		{"干扰区分", "细胞是如何在线粒体里氧化分解葡萄糖来释放能量的？", []string{"细胞呼吸"}},
		{"精确术语", "candidate_k 这个参数是用来控制什么的？", []string{"HexClaw 知识库检索配置"}},
		{"精确符号", "在 Go 里 goroutine 之间用什么来传递数据？", []string{"Go 并发模型"}},
		{"多chunk定位", "光合作用的光反应阶段具体在叶绿体的哪个结构上发生？", []string{"光合作用"}},
		{"跨语种EN→ZH", "How do plants use sunlight to produce food and release oxygen?", []string{"光合作用"}},
		{"中文运河", "古代中国连通南北、贯穿五大水系的人工水道是什么？", []string{"京杭大运河"}},
		{"数量信息", "京杭大运河全长大约多少公里？", []string{"京杭大运河"}},
		{"缩写CSP", "CSP 通信顺序进程是哪门编程语言的并发模型基础？", []string{"Go 并发模型"}},
		{"跨语种ZH→EN", "向量数据库用哪种图索引来做低延迟、高召回的近似最近邻搜索？", []string{"Vector Database Indexing"}},
		{"英文EN→EN", "What graph index do vector databases use for low-latency approximate nearest neighbor search?", []string{"Vector Database Indexing"}},
		{"定义查询", "什么是卡尔文循环？", []string{"光合作用"}},
		{"时间查询", "京杭大运河最早是什么时候开始修建的？", []string{"京杭大运河"}},
		{"裸关键词HNSW", "HNSW", []string{"Vector Database Indexing"}},
		{"代码符号点", "Go 里 sync.Mutex 是用来做什么的？", []string{"Go 并发模型"}},
		{"缩写ANN", "ANN 近似最近邻搜索一般用什么索引结构？", []string{"Vector Database Indexing"}},
		{"Redis持久化", "Redis 的 RDB 和 AOF 有什么区别？", []string{"Redis 持久化"}},
		{"HTTP协商缓存", "ETag 和 If-None-Match 怎么实现 304 协商缓存？", []string{"HTTP 缓存"}},
		{"营养区分", "哪种维生素长期缺乏会导致坏血病？", []string{"维生素C"}},
		{"口语化", "绿叶子是怎么靠晒太阳来制造养分的？", []string{"光合作用"}},
		{"节气起点", "二十四节气是从哪个节气开始的？", []string{"二十四节气"}},
	}
}

// StressDataset 是硬核压力场景（错别字/否定/极含糊/多跳/跨域混淆/裸词/反向跨语种/歧义），
// 用于探测召回鲁棒性极限——只测量、不做硬断言（部分场景天然难，允许 miss）。
func StressDataset() EvalDataset {
	return EvalDataset{
		{"错别字", "光合做用是怎么放出氧气的？", []string{"光合作用"}},
		{"否定式", "我想知道把二氧化碳变成糖的那个过程，不是细胞呼吸那个", []string{"光合作用"}},
		{"极含糊", "那个绿色叶子晒太阳的事儿", []string{"光合作用"}},
		{"多跳关系", "光合作用和细胞呼吸之间是什么关系？", []string{"光合作用", "细胞呼吸"}},
		{"跨域混淆", "数据库和缓存里，哪个用图索引做检索？", []string{"Vector Database Indexing"}},
		{"英文裸词", "RDB AOF", []string{"Redis 持久化"}},
		{"反向跨语种EN→ZH", "Which traditional Chinese solar-term system was inscribed by UNESCO?", []string{"二十四节气"}},
		{"歧义氧化", "在线粒体里氧化分解葡萄糖、释放能量是在哪进行的？", []string{"细胞呼吸"}},
	}
}
