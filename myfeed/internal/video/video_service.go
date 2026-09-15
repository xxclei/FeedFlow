package video

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"myfeed/internal/apierror"
	"myfeed/internal/config"
	rediscache "myfeed/internal/middleware/redis"

	"gorm.io/gorm"
)

// vectorIndexTimeout 发布后那次异步向量索引的超时。
//
// 给得比"查询侧 embedding"宽松得多（查询侧是 2s 量级）：发布后的索引
// 用户已经拿到响应了，慢一点没人等；而查询侧慢一点就是把用户卡在搜索框上。
// 两者用同一个超时是错的，这个区别值得写下来。
const vectorIndexTimeout = 30 * time.Second

// VectorIndexer 把一条视频的文本送去做向量索引（语义检索那一半）。
//
// ---------- 这个接口声明在 video 侧，实现却在 search 包 ----------
//
// 因为依赖方向必须单向：video 不能 import search（search 已经 import video 了，
// 为了用 video.Video 这个模型）。声明一个**接口**、由 search.Service 隐式满足它
// （Go 的接口是结构化的，不需要对方知道这个接口存在），import 环就不存在了。
//
// 反向的那一半同理：search 侧声明了 EmbeddingMarker，由 video.VideoRepository
// 隐式满足。两个接口各自属于"需要对方能力"的那一边。
//
// 允许为 nil（没人接 = 向量检索没启用）：那样发布照常工作，只是搜不到 ——
// 和 Redis/MQ 在本项目里一贯的降级地位一致。
type VectorIndexer interface {
	IndexVideo(ctx context.Context, id uint, text string) error
}

// 阶段7 详情缓存的四个参数。全是"猜一个数"的参数，所以每个都写清楚为什么。
const (
	// detailCacheTTL 详情缓存的存活时间。
	// 短的收益：视频被改（点赞数/热度）后最多脏 5 分钟。
	// 长的收益：DB 压力更小。5 分钟是这个项目里"读多写少、允许短暂陈旧"的折中。
	detailCacheTTL = 5 * time.Minute

	// detailCacheOpTimeout 单次 Redis 操作的超时。
	// 缓存是加速件 —— 给它设上限，Redis 抖动时宁可快速失败回源，
	// 也不能让一个本来 15ms 的读请求卡在 Redis 上变成 2 秒。
	detailCacheOpTimeout = 50 * time.Millisecond

	// detailLockTTL 防击穿锁的 TTL。
	// 按"一次 DB 回源 + 一次写缓存"的最坏耗时估的：太短→回源没做完锁就易主，
	// 防击穿失效；太长→持锁的 goroutine 挂了，其他请求白等。
	detailLockTTL = 2 * time.Second

	// detailLockWaitStep / detailLockWaitRounds 没抢到锁时的轮询节奏：
	// 5 × 20ms = 最多等 100ms。等不到就自己兜底查 DB —— 防击穿是"削峰"，
	// 不是"必须所有人都等着"，可用性优先于完美防护。
	detailLockWaitStep   = 20 * time.Millisecond
	detailLockWaitRounds = 5
)

// VideoService 视频业务：发布事务、详情查询、计数变更。
// cache 阶段7新增（GetDetail 防击穿缓存）、popularityMQ 阶段9回填（热度事件）
type VideoService struct {
	repo *VideoRepository
	// vectorIdx 本轮新增：发布后异步补向量。可以为 nil
	vectorIdx VectorIndexer
	// cache 阶段7新增：详情缓存 + 防击穿锁。**可以为 nil**（启动降级）——
	// 所有用到它的地方都先 `if vs.cache == nil` 走纯 DB 路径，
	// 封装层也做了 nil 接收器保护，双保险
	cache *rediscache.Client
	// cacheTTL 做成字段而不是直接用常量：测试里想验"过期后回源"就得能调小它
	cacheTTL time.Duration
	// storage 本轮新增：上传根目录。Publish 要按 PlayURL 反查出磁盘路径
	// 才能去探测源文件（见 probeSource）。
	//
	// 传值而不是指针：它只有一个字符串字段，拷贝成本为零，
	// 而且值语义意味着**发布过程中它不可能被别处改掉**。
	storage config.StorageConfig
}

func NewVideoService(repo *VideoRepository, vectorIdx VectorIndexer, cache *rediscache.Client, storage config.StorageConfig) *VideoService {
	return &VideoService{
		repo:      repo,
		vectorIdx: vectorIdx,
		cache:     cache,
		cacheTTL:  detailCacheTTL,
		storage:   storage,
	}
}

