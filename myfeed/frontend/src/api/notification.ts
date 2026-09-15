// 通知模块 API（阶段9）：三个读写接口 + 一条 SSE 长连接。
//
// 四个接口全部挂鉴权，而且**挂的不是 JWTAuth 而是 QueryTokenAuth** ——
// 这是全项目唯一一处，原因写在下面 createNotificationStream 里
// （一句话：EventSource 发不了自定义 header）。
//
// 三条纪律先摆在前面，后面每个函数都在兑现它们：
//
//	① **表是真相源，SSE 只是锦上添花。** 后端 save 的顺序是"先落库再推送"，
//	   推送还可能因为缓冲满被丢掉（Push 是非阻塞的）。所以"漏了一条推送"
//	   是正常情况，靠重连后调 listNotifications 补 —— 前端这侧同理：
//	   SSE 只负责"多一条就插一条"，**不能**拿它当数据的唯一来源。
//	② **推送可以丢，数据不能丢**，所以前端收到推送后也不做"必须先拉全量对齐"
//	   这种重动作，插进去就完了。
//	③ 三种通知的 target_id 语义不同（见 notificationTarget），前端必须按 type 分流。

import { postJson, tryRefresh } from './client'
import { useAuthStore } from '../stores/auth'

// 和 client.ts 里那个 API_BASE 是同一个值（vite 把 /api 代理到 127.0.0.1:8080）。
// 没有从 client.ts import 是因为它**没被导出**，而它这辈子只会是这一个值 ——
// 为了一行常量去改那个文件的对外形状，不划算。
const API_BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api'

/**
 * 一条通知。对应 notification/entity.go 里的 Notification。
 *
 * ---------- type ----------
 *
 *	like    点赞了你的视频（阶段9，走 MQ）
 *	comment 评论了你的视频（阶段9，走 MQ）
 *	follow  关注了你（阶段9，走 MQ）
 *	mention 评论里 @ 了你（阶段5，**同步**写，不走 MQ）
 *
 * mention 是前一阶段留下的第四种，很容易被漏掉：它和 comment 长得很像，
 * 但 comment 是"评论了你的视频"、mention 是"某人在评论正文里 @ 了你"，
 * 同一条评论可能同时产生两条（内容不同、Type 不同），**不是重复**。
 *
 * 类型写成 `string` 而不是联合类型、并且把四个常量都列出来，是因为
 * **后端以后再加一种 type 前端不该编译不过** —— 未知 type 应当降级成
 * "一条普通通知"，而不是白屏。
 */
export interface NotificationItem {
  id: number
  recipient_id: number
  sender_id: number
  type: string
  target_id: number
  content: string
  is_read: boolean
  /** RFC3339 字符串，不是 Unix 秒 —— 喂给 formatTime 前先过 isoToUnixSeconds */
  created_at: string
}

/**
 * 最近 50 条通知（后端写死的 LIMIT，无游标 —— 第 51 条拿不到，
 * 而且**用户看不出来自己漏了**，列表就是变短了。原项目如此）。
 *
 * 顺序是 `created_at DESC, id DESC`。**第二键 id 是必需的**：只按时间排的话，
 * 同一秒内的两条通知顺序是数据库实现相关的，刷新两次可能不一样。
 *
 * 空结果是 `{"notifications":[]}`（handler 里有 nonNil 兜底），但这里照样
 * `?? []` —— 接口契约两边各做一次才算数。
 */
export async function listNotifications(): Promise<NotificationItem[]> {
  const res = await postJsonAuth<{ notifications: NotificationItem[] | null }>('/notification/list', {})
  return res?.notifications ?? []
}

/**
 * 标记已读：**带 id 标一条，不带 id 全标**。
 *
 * 后端是 `_ = c.ShouldBindJSON(&req)` 之后看 `req.ID > 0` 分流的，
 * 所以空 body、`{}`、`{"id":5}` 三种输入都能工作（用 ShouldBindJSON 的
 * 报错来判分支是错的：空 body 报 EOF、`{}` 不报，而两者语义完全一样）。
 * 前端这边就按"不传 id 就发 `{}`"来，不去玩空 body 的花活。
 *
 * 返回值是固定的 `{"message":"ok"}`，**不报告标了几条** —— 所以这个方法
 * 之后不要拿返回值去更新未读数，自己在 store 里减。
 */
export function markRead(id?: number) {
  return postJsonAuth<{ message: string }>('/notification/markRead', id ? { id } : {})
}

/**
 * 未读数（铃铛上那个红点里的数字）。一次 COUNT(*)，走 recipient_id 索引。
 *
 * 什么时候调：初次登录、SSE 重建成功、以及用户点开面板之后。
 * **不做轮询** —— 后端注释里提到"前端会轮询它做兜底"，本项目不打算那么做：
 * SSE 断了会自己重连，重连后补一次未读数就够了，轮询只是把"漏一条"的
 * 窗口从"几秒"缩短到"几秒"，代价是每条连接每 N 秒一次 SQL。
 */
