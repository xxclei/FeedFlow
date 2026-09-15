// 播放质量埋点（QoE）的上报与查询。对应后端 internal/qoe/。
//
// 这个模块只有两个接口，但**上报那条有两条实现路径** —— 这是全项目唯一
// 一个"同一个请求有两种发法"的地方，原因见下面 reportOnUnload 的长注释。

import { postJson } from './client'
import { useAuthStore } from '../stores/auth'

// 和 notification.ts 一样本地重声明一份：client.ts 里那个常量没有 export，
// 为了一个字符串去改它的公开形状不值得。
const API_BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api'

/**
 * 一次播放会话的聚合结果。字段与后端 qoe.ReportQoERequest 一一对应。
 *
 * 注意**所有字段都是必填的**（后端那边是 Go 的零值，不传就是不传，不会报错）。
 * 这里写成必填是为了让 useQoE.ts 在构造对象时**必须显式给每个字段一个值** ——
 * 漏掉一个字段在 TypeScript 下会编译失败，而如果写成可选，
 * 漏掉它只会让服务端收到 0，表现是"这个指标永远是 0"，
 * 而那看起来完全像是"确实没有卡顿"。
 */
export interface QoEReport {
  session_id: string
  video_id: number

  stall_count: number
  stall_total_ms: number
  played_ms: number

  avg_bitrate_kbps: number
  tier_switches: number

  startup_ms: number
  dropped_frames: number
  error_code: string

  effective_type: string
  downlink_mbps: number
}

/** 服务端落库后的快照（多了 id / account_id / created_at）。 */
export interface QoEReportAck extends QoEReport {
  id: number
  account_id: number
  created_at: string
}

/**
 * 上报一次播放会话。**走常规 fetch**：带 token、401 时自动续期重试。
 *
 * 用在两条"页面还活着"的刷出路径上（组件卸载、标签页切到后台）——
 * 那时请求能正常完成，也能拿到响应，出错了调用方还能知道。
 *
 * 不传 `authRequired`：这是**软鉴权**接口，登录与否都该上报成功。
 * 传了 `authRequired: true` 会在没 token 时直接 reject，
 * 而游客的数据恰恰是最不能丢的那部分（见后端 qoe/handler.go 的说明）。
 */
export function reportQoE(body: QoEReport) {
  return postJson<QoEReportAck>('/qoe/report', body)
}

/** 聚合看板。video_id=0 全部视频；days 见后端（0=默认 7 天，负数=不限时间）。 */
export function getQoEStats(videoId = 0, days = 0) {
  return postJson<QoEStats>('/qoe/stats', { video_id: videoId, days })
}

/**
 * 页面**正在卸载**时的上报。发出去就不管了，没有返回值。
 *
 * ---------- 为什么不是 navigator.sendBeacon ----------
 *
 * 这是本模块唯一一处值得细看的设计，因为**最直觉的答案（sendBeacon）是错的**。
 *
 * sendBeacon 是"页面卸载时上报"的标准答案，理由是浏览器保证它会发出去
 * （请求由浏览器进程接管，不随页面一起死）。这一条它确实做得好。
 *
 * 但它有一个致命的限制：**不能设置任何自定义请求头。**
 * 而本项目的鉴权完全依赖 `Authorization: Bearer <token>`。
 * 后果是一个**静默的数据污染**：
 *
 *   登录用户在播放页直接关掉标签页 → 这条路把这次播放报成 account_id = 0
 *   → 服务端是 upsert（同 session_id 覆盖）
 *   → **前面那条带正确身份的记录被覆盖成"游客"**
 *   → 看板上"游客占比"虚高，登录用户的 QoE 样本凭空消失一部分
 *
 * 而它不会报任何错。这类"少了一部分样本但不报错"的过滤，
 * 正是后端 handler.go 里点名过的"埋点系统最常见的死法"。
 *
 * ---------- fetch + keepalive 为什么更好 ----------
 *
 * `fetch(url, { keepalive: true })` 提供**同样的卸载存活保证**
 * （规范里就是为这个场景设计的：请求交给浏览器，页面死了也发完），
 * 而且**可以正常设置 Authorization 头**。
 *
 * 代价是请求体上限 64KB（sendBeacon 也是这个量级），对本项目这种
 * 几百字节的 JSON 完全不是问题。
 *
 * ---------- 为什么还留着 sendBeacon 兜底 ----------
 *
 * keepalive 的浏览器支持比 sendBeacon 窄（Safari 13+ / 现代 Chromium 有，
 * 更老的没有）。而 fetch 遇到不支持的选项**不会抛异常，只会忽略它** ——
 * 所以这里判不出来。
 *
 * 真正会走进兜底的是**同步抛错**那种情况（比如环境里根本没有 fetch，
 * 或者被某个 polyfill 弄坏了）。那时退到 sendBeacon：牺牲身份，
 * **但至少把指标保下来** —— 丢身份只是分组不精确，丢整条数据是永久损失。
 * 这个取舍的方向和别处一致：**宁可数据不精确，不可数据不存在。**
 */
export function reportOnUnload(body: QoEReport): void {
  const auth = useAuthStore()
  const headers: Record<string, string> = { 'Content-Type': 'application/json' }
  if (auth.token) headers.Authorization = `Bearer ${auth.token}`

  const payload = JSON.stringify(body)

  try {
    // 不 await：这里必须同步返回。页面正在卸载，等一个 Promise
    // 只会让这个函数在微任务队列里被丢弃 —— 请求发不出去。
    // keepalive 的全部意义就是"不用等它"。
    void fetch(`${API_BASE}/qoe/report`, {
      method: 'POST',
      headers,
      body: payload,
      keepalive: true,
    }).catch(() => {
      /* 页面都走了，没地方报错，也没人能处理 */
    })
  } catch {
    // fetch 同步抛错（极罕见）→ 退到 beacon。beacon 不接受自定义头，
    // 所以这次会上报成游客 —— 见上面那段，这是刻意的取舍。
    try {
      navigator.sendBeacon?.(
        `${API_BASE}/qoe/report`,
        new Blob([payload], { type: 'application/json' }),
      )
    } catch {
      /* 两条路都断了，放弃。埋点丢一条不该影响任何别的事 */
    }
  }
}

/** 后端 qoe.StatsResponse 的镜像。 */
export interface QoEStats {
  total_sessions: number
  guests: number
  stalled_sessions: number
  stall_ratio: number

  startup: {
    n: number
    p50_ms: number
    p95_ms: number
    p99_ms: number
  }

  avg_bitrate_kbps: number
  bitrate_sessions: number
  tiers: { label: string; count: number }[]
  total_dropped_frames: number
  total_tier_switches: number
}
