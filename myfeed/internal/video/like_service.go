package video

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// LikeService 点赞业务。本阶段（阶段4）只有**一条**路径：直写事务。
//
// 阶段7/9 会插进来另外三个依赖（cache / likeMQ / popularityMQ），那时的顺序是
// "先试 MQ 投递，失败再降级到这里的直写事务"。所以下面这段直写代码**不是临时代码，
// 它是永久的降级路径** —— 阶段9 接完 MQ，它一行都不用删。
// 本阶段不写 MQ 分支（写了也是死代码），等阶段9 再插。
type LikeService struct {
	repo      *LikeRepository
	videoRepo *VideoRepository
}

// NewLikeService 阶段4 只有两个依赖。
// 文档给的签名是 5 个参数（后三个传 nil），但 rediscache / rabbitmq 两个包现在
// 还不存在、类型都没有，写不出来；阶段7/9 建包时再把参数补上。
func NewLikeService(repo *LikeRepository, videoRepo *VideoRepository) *LikeService {
	return &LikeService{repo: repo, videoRepo: videoRepo}
}

// Like 点赞。三层保险，一层比一层强：
//
//	① 参数校验                    挡畸形请求
//	② 存在性预检 + 已赞预检        挡 99% 的重复点击，给用户友好文案（**只是体验**）
//	③ 事务内存在性检查 + 唯一索引   真正保住数据正确性的那一层
//
// ② 和 ③ 看着重复，但解决的不是同一件事：预检和插入之间永远存在时间窗 ——
// 两个人同时点、或者点下去的那一瞬间视频正好被作者删了，只有 ③ 挡得住。
// 这就是全项目那句"预检只是体验、约束才是防线"。
func (s *LikeService) Like(ctx context.Context, like *Like) error {
	if like == nil || like.VideoID == 0 || like.AccountID == 0 {
		return errors.New("video_id and account_id are required")
	}

	// 预检①：视频在不在。不进事务就能给出友好错误，省一次开事务
	exist, err := s.videoRepo.IsExist(ctx, like.VideoID)
	if err != nil {
		return err
	}
	if !exist {
		return errors.New("video not found")
	}

	// 预检②：是不是已经赞过。挡住重复点击，文案是"你已经赞过了"而不是一句 1062
	liked, err := s.repo.IsLiked(ctx, like.VideoID, like.AccountID)
	if err != nil {
		return err
	}
	if liked {
		return errors.New("user has liked this video")
	}

	// 点赞时刻：显式赋值，不交给 GORM 的默认填充。
	// 理由在阶段9 —— Worker 从 MQ 消息里带的是事件发生时刻 occurred_at，
	// 那时必须能覆盖这个字段（见 like_entity.go 上的说明）。
	like.CreatedAt = time.Now()

	return s.repo.Transaction(ctx, func(tx *gorm.DB) error {
		// 事务内**再查一次**存在性。这一次才是防线，上面的预检只是体验：
		// 从预检到这里之间，视频可能已经被删了。查不到就 return error → 整个事务
		// 回滚，不会留下一条指向不存在视频的点赞流水。
		//
		// 这道检查本质上是在**手工模拟外键**：我们的表没有 FK（GORM 不会自己加），
		// "likes.video_id 一定指向一条存在的 videos" 这件事没有任何人替我们保证。
		exist, err := s.videoRepo.IsExistTx(tx, like.VideoID)
		if err != nil {
			return err
		}
		if !exist {
			return errors.New("video not found")
		}

		// 插流水。并发双击时**这一步**会撞唯一索引 1062 —— 注意必须把它翻译成
		// 业务错误并 return 出去：忘了 return（或只 log 一句继续往下走），
		// GORM 会把事务 commit 掉，于是"插入失败、计数却 +1"，而且流水里没痕迹。
		if err := s.repo.LikeTx(tx, like); err != nil {
			if isDupKey(err) {
				return errors.New("user has liked this video")
			}
			return err
		}

		// 计数：**相对更新**，传的是增量（+1）不是目标值。
		// `SET likes_count = likes_count + 1` 是单条 SQL，InnoDB 对同一行的 UPDATE
		// 由行锁串行化，N 个并发结果必定是 +N。
		// 写成"读出来 +1 再写回"或者 `SET likes_count = 3` 都会丢更新。
		if err := s.videoRepo.ChangeLikesCountTx(tx, like.VideoID, +1); err != nil {
			return err
		}
		return s.videoRepo.ChangePopularityTx(tx, like.VideoID, +1)
	})
}

