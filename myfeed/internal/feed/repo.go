package feed

import (
	"context"
	"time"

	"myfeed/internal/social"
	"myfeed/internal/video"

	"gorm.io/gorm"
)

// FeedRepository 读视图层：直接读 videos/tags 表，不拥有任何表。
// 四个查询的共同纪律：ORDER BY 的键和 WHERE 的游标键必须是同一组（镜像），
// 这是"翻页不重不漏"不变量的实现基础
type FeedRepository struct {
	db *gorm.DB
}

func NewFeedRepository(db *gorm.DB) *FeedRepository {
	return &FeedRepository{db: db}
}

// ListLatest 最新流：时间游标。
// latestBefore 为零值 = 第一页（不加工过滤条件）
func (repo *FeedRepository) ListLatest(ctx context.Context, limit int, latestBefore time.Time) ([]*video.Video, error) {
	var videos []*video.Video
	query := repo.db.WithContext(ctx).Model(&video.Video{}).
		Order("create_time DESC") // 排序键
	if !latestBefore.IsZero() {
		query = query.Where("create_time < ?", latestBefore) // ← 镜像：同一键的"严格小于"
	}
	if err := query.Limit(limit).Find(&videos).Error; err != nil {
		return nil, err
	}
	return videos, nil
}

// ListLikesCountWithCursor 点赞榜：复合游标（likes_count, id 双键）。
// 热度相同的视频按 id 从大到小排——id 是唯一键，保证任意两条视频的先后关系
// 永远确定（这是"不重不漏"的另一半：排序必须全序）
func (repo *FeedRepository) ListLikesCountWithCursor(ctx context.Context, limit int, cursor *LikesCountCursor) ([]*video.Video, error) {
	var videos []*video.Video
	query := repo.db.WithContext(ctx).Model(&video.Video{}).
		Order("likes_count DESC, id DESC") // 排序键：双键

	if cursor != nil {
		// 镜像过滤：热度更低的，或同热度但 id 更小的。
		// 少这个 OR 分支，同热度视频就会在翻页时重复/丢失
		query = query.Where(
			"(likes_count < ?) OR (likes_count = ? AND id < ?)",
			cursor.LikesCount,
			cursor.LikesCount, cursor.ID,
		)
	}

	if err := query.Limit(limit).Find(&videos).Error; err != nil {
		return nil, err
	}
	return videos, nil
}

// ListByPopularity 热榜 DB 兜底版：三键游标（popularity, create_time, id）。
// 注意 popularity=0 是合法值，所以"有没有游标"不能用 popularity 判断，
// 必须看 timeBefore 是否为零值 + idBefore>0（完整提供才算有游标）
// 阶段8：主路径换 Redis 快照，本方法是降级通道
func (repo *FeedRepository) ListByPopularity(ctx context.Context, limit int, popularityBefore int64, timeBefore time.Time, idBefore uint) ([]*video.Video, error) {
	var videos []*video.Video
	query := repo.db.WithContext(ctx).Model(&video.Video{}).
		Order("popularity DESC, create_time DESC, id DESC") // 排序键：三键

	if !timeBefore.IsZero() && idBefore > 0 {
		// 三键镜像：层层加细分，直到唯一
		query = query.Where(
			"(popularity < ?) OR (popularity = ? AND create_time < ?) OR (popularity = ? AND create_time = ? AND id < ?)",
			popularityBefore,
			popularityBefore, timeBefore,
			popularityBefore, timeBefore, idBefore,
		)
	}

	if err := query.Limit(limit).Find(&videos).Error; err != nil {
		return nil, err
	}
	return videos, nil
}

// GetByIDs 按 ID 批量取视频（阶段7 时间线 ZSET 吐出 ID 列表后，就是来这里取详情的）
func (repo *FeedRepository) GetByIDs(ctx context.Context, ids []uint) ([]*video.Video, error) {
	var videos []*video.Video
	if len(ids) == 0 {
		return videos, nil
	}
	if err := repo.db.WithContext(ctx).Model(&video.Video{}).
		Where("id IN ?", ids).Find(&videos).Error; err != nil {
		return nil, err
	}
	return videos, nil
}

