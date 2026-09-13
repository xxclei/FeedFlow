package feed

import (
	"context"
	"errors"
	"time"

	"myfeed/internal/search"
	"myfeed/internal/video"
)

// FeedService Feed 业务：游标编解码、limit 归一化、响应组装。
// Redis/localcache/singleflight 阶段7回填
// （GetVideoByIDs 三级缓存 + ListLatest 冷热分离主路径）
type FeedService struct {
	repo *FeedRepository

	// likeRepo 是阶段4 接进来的（is_liked 批量查询）。
	// 它属于 video 包而不是 feed 包：likes 表是视频模块的数据，
	// feed 只是**消费**方 —— 所以这里依赖的是 video.LikeRepository，
	// feed 自己没有也不该有 likes 的 repo。
	likeRepo *video.LikeRepository

	// searchSvc 本轮接进来的混合检索。它为 nil 时 Search 返回错误，
	// 而不是静默返回空结果（"搜索坏了"和"没有结果"必须能分开）
	searchSvc *search.Service
}

func NewFeedService(repo *FeedRepository, likeRepo *video.LikeRepository, searchSvc *search.Service) *FeedService {
	return &FeedService{repo: repo, likeRepo: likeRepo, searchSvc: searchSvc}
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

// ListByFollowing 关注流（阶段6）。协议和 ListLatest 完全一样，只有数据源不同：
// 一个查全表，一个先用子查询收窄到"我关注的人"。
//
// **游标单位：毫秒，和 ListLatest 一致。**
//
// 这里和原项目**不一致**，是刻意改的。原项目的 listByFollowing 请求收 `latest_time`
// 是 **Unix 秒**（`time.Unix(req.LatestTime, 0)`）、响应 `next_time` 也是秒
// （`CreateTime.Unix()`），而 listLatest 两个方向都是毫秒。同一个字段名、
// 同一种游标语义，两条流用了两个单位。
//
// 为什么不能照抄：这两个流的游标在**前端共用同一个 composable**（useFeedStream），
// 传出去再传回来是一模一样的代码路径。后端一个毫秒一个秒，前端就得为每条流记一个
// 单位常量，或者更糟 —— 在某处悄悄漏掉换算，于是翻页跳过或重复若干条，
// 而且**只在第二页之后才看得出来**（第一页不传游标，永远是对的）。
// 他的 feed/service.go 里已经有一处被这个单位混乱咬过的注释
// （"注意：这里是秒！和 latest 游标的毫秒不同（原项目的时间单位瑕疵）"）。
//
// 所以这里统一成毫秒：和 ListLatest 的请求、响应、以及 FeedVideoItem.CreateTime 的
// 上游值同一套。**唯一没统一的是 FeedVideoItem.CreateTime 本身（秒）** ——
// 那是给前端 formatTime(sec) 用的展示值，不是游标，两者本来就是两回事。
func (f *FeedService) ListByFollowing(ctx context.Context, limit int, latestBefore time.Time, viewerAccountID uint) (ListByFollowingResponse, error) {
	videos, err := f.repo.ListByFollowing(ctx, limit, viewerAccountID, latestBefore)
	if err != nil {
		return ListByFollowingResponse{}, err
	}
	feedVideos, err := f.buildFeedVideos(ctx, videos, viewerAccountID)
	if err != nil {
		return ListByFollowingResponse{}, err
	}

	// 书签 = 本页最后一条的**毫秒**时间戳（和 ListLatest 一模一样）
	var nextTime int64
	if len(videos) > 0 {
		nextTime = videos[len(videos)-1].CreateTime.UnixMilli()
	}

	return ListByFollowingResponse{
		VideoList: feedVideos,
		NextTime:  nextTime,
		HasMore:   len(videos) == limit, // 满页才认为可能还有下一页
	}, nil
}

// ListByTag 标签流
func (f *FeedService) ListByTag(ctx context.Context, tagName string, limit int, viewerAccountID uint) ([]FeedVideoItem, error) {
	videos, err := f.repo.ListByTag(ctx, tagName, limit)
	if err != nil {
		return nil, err
	}
	return f.buildFeedVideos(ctx, videos, viewerAccountID)
}

// Search 混合检索：两路召回 → RRF 融合 → 取详情 → 组装。
//
// 入口放在 feed 包而不是 search 包，唯一的理由就是下面这一步：
// **搜索结果必须是 FeedVideoItem**（带 is_liked、带 author）。
// 在 search 包里组装的话，is_liked 的批量查询、匿名降级、
// nil 切片兜底都得再写一遍 —— 而这个项目所有流的出口都收敛在 buildFeedVideos，
// 搜索没有理由成为例外。
func (f *FeedService) Search(ctx context.Context, q search.Query, cur *search.Cursor, limit int, viewerAccountID uint) (SearchResponse, error) {
	if f.searchSvc == nil {
		// 漏接线时**报错**而不是返回空列表：返回空列表的话，
		// 搜索页会显示"没有找到相关视频"，让人以为是语料问题
		return SearchResponse{}, errors.New("搜索服务未启用")
	}

	res, nextCursor, err := f.searchSvc.Search(ctx, q, cur, limit)
	if err != nil {
		return SearchResponse{}, err
	}

	items, err := f.hydrateInOrder(ctx, res.IDs, viewerAccountID)
	if err != nil {
		return SearchResponse{}, err
	}

	return SearchResponse{
		VideoList:  items,
		NextCursor: nextCursor,
		HasMore:    nextCursor != "",
		Mode:       res.Mode,
		Arms:       res.Arms,
		Total:      res.Total,
	}, nil
}

// hydrateInOrder 按给定顺序取视频详情并组装。**搜索最容易出的 bug 就在这里**。
//
// GetByIDs 用的是 `WHERE id IN ?`，它返回的是 **MySQL 觉得方便的**顺序
// （通常是主键顺序），**不是融合顺序**。所以必须自己重排一次。
//
// 不重排的后果特别隐蔽：相关度排序会静默退化成一团乱序，而且
// ——不报错、不返回空、页面上有内容、只是"排序看起来不太准"。
// 没有人会为"排序不太准"去查代码。这一条和 feed/repo.go 顶部那句
// "排序键和 WHERE 游标键必须是同一组"是同一类纪律的两个面。
//
// 顺带说明为什么用 map 而不是保持两个切片对齐：**GetByIDs 可能少返回**。
// 冻结列表是第 1 页算出来的快照，翻到第 3 页时其中某条视频可能已经被删了 ——
// 那时它查不出来，只能丢掉（而不是填一个空洞）。表现是这一页少一条，
// 属于快照语义的固有代价。
func (f *FeedService) hydrateInOrder(ctx context.Context, ids []uint, viewerAccountID uint) ([]FeedVideoItem, error) {
	videos, err := f.repo.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[uint]*video.Video, len(videos))
	for _, v := range videos {
		byID[v.ID] = v
	}
	ordered := make([]*video.Video, 0, len(videos))
	for _, id := range ids {
		if v, ok := byID[id]; ok {
			ordered = append(ordered, v)
		}
	}
	return f.buildFeedVideos(ctx, ordered, viewerAccountID)
}