// Unlike 取消点赞。和 Like 几乎对称，但有三处**刻意的不对称**，每一处都有理由：
//
//	① 预检方向反过来：没赞过就不能取消
//	② 删除必须看 RowsAffected —— 删 0 行不是错误，不看它计数会凭空变少
//	③ **不需要**事务内再查一次存在性（Like 需要）—— 见下面的解释
func (s *LikeService) Unlike(ctx context.Context, like *Like) error {
	if like == nil || like.VideoID == 0 || like.AccountID == 0 {
		return errors.New("video_id and account_id are required")
	}

	exist, err := s.videoRepo.IsExist(ctx, like.VideoID)
	if err != nil {
		return err
	}
	if !exist {
		return errors.New("video not found")
	}

	liked, err := s.repo.IsLiked(ctx, like.VideoID, like.AccountID)
	if err != nil {
		return err
	}
	if !liked {
		return errors.New("user has not liked this video")
	}

	return s.repo.Transaction(ctx, func(tx *gorm.DB) error {
		// ③ 为什么这里不查存在性：取消点赞在"视频已被删"的情况下是**无害**的 ——
		// 删掉那条流水，两条计数 UPDATE 命中 0 行、不报错也不改任何东西，结果自洽。
		// 而点赞在视频不存在时是**有害**的：会留下一条孤儿流水（没有 FK 拦它）。
		// 不对称的根源是"插入"和"删除"对不存在的外键反应不同。
		deleted, err := s.repo.DeleteByVideoAndAccountTx(tx, like.VideoID, like.AccountID)
		if err != nil {
			return err
		}
		// ② 删 0 行 = 本来就没点过（并发下另一个请求刚好先删掉了）。
		// 必须报错回滚：计数不能再减，否则同一次取消被减两遍。
		// 注意 GREATEST(x-1, 0) **挡不住这个** —— 它只防负数，防不了凭空变少。
		if !deleted {
			return errors.New("user has not liked this video")
		}

		if err := s.videoRepo.ChangeLikesCountTx(tx, like.VideoID, -1); err != nil {
			return err
		}
		return s.videoRepo.ChangePopularityTx(tx, like.VideoID, -1)
	})
}

// IsLiked 单个视频的点赞状态，纯透传。
//
// **对不存在的视频返回 false 而不报错**（原项目的产品行为，不是漏判）：这是查询接口，
// 幂等地回答"你赞过吗"，答案是"没有"，没必要为查不到的视频单独报错。
//
// 注意别和另一件事混了：`video_id <= 0`（缺失/传 0）是**参数问题**，由 handler
// 返回 400 `video_id is required`，走不到这里；这里处理的是"id 合法但视频不存在"。
func (s *LikeService) IsLiked(ctx context.Context, videoID, accountID uint) (bool, error) {
	return s.repo.IsLiked(ctx, videoID, accountID)
}

// ListLikedVideos 我的点赞列表（repo 侧按点赞时间倒序 + Limit(200)）。
//
// nil 切片 → []Video{} 的转换放在 handler，和 feed 那边 nonNilFeedVideoItems 同一层，
// 保持"JSON 形状的兜底统一在 HTTP 层"这条纪律。
func (s *LikeService) ListLikedVideos(ctx context.Context, accountID uint) ([]Video, error) {
	return s.repo.ListLikedVideos(ctx, accountID)
}
