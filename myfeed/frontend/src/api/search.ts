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
 *                  所以走了 LIKE 全表扫。**单字搜索靠的就是这条**，是预期行为
 * - `like-fallback` 查询词**够长却零命中**，词法退成 LIKE 兜底 —— 这一档要
 *                  当成**索引可疑**的告警看：ngram 的停用词过滤把 bigram
 *                  削掉了（"java" 的 ja/av/va 全含字母 a，一个都不剩）、
 *                  ngram_token_size 被人改过、或者索引建完没重建。
 *                  它和 `like` 都会返回结果，区别在于**排查方向完全不同**：
 *                  `like` 没事可修，`like-fallback` 说明索引该重建了。
 *                  后端那边对应 video/search_index.go 的坑 #2 / #5
 * - `lexical-only` 向量那一路没启用或挂了（本轮恒是这个之外的取值，见下）
 * - `vector-only`  词法那一路不可用（FULLTEXT 索引没建成功）
 *
 * ⚠ **本轮实际只会看到 `lexical-only` 一个值。** 不是这里列错了，是后端
 * service.go 的那段 switch：它只判断"哪几路跑了"，`runVec` 为 false 时
 * 直接给 `lexical-only` 并**丢掉词法那一路自己的形态**。
 * 而本轮向量那一路没接线（router.go 里 `NewService(lex, nil, nil, nil, ...)`
 * 那三个 nil），所以 runVec 恒为 false —— 上面 ngram / ngram-or / like /
 * like-fallback 这四个在调试面板上**一次都不会出现**。
 *
 * 这条**已知**：它让"搜索为什么搜不到"这类问题少了一个现成的读数
 * （真拿到过 `like-fallback` 的话，一眼就能看出索引可疑）。
 * 要修就得让 mode 能同时表达"哪几路跑了"和"词法那一路跑成什么形态"，
 * 那是给 Result 加字段、并且要连着改 cursor（Mode 会被冻结进游标）——
 * 不是一行的事，先记在这里不顺手改。
 */
export type SearchMode = 'ngram' | 'ngram-or' | 'like' | 'like-fallback' | 'lexical-only' | 'vector-only'

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
