package search

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
)

const (
	// vectorIndexName RediSearch 索引名
	vectorIndexName = "idx:video_vec"
	// vectorKeyPrefix 向量文档的键前缀。
	// **必须和 FTCreate 里声明的 PREFIX 完全一致**（含那个结尾的冒号）——
	// 不一致的表现是索引里有 0 个文档、搜索永远返回空集，且不报任何错
	vectorKeyPrefix = "vec:video:"
	// vectorFieldName 存向量的哈希字段名。常量而不是字面量，
	// 因为它在三处出现：FTCreate 的 schema、HSET、KNN 查询的 @字段名
	vectorFieldName = "embedding"
)

// VectorStore 语义那一路：向量的家在 Redis（RediSearch 模块）。
//
// **本轮状态：没有构造点**（router.go 里给 NewService 的 vec 传的是 nil）。
// 要通电需要三件事：Redis 换成 redis/redis-stack-server、跑一次 EnsureIndex、
// 再跑回填命令把 127 条视频的向量灌进去 —— 见 search/entity.go 的"本轮状态"。
//
// ---------- 为什么向量存 Redis 而不是 MySQL ----------
//
// 三条理由，第一条是决定性的：
//
//  1. **MySQL 8.0 根本没有向量类型**（VECTOR 是 9.0 才加的），而且即使升级，
//     社区版也没有 DISTANCE()/向量索引 —— 那是 HeatWave/Enterprise 独有功能。
//     所以"MySQL 原生向量检索"这条路不是"麻烦"，是**不通**。
//  2. 向量检索在本项目里是**可降级**的：Redis 挂了 → 搜索退成纯关键词，
//     而不是崩掉。这和项目里 Redis 一直以来的定位（可选加速层）完全一致。
//  3. 127 条 × 4KB 的二进制列放在唯一真相源里，会让每次 SELECT * 都拖着它走。
//
// ---------- 一条必须记住的取舍 ----------
//
// 向量**不在** MySQL 里，所以 MySQL 那边只能用 embedding_model 这一列**记录**
// "嵌过没有"，而无法校验"向量还在不在"（Redis 可能被 flush、可能换了实例）。
// 因此"Redis 里的向量"和"MySQL 里的标记"是会漂移的，回填命令是唯一的修复手段。
// 这也是为什么它必须**幂等、可反复重跑**。
type VectorStore struct {
	rdb *redis.Client
	dim int
}

func NewVectorStore(rdb *redis.Client, dim int) *VectorStore {
	return &VectorStore{rdb: rdb, dim: dim}
}

// EnsureIndex 幂等地建 RediSearch 索引。由 main.go 在 Redis 连上之后调用。
//
// ---------- 为什么用 FT._LIST 探而不是 FT.INFO ----------
//
// FT.INFO 对不存在的索引返回的是一个**普通错误**，要区分"索引不存在"和
// "Redis 没起来"，只能去匹配错误文案（"Unknown index name"）—— 而匹配错误文案
// 是这个项目一以贯之要避免的做法（比错误码，不比文案）。
// FT._LIST 直接返回所有索引名，判断是纯字符串比较，没有歧义。
//
// 顺带它还是**能力探针**：普通 redis:7-alpine 没有 RediSearch 模块，
// FT._LIST 会返回 "unknown command" —— 这正是我们要的失败方式（下面会返回错误，
// 调用方记日志、降级），而不是让第一次搜索时才炸。
func (v *VectorStore) EnsureIndex(ctx context.Context) error {
	names, err := v.rdb.FT_List(ctx).Result()
	if err != nil {
		return fmt.Errorf("FT._LIST 失败（这个 Redis 不是 Redis Stack，没有 RediSearch 模块？）: %w", err)
	}
	if slices.Contains(names, vectorIndexName) {
		return nil
	}

	// 用 typed FTCreate 而不是拼 FT.CREATE 字符串。理由不只是省事：
	// 原始命令里 `VECTOR HNSW 6 TYPE FLOAT32 DIM 1024 ...` 那个 **6** 是
	// "后面还有几个属性 token"的**手工计数** —— 少一个多一个都是语法错误，
	// 而且它和属性列表隔了好几行，改属性时极易漏改。
	// 交给库去数，这一类错误就不存在了。
	return v.rdb.FTCreate(ctx, vectorIndexName,
		&redis.FTCreateOptions{
			OnHash: true,
			Prefix: []interface{}{vectorKeyPrefix},
		},
		// 注意这里是**指针**：go-redis 的 FTCreate 收的是
		// `...*redis.FieldSchema`，不是值 —— 少了 & 是编译错误（幸好是编译错误）
		&redis.FieldSchema{
			FieldName: "model",
			FieldType: redis.SearchFieldTypeText,
		},
		&redis.FieldSchema{
			FieldName: vectorFieldName,
			FieldType: redis.SearchFieldTypeVector,
			VectorArgs: &redis.FTVectorArgs{
				HNSWOptions: &redis.FTHNSWOptions{
					Type: "FLOAT32",
					Dim:  v.dim,
					// COSINE 而不是 L2：bge-m3 的输出被 Ollama 做过 L2 归一化，
					// 归一化之后余弦相似度和内积排序一致，而余弦对"文本长度"
					// 天然不敏感（长描述不会因为长度大就被判远）。
					DistanceMetric: "COSINE",
				},
			},
		},
	).Err()
}

