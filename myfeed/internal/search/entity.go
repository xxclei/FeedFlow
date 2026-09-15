// Package search 混合检索：词法（MySQL FULLTEXT + ngram）和语义（Redis 向量 KNN）
// 两路召回，用 RRF 融合成一个全序列表。
//
// ---------- 为什么这个包没有表、没有 HTTP ----------
//
// 它是一个 **provider 包**：不拥有任何表（向量的家在 Redis，词法查询打的是
// videos 表）、也不注册路由。HTTP 入口挂在 internal/feed 的 /feed/search ——
// 理由是响应格式：搜索结果必须复用 feed 的 buildFeedVideos，
// 否则 is_liked 就得在这里重写一遍（还会漏掉"匿名用户降级"那条已经写好的路径）。
//
// 依赖方向：feed → search → video，不成环。
//
// ---------- 为什么两路都要，而不是选一路 ----------
//
// 两路的**失效方式互补**，这才是整个设计的存在理由：
//
//	词法（BM25/ngram）  精确命中稀有串（用户名、型号、"红烧肉"），
//	                    但只会字面匹配 —— 搜"做菜"找不到写"烹饪"的
//	向量（余弦）        懂语义相近，但会把稀有词压糊（在只有 127 条的语料上尤其明显）
//
// 业界在 Elasticsearch 上的实测：BM25 单路 Recall@10 = 0.624，纯 kNN = 0.712，
// **RRF 融合 = 0.841**。融合那一步拿走了大部分收益，而延迟只翻一倍。
//
// ---------- 为什么融合必须在 Go 里做 ----------
//
// 两路分别活在 **MySQL 和 Redis 两个不同的引擎**里，没有任何一个引擎能同时
// 看到两边。顺带一个业界脚注：新版 Redis 有 FT.HYBRID 可以做服务端"向量 + 文本 + 融合"，
// 但它要求两路**都在 Redis 里**，所以解决不了这个问题。
//
// ---------- 本轮状态：只接了词法那一路 ----------
//
// 上面写的两路是**设计**，不是当前的运行形态。本轮向量那一路先跳过
// （Redis Stack 换镜像 + Ollama 拉模型 + 回填命令是一整套运维动作），
// router.go 里 `NewService(lex, nil, nil, nil, cfg)` 那三个 nil 就是它没接的证据。
//
// 具体哪些东西是"写好了但没通电"的：
//
//	embed.go        OllamaEmbedder        没有任何构造点
//	vector_repo.go  VectorStore           没有任何构造点
//	service.go      vectorArm/IndexVideo  前者因 vec/embed 为 nil 而永不执行，
//	                                      后者因 IndexVideo 不在 VectorIndexer 的
//	                                      接线路径上（video 侧传的也是 nil）而无人调用
//
// **保留而不是删掉**，是因为接线时这三处一行都不用改逻辑，只是把 nil 换成真实实现；
// 而删掉就等于把"向量存哪、距离还是相似度、DIALECT 2、LIMIT 默认 10、
// 先写 Redis 再写标记"这些踩过的坑重新踩一遍。代价是本轮树里有几处没有读者的代码，
// 所以每个文件的开头都标着它现在的状态。
//
// 融合那一步（fuse.go）同样保留：**单路喂进 FuseRRF 的结果恒等于该路的名次**
// （1/(k+rank) 随 rank 严格递减），所以留在这里既不影响本轮的排序，
// 又让接上第二路时不需要改任何调用点 —— 这是 RRF "只看名次"这个性质的红利。
package search

import "fmt"

// Mode 本次搜索实际跑成的形态。**必须如实回给前端**。
//
// 理由和 /feed/listByPopularity 返回 as_of: 0 是同一条：降级本身不是问题，
// 悄悄降级才是 —— 用户以为"搜不出来是内容没有"，实际是索引没建好。
// 前端把它显示在调试面板上，"搜索变差了"才能一眼看出是哪一路塌了。
type Mode string

const (
	// ModeNgram 两路都在，词法那一路用 AND（每个 token 都必须出现）
	ModeNgram Mode = "ngram"
	// ModeNgramOr 两路都在，但 AND 的命中数不够，词法退成了 OR
	ModeNgramOr Mode = "ngram-or"
	// ModeLike 两路都在，但查询词太短（< ngram_token_size），词法退成 LIKE
	ModeLike Mode = "like"
	// ModeLikeFallback 两路都在，但**全文索引连一条都没命中**，词法退成 LIKE 兜底。
	//
	// 和 ModeLike 分开报，是因为这两种"跑了 LIKE"的排查方向完全不同：
	//
	//	ModeLike          查询词太短，索引里根本没有这么短的 token —— **预期行为**，
	//	                  单字搜索靠的就是它，没什么可修的
	//	ModeLikeFallback  查询词够长却零命中 —— **索引本身可疑**：
	//	                  停用词把 n-gram 削掉了、ngram_token_size 被人改过、
	//	                  或者索引压根没重建
	//
	// 换句话说它是"搜视频搜不到"这类 bug 的**仪表盘读数**。这正是 Mode 存在的理由
	// （见上面那段）：悄悄降级才是问题，降级了还说一声就不是。
	ModeLikeFallback Mode = "like-fallback"
	// ModeLexicalOnly 向量那一路不可用（Redis 或 Ollama 挂了 / 没接）
	ModeLexicalOnly Mode = "lexical-only"
	// ModeVectorOnly 词法那一路不可用（FULLTEXT 索引没建成功）
	ModeVectorOnly Mode = "vector-only"
)

