// 通知中心的全局状态：items（最近的通知）、unread（红点上的数字）、
// 以及**那条 SSE 长连接的句柄**。
//
// 为什么连接句柄要放在 store 里、而不是放在铃铛组件的 setup 里：
// 铃铛挂在壳（DesktopShell / MobileShell）上，而拖窗口跨过 768px 断点会
// 换一个壳 —— 组件一卸载一挂载，连接就被"断开又重连"了一次，中间那几秒的
// 推送正好落在"已 Unsubscribe 但还没 Subscribe"的窗口里（见后端 SSEHub 的
// 注册表）。所以连接的生命周期挂在**比壳更长寿**的地方：这里。
// 真正的 connect/disconnect 时机由 App.vue 按登录态触发。

import { defineStore } from 'pinia'
import { ref } from 'vue'

import {
  createNotificationStream,
  listNotifications,
  markRead,
  unreadCount,
  type NotificationItem,
} from '../api/notification'
import { useAuthStore } from './auth'

/**
 * 内存里最多留多少条。
 *
 * 面板只渲染最近 50 条（后端也只给 50 条），但 SSE 是**只增不减**的：
 * 一个开着页面上一整天的用户，每来一条通知 unshift 一条，数组会一直长。
 * 不设上限的话，真正泄漏的是 DOM 之外的那部分 —— 而且它增长得毫无征兆
 * （没有任何一次请求是"多"的）。超了就砍尾巴，反正用户不会翻到第 100 条。
 */
const MAX_KEEP = 100