// Publish 发布：校验 → 一个事务里做三件事：
//
//	① 插 videos        业务本体
//	② 插 outbox_msgs   事件信存根（阶段9 Poller 搬运到 MQ；和①同事务=要么都在要么都不在）
//	③ 挂标签           tags + video_tags 关联
//
// 注意：这里直接用 vs.repo.db 开事务——原项目的分层瑕疵
// （service 越过 repo 摸库）。标签那一步本轮已经下沉成 repo.AttachTagsTx，
// 前两步保持对齐原项目；这个味道还在，只是小了一点。
//
// tagNames 是本轮新增的**第二个标签来源**（批量上传统一设置的那些），
// 和描述文本里手写的 #xxx 取并集。传 nil/空切片 = 只有文本里那个来源，
// 行为和不传这个参数之前完全一样 —— 这是并集设计的全部好处。
//
// **这里绝不碰 Ollama。** 向量是搜索用的，发布不能被它拖住：embedding 是一次
// 网络调用，放进这个事务里意味着 Ollama 慢或挂 → 事务长时间持有 → 用户发不出视频。
// 向量在事务提交**之后**由异步路径补（见下面的 indexAsync），丢了由回填命令兜底。
// 本轮 vectorIdx 传的是 nil，所以这个口子现在是关着的（副作用：发布路径零外部调用）。
func (vs *VideoService) Publish(ctx context.Context, video *Video, tagNames []string) error {
	if video == nil {
		return errors.New("video is nil")
	}
	video.Title = strings.TrimSpace(video.Title)
	video.PlayURL = strings.TrimSpace(video.PlayURL)
	video.CoverURL = strings.TrimSpace(video.CoverURL)

	if video.Title == "" {
		return errors.New("title is required")
	}
	if video.PlayURL == "" {
		return errors.New("play url is required")
	}
	if video.CoverURL == "" {
		return errors.New("cover url is required")
	}

	// ---------- 上传质量门禁：探测源文件，决定直传还是转码 ----------
	//
	// **必须在事务之前**，有两个理由：
	//
	//  1. 它是本地文件头读取（约 1ms），但**事务里不该有 IO**。
	//     事务持有行锁的时间越短越好，把一个文件读放进事务里，
	//     一旦遇到慢盘（网络盘、杀软实时扫描）就会把锁的时间放大几个量级。
	//  2. 探测的结果要写进**下面那条 INSERT**（①），所以它必须先算出来。
	//
	// 探测用的是 PlayURL 反查出来的磁盘路径，而 PlayURL 是**客户端传的** ——
	// 所以这里既可能拿不到路径（穿越/格式不对），也可能文件根本不在
	// （客户端先发布、文件后到，或者文件被手工删了）。两种都算 probe 失败。
	probe := vs.probeSource(video.PlayURL)
	video.ProbeStatus = probe.status
	if probe.info != nil {
		video.SrcWidth = probe.info.Width
		video.SrcHeight = probe.info.Height
		video.SrcBitrateKbps = probe.info.BitrateKbps
		video.SrcDurationMS = probe.info.DurationMS
	}

	decision := decideTranscode(probe.info, probe.status == ProbeStatusOK)
	// "skipped" 和 "" 是两件事："" 是存量数据（从没经过门禁），
	// "skipped" 是"门禁看过了，判定为直传"。前端两者都走直传路径，
	// 但运营看板上必须能区分 —— 否则"有多少条真的过了门禁"永远说不清。
	//
	// 注意 "failed" 是**转码失败**（worker 写），不是探测失败。
	// 探测失败在这里也是 "skipped"：结果是直传，和探明后判定直传一样。
	video.TranscodeStatus = TranscodeSkipped
	if decision.Transcode {
		video.TranscodeStatus = TranscodePending
	}
	log.Printf("[Publish] 门禁 video=%s 探测=%s 源=%dx%d@%dkbps %vms → %s（%s）",
		video.PlayURL, video.ProbeStatus, video.SrcWidth, video.SrcHeight,
		video.SrcBitrateKbps, video.SrcDurationMS, video.TranscodeStatus, decision.Reason)

	// 事务保证视频写库和事件信写入的一致性
	err := vs.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// ① 视频本体
		if err := tx.Create(video).Error; err != nil {
			return err
		}

		// ② 事件信（publish 产生的"广播"先存根，阶段9才寄出）
		msg := OutboxMsg{
			VideoID:    video.ID,
			EventType:  EventTypeVideoPublished,
			Status:     "pending",
			CreateTime: video.CreateTime,
		}
		if err := tx.Create(&msg).Error; err != nil {
			return err
		}

		// ②' 转码任务信 —— **和上面那条在同一个事务里**。
		//
		// 这就是 outbox 模式的价值所在：如果转码消息在事务提交之后单独发，
		// 那么"提交成功但发消息失败"会留下一条**永远不会被转码**的视频，
		// 而它在库里看起来完全正常（transcode_status 停在 pending 而已）。
		// 放进同一个事务，"视频存在 ⇔ 转码任务已记录"由数据库保证。
		//
		// 注意 EventType 现在是**有读者的**（pollOnce 按它分流）——
		// 在这之前它是个死字段，加第二种事件会静默投错队列。
		if decision.Transcode {
			tmsg := OutboxMsg{
				VideoID:    video.ID,
				EventType:  EventTypeVideoTranscode,
				Status:     "pending",
				CreateTime: video.CreateTime,
			}
			if err := tx.Create(&tmsg).Error; err != nil {
				return err
			}
		}

		// ③ 标签：两个来源取并集，再挂上去。
		//
		//    来源一 = 从 title+" "+description 里解析出的 #xxx（用户手写的）
		//    来源二 = tagNames（调用方显式传的，批量上传的 chip 编辑器产出）
		//
		// **并集而不是二选一**，所以本轮改动对既有路径是纯增量：
		// 单独发布页那个 TagInput 往描述里写 #标签的老行为一个字不用改。
		//
		// 并集之后统一过一遍 normalizeTagNames，它一次做四件事：去空白、剥前导 #、
		// 按 rune 截到列宽、**按不区分大小写去重**。最后那件事是必须的 ——
		// 见 tag_repo.go 里那段说明：tags.name 是 _ci 排序规则，
		// 不去重的话第二轮 FirstOrCreate 会命中同一个 tagID，
		// 于是插出重复的 (video, tag) 关联，撞上本轮新加的复合唯一索引 → 整个发布回滚。
		names := normalizeTagNames(append(ExtractTags(video.Title+" "+video.Description), tagNames...))
		if err := vs.repo.AttachTagsTx(tx, video.ID, names); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	// 走到这里事务已经提交，视频一定在库里了 —— 才轮到向量。
	// **顺序不能反**：先嵌向量后提交事务的话，事务回滚会留下一条指向
	// 不存在视频的向量（虽然会被 GetByIDs 自然过滤掉，但那是运气不是设计）
	vs.indexAsync(video.ID, video.Title, video.Description)
	return nil
}