export async function unreadCount(): Promise<number> {
  const res = await postJsonAuth<{ count: number }>('/notification/unreadCount', {})
  return res?.count ?? 0
}

/**
 * 通知点击后该去哪。
 *
 * ---------- 这是三个 type 唯一的**语义分歧点** ----------
 *
 *	like    → target_id = videoID   （点通知跳回被赞的视频）
 *	comment → target_id = videoID   （跳回被评论的视频）
 *	mention → target_id = videoID   （跳回被 @ 的那条视频）
 *	follow  → target_id = **关注者的账号 ID**
 *
 * follow 的动作对象是**人**不是物，所以三种都是"动作对象"，跳转路由却不同。
 * 不分流、直接 `/video/${target_id}` 的话，follow 通知会跳到一个
 * "不存在的视频页" —— 而那个 id 通常**是有效的视频 id**（账号 3 和视频 3
 * 都存在），所以不会 404，只会打开一条风马牛不相及的视频。**这种 bug 最难发现。**
 *
 * 未知 type 默认走视频页：以后加的第三种"关于视频的通知"多半也是 videoID，
 * 猜错的代价比"什么都不做"小。
 */
export function notificationTarget(n: NotificationItem): string {
  return n.type === 'follow' ? `/profile/${n.target_id}` : `/video/${n.target_id}`
}

/** 上面三个接口共用的壳：全部需要登录，body 一律是对象（空 body 会 bind 失败） */
function postJsonAuth<T>(path: string, body: unknown): Promise<T> {
  return postJson<T>(path, body, { authRequired: true })
}

/**
 * 建立 SSE 长连接。返回一个 EventSource；`onReplace` 在**重建**时告诉你新句柄。
 *
 * ================== 为什么 token 只能走 query ==================
 *
 * `new EventSource(url)` 只有两个参数：url 和 withCredentials ——
 * **没有 headers**。这不是"某版本没加"，是规范就这么定的：EventSource 的
 * 请求头完全由浏览器构造，应用代码碰不到。而 Authorization 是自定义头，
 * 所以"带 token 的 SSE"只有两条路：cookie 或者 query。
 *
 * 后端选了 query（`jwt.QueryTokenAuth` 先看 `?token=` 再回退到 header），
 * 代价写在那个函数的注释里：token 会进服务器访问日志、浏览器历史、Referer。
 * 前端这边能做的就是**不要把这个 URL 记到别的地方去**。
 *
 * ================== 断开为什么要重建（不能只靠浏览器自动重连） ==================
 *
 * EventSource 其实自带重连，但它**只对"可恢复的故障"生效**：
 *
 *	网络抖动 / 后端重启 → 浏览器自己按 3 秒退避重试，readyState 停在 CONNECTING
 *	                     → **这种情况我们绝对不能 close()**，一 close 就把
 *	                       浏览器那套带退避的自动重连干掉了，比不重连还糟
 *	401 / Content-Type 不对 → 按规范"fail the connection"：**永久失败，不再重连**，
 *	                     readyState 直接变 CLOSED
 *
 * 所以 `readyState === CLOSED` 是这里唯一的判据，它精确地划出了
 * "该我们出手了"的那一半。第二类里最常见的是 **token 过期**：
 * 连接悄无声息地死掉，红点就此不动，而用户看不出任何异常 ——
 * 这是本文件存在的主要理由。
 *
 * ================== 局限（如实记下） ==================
 *
 *	① 判据是 CLOSED 而不是"读到了 401"：代理/防火墙把连接掐死也可能落到
 *	   CLOSED，这时我们会多花一次 refresh 的代价去重建。**方向是安全的**
 *	   （refresh 成功就重建、失败就退避），只是不精确。
 *	② 重建**不是无缝的**：断到重建之间那几秒的通知，如果后端推送时
 *	   这条连接已经不在注册表里，就只落库不推送 —— 靠调用方在重建后
 *	   调一次 listNotifications / unreadCount 补齐（store 里做了）。
 *	③ 退避是**有限次**的（见 maxRetry）。无限重试在"后端根本没启动"
 *	   的场景下只会把浏览器控制台刷满，而用户该看到的是"没连上"而不是
 *	   "一直在转"。
 */
