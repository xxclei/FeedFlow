// 关注模块 API（阶段6）。
//
// **五个接口全部挂 JWT** —— router.go 里整个 /social 组都在 JWTAuth 之下，
// 没有一条公开的。所以这里每个函数都带 authRequired: true。
//
// 为什么关注不能匿名：关注关系的语义就是"**我**关注了他"，没有"我"这个问句不成立。
// 对比 /feed/*（SoftJWTAuth，游客能刷流，只是 is_liked 恒 false）——
// 关注列表连"恒空"都不行，它压根没有匿名版本。
//
// 顺带一个设计上的空缺，写在这里免得后面踩：**没有 isFollowed 接口**。
// 想知道"我关注他了吗"没有直通车，只能拉 getAllVloggers 倒推。详见 FollowButton.vue。

import { postJson } from './client'
import type { AccountInfo } from './account'

/**
 * 关注。
 *
 * 失败分支里最容易忽略的是 **500 + "already followed"**：
 * service 里有一次 IsFollowed 预检查，重复关注会被它拦下并返回裸 error
 * （裸 error 经 ClassifyHTTPStatus 兜成 500，不是 409）。
 * 也就是说"重复关注"和"服务器炸了"在状态码上**分不开**，只能看 message。
 *
 * 这是原项目的形状，本项目没改：真正修法是给重复关注单独定义一个 apierror
 * （就像删别人评论用了 ErrForbidden）。前端这边按"可能是重复关注"处理即可 ——
 * 反正失败后重新对一次账，重复关注对账出来的结果就是"已关注"，正好是对的状态。
 */
export function follow(vloggerID: number) {
  return postJson<{ message: string }>(
    '/social/follow',
    { vlogger_id: vloggerID },
    { authRequired: true },
  )
}

/** 取关。没关注时取关会失败（service 先 IsFollowed 预检查），同样落成 500。 */
export function unfollow(vloggerID: number) {
  return postJson<{ message: string }>(
    '/social/unfollow',
    { vlogger_id: vloggerID },
    { authRequired: true },
  )
}

/**
 * 某人的粉丝列表。**vlogger_id 传 0 的语义是"查我自己"**（后端约定，
 * 见 social/entity.go 里的说明）—— 这个"0 = 从 JWT 取自己"只在这两个列表接口上有。
 *
 * 返回的 followers 是完整的 Account JSON，可以直接塞进 AccountInfo。
 */
export function getAllFollowers(vloggerID = 0) {
  return postJson<{ followers: AccountInfo[]; follower_count: number }>(
    '/social/getAllFollowers',
    { vlogger_id: vloggerID },
    { authRequired: true },
  )
}

/**
 * 某人的关注列表。**follower_id 传 0 = 查我自己**。
 *
 * 后端硬编码 `LIMIT 200` 且没有游标，而且**不保证顺序**（两阶段查询：
 * 先在 socials 上取 id，再 `WHERE id IN (...)`，MySQL 的 IN 不保序）——
 * 所以别把它当"按关注先后排序"来展示。
 *
 * 它还有一个非预期的用途：**倒推"我关注他了吗"**。见 FollowButton.vue。
 */
export function getAllVloggers(followerID = 0) {
  return postJson<{ vloggers: AccountInfo[]; vlogger_count: number }>(
    '/social/getAllVloggers',
    { follower_id: followerID },
    { authRequired: true },
  )
}

/**
 * 我的两个计数（粉丝数 / 关注数）。
 *
 * 没有请求体 —— 后端 handler 刻意不 bind（空 body 调 ShouldBindJSON 返回 EOF，
 * 会把查询变成 400），所以发一个 `{}` 就行。和 /like/listMyLikedVideos 同一套。
 *
 * 这里的数字是**实时 COUNT(*)** 查出来的，不是冗余列（对比 videos.likes_count）。
 * 阶段6 的取舍：计数读得少（只在个人主页），维护冗余列的双向一致性不划算。
 */
export function getCounts() {
  return postJson<{ follower_count: number; vlogger_count: number }>(
    '/social/getCounts',
    {},
    { authRequired: true },
  )
}
