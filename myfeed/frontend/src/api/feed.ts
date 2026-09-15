// Feed 模块 API。
// 四个流（最新/点赞榜/热门榜/关注）+ 一个标签流。
//
// **前四个里只有「关注」需要登录**：/feed 整组挂的是 SoftJWTAuth（有 token 就识别身份，
// 没有也放行），而 /feed/listByFollowing 额外叠了一层 JWTAuth ——
// 因为"我关注的人"必须先有"我"。所以这一个传 authRequired: true，其余都不传，
// 这正是"游客能刷流、但不能看关注流"的前端体现。

import { postJson } from './client'

export interface FeedAuthor {
  id: number
  username: string
}

export interface FeedVideoItem {
  id: number
  author: FeedAuthor
  title: string
  description?: string
  play_url: string
  cover_url: string
  create_time: number // Unix 秒（后端 .Unix()）
  likes_count: number
  is_liked: boolean // 阶段4已接入：登录用户是真值；游客恒 false（后端拿不到"我"是谁）

  /**
   * 转码状态：""（改造前的存量行）/ "pending" / "running" / "ready" / "failed" / "skipped"。
   *
   * 六个取值的完整语义见后端 entity.go；播放端只关心**一个**：`=== 'ready'`。
   * 其余全部走直传老路（`skipped` 是"门禁判定不需要转码"，走直传是它的正常态，
   * 不是降级）。
   *
   * 为什么是可选（`?`）而不是必填：这个接口的响应来自 `/feed/*`，
   * 而 `/feed/*` 用的是**另一个**实体类型（feed 包的），不保证带这两个字段。
   * 详情页那条路（`/video/getDetail`，裸 Video struct → 一定带）
   * 由 `api/video.ts` 的 `VideoItem` 负责，那边是必填。
   */
  transcode_status?: string

  /**
   * HLS 的 master playlist 路径（`/static/hls/<id>/master.m3u8`），**非空即代表可以走 HLS**。
   *
   * 后端的不变量：这一列**只在 `transcode_status='ready'` 时**才非空
   * （转码开始 / 失败都显式写空串，见 transcode_worker.go）。
   * 所以播放端判断"能不能用 HLS"只认它一个，不必再去看状态 ——
   * 两个字段各判一次的话，早晚会出现两边不一致的分支，
   * 而那个分支的表现是"明明转好了却在直传"或者反过来"指向一个不存在的 playlist"。
   *
   * 它是**路径**不是完整 URL（后端刻意这么存，换域名/CDN 时只改前端一处）。
   * 纯路径、不带 query/hash —— 这一点是必须的，见 `staticURL` 的说明。
   */
  hls_url?: string
}

// ---- 最新流：单值游标（毫秒） ----

export interface ListLatestResponse {
  video_list: FeedVideoItem[]
  next_time: number // 本页最后一条的 create_time 毫秒数
  has_more: boolean
}

export function listLatest(input: { limit: number; latest_time: number }) {
  return postJson<ListLatestResponse>('/feed/listLatest', input)
}

// ---- 点赞榜：复合游标（likes_count, id） ----

export interface ListLikesCountResponse {
  video_list: FeedVideoItem[]
  next_likes_count_before?: number
  next_id_before?: number
  has_more: boolean
}

export function listLikesCount(input: {
  limit: number
  likes_count_before?: number
  id_before?: number
}) {
  const body: Record<string, unknown> = { limit: input.limit }
  // 后端要求两个游标"要么全给要么全不给"，只给一半会 400。
  // 这里统一成对发送，缺的那个补 0——和原项目前端一致
  if (typeof input.likes_count_before === 'number' || typeof input.id_before === 'number') {
    body.likes_count_before = input.likes_count_before ?? 0
    body.id_before = input.id_before ?? 0
  }
  return postJson<ListLikesCountResponse>('/feed/listLikesCount', body)
}

// ---- 热门榜：三键游标（popularity, create_time, id） ----

export interface ListByPopularityResponse {
  video_list: FeedVideoItem[]
  as_of: number // 阶段8 Redis 快照才有效，现在恒 0
  next_offset: number
  has_more: boolean
  next_latest_popularity?: number
  next_latest_before?: string // RFC3339 字符串！原样回传，不要自己解析再格式化
  next_latest_id_before?: number
}

export function listByPopularity(input: {
  limit: number
  as_of?: number
  offset?: number
  latest_popularity?: number
  latest_before?: string
  latest_id_before?: number
}) {
  const body: Record<string, unknown> = {
    limit: input.limit,
    as_of: input.as_of ?? 0,
    offset: input.offset ?? 0,
  }
  // 三件套同样必须齐全。注意 latest_before 是 time.Time 字段，
  // 传空字符串会解析失败报 400——所以首页必须整个字段不发，而不是发 ""
  if (input.latest_before && typeof input.latest_id_before === 'number') {
    body.latest_popularity = input.latest_popularity ?? 0
    body.latest_before = input.latest_before
    body.latest_id_before = input.latest_id_before
  }
  return postJson<ListByPopularityResponse>('/feed/listByPopularity', body)
}

// ---- 关注流（阶段6）：唯一一条必须登录的 ----

export interface ListByFollowingResponse {
  video_list: FeedVideoItem[]
  next_time: number // 毫秒，和 listLatest 同一套
  has_more: boolean
}

/**
 * 我关注的人发的最新视频。
 *
 * **必须登录**（authRequired: true），这是 /feed 下唯一一条 ——
 * router.go 里它挂在一个额外的空路径子组上，那个子组又叠了一层 JWTAuth。
 *
 * 游客**不要发这个请求**。不是因为浪费：client.ts 的 handleResponse
 * 对**任何** 401 都会 auth.clearTokens()，游客打一次就把本地残留的、
 * 已经过期的 token 状态搅一遍。界面上（FeedView 的分段控件、桌面频道条）
 * 已经按登录状态把它藏起来了，这里是第二道。
 *
 * 游标单位是**毫秒**，和 listLatest 完全一致 —— 后端刻意改的（原项目这里是秒），
 * 为的就是这两条流能共用 useFeedStream 里同一段游标代码，不用记两个单位。
 */
export function listByFollowing(input: { limit: number; latest_time: number }) {
  return postJson<ListByFollowingResponse>('/feed/listByFollowing', input, {
    authRequired: true,
  })
}

// ---- 标签流：本阶段无游标，只取最新 N 条 ----

export function listByTag(tagName: string, limit = 20) {
  return postJson<{ video_list: FeedVideoItem[] }>('/feed/listByTag', {
    tag_name: tagName,
    limit,
  })
}
