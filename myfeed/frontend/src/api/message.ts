// 私信模块 API（阶段12）。全项目最后一个业务模块，也是**最普通**的一个：
// 一张表、两个接口、没有缓存、没有 MQ、没有并发竞争。
//
// 两条都是 POST，两条都挂 JWT（router.go 里 /message 整组在保护之下）。
// 为什么私信不能匿名：**发送者不在请求体里，只从 JWT 取** —— 让客户端传 from_id
// 等于让任何人冒充任何人发私信。这和评论的 author_id 是同一条纪律。
//
// 所以这个文件里没有一条公开接口，两个函数都带 authRequired: true。

import { postJson } from './client'

/**
 * 一条私信。形状直接对应 message/entity.go 里的 Message 结构体。
 *
 * **created_at 是 RFC3339 字符串，不是 Unix 秒**（`time.Time` 直接序列化的结果）。
 * 这是本项目的第三套时间形状了 —— formatTime/fullTime 只认秒，
 * 所以喂给它俩之前必须先过 `isoToUnixSeconds`，否则渲染出来是 "Invalid Date"。
 * 同一个坑在 /video/* 和 /notification/* 上都有。
 *
 * is_read 是**死字段**（原项目和本项目都不写它，详见 entity.go 里那段
 * "字段能存不等于语义已定"），所以界面上**不要**拿它做什么已读/未读样式 ——
 * 它恒为 false，拿它渲染会让每条消息看起来都"未读"。
 */
export interface Message {
  id: number
  from_id: number
  to_id: number
  content: string
  is_read: boolean
  created_at: string
}

/**
 * 发一条私信。**返回值就是落库后的那一行**（含 id 和 created_at）——
 * 后端 Send 之后直接 `c.JSON(200, m)`，GORM 的 Create 已经把自增 id
 * 和 autoCreateTime 回填进去了。
 *
 * 这条性质决定了聊天窗的写法：发完**不用重新拉全量**，把返回的这个对象
 * 追加到列表尾部即可。少一次列表往返，也少一次"发送成功后列表闪一下"。
 */
export function sendMessage(toId: number, content: string) {
  return postJson<Message>('/message/send', { to_id: toId, content }, { authRequired: true })
}

/**
 * 拉和某个人的会话历史（最近 50 条，后端写死的 LIMIT，没有游标）。
 *
 * ---------- 两个必须在这里处理的形状问题 ----------
 *
 * ① **可能是 null。** 空会话时 Go 里的 `[]Message` 是 nil，序列化出去就是
 *    `{"messages":null}` —— 前端直接 `.map` 就是 TypeError。
 *    entity.go 里明确要求 handler 兜成 `[]Message{}`，但**前端不能依赖它**：
 *    这条兜底是"接口契约"层面的，两边各做一次才叫契约（后端改了不会通知前端）。
 *    这里 `?? []` 一行，换掉的是聊天窗打开空会话直接白屏。
 *
 * ② **顺序是倒序的。** 后端 `Order("created_at desc")` —— 最新的在前，
 *    因为原项目是把它当"最近消息列表"用的。聊天窗要的是**正序**
 *    （老消息在上、新消息在下），所以调用方拿到之后要 reverse。
 *    这里**不替调用方 reverse**：接口返回什么就是什么，翻转是展示层的决定。
 *
 * 还有一条不在返回值里但要知道的：**没有已读回执**，拉历史**不会**把对方的
 * 消息标成已读（IsRead 是死字段）。所以别指望"打开聊天窗对方那边就显示已读"。
 */
export async function listMessages(peerId: number): Promise<Message[]> {
  const res = await postJson<{ messages: Message[] | null }>(
    '/message/list',
    { peer_id: peerId },
    { authRequired: true },
  )
  return res?.messages ?? []
}