// ListByTag 标签流：双 JOIN（videos ↔ video_tags ↔ tags），无游标，时间倒序
func (repo *FeedRepository) ListByTag(ctx context.Context, tagName string, limit int) ([]*video.Video, error) {
	var videos []*video.Video
	err := repo.db.WithContext(ctx).Model(&video.Video{}).Table("videos").
		Joins("JOIN video_tags ON video_tags.video_id = videos.id").
		Joins("JOIN tags ON tags.id = video_tags.tag_id").
		Where("tags.name = ?", tagName).
		Order("videos.create_time desc").
		Limit(limit).
		Find(&videos).Error
	return videos, err
}

// ListByFollowing 关注流：子查询"我关注的人" → 时间倒序翻页。
//
// 这是全项目**唯一一条跨模块的 SQL**，也是 feed 包第一次认识别人的表结构：
//
//	SELECT * FROM videos
//	 WHERE author_id IN (SELECT vlogger_id FROM socials WHERE follower_id = ?)
//	   AND create_time < ?
//	 ORDER BY create_time DESC LIMIT ?
//
// 依赖方向 feed → social 发生在**模型层**（import social.Social 只为了拿到表名）。
// 另一种做法是让 social repo 暴露 `FollowingIDs(ctx, followerID) ([]uint, error)`，
// feed 拿到 ID 列表再拼 `IN ?` —— 那样 feed 就不用认识 socials 表了。两种写法
// 各有一个真实的代价：
//
//	子查询下推（本实现）：一次往返；代价是 feed 依赖 social 的**表结构**
//	先查 ID 再 IN：解耦；代价是两次往返 + 关注数很大时 IN 列表很长
//
// 原项目选了前者。注意"少一次往返"在这个查询里的分量：这是一条**翻页**查询，
// 用户每次滚动都要打一次，往返次数直接乘在滑动的流畅度上。
//
// **viewerAccountID > 0 这个判断要盯死。** 它的两种走向差别极大：
//
//	> 0 → 加过滤，只看到关注的人的视频
//	== 0 → **完全不过滤**，退化成"全站最新流"
//
// 也就是说：这个判断写反、或者让 0 流进来，匿名用户就会看到全站内容 ——
// 而"全站最新流"恰好是 /feed/listLatest，看起来完全正常，不会报错。
// 防御不在这一层（repo 老实描述 SQL 的行为），在 **handler**：
// listByFollowing 挂强鉴权，且 GetAccountID 失败时**直接 401，绝不放行**。
//
// 顺带一个正确的边界情况：一个人谁也没关注时，子查询返回**空集**，
// `IN (空集)` 匹配 0 行 → 空列表。这正是想要的（**不是**全站视频）。
// 这个正确性不是我们争取来的，是 SQL 语义自带的 —— 所以"空关注列表"这条
// 验收必测：它一旦坏了，坏法和"忘记加过滤"一模一样。
func (repo *FeedRepository) ListByFollowing(ctx context.Context, limit int, viewerAccountID uint, latestBefore time.Time) ([]*video.Video, error) {
	var videos []*video.Video
	query := repo.db.WithContext(ctx).Model(&video.Video{}).
		Order("create_time DESC") // 排序键，和 ListLatest 一致

	if viewerAccountID > 0 {
		// 子查询**下推给 MySQL**，不在 Go 里先查 ID 再拼 IN（理由见上）
		followingSubQuery := repo.db.WithContext(ctx).
			Model(&social.Social{}).
			Select("vlogger_id").
			Where("follower_id = ?", viewerAccountID)
		query = query.Where("author_id IN (?)", followingSubQuery)
	}

	if !latestBefore.IsZero() {
		query = query.Where("create_time < ?", latestBefore) // 镜像：同一键的"严格小于"
	}

	if err := query.Limit(limit).Find(&videos).Error; err != nil {
		return nil, err
	}
	return videos, nil
}
