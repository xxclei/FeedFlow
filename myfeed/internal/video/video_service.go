package video

import (
	"context"
	"errors"
	"strings"

	"myfeed/internal/apierror"

	"gorm.io/gorm"
)

// VideoService 视频业务：发布事务、详情查询、计数变更。
// cache 阶段7回填（GetDetail 防击穿缓存）、popularityMQ 阶段9回填（热度事件）
type VideoService struct {
	repo *VideoRepository
}

func NewVideoService(repo *VideoRepository) *VideoService {
	return &VideoService{repo: repo}
}

// Publish 发布：校验 → 一个事务里做三件事：
//
//	① 插 videos        业务本体
//	② 插 outbox_msgs   事件信存根（阶段9 Poller 搬运到 MQ；和①同事务=要么都在要么都不在）
//	③ 解析 #标签 并建 tags/video_tags 关联
//
// 注意：这里直接用 vs.repo.db 开事务、在事务里写 SQL——原项目的分层瑕疵
// （service 越过 repo 摸库）。更干净的做法是把这三步下沉成 repo 的一个
// PublishTx 方法；我们保持对齐原项目，但你要知道这个味道在哪。
func (vs *VideoService) Publish(ctx context.Context, video *Video) error {
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

		// ③ 标签：从标题+描述里解析 #xxx，标签去重建（FirstOrCreate），
		//    再建视频↔标签关联。任何一步失败 → 整个事务回滚
		tags := ExtractTags(video.Title + " " + video.Description)
		for _, tagName := range tags {
			var tag Tag
			if err := tx.Where("name = ?", tagName).FirstOrCreate(&tag, Tag{Name: tagName}).Error; err != nil {
				return err
			}
			if err := tx.Create(&VideoTag{VideoID: video.ID, TagID: tag.ID}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// Delete 删除：只有作者本人能删
func (vs *VideoService) Delete(ctx context.Context, id uint, authorID uint) error {
	video, err := vs.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if video == nil {
		return errors.New("video not found")
	}
	if video.AuthorID != authorID {
		return apierror.ErrUnauthorized
	}
	if err := vs.repo.DeleteVideo(ctx, id); err != nil {
		return err
	}
	// 阶段7回填：失效详情缓存 vs.cache.Del("video:detail:id=%d")
	return nil
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
