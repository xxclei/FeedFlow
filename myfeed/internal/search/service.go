package search

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
)

// EmbeddingMarker 记下"这条视频已经用某个模型嵌过了"。
//
// **声明在这里，由 video.VideoRepository 隐式满足**（它有一个
// `MarkEmbedded(ctx, id uint, model string) error`，签名一致就自动满足这个接口，
// Go 的接口是结构化的，不需要对方知道这个接口存在）。
//
// 这个手法解决的是一个真实的循环依赖：video 包需要在发布后触发索引，
// search 包需要在索引成功后把标记写回 videos 表 —— 两边都要认识对方。
// 用两个接口（video 侧声明 VectorIndexer、search 侧声明 EmbeddingMarker）
// 把它们切成两条单向的依赖，import 环就不存在了。
// 这和项目里 account/video 分家的手法是同一个。
type EmbeddingMarker interface {
	MarkEmbedded(ctx context.Context, id uint, model string) error
}

// Service 检索服务：并跑两路 + 融合 + 分页。
type Service struct {
	lex *LexicalSearcher
	// vec 和 embed 都可能为 nil —— nil 的含义是"这一路没接"，
	// 和"这一路挂了"有区别（前者是配置选择，后者是故障），
	// 但对搜索的用户来说结果一样：少一路召回
	vec    *VectorStore
	embed  Embedder
	marker EmbeddingMarker
	cfg    Config
}

// NewService 组装。vec / embed / marker 允许为 nil，语义是"语义那一路没启用"。
//
// **构造时不探测 Ollama**（不发任何请求）：探测会让服务启动依赖一个
// 可能没起来的推理服务，而且探测通过也不代表之后一直通。
// 真实连通性由第一次调用决定，失败就地降级 —— 这比"启动时检查一次"更诚实。
func NewService(lex *LexicalSearcher, vec *VectorStore, embed Embedder, marker EmbeddingMarker, cfg Config) *Service {
	return &Service{
		lex:    lex,
		vec:    vec,
		embed:  embed,
		marker: marker,
		cfg:    cfg.withDefaults(),
	}
}

// Enabled 报告两路各自是否接上了。启动日志用它，让人一眼知道当前的降级形态。
func (s *Service) Enabled() (lexical, vector bool) {
	return s.lex != nil, s.vec != nil && s.embed != nil
}

// Search 搜索主入口。返回 (结果, 下一页游标, 错误)。
// 游标为空串表示没有下一页。
//
// cur 不为 nil 时**不跑召回**，直接从冻结列表里切片（见 cursor.go）。
func (s *Service) Search(ctx context.Context, q Query, cur *Cursor, limit int) (Result, string, error) {
	if cur != nil {
		return s.pageFromCursor(cur, limit)
	}
	return s.firstPage(ctx, q, limit)
}

// pageFromCursor 翻页：从冻结的排名里切一段。不查库、不查 Redis、不调 Ollama。
func (s *Service) pageFromCursor(cur *Cursor, limit int) (Result, string, error) {
	ids := cur.IDs
	end := cur.Offset + limit
	hasMore := end < len(ids)
	if end > len(ids) {
		end = len(ids)
	}
	if cur.Offset > len(ids) {
		// DecodeCursor 已经挡过一次，这里是"读代码时不必回头确认"的那一层。
		// 越界的切片会 panic，而 panic 在 HTTP handler 里是 500 + 堆栈
		cur.Offset = len(ids)
	}

	res := Result{
		IDs:   ids[cur.Offset:end],
		Mode:  cur.Mode,
		Arms:  cur.Arms,
		Total: len(ids),
	}
	if !hasMore {
		return res, "", nil
	}
	next, err := EncodeCursor(Cursor{IDs: ids, Offset: end, Mode: cur.Mode, Arms: cur.Arms})
	if err != nil {
		return Result{}, "", err
	}
	return res, next, nil
}

