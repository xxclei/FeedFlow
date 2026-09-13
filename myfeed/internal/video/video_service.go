package video

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"myfeed/internal/apierror"

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

// VideoService 视频业务：发布事务、详情查询、计数变更。
// cache 阶段7回填（GetDetail 防击穿缓存）、popularityMQ 阶段9回填（热度事件）
type VideoService struct {
	repo *VideoRepository
	// vectorIdx 本轮新增：发布后异步补向量。可以为 nil
	vectorIdx VectorIndexer
}

func NewVideoService(repo *VideoRepository, vectorIdx VectorIndexer) *VideoService {
	return &VideoService{repo: repo, vectorIdx: vectorIdx}
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

	// 事务保证视频写库和事件信写入的一致性
	err := vs.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// ① 视频本体
		if err := tx.Create(video).Error; err != nil {
			return err
		}

		// ② 事件信（publish 产生的"广播"先存根，阶段9才寄出）
		msg := OutboxMsg{
			VideoID:    video.ID,
			EventType:  "video_published",
			Status:     "pending",
			CreateTime: video.CreateTime,
		}
		if err := tx.Create(&msg).Error; err != nil {
			return err
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
	// 阶段7回填：失效详情缓存 vs.cache.Del("video:detail:id=%d")
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

	// 阶段7回填：逐条失效详情缓存（批量删除这里要考虑 pipeline，不是 N 次 Del）
	return DeleteBatchResponse{
		Deleted:    len(owned),
		DeletedIDs: owned,
		SkippedIDs: skipped,
	}, nil
}

// ListByAuthorID 某作者的视频列表（时间倒序，200 条封顶）
func (vs *VideoService) ListByAuthorID(ctx context.Context, authorID uint) ([]Video, error) {
	return vs.repo.ListByAuthorID(ctx, int64(authorID))
}

// GetDetail 详情。
// 阶段7回填：先查 Redis（50ms 超时），未命中时加防击穿锁（Lock→DoubleCheck→回填），
// 拿不到锁的请求短暂等待读缓存，最终都回落本方法直查 DB
func (vs *VideoService) GetDetail(ctx context.Context, id uint) (*Video, error) {
	return vs.repo.GetByID(ctx, id)
}

// UpdateLikesCount 用指定值覆盖计数（阶段4 LikeWorker 用）
func (vs *VideoService) UpdateLikesCount(ctx context.Context, id uint, likesCount int64) error {
	return vs.repo.UpdateLikesCount(ctx, id, likesCount)
}

// UpdatePopularity 热度增减（数据库端原子 + 下限0）。
// 阶段8/9回填：先试 popularityMQ 事件；失败或未启用时写 Redis 热榜分钟桶（ZincrBy + 2h 过期）
func (vs *VideoService) UpdatePopularity(ctx context.Context, id uint, change int64) error {
	return vs.repo.UpdatePopularity(ctx, id, change)
}
