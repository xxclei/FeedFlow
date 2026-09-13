package feed

import (
	"context"
	"time"

	"myfeed/internal/video"
)

// FeedService Feed 业务：游标编解码、limit 归一化、响应组装。
// likeRepo 阶段4回填（is_liked 批量查询）；Redis/localcache/singleflight 阶段7回填
// （GetVideoByIDs 三级缓存 + ListLatest 冷热分离主路径）
type FeedService struct {
	repo *FeedRepository
}

func NewFeedService(repo *FeedRepository) *FeedService {
	return &FeedService{repo: repo}
}

// ListLatest 最新流（时间游标，DB 直查版）。
// 阶段7回填：Redis 冷热分离主路径（水位判断/自举重建/边界拼接），
// 那时本逻辑作为缓存未命中与 Redis 故障的兜底路径存在
func (f *FeedService) ListLatest(ctx context.Context, limit int, latestBefore time.Time, viewerAccountID uint) (ListLatestResponse, error) {
	videos, err := f.repo.ListLatest(ctx, limit, latestBefore)
	if err != nil {
		return ListLatestResponse{}, err
	}
	feedVideos, err := f.buildFeedVideos(ctx, videos, viewerAccountID)
	if err != nil {
		return ListLatestResponse{}, err
	}

	// 书签 = 本页最后一条的毫秒时间戳（客户端下一页原样带回来）
	var nextTime int64
	if len(videos) > 0 {
		nextTime = videos[len(videos)-1].CreateTime.UnixMilli()
	}

	// 满页才认为可能还有下一页（近似法：总数恰为 limit 整数倍时，
	// 最后一页会多返回一次空页——原项目接受这个取舍）
	hasMore := len(videos) == limit

	return ListLatestResponse{
		VideoList: feedVideos,
		NextTime:  nextTime,
		HasMore:   hasMore,
	}, nil
}

// ListLikesCount 点赞榜（复合游标）
func (f *FeedService) ListLikesCount(ctx context.Context, limit int, cursor *LikesCountCursor, viewerAccountID uint) (ListLikesCountResponse, error) {
	videos, err := f.repo.ListLikesCountWithCursor(ctx, limit, cursor)
	if err != nil {
		return ListLikesCountResponse{}, err
	}
	feedVideos, err := f.buildFeedVideos(ctx, videos, viewerAccountID)
	if err != nil {
		return ListLikesCountResponse{}, err
	}
	resp := ListLikesCountResponse{
		VideoList: feedVideos,
		HasMore:   len(videos) == limit,
	}
	// 书签 = 本页最后一条的 (点赞数, ID) 二元组，指针形式（omitempty）
	if len(videos) > 0 {
		last := videos[len(videos)-1]
		nextLikesCountBefore := last.LikesCount
		nextIDBefore := last.ID
		resp.NextLikesCountBefore = &nextLikesCountBefore
		resp.NextIDBefore = &nextIDBefore
	}
	return resp, nil
}

// ListByPopularity 热榜。
// 阶段8回填：Redis 分钟桶 ZUnionStore 快照（as_of + offset 稳定分页）为主路径，
// 本 DB 三键游标版作为快照不可用时的降级通道。
// 注意 DB 版没有"快照"语义：AsOf/NextOffset 返回 0，翻页只靠三键游标
func (f *FeedService) ListByPopularity(ctx context.Context, limit int, reqAsOf int64, offset int, viewerAccountID uint, latestPopularity int64, latestBefore time.Time, latestIDBefore uint) (ListByPopularityResponse, error) {
	videos, err := f.repo.ListByPopularity(ctx, limit, latestPopularity, latestBefore, latestIDBefore)
	if err != nil {
		return ListByPopularityResponse{}, err
	}
	items, err := f.buildFeedVideos(ctx, videos, viewerAccountID)
	if err != nil {
		return ListByPopularityResponse{}, err
	}
	resp := ListByPopularityResponse{
		VideoList:  items,
		AsOf:       0,
		NextOffset: 0,
		HasMore:    len(items) == limit,
	}
	if len(videos) > 0 {
		last := videos[len(videos)-1]
		nextPopularity := last.Popularity
		nextBefore := last.CreateTime
		nextID := last.ID
		resp.NextLatestPopularity = &nextPopularity
		resp.NextLatestBefore = &nextBefore
		resp.NextLatestIDBefore = &nextID
	}
	return resp, nil
}

// ListByTag 标签流
func (f *FeedService) ListByTag(ctx context.Context, tagName string, limit int, viewerAccountID uint) ([]FeedVideoItem, error) {
	videos, err := f.repo.ListByTag(ctx, tagName, limit)
	if err != nil {
		return nil, err
	}
	return f.buildFeedVideos(ctx, videos, viewerAccountID)
}

// buildFeedVideos 组装器：把 video.Video 列表翻译成前端要的 FeedVideoItem 列表。
// 所有 Feed 接口共用这一个出口，is_liked 的接入点也在这里
func (f *FeedService) buildFeedVideos(ctx context.Context, videos []*video.Video, viewerAccountID uint) ([]FeedVideoItem, error) {
	feedVideos := make([]FeedVideoItem, 0, len(videos))
	for _, video := range videos {
		feedVideos = append(feedVideos, FeedVideoItem{
			ID:          video.ID,
			Author:      FeedAuthor{ID: video.AuthorID, Username: video.Username},
			Title:       video.Title,
			Description: video.Description,
			PlayURL:     video.PlayURL,
			CoverURL:    video.CoverURL,
			CreateTime:  video.CreateTime.Unix(), // 注意：这里是秒！和 latest 游标的毫秒不同（原项目的时间单位瑕疵）
			LikesCount:  video.LikesCount,
			IsLiked:     false, // 阶段4回填：likeRepo.BatchGetLiked(ctx, ids, viewerAccountID) 批量查后填入
		})
	}
	return feedVideos, nil
}