// firstPage 第一页：两路并发召回 → RRF 融合 → 冻结进游标 → 切首页。
func (s *Service) firstPage(ctx context.Context, q Query, limit int) (Result, string, error) {
	depth := s.cfg.Depth

	// ---------- 两路并发 ----------
	//
	// 每个变量只被一个 goroutine 写、在 Wait 之后才读，所以不需要加锁
	// （WaitGroup 的 Wait 提供了 happens-before 边）。
	//
	// 用裸 goroutine + WaitGroup 而不是 errgroup：这里不需要"第一个错误就取消其它"的语义
	// —— 两路的失败都是**各自降级**，不是整体失败。用 errgroup 反而要额外处理
	// "一边失败导致 ctx 被取消、另一边被连累"这件事。
	var (
		wg      sync.WaitGroup
		lexList RankedList
		lexMode Mode
		lexErr  error
		vecList RankedList
		vecErr  error
	)
	runLex := s.lex != nil
	runVec := s.vec != nil && s.embed != nil

	if runLex {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lexList, lexMode, lexErr = s.lex.Search(ctx, q, depth)
		}()
	}
	if runVec {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vecList, vecErr = s.vectorArm(ctx, q.Raw, depth)
		}()
	}
	wg.Wait()

	// ---------- 降级：任何一路挂了都只是"少一路"，不是失败 ----------
	//
	// 这是整个设计里最重要的一条纪律。查询侧 embedding 失败**绝不能变成 500**：
	// 那会让"搜索"这个功能因为一个可选组件（Ollama / Redis）而整体不可用，
	// 而词法那一路明明能干活。
	if lexErr != nil {
		if errors.Is(lexErr, ErrNoFullTextIndex) {
			log.Printf("[search] videos 上没有可用的全文索引，本次只用向量那一路")
		} else {
			log.Printf("[search] 词法那一路失败，降级为只用向量: %v", lexErr)
		}
		runLex, lexList = false, nil
	}
	if vecErr != nil {
		// 这里会打得很频繁（Ollama 停着的时候每次搜索一条）。
		// 接受它：这类日志的价值就在于"能看出从哪一刻开始塌的"，
		// 降噪的做法应该是调日志级别，而不是少记
		log.Printf("[search] 向量那一路失败（Redis 或 Ollama），降级为只用词法: %v", vecErr)
		runVec, vecList = false, nil
	}
	if !runLex && !runVec {
		// 两路都不可用 —— 这才真的没得降了。
		// 把两个原因都带上：只报一个会让人修完一个再撞一次墙
		return Result{}, "", fmt.Errorf("词法与向量两路都不可用: 词法=%v; 向量=%v", lexErr, vecErr)
	}

	// ---------- 模式 ----------
	//
	// Mode 描述的是"这一路跑成了什么形态"，和 Arms（数量）是两个正交的维度：
	// 词法返回 0 条时 mode 仍然是 ngram，而 Arms.Lexical = 0 ——
	// 硬把这种情况报成 vector-only 会把"跑过但没命中"和"根本没跑"混起来，
	// 而这两者的排查方向完全不同（前者是语料问题，后者是配置问题）。
	var mode Mode
	switch {
	case runLex && runVec:
		mode = lexMode // ngram / ngram-or / like
	case runLex:
		mode = ModeLexicalOnly
	default:
		mode = ModeVectorOnly
	}

	ids := FuseRRF(s.cfg.RRFK, lexList, vecList)
	arms := Arms{Lexical: len(lexList), Vector: len(vecList)}

	end := limit
	hasMore := end < len(ids)
	if end > len(ids) {
		end = len(ids)
	}

	res := Result{
		IDs:   ids[:end],
		Mode:  mode,
		Arms:  arms,
		Total: len(ids),
	}
	if !hasMore {
		return res, "", nil
	}
	// 冻结：把**完整的**融合列表 + 下一步的偏移塞进令牌。
	// 注意塞的是 ids 全量而不是 ids[end:] —— 偏移是相对全量的，
	// 存剩余段的话偏移语义会随页数漂移
	next, err := EncodeCursor(Cursor{IDs: ids, Offset: end, Mode: mode, Arms: arms})
	if err != nil {
		return Result{}, "", err
	}
	return res, next, nil
}

// vectorArm 语义那一路：把查询文本 embed 成向量，再去 Redis 做 KNN。
func (s *Service) vectorArm(ctx context.Context, raw string, k int) (RankedList, error) {
	vecs, err := s.embed.Embed(ctx, []string{raw})
	if err != nil {
		return nil, fmt.Errorf("查询向量化失败: %w", err)
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("查询向量化返回了 %d 条向量，期望 1 条", len(vecs))
	}
	return s.vec.KNN(ctx, vecs[0], k, s.cfg.MaxDistance)
}

// IndexVideo 给一条视频建向量索引：embed → 写 Redis → 写标记。
//
// ---------- 这是"向量索引"这件事的**唯一**实现 ----------
//
// 发布后的异步补和回填命令都调它。只有一个实现才有意义，因为**写的顺序**
// 是这里的关键，而顺序错了不会报错：
//
//	① 先写 Redis，② 再写 MySQL 的标记
//
// 崩溃时：标记没写、向量在 → 回填会重算一次（幂等，无害）。
// 反过来（先标记后写 Redis）崩溃时：标记写了、向量没写 → **回填永远不会补它**，
// 因为"没嵌过"的条件已经不成立了。一条视频就此永久搜不到，且没有任何迹象。
//
// video_repo.go 的 MarkEmbedded 上面记着同一条规矩，两处必须一致。
//
// ---------- 它绝不放进发布事务 ----------
//
// embedding 是一次**网络调用**（几百毫秒起）。放进 Publish 的 DB 事务里意味着
// Ollama 慢或挂 → 事务长时间持有 → 用户发不出视频，而向量只是搜索用的。
// 所以调用方一律是"事务提交之后、fire-and-forget"，见 video_service.go 的 Publish。
func (s *Service) IndexVideo(ctx context.Context, id uint, text string) error {
	if s.embed == nil || s.vec == nil {
		return errors.New("向量检索未启用（没接 Redis 或 Embedder）")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		// 没有文本可嵌时**不写标记**：写了的话回填会认为它已经处理过，
		// 而它其实永远不会被嵌 —— 一条永远搜不到的视频
		return errors.New("视频没有可嵌入的文本（标题和描述都是空的）")
	}

	vecs, err := s.embed.Embed(ctx, []string{text})
	if err != nil {
		return err
	}
	if len(vecs) != 1 {
		return fmt.Errorf("embed 返回了 %d 条向量，期望 1 条", len(vecs))
	}

	// ① 先写 Redis
	if err := s.vec.Upsert(ctx, id, s.embed.Model(), vecs[0]); err != nil {
		return fmt.Errorf("写向量到 Redis 失败: %w", err)
	}

	// ② 再写标记。这一步失败**不回滚**（也回滚不了：一个在 Redis、一个在 MySQL，
	// 没有跨存储事务）。不回滚是对的 —— 向量在、标记不在的后果只是重算一次
	if s.marker == nil {
		return nil
	}
	if err := s.marker.MarkEmbedded(ctx, id, s.embed.Model()); err != nil {
		return fmt.Errorf("向量已写入 Redis，但标记写失败（回填会重算这条，无害）: %w", err)
	}
	return nil
}