// indexAsync 事务提交之后，异步给这条视频补向量。
//
// 四个刻意的选择：
//
//  1. **绝不在事务里**（见上面 Publish 的结尾）。embedding 是一次网络调用，
//     几百毫秒起，Ollama 慢或挂就会把事务拖着 —— 用户发不出视频，
//     而向量只是搜索用的。这一条是"发布绝不被 embedding 拖住"的落地点。
//
//  2. **失败只记日志**。发布已经成功了，向量缺失的后果是"这条暂时搜不到"，
//     而不是功能坏掉。敢让它 fire-and-forget 的前提是**有一个幂等的回填命令兜底**
//     （cmd/backfill，本轮没做），否则这里就该做重试队列了。
//
//  3. **自己起一个带超时的 context，不用请求的 context**。请求的 ctx 在响应
//     写完之后随时可能被取消（客户端断开、超时中间件），那样这次索引会半路夭折 ——
//     而它在语义上属于"发布之后发生的事"，不属于"这个请求"。
//
//  4. **它是没有任何人等的 goroutine**（不在任何 WaitGroup 里）：进程退出时会被
//     直接掐掉。这正是回填命令必须存在的理由 —— 有了它，"丢一次"是可修复的，
//     没有它，这种写法就是在赌运气。
//
// 文本用 title + " " + description，和词法那边的检索范围一致（虽然 FTS 是分列的、
// 这里是拼接的，但这个差异对向量没有影响：模型看的是整段文本）。
func (vs *VideoService) indexAsync(id uint, title, description string) {
	if vs.vectorIdx == nil {
		return
	}
	text := strings.TrimSpace(title + " " + description)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), vectorIndexTimeout)
		defer cancel()
		if err := vs.vectorIdx.IndexVideo(ctx, id, text); err != nil {
			log.Printf("[video] 视频 %d 的向量索引失败（可跑回填命令补上）: %v", id, err)
		}
	}()
}