// Ranked 一路召回里的一个结果。
//
// **名次就是它在切片里的下标**，这是刻意的：RRF 只用名次、不用分数，
// 所以把分数当成"必须原样保真的东西"是没有意义的（它连量纲都不一致：
// 词法那一路是 BM25 相关度、无上界；语义那一路是余弦**距离**、在 [0,2]）。
// 分数留下来只为一件事：日志和调试时能看出"这一路是不是全都无关"。
type Ranked struct {
	ID    uint
	Score float64
}

// RankedList 一路召回的完整结果，**已按该路的相关度排好序**。
type RankedList []Ranked

// IDs 抽出 id 序列（融合只关心这个）
func (l RankedList) IDs() []uint {
	out := make([]uint, 0, len(l))
	for _, r := range l {
		out = append(out, r.ID)
	}
	return out
}

// Arms 两路各召回了多少条。给调试面板用。
//
// 单独一个结构而不是两个 int，是因为它要**被塞进游标**（见 cursor.go）：
// 翻到第二页时召回根本没跑（用的是冻结列表），不带上它，
// 调试面板的数字会在翻页时突然变成 0，看起来像"第二页有一路塌了"。
type Arms struct {
	Lexical int `json:"lexical"`
	Vector  int `json:"vector"`
}

// Result 一次搜索的结果。
type Result struct {
	IDs  []uint // 融合后的**完整**有序 id 列表（本页要的那一段已经在 service 里切好）
	Mode Mode
	Arms Arms
	// Total 冻结列表的总长度。不是"命中总数"（那需要精确计数），
	// 而是"这次搜索一共产出了多少条可翻的候选" —— 分页指示器用得上。
	Total int
}

// Config 检索参数。默认值在 withDefaults 里，配置缺失时兜住。
type Config struct {
	// Depth 每路召回的条数（融合前）。这是"召回深度"：
	// 调大 → 融合的候选更多、质量更好，代价是两路各多查一批。
	// 和 limit（每页展示几条）是两件事：depth 决定候选池，limit 决定切多少。
	Depth int

	// RRFK RRF 公式里的 k。60 是 Cormack 2009 那篇论文的经验值，
	// 至今仍是各家默认（它起的作用是压低"第 1 名"的权重优势，
	// 让两路的中段结果也有机会往上走）。
	RRFK int

	// MaxDistance 语义那一路的**余弦距离**上限，超过的直接丢掉。
	//
	// 为什么必须有这个门槛：**RRF 丢掉了分数的量级**，只看名次。
	// 所以一路"最不坏的无关结果"也能拿到完整的 1/(k+rank) 分 ——
	// 在小语料（本项目 127 条）上这个污染特别明显：向量那一路总能凑满 k 个
	// "最近"的结果，哪怕它们最近的也有 0.9 的距离。
	//
	// 阈值的量纲要注意：RediSearch 返回的是**距离**（越小越像），不是相似度。
	// bge-m3 的输出已经被 Ollama 做过 L2 归一化，所以余弦距离 = 1 - 余弦相似度。
	// 0.6 大致对应"余弦相似度 0.4 以上"，是个偏松的起点；
	// 真正的调参需要一组金标数据（本轮没有，见 PROGRESS 的"本轮不做"）。
	MaxDistance float64
}

// withDefaults 补默认值。
//
// 不 panic 也不报错的理由：这三个值调错了不会让结果**错**，
// 只会让结果**差**（少召回几条、或者多混进几条无关的）。
// 为"参数没填"让整个服务起不来是不划算的 —— 和 db.go 里
// "全文索引建不出来只记日志"是同一条取舍。
func (c Config) withDefaults() Config {
	if c.Depth <= 0 {
		c.Depth = 50
	}
	if c.RRFK <= 0 {
		c.RRFK = 60
	}
	if c.MaxDistance <= 0 {
		c.MaxDistance = 0.6
	}
	return c
}

// UnsearchableError 查询里连一个字母/数字都没有（用户只打了标点或空白）。
//
// 是**请求形状问题**，不是服务端故障，所以 handler 要把它翻成 400。
// 单独一个类型而不是一个 sentinel error，是为了让 400 的文案能带上原始输入。
type UnsearchableError struct {
	Raw string
}

func (e *UnsearchableError) Error() string {
	return fmt.Sprintf("查询里没有可检索的词（至少要有一个字母或数字）: %q", e.Raw)
}