export function createNotificationStream(
  onNotification: (n: NotificationItem) => void,
  onReplace?: (next: EventSource) => void,
  /**
   * 连接"死透了"时通知调用方（false = 已经被判定不可恢复，正在续期/退避）。
   *
   * 为什么需要它：SSE 最坑的一点是**断了以后界面上完全看不出来** ——
   * 红点就是不动了，和"最近确实没人理你"长得一模一样。有了这个回调，
   * 面板才能说一句"实时推送未连上"。它不是装饰，是这条链路唯一的可观测性。
   */
  onState?: (live: boolean) => void,
): EventSource {
  let stopped = false
  let timer: number | null = null
  let attempt = 0
  let current: EventSource | null = null

  const retryBase = 3000
  const maxRetry = 3

  function open(): EventSource {
    const auth = useAuthStore()
    const url = `${API_BASE}/notification/stream?token=${encodeURIComponent(auth.token ?? '')}`
    const es = new EventSource(url)

    // 把 close 包一层：调用方主动关（登出）时，内部那个退避重连的定时器
    // 也必须跟着停 —— 否则它醒来会重建一条**没人认领**的连接，
    // 而调用方手里只有旧句柄，再也关不掉它。那就成了"登出后连接泄漏"，
    // 正是 backend SSEHub.Unsubscribe 那段注释在防的同一件事（那边漏的是
    // goroutine 和 channel，这边漏的是浏览器连接和后端的注册表条目）。
    const rawClose = es.close.bind(es)
    es.close = () => {
      stopped = true
      if (timer !== null) {
        window.clearTimeout(timer)
        timer = null
      }
      rawClose()
    }

    es.onmessage = (ev: MessageEvent<string>) => {
      if (stopped) return
      attempt = 0 // 收到了真数据 = 这条连接是活的，退避计数归零
      try {
        const n = JSON.parse(ev.data) as NotificationItem
        // 后端 mustJSON 序列化失败时会发一个 `{}`（宁可丢一条也不断连接），
        // 那种帧没有 id —— 丢掉它，但**不能让它抛出去**
        if (n && typeof n.id === 'number' && n.id > 0) onNotification(n)
      } catch {
        // 非 JSON 帧。按 SSE 规范客户端本来就没有"处理不了"这个选项，
        // 除了忽略没别的做法（注释帧 `: keepalive` 连 onmessage 都进不来）
      }
    }

    // onState(true) 挂在 **onopen** 上、不挂在构造函数返回的那一刻：
    // 这条链路的"连上了"必须有事实依据，而"我 new 了一个 EventSource"
    // 不是依据 —— 后端没跑的时候它也照样 new 得出来。
    // （后端在写出响应头之后立刻 Flush，所以 onopen 到得很早，
    //   正常情况下面板上那句提示只会闪一下、甚至看不到。）
    es.onopen = () => {
      if (stopped || es !== current) return
      // ⚠ **attempt 必须在 onopen 归零，不能只在 onmessage 归零。**
      //
      // attempt 的语义是"**连续**几次重建失败"，所以只要连上了就该归零。
      // 只放在 onmessage 里的漏洞：一个重连成功、但**此后一直没收到任何通知**
      // 的用户，attempt 会一直停在上次的值 —— 每次断线都 +1，攒到 maxRetry
      // 就永久不再重连了。而这条路上的断开本来就是零星的（切网络、休眠唤醒），
      // 攒够 3 次可能要几周，然后红点**悄无声息地永远不动了** ——
      // 正是本文件开头那句"SSE 最坑的一点是断了以后界面上完全看不出来"。
      //
      // onmessage 里那次归零仍然保留：它多覆盖一种情况（重连后连 401 都没报
      // 就接到了数据，说明确实活着），两处都归零不冲突。
      attempt = 0
      onState?.(true)
    }

    es.onerror = () => {
      // 已经换了新连接的老连接报错、或者已经登出，都别再管 ——
      // 不判 es !== current 的话，一条被替换掉的连接超时会把新连接也拖去重建
      if (stopped || es !== current) return
      // CONNECTING = 浏览器正在自动重连（网络抖动）。**这行是整段逻辑的重点**：
      // 这种情况连 onState(false) 都不能发 —— 它下一秒就自己连回来了，
      // 闪一句"未连上"再闪回去，比不提示更糟
      if (es.readyState !== EventSource.CLOSED) return
      onState?.(false)
      void recover()
    }

    current = es
    return es
  }

  async function recover() {
    if (stopped || attempt >= maxRetry) return
    attempt++

    // 先试续期：token 过期是这条路上唯一能自愈的故障，而 tryRefresh 是
    // client.ts 里那个**单飞**版本（并发 401 只发一次 refresh）——
    // 这里复用它，而不是自己 fetch 一遍 /account/refresh。
    const fresh = await tryRefresh()
    if (stopped) return

    if (fresh) {
      // 拿到新 token → 立刻重建。**不重试原来那条**：EventSource 的 url
      // 是构造时定死的，改不了，只能新建一个
      onReplace?.(open())
      return
    }

    // 续期失败：client.ts 里已经 clearTokens()，isLoggedIn 变 false，
    // 上层（App.vue 的 watch）会在这个 tick 之后调 disconnect()。
    // 这里直接收手，不去重试一个已经登出的账号
    if (!useAuthStore().isLoggedIn) return

    // 剩下的是"token 好着但就是连不上"（后端没跑、代理挂了）。
    // 退避重试有限次 —— 见上面局限 ③
    timer = window.setTimeout(() => {
      timer = null
      if (stopped) return
      onReplace?.(open())
    }, retryBase * attempt)
  }

  // 首连。**构造完立刻返回**，不等 onopen —— EventSource 是"连上之前就已经
  // 可以往里塞回调"的对象，让调用方 await 一个可能永远不 resolve 的
  // Promise 才是坑（后端那条连接正常情况下永不返回）
  return open()
}