// Delete 删除单条：只有作者本人能删。
//
// 本轮把"不是你的"从 401 改成了 **403**，和 comment_service.Delete 对齐。
// 这不是分类学上的讲究，是这个项目唯一一处不分开就会出真 bug 的地方：
// 前端 client.ts 的 handleResponse 对**任何** 401 都调 auth.clearTokens()，
// 所以一个手滑点到别人视频删除键的用户会被静默登出，还完全不知道为什么。
//
// 保留"先查再判"而不是直接交给批量路径，是为了维持两个既有语义：
//
//	视频不存在 → gorm.ErrRecordNotFound → 404（批量路径下"不存在"和"不是你的"
//	             被合并成同一种结果，因为那条 SQL 无法区分，也不该区分 ——
//	             但单条删除的调用方确实需要 404）
//	不是你的   → 403
//
// 归属判断因此发生两次（这里一次，repo 的 DELETE 里一次）。**这是刻意的**：
// repo 那一层是最后的把关，关掉"读了再写"中间的窗口；代价只是一个恒真的条件。
func (vs *VideoService) Delete(ctx context.Context, id uint, authorID uint) error {
	video, err := vs.repo.GetByID(ctx, id)
	if err != nil {
		// GetByID 返回 (nil, gorm.ErrRecordNotFound)，**从不返回 (nil, nil)** ——
		// 所以这里不需要（也不该有）`if video == nil` 那种检查。原项目那段
		// `video == nil` 分支是死代码，本轮顺手清掉
		return err
	}
	if video.AuthorID != authorID {
		return apierror.ErrForbidden
	}
	_, err = vs.repo.DeleteOwnedVideos(ctx, []uint{id}, authorID)
	// 阶段7：删库成功才失效缓存
	vs.invalidateDetails([]uint{id})
	return err
}

// DeleteBatch 批量删除：删掉 ids 里属于 authorID 的那些，如实报告跳过了哪些。
//
// 返回 DeleteBatchResponse 而不是裸 error，因为**部分成功是正常结果**：
// 前端的多选列表可能是旧的（另一个标签页删过了，或者勾的时候视频还在、
// 提交时已经没了）。让整个请求失败，用户只会看到"删除失败"却不知道哪条出了问题；
// 回一个"3 条里删掉 2 条、这 1 条跳过了"，前端就能把没删掉那条留在选中态里重试。
//
// **这里不做"先验证全部 id 都属于我，有一个不是就整体拒绝"** —— 那个语义听起来
// 更严格，实际更难用：一次手滑多勾一条就整批白删。
func (vs *VideoService) DeleteBatch(ctx context.Context, ids []uint, authorID uint) (DeleteBatchResponse, error) {
	// 去重在这里做，不在 handler —— "同一个 id 传三次"是**批量语义**的问题，
	// 不是参数绑定格式的问题。不去重的话 skipped_ids 会把重复的 id 算进去
	// （因为它按输入顺序做差集），前端会提示"跳过了 2 条"而实际只有 1 条
	uniq := dedupeIDs(ids)

	owned, err := vs.repo.DeleteOwnedVideos(ctx, uniq, authorID)
	if err != nil {
		return DeleteBatchResponse{}, err
	}

	deleted := make(map[uint]bool, len(owned))
	for _, id := range owned {
		deleted[id] = true
	}
	skipped := make([]uint, 0, len(uniq)-len(owned))
	for _, id := range uniq {
		if !deleted[id] {
			skipped = append(skipped, id)
		}
	}

	// 阶段7：只失效**真正删掉**的那些（owned），skipped 的视频还在，缓存要留着。
	// 走 pipeline 而不是 N 次 Del —— 批量接口的 N 本来就是大数
	vs.invalidateDetails(owned)
	return DeleteBatchResponse{
		Deleted:    len(owned),
		DeletedIDs: owned,
		SkippedIDs: skipped,
	}, nil
}

// invalidateDetails 失效若干条视频的详情缓存。
//
// 三个刻意的选择：
//
//	① 用 context.Background() 而不是请求 ctx：删除**已经落库了**，
//	   不能因为客户端断开就留下一个脏缓存。写操作后的失效必须跑完。
//	② 忽略错误：缓存删不掉的最坏后果是"旧详情多活 5 分钟"（TTL 兜底），
//	   而返回错误会让用户看到"删除失败"却其实已经删了 —— 更糟。
//	   这也正是给缓存设 TTL 的意义：失效是尽力而为的，TTL 才是最终保底。
//	③ pipeline：N 条 DEL 压成一次往返。N=100 时省下 99 次 RTT。
func (vs *VideoService) invalidateDetails(ids []uint) {
	if vs.cache == nil || len(ids) == 0 {
		return
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, vs.cache.Key("video:detail:id=%d", id))
	}
	opCtx, cancel := context.WithTimeout(context.Background(), detailCacheOpTimeout)
	defer cancel()
	_ = vs.cache.DelMany(opCtx, keys)
}

