package video

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

type VideoRepository struct {
	db *gorm.DB
}

func NewVideoRepository(db *gorm.DB) *VideoRepository {
	return &VideoRepository{db: db}
}

func (vr *VideoRepository) CreateVideo(ctx context.Context, video *Video) error {
	return vr.db.WithContext(ctx).Create(video).Error
}

// CreateMsg 写事件信存根（publish 事务里用）
func (vr *VideoRepository) CreateMsg(ctx context.Context, msg *OutboxMsg) error {
	return vr.db.WithContext(ctx).Create(msg).Error
}

func (vr *VideoRepository) DeleteVideo(ctx context.Context, id uint) error {
	return vr.db.WithContext(ctx).Delete(&Video{}, id).Error
}

func (vr *VideoRepository) ListByAuthorID(ctx context.Context, authorID int64) ([]Video, error) {
	var videos []Video
	if err := vr.db.WithContext(ctx).
		Where("author_id = ?", authorID).
		Order("create_time desc").
		Limit(200).
		Find(&videos).Error; err != nil {
		return nil, err
	}
	return videos, nil
}

func (vr *VideoRepository) GetByID(ctx context.Context, id uint) (*Video, error) {
	var video Video
	if err := vr.db.WithContext(ctx).First(&video, id).Error; err != nil {
		return nil, err
	}
	return &video, nil
}

func (vr *VideoRepository) UpdateLikesCount(ctx context.Context, id uint, likesCount int64) error {
	return vr.db.WithContext(ctx).Model(&Video{}).
		Where("id = ?", id).
		Update("likes_count", likesCount).Error
}

// IsExist 把"查无此视频"翻译成 bool（调用方不用碰 gorm.ErrRecordNotFound）
func (vr *VideoRepository) IsExist(ctx context.Context, id uint) (bool, error) {
	var video Video
	if err := vr.db.WithContext(ctx).First(&video, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// UpdatePopularity 数据库端原子增减：UPDATE videos SET popularity = popularity + ?
func (vr *VideoRepository) UpdatePopularity(ctx context.Context, id uint, change int64) error {
	return vr.db.WithContext(ctx).Model(&Video{}).
		Where("id = ?", id).
		Update("popularity", gorm.Expr("popularity + ?", change)).Error
}

// ChangeLikesCount 计数增减且不许为负（取消点赞后最低归 0）
func (vr *VideoRepository) ChangeLikesCount(ctx context.Context, id uint, change int64) error {
	return vr.db.WithContext(ctx).Model(&Video{}).
		Where("id = ?", id).
		UpdateColumn("likes_count", gorm.Expr("GREATEST(likes_count + ?, 0)", change)).Error
}

func (vr *VideoRepository) ChangePopularity(ctx context.Context, id uint, change int64) error {
	return vr.db.WithContext(ctx).Model(&Video{}).
		Where("id = ?", id).
		UpdateColumn("popularity", gorm.Expr("GREATEST(popularity + ?, 0)", change)).Error
}

// CountByAuthor 该作者的视频总数（getProfile 聚合用，阶段6）
func (vr *VideoRepository) CountByAuthor(ctx context.Context, authorID uint) (int64, error) {
	var count int64
	if err := vr.db.WithContext(ctx).Model(&Video{}).Where("author_id = ?", authorID).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// TotalLikesByAuthor 该作者全部视频的点赞总数（COALESCE：没视频时 SUM 返回 NULL，兜成 0）
func (vr *VideoRepository) TotalLikesByAuthor(ctx context.Context, authorID uint) (int64, error) {
	var total int64
	if err := vr.db.WithContext(ctx).Model(&Video{}).Where("author_id = ?", authorID).Select("COALESCE(SUM(likes_count), 0)").Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}