export const useNotificationStore = defineStore('notification', () => {
  const auth = useAuthStore()

  const items = ref<NotificationItem[]>([])
  const unread = ref(0)
  /** 面板打开时才拉历史（见 load），这个标志给面板渲染"打开中…" */
  const loading = ref(false)
  const error = ref('')
  /** SSE 是否活着。用来在面板里提示"实时推送断了，手动刷新一下" */
  const connected = ref(false)

  // 非响应式的局部变量：句柄本身变了**不需要**触发任何重渲染，
  // 放进 ref 只会让 EventSource 被 Vue 代理包一层（还可能踩到
  // "代理过的对象不等于原对象"这种诡异问题）
  let es: EventSource | null = null

  /**
   * 建立 SSE。**幂等** —— 重复调用不会开出第二条连接。
   *
   * 这一点是必需的而不是防御性的：登录、壳切换、面板打开都可能触发它，
   * 而"两条连接"的代价不是双倍流量，是**每条通知都收到两遍**
   * （后端 SSEHub 是 map[uint][]chan，一个人几条连接就推几份）。
   */
  function connect() {
    // ⚠ 判据是"**活着**"而不是"存在"。
    //
    // 只写 `if (es) return` 会漏掉一种死法：createNotificationStream 内部退避
    // 次数用尽后**放弃重建**，此时它手里那条连接已经 CLOSED，但它不会回来告诉
    // 我们（onState(false) 发了，句柄没换）。于是 store 里 es 非 null、连接却
    // 早就死了，之后每次 connect() 都被这一行挡回去 —— **再也没有任何路能接回来**，
    // 只能靠登出再登录（disconnect 才会把 es 清成 null）。
    //
    // 拿 readyState 判一下就补上了：CLOSED 的旧句柄是彻底的死对象，
    // 重开一条不会和它打架（它的 close 包壳只影响它自己那个闭包）。
    if (es && es.readyState !== EventSource.CLOSED) return
    if (!auth.isLoggedIn) return
    if (typeof EventSource === 'undefined') return // 老浏览器/SSR：静默降级成"没有实时推送"

    // onReplace：连接重建（token 续期后）时会给出**新句柄**。
    // 不接这个回调的话，store 手里永远是那条死掉的连接，disconnect() 关的是
    // 一个已经 CLOSED 的对象 —— 新连接就永远挂着，正是要防的那种泄漏。
    es = createNotificationStream(
      onPush,
      (next) => {
        es = next
      },
      (live) => {
        connected.value = live
      },
    )

    // 补一次未读数：SSE 只推"之后发生的"，登录那一刻已经躺在表里的
    // 未读通知它一条都不会推（后端 Push 只对在线连接生效）
    void refreshUnread()
  }

  /**
   * 关闭连接。**必须真的 close()**。
   *
   * 不关的后果是双向的，而且两边都很隐蔽：
   *
   *	浏览器这侧：连接一直开着，后端每次「这个用户的通知」都要往它的
   *	            channel 里写一份（写满了还会记一条日志），
   *	后端那侧：SSEHandler 那条 goroutine 和注册表里的 channel 都活着 ——
   *	          它靠 c.Request.Context() 感知断开，而连接不断，ctx 就不 Done。
   *
   * 登出后再登录、反复几次，就会攒下一堆只写不读的死连接，
   * 表现是"通知越用越慢、日志里全是缓冲已满"。
   *
   * 顺带**清空 items/unread**：这个 store 是全局单例，不清的话
   * 换个账号登录会先看到上一个人的通知列表（红点数字也是他的）。
   */
  function disconnect() {
    es?.close()
    es = null
    connected.value = false
    items.value = []
    unread.value = 0
    error.value = ''
  }

  /** SSE 每收到一帧走这里 */
  function onPush(n: NotificationItem) {
    // 去重：重连之后补拉历史（load）和一条**同时**到达的推送可能撞上。
    // 幂等是这里唯一便宜的做法 —— 后端在"落库成功但重投"时本来就可能
    // 插出两条一模一样的通知（通知表没有唯一索引，那儿如实记下了不修）
    if (items.value.some((i) => i.id === n.id)) return

    items.value.unshift(n) // 最新的在最上面，和后端 list 的顺序一致
    if (items.value.length > MAX_KEEP) items.value.length = MAX_KEEP
    if (!n.is_read) unread.value += 1
  }

  /**
   * 拉最近 50 条历史。**面板每次打开都拉**，不做"拉过就不拉"的缓存 ——
   * 因为推送是会丢的（缓冲满、断线期间），表才是真相源。
   * 一次 COUNT 级别的查询换"打开面板看到的一定是对"，值。
   */
  async function load() {
    if (!auth.isLoggedIn) return
    loading.value = true
    error.value = ''
    try {
      items.value = await listNotifications()
      // 未读数**以服务端为准**重算，不用本地累加值：本地那个数字是
      // "打开页面之后收到的"，页面开着之前攒的那批它根本不知道
      void refreshUnread()
    } catch (e) {
      error.value = e instanceof Error ? e.message : '加载通知失败'
    } finally {
      loading.value = false
    }
  }

  async function refreshUnread() {
    if (!auth.isLoggedIn) return
    try {
      unread.value = await unreadCount()
    } catch {
      // 一个只影响红点数字的请求失败，不值得在界面上报错。
      // 保留上一次的数字，比清成 0 诚实（清成 0 = "你没有未读"，那是撒谎）
    }
  }

  /**
   * 全部已读。**乐观更新**：先清红点再发请求。
   *
   * 理由是这个动作的结果几乎必然成功，而等一次往返才清红点会让点击
   * 看起来像卡了一下（同 FollowButton 的取舍）。失败不打回 —— 下次
   * load() 会以服务端为准把红点恢复回来，那才是真相源。
   */
  async function markAllRead() {
    if (!auth.isLoggedIn) return
    const prevUnread = unread.value
    const prevFlags = items.value.map((i) => i.is_read)
    unread.value = 0
    items.value.forEach((i) => {
      i.is_read = true
    })
    try {
      await markRead() // 不带 id = 全标
    } catch (e) {
      // 401 之外才回滚：401 说明已经登出了，回滚只会把上一个账号的红点
      // 又画回来（而 disconnect() 马上就要清空它们）
      if (prevUnread > 0 && auth.isLoggedIn) {
        unread.value = prevUnread
        items.value.forEach((i, idx) => {
          i.is_read = prevFlags[idx]
        })
      }
      error.value = e instanceof Error ? e.message : '标记已读失败'
    }
  }

  /** 标记单条已读（点开一条通知时调）。同样乐观 + 幂等：已读的不会再发请求 */
  async function markOne(id: number) {
    const target = items.value.find((i) => i.id === id)
    if (!target || target.is_read) return

    const prevUnread = unread.value
    target.is_read = true
    if (unread.value > 0) unread.value -= 1
    try {
      await markRead(id)
    } catch (e) {
      if (auth.isLoggedIn) {
        target.is_read = false
        unread.value = prevUnread
      }
      error.value = e instanceof Error ? e.message : '标记已读失败'
    }
  }

  return {
    items,
    unread,
    loading,
    error,
    connected,
    connect,
    disconnect,
    load,
    refreshUnread,
    markAllRead,
    markOne,
  }
})