// ListByAuthorID 某作者的视频列表（时间倒序，200 条封顶）
func (vs *VideoService) ListByAuthorID(ctx context.Context, authorID uint) ([]Video, error) {
	return vs.repo.ListByAuthorID(ctx, int64(authorID))
}

// GetDetail 详情（阶段7：缓存旁路 + 防击穿锁）。
//
// 完整链路：
//
//	读缓存(50ms) ─命中─► 返回
//	   │ miss
//	   ▼
//	二次裸读 → 区分「真未命中」和「Redis 故障」
//	   │ 真 miss                            │ 故障
//	   ▼                                    ▼
//	SETNX lock:<cacheKey> 随机token, 2s     直接回源（抢锁也是白抢）
//	   │ 拿到锁            │ 没拿到锁
//	   ▼                   ▼
//	双重检查缓存          轮询 5×20ms 读缓存 ─读到─► 返回
//	   │ 仍 miss                            │ 还没读到
//	   ▼                                    ▼
//	查 DB → 回填 → Unlock   兜底自己查 DB（不再等）
//
// ---------- 为什么"二次裸读"这一步不能省 ----------
//
// 下面的 getCached 闭包把**所有** error 都当成"没命中"（缓存故障不该让读请求失败）。
// 但抢锁之前必须知道到底是"key 不在"还是"Redis 挂了"：Redis 挂了的时候 SETNX 一样
// 会失败，白跑一趟还多 50ms 延迟。所以需要一次**能看见错误码**的裸读来做裁决。
//
// 这也是原项目那段的写法，但它多调了一次 getCached 造成冗余读；这里合并掉了。
func (vs *VideoService) GetDetail(ctx context.Context, id uint) (*Video, error) {
	// 启动降级：Redis 没连上，整个缓存层跳过，功能一条不少
	if vs.cache == nil {
		return vs.repo.GetByID(ctx, id)
	}

	cacheKey := vs.cache.Key("video:detail:id=%d", id)

	// getCached：读缓存。任何失败（未命中 / 超时 / 反序列化错）都返回 err，
	// 由调用方决定是回源还是抢锁。**不在这里打日志** —— miss 是正常路径，打日志会刷屏。
	getCached := func(ctx context.Context) (*Video, error) {
		opCtx, cancel := context.WithTimeout(ctx, detailCacheOpTimeout)
		defer cancel()
		raw, err := vs.cache.GetBytes(opCtx, cacheKey)
		if err != nil {
			return nil, err
		}
		var v Video
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		return &v, nil
	}

	// setCached：回填缓存，尽力而为。写失败只记日志 —— 数据已经查出来了，
	// 因为缓存写不进去就把这个请求判失败，是本末倒置。
	setCached := func(ctx context.Context, v *Video) {
		raw, err := json.Marshal(v)
		if err != nil {
			return
		}
		// 用独立的 ctx：调用方的 ctx 可能在这时候已经因为客户端断开被取消了，
		// 但"查询已经完成、把结果存进缓存"这件事仍然值得做完（下一个请求就能命中）
		opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detailCacheOpTimeout)
		defer cancel()
		if err := vs.cache.SetBytes(opCtx, cacheKey, raw, vs.cacheTTL); err != nil {
			log.Printf("[video] 回填详情缓存失败 id=%d: %v", id, err)
		}
	}

	// ---------- ① 读缓存 ----------
	if v, err := getCached(ctx); err == nil {
		return v, nil
	}

	// ---------- ② 二次裸读：裁决「真未命中」还是「Redis 故障」 ----------
	probeCtx, cancelProbe := context.WithTimeout(ctx, detailCacheOpTimeout)
	probeRaw, probeErr := vs.cache.GetBytes(probeCtx, cacheKey)
	cancelProbe()

	switch {
	case probeErr == nil:
		// 两次读之间别人刚回填好了 —— 直接用，锁都不用抢
		var v Video
		if err := json.Unmarshal(probeRaw, &v); err == nil {
			return &v, nil
		}
		// 反序列化失败 = 缓存里是脏数据。往下走：抢锁 → 双检失败 → 回源，
		// 回源后 setCached 会把脏数据覆盖掉，等于顺手修好了
	case !rediscache.IsMiss(probeErr):
		// Redis 故障（超时/拒连/权限）：抢锁也会失败，直接回源
		v, err := vs.repo.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		setCached(ctx, v)
		return v, nil
	}

	// ---------- ③ 真未命中：抢防击穿锁，只放一个人去回源 ----------
	lockKey := "lock:" + cacheKey // 锁 key 就是缓存 key 加前缀，一眼能看出锁的是谁
	lockCtx, cancelLock := context.WithTimeout(ctx, detailCacheOpTimeout)
	token, locked, err := vs.cache.Lock(lockCtx, lockKey, detailLockTTL)
	cancelLock()
	if err != nil {
		// 锁操作本身出错（Redis 抖动）：不阻塞主流程，直接回源
		return vs.repo.GetByID(ctx, id)
	}

	if locked {
		// 释放用 Lua「GET==token 才 DEL」——见 Unlock 的实现。
		// 用 Background：请求可能已经结束/取消，锁还是必须放掉，
		// 否则要等满 2s TTL 才自动过期，期间所有并发请求都在空转
		defer func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), detailCacheOpTimeout)
			defer cancel()
			_ = vs.cache.Unlock(unlockCtx, lockKey, token)
		}()

		// 双重检查：从「读到 miss」到「抢到锁」之间有窗口期，
		// 前一个持锁者可能已经回填完了。不复查就会重复查 DB
		if v, err := getCached(ctx); err == nil {
			return v, nil
		}

		v, err := vs.repo.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		setCached(ctx, v)
		return v, nil
	}

	// ---------- ④ 没抢到锁：别人正在回源，轮询等一下 ----------
	for i := 0; i < detailLockWaitRounds; i++ {
		select {
		case <-ctx.Done():
			// ctx 感知很重要：客户端已经断开就别再空转 100ms 了
			return nil, ctx.Err()
		case <-time.After(detailLockWaitStep):
		}
		if v, err := getCached(ctx); err == nil {
			return v, nil
		}
	}

	// ---------- ⑤ 等不到（持锁者挂了 / DB 特别慢）：自己兜底 ----------
	// 宁可多打一次 DB，也不能不响应。这就是"防击穿"和"分布式事务"的区别 ——
	// 前者只保证**通常情况**下回源被收敛成一次，不保证严格唯一
	v, err := vs.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	setCached(ctx, v)
	return v, nil
}

