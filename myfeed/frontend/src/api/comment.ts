// 评论模块 API（阶段5）。
//
// 三个接口的鉴权强度**不一样**，这在前端就直接体现成"带不带 authRequired"：
//
//   /comment/listAll  公开（router.go 里挂在没有 JWT 中间件的那一组）→ 不带
//   /comment/publish  强鉴权 → authRequired: true
//   /comment/delete   强鉴权 → authRequired: true
//
// 看评论不需要登录 —— 这和 /video/getDetail、/feed/* 是同一种产品判断；
// 而写评论必须有"我"，所以和 /like/* 同类。
//
// 响应形状：listAll 是**裸数组**（不是 {"comments": [...]}），
// 和 /video/listByAuthorID、/like/listMyLikedVideos 一致（后端 nonNilComments 兜的 nil）。

import { postJson } from './client'

export interface CommentItem {
  id: number
  /** 写时冗余的快照 —— 是作者**发评论那一刻**的名字，改名后这里不会变。
   *  界面上要显示"当前名字"就去 /profile/:author_id，别拿这个当权威。 */
  username: string
  video_id: number
  /** 删除权限的唯一依据。注意不是 username —— 名字会撞、会改，id 不会。 */
  author_id: number
  content: string
  /** RFC3339 字符串（Go 的 time.Time 直接序列化）。
   *  **不是** Unix 秒 —— 想过 formatTime 必须先过 isoToUnixSeconds。 */
  created_at: string
}

/**
 * 某条视频的全部评论。
 *
 * 后端 `WHERE video_id = ? ORDER BY created_at ASC LIMIT 200`，**没有游标**：
 * 一次拉完，超过 200 条的部分静默不返回（不报错、不告诉你有多少条被截掉了）。
 * 前端拿不到总数，所以界面上不能写"共 N 条评论"这种话 —— 那是编数据。
 */
export function listAllComments(videoID: number) {
  return postJson<CommentItem[]>('/comment/listAll', { video_id: videoID })
}

/**
 * 发表评论。
 *
 * 请求体里**只有 video_id 和 content**，没有 author_id —— 让客户端能传作者 ID
 * 等于把"代表谁发言"的权力交出去，后端只从 token 里取身份。
 *
 * 返回的是 `{message}`，**不含刚创建的那条评论**。所以发完之后前端必须重拉一次
 * 列表才能看到它 —— 不能乐观插入。理由见 CommentSection.vue 里的说明
 * （id 和 created_at 都在服务端，猜不出来；username 猜的话就是上面那个快照陷阱）。
 */
export function publishComment(videoID: number, content: string) {
  return postJson<{ message: string }>(
    '/comment/publish',
    { video_id: videoID, content },
    { authRequired: true },
  )
}

/**
 * 删除评论。请求体里只有 comment_id，身份从 token 取。
 *
 * 三种失败各走各的状态码，调用方要分开处理：
 *
 *   403 是**你本人但这条不是你的**（apierror.ErrForbidden）→ 别登出、别提示重登
 *   401 是凭证无效 → client.ts 已经自动 clearTokens 了，界面只能说"登录已失效"
 *   500 里有"comment not found"（裸 error 被兜成 500）→ 通常是在别处已经删过了
 *
 * 403 和 401 必须分开这件事，是全项目唯一一处不这么做就会出真 bug 的地方：
 * handleResponse 对**任何** 401 都 clearTokens，把"删错评论"报成 401
 * 会把手滑的用户莫名其妙登出。后端为此专门加了 ErrForbidden。
 */
export function deleteComment(commentID: number) {
  return postJson<{ message: string }>(
    '/comment/delete',
    { comment_id: commentID },
    { authRequired: true },
  )
}
