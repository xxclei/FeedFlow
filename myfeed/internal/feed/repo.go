package feed

import (
	"context"
	"time"

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

// ListByFollowing 关注流：子查询"我关注的人"→ 倒序翻页。
// 阶段6回填（依赖 social 包，本阶段先不实现）：
//   viewerAccountID > 0 时加 author_id IN (SELECT vlogger_id FROM socials WHERE follower_id = ?)
//   匿名（0）时原项目退化为全局最新流