// buildFeedVideos 组装器：把 video.Video 列表翻译成前端要的 FeedVideoItem 列表。
// 所有 Feed 接口共用这一个出口，is_liked 的接入点也在这里
//
// 不 nil-guard f.likeRepo：漏接线时应该当场 panic，而不是静默退回"is_liked 恒 false"
// —— 那正是阶段3 的旧行为，会让人以为功能没坏（"我明明赞了，怎么还是空心"）。
func (f *FeedService) buildFeedVideos(ctx context.Context, videos []*video.Video, viewerAccountID uint) ([]FeedVideoItem, error) {
	// 阶段3 留的口子在这里堵上：先收齐本页的 video id，**一次 IN 查**把这批视频
	// 的"我赞过没有"全部拿回来。逐条调 IsLiked 就是 N+1 —— 一页几十条要几十次往返。
	//
	// 匿名/未登录（viewerAccountID == 0）时 repo 直接返回空 map，
	// 取值天然落成 false，所以这里**不需要**为游客写任何分支
	ids := make([]uint, 0, len(videos))
	for _, v := range videos {
		ids = append(ids, v.ID)
	}
	likedMap, err := f.likeRepo.BatchGetLiked(ctx, ids, viewerAccountID)
	if err != nil {
		return nil, err
	}

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
			// 取值不需要判"在不在 map 里"：BatchGetLiked 返回的是非 nil map，
			// 没赞过的 key 不存在 → 零值 false，正好就是我要的答案
			IsLiked: likedMap[video.ID],
		})
	}
	return feedVideos, nil
}