// Upsert 写入一条向量。
//
// 用**一条 HSET** 写三个字段而不是三次 HSET：RediSearch 对哈希的索引是增量的，
// 三次分开写会出现"文档已经进了索引但 embedding 字段还没写"的中间态
// （被搜到、但距离算不出来）。单条 HSET 是原子的，没有这个窗口。
func (v *VectorStore) Upsert(ctx context.Context, id uint, model string, vec []float32) error {
	if len(vec) != v.dim {
		return fmt.Errorf("向量维度 %d 与索引声明的 %d 不一致", len(vec), v.dim)
	}
	key := vectorKeyPrefix + strconv.FormatUint(uint64(id), 10)
	return v.rdb.HSet(ctx, key, map[string]interface{}{
		// video_id 存成字段而不是从键名解析，是为了让 KNN 查询能
		// `RETURN 1 video_id` —— 见下面 KNN 的说明
		"video_id":      id,
		"model":         model,
		vectorFieldName: encodeVector(vec),
	}).Err()
}

// scoreMissingOnce 让"score 字段读不到"这件事只警告一次。
// 每次查询都打日志会把日志淹掉，而这件事是**配置/版本级**的问题，说一次就够。
var scoreMissingOnce sync.Once

// KNN 向量近邻查询。返回按余弦距离**从近到远**排序的列表。
//
// ---------- 返回的是距离，不是相似度 ----------
//
// RediSearch 返回的 score 是 **cosine distance**：越小越像，0 = 完全相同。
// 所以"过滤掉不够像的"要写成 `distance > maxDistance 就丢`，
// 而展示成"相似度"要写成 `1 - distance`。**方向搞反不会报错**，
// 只会让结果完全反过来（最不相关的排最前面）—— 这是本轮最容易写反的一行。
//
// ---------- 关于 RETURN video_id ----------
//
// 正常情况下 `Document.ID`（Redis 的键）里已经带着 id 了，怎么还要存一个字段？
// 因为解析键名要依赖 PREFIX 的字符串形状，而 RETURN 一个自己存的字段
// **不依赖任何外部约定**。代价是每条多存 8 字节，换掉一整类"前缀改了但解析没改"的
// 静默错位问题，划算。
func (v *VectorStore) KNN(ctx context.Context, vec []float32, k int, maxDistance float64) (RankedList, error) {
	if len(vec) != v.dim {
		return nil, fmt.Errorf("查询向量维度 %d 与索引声明的 %d 不一致", len(vec), v.dim)
	}

	res, err := v.rdb.FTSearchWithArgs(ctx, vectorIndexName,
		// KNN 查询语法：`*` 是"全部文档"的底集，`=>[KNN $k @字段 $q AS score]`
		// 在这个底集上做近邻。`AS score` 起的别名是后面 SORTBY/RETURN 引用的名字。
		"*=>[KNN $k @"+vectorFieldName+" $q AS score]",
		&redis.FTSearchOptions{
			Params: map[string]interface{}{
				"k": k,
				// q 是**原始二进制**，不是 JSON 数组、不是 base64。
				// RediSearch 要的就是 DIM×4 字节的小端 FLOAT32 序列。
				// 传成 JSON 数组会得到一个"维度解析失败"之类看不懂的错误
				"q": encodeVector(vec),
			},
			// 显式 SORTBY：纯 KNN 查询默认就是按距离升序，但把默认值写出来
			// 比依赖它更安全（默认值会变，而这里写反了会静默返回最远的 k 条）
			SortBy: []redis.FTSearchSortBy{{FieldName: "score", Asc: true}},
			Return: []redis.FTSearchReturn{{FieldName: "score"}},
			// **Limit 必须显式给**：go-redis 在 Limit==0 时**不发送 LIMIT 子句**，
			// 于是落到 RediSearch 的默认每页 10 条 —— 也就是说 KNN 要了 50 个近邻
			// 却只拿回 10 个，而且不报错。这个坑很隐蔽：k 和 LIMIT 是两件事
			LimitOffset: 0,
			Limit:       k,
			// DIALECT 2 是向量语法必需的。go-redis 在 DialectVersion<=0 时
			// 默认就发 DIALECT 2，但还是写出来，让人知道这一行是有讲究的
			DialectVersion: 2,
		}).Result()
	if err != nil {
		return nil, err
	}

	list := make(RankedList, 0, len(res.Docs))
	missingScore := false
	for _, doc := range res.Docs {
		id, ok := parseVideoID(doc)
		if !ok {
			continue // 不是我们的文档（PREFIX 理论上已经挡住），跳过而不是让整次搜索失败
		}

		distance := 0.0
		if raw, ok := doc.Fields["score"]; ok {
			if f, err := strconv.ParseFloat(raw, 64); err == nil {
				distance = f
			} else {
				missingScore = true
			}
		} else {
			missingScore = true
		}

		// 门槛过滤。读不到距离时**放行**（distance 保持 0）——
		// 丢结果的代价（搜索变少）比放进几条无关结果更严重，
		// 而且"读不到距离"是个需要人去修的问题，不该由用户承担后果
		if distance > maxDistance {
			continue
		}
		list = append(list, Ranked{ID: id, Score: distance})
	}
	if missingScore {
		scoreMissingOnce.Do(func() {
			log.Printf("[search] KNN 结果里读不到 score 字段，已跳过相似度门槛过滤。" +
				"请检查 RediSearch 版本对 `AS score` 别名的返回行为（RETURN 1 score 是否生效）")
		})
	}
	return list, nil
}