// UpdateLikesCount 用指定值覆盖计数（阶段4 LikeWorker 用）
//
// **缓存失效不在这里做**（阶段8 回填）。这里只写 MySQL，缓存里的 likes_count
// 会在 TTL 内是旧值 —— 见 UpdatePopularity 上的说明，两个方法共用同一套失效逻辑。
func (vs *VideoService) UpdateLikesCount(ctx context.Context, id uint, likesCount int64) error {
	return vs.repo.UpdateLikesCount(ctx, id, likesCount)
}

// UpdatePopularity 热度增减（数据库端原子 + 下限0）。
//
// 阶段8/9回填：先试 popularityMQ 事件；失败或未启用时写 Redis 热榜分钟桶
// （ZincrBy + 2h 过期）+ 连带失效 detail/entity 两个缓存 —— 那段逻辑落在
// internal/video/popularity_cache.go 的 UpdatePopularityCache。
//
// ---------- 一个必须知道的"脏读窗口"（阶段7 的已知取舍） ----------
//
// 点赞链路（LikeService.Like）是在**事务里**直接 ChangeLikesCountTx，
// 根本不经过这个方法。所以现在：点赞后 /video/getDetail 返回的 likes_count
// 最多会旧 5 分钟（detailCacheTTL），直到缓存自然过期。
//
// 这不是漏写，是顺序问题：点赞的失效要等阶段9 的 PopularityWorker 把事件
// 消费到、调 UpdatePopularityCache 才闭环。原项目也是这个顺序。
// 真要在阶段7 就补，正确做法是给 LikeService 也注入 cache、事务 commit
// 之后 DEL 详情 key —— 但那样 stage 8/9 会被重复失效，所以这里先按下不表。
func (vs *VideoService) UpdatePopularity(ctx context.Context, id uint, change int64) error {
	return vs.repo.UpdatePopularity(ctx, id, change)
}
