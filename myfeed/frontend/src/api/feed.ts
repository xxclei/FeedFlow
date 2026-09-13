// Feed 模块 API。
// 三个流 + 一个标签流，全部匿名可访问（后端挂的是 SoftJWTAuth：有 token 就识别身份，
// 没有也放行）。注意这里都不传 authRequired——这正是"游客能刷流"的前端体现。

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
  is_liked: boolean // 阶段4回填，当前恒 false
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

// ---- 标签流：本阶段无游标，只取最新 N 条 ----

export function listByTag(tagName: string, limit = 20) {
  return postJson<{ video_list: FeedVideoItem[] }>('/feed/listByTag', {
    tag_name: tagName,
    limit,
  })
}