// Delete 删掉一批向量。**本轮没有任何调用点** —— 删视频目前不清 Redis。
//
// 留着的理由是它就是要补的那个缺口的第一步，而且缺口本身是记在案的：
// 删视频之后 Redis 里会留下一条指向不存在视频的向量，
// 它会被搜到、进融合，然后在 feed 侧的 GetByIDs 那里被自然丢掉
// （查不到详情 → 组装不出 item）。表现是"这一页比预期少几条"，
// 以及 has_more 可能提前变 false。
//
// 补它需要 video 包在删除路径上回调 search 包（方向和 Publish 的异步索引一样，
// 走接口注入），是独立的一小步，见 PROGRESS.md 的已知缺口。
func (v *VectorStore) Delete(ctx context.Context, ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, vectorKeyPrefix+strconv.FormatUint(uint64(id), 10))
	}
	return v.rdb.Del(ctx, keys...).Err()
}

// parseVideoID 从返回的文档里拿视频 id。
//
// 优先用我们自己存的 video_id 字段；拿不到再退回去解析键名
// （返回里的键形如 `vec:video:123`）。
func parseVideoID(doc redis.Document) (uint, bool) {
	if raw, ok := doc.Fields["video_id"]; ok {
		if n, err := strconv.ParseUint(raw, 10, 64); err == nil {
			return uint(n), true
		}
	}
	if strings.HasPrefix(doc.ID, vectorKeyPrefix) {
		if n, err := strconv.ParseUint(strings.TrimPrefix(doc.ID, vectorKeyPrefix), 10, 64); err == nil {
			return uint(n), true
		}
	}
	return 0, false
}

// encodeVector []float32 → Redis 要的原始字节。
//
// 格式是**小端 IEEE754 FLOAT32 序列**：DIM × 4 字节（1024 维 = 4096 字节）。
// 不是 JSON 数组、不是 base64、不是 float64。
//
// 手写而不是用 `unsafe.Pointer` 做零拷贝转换：`unsafe` 那种写法
// 隐含了"本机就是小端"和"切片内存布局就是连续的 4 字节对齐"两个假设，
// 端序假设在大端机器上是**静默错**（能跑，只是每个 float 的字节序反了，
// 算出来的距离全是垃圾），而这种机器今天的云上确实还有。
// 这个循环的代价在 4096 字节上可以忽略。
func encodeVector(vec []float32) []byte {
	buf := make([]byte, 4*len(vec))
	for i, f := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}
