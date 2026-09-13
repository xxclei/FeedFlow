// 搜索 API：POST /feed/search（阶段7 本轮先落词法那一路）
//
// **不传 authRequired**：/feed 整组挂的是 SoftJWTAuth，游客也能搜 ——
// 和 listLatest / listByTag 同类。传了反而会让登出状态下的搜索直接 401，
// 而 client.ts 对任何 401 都会 clearTokens()，游客搜一次就被"登出"一次。

import { postJson } from './client'
import type { FeedVideoItem } from './feed'

/**
 * 本次搜索**实际跑成的形态**。后端如实回传，不是前端猜的。
 *
 * - `ngram`        词法跑了 FULLTEXT + ngram，且每个词都必须出现（AND）
 * - `ngram-or`     AND 命中不够，退成了 OR（更宽，相关度也更松）
 * - `like`         查询词太短（< ngram_token_size=2），FULLTEXT 索引永远匹配不到，
 *                  所以走了 LIKE 全表扫。**单字搜索靠的就是这条**
 * - `lexical-only` 向量那一路没启用或挂了（本轮恒是这个之外的取值，见下）
 * - `vector-only`  词法那一路不可用（FULLTEXT 索引没建成功）
 *
 * 本轮后端只接了词法那一路，所以**实际只会出现前三个**。
 */
export type SearchMode = 'ngram' | 'ngram-or' | 'like' | 'lexical-only' | 'vector-only'

/** 两路各召回了多少条候选。这一页最有价值的数字就是它 */
export interface SearchArms {
  lexical: number
  vector: number
}

export interface SearchResponse {
  video_list: FeedVideoItem[]
  /** 不透明令牌，原样回传即可；没有下一页时后端不发这个字段 */
  next_cursor?: string
  has_more: boolean
  mode: SearchMode
  arms: SearchArms
  /** 冻结列表的总长度（可翻的候选数，不是精确命中数） */
  total: number
}

export interface SearchInput {
  query: string
  /** 上一页的 next_cursor。首页**不要传**（传空串也行，后端按空串当首页） */
  cursor?: string
  limit?: number
}

/**
 * 搜索。
 *
 * ---------- 两种失败要分开处理 ----------
 *
 * - **400**：游标脏了，或者查询里一个字母/数字都没有（只打了标点）。
 *   前者的正确处理是"**从头搜一次**"，不是重试同一个令牌 —— 重试一万次都是同样的 400。
 *   后端刻意回 400 而不是空列表，因为空列表的语义是"没有这个视频"。
 * - **5xx**：两路召回都不可用（本轮 = FULLTEXT 索引没建成功且没有向量兜底）。
 *   这才是真的故障。注意它**不会**因为"查询词没人匹配"而出现。
 */
export function searchVideos(input: SearchInput) {
  return postJson<SearchResponse>('/feed/search', {
    query: input.query,
    cursor: input.cursor ?? '',
    limit: input.limit ?? 20,
  })
}
