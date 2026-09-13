// 点赞模块 API（阶段4）。四个接口**全部**挂在 JWT 分组下，游客一个都用不了 ——
// 所以每个都带 authRequired: true。
//
// 为什么四个都带：点赞的语义是"**我**赞了这个视频"，没有"我"就无从谈起。
// 对比 /feed/* 挂的是 SoftJWTAuth（游客能刷流，只是 is_liked 恒 false），
// 而 /like/* 在 router.go 里挂的是 JWTAuth —— 没 token 直接 401 掐断。
// 前端这边 authRequired 只是省掉一次注定 401 的请求，**不是**安全边界。

import { postJson } from './client'
import type { VideoItem } from './video'

export function likeVideo(videoID: number) {
  return postJson<{ message: string }>('/like/like', { video_id: videoID }, { authRequired: true })
}

export function unlikeVideo(videoID: number) {
  return postJson<{ message: string }>('/like/unlike', { video_id: videoID }, { authRequired: true })
}

/**
 * 单个视频的点赞状态。
 *
 * 详情页**必须**单独调它：`/video/getDetail` 返回的是 videos 表那一行，
 * 里面没有"我赞过没有"—— 那是 likes 表和"我"的交叉信息，不属于视频本身。
 */
export function isLiked(videoID: number) {
  return postJson<{ is_liked: boolean }>('/like/isLiked', { video_id: videoID }, { authRequired: true })
}

/**
 * 我的点赞列表。
 *
 * 两个形状上的坑：
 *
 *   1. **没有请求体**。后端 handler 刻意不 bind（空 body 调 ShouldBindJSON 会返回 EOF，
 *      把查询变成 400）。这里发一个 `{}` 就行，字段一个都不需要。
 *   2. 返回的是 **VideoItem[]**（videos 表的原始形状：扁平的 `username` +
 *      RFC3339 的 `create_time`），**不是** FeedVideoItem。所以调用方必须过一遍
 *      `normalizeVideo`，否则时间会变成 "Invalid Date"、作者名会是 undefined。
 *      原因是这个接口在 Go 里返回 `[]video.Video`，而 FeedVideoItem 是 feed 包
 *      自己组装的另一套形状。
 *
 * 后端硬编码 Limit(200) 且没有游标，所以它是**一次性**拉取，没有翻页。
 */
export function listMyLikedVideos() {
  return postJson<VideoItem[]>('/like/listMyLikedVideos', {}, { authRequired: true })
}
