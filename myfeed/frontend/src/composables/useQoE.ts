// 播放质量采集器。挂在 <video> 元素上，把一次播放聚合成一行数据。
//
// 这是全项目第一个"计时 + 累加 + 批量刷出"的东西 —— `src/` 里此前
// 没有任何 setInterval / 定时器 / visibilitychange（grep 过，零命中）。
// 所以这个文件的形状没有先例可抄，下面每一处取舍都写了理由。
//
// 后端 counterpart：internal/qoe/。字段口径必须两边对齐，
// 尤其是 startup_ms 用 playing 而不是 play、played_ms 不含暂停时间这两条。

import { onUnmounted, shallowRef, watch, type Ref } from 'vue'

import { reportOnUnload, reportQoE, type QoEReport } from '../api/qoe'
import { newId } from '../utils/uuid'

/**
 * 一次播放的累计状态。
 *
 * ---------- 为什么是 shallowRef 而不是 ref ----------
 *
 * 这些值每秒钟被改好几次（timeupdate 约 4Hz），但**没有任何东西在渲染它们** ——
 * 它们只在最后被序列化一次。用 `ref()` 会把整个对象包成深度响应式代理，
 * 于是每一次 `s.playedMs += ...` 都要过一遍 Proxy 的 set 陷阱 + 触发通知，
 * 而通知的接收者是零。**纯开销，零收益。**
 *
 * `shallowRef` 只代理 `.value` 这一层，改内层字段不触发任何东西 ——
 * 正是我们要的。留着一个 ref 而不是退回普通变量的理由只有一个：
 * 将来调试面板如果要在播放时实时显示这些数，只要把 `.value` 整体替换一次
 * 就能接上响应式，不用改这个文件的数据结构。
 *
 * （同样的"别让无关的东西进响应式图"的判断在 uploadQueue.ts 里出现过 ——
 *   那边是 File 和指纹用模块级 Map 侧存。）
 */
interface QoEState {
  /** 用户是否按过播放。没按过就不上报 —— 见 shouldReport 的说明 */
  pressedPlay: boolean

  /** performance.now() 起点。取 bind 时刻和 loadstart 时刻里**更早**的那个 */
  t0: number | null
  /** 首次 playing 的时刻。一旦写入就不再改（首帧只算第一次） */
  firstPlayingAt: number | null

  /** 当前是否在卡顿中。用来把 waiting/playing 配成对 */
  stallStartedAt: number | null
  stallCount: number
  stallTotalMs: number

  playedMs: number
  /** 上一次 timeupdate 的 currentTime，用来算增量 */
  lastCurrentTime: number | null

  /** 当前档位码率（HLS 才有）。直传 mp4 永远是 0 */
  currentBitrateKbps: number
  /** Σ(码率 × 该档位持续时长)，最后除以 playedMs 得到按时长加权的均值 */
  bitrateMsWeighted: number
  tierSwitches: number
}

function freshState(): QoEState {
  return {
    pressedPlay: false,
    t0: null,
    firstPlayingAt: null,
    stallStartedAt: null,
    stallCount: 0,
    stallTotalMs: 0,
    playedMs: 0,
    lastCurrentTime: null,
    currentBitrateKbps: 0,
    bitrateMsWeighted: 0,
    tierSwitches: 0,
  }
}

// session_id 的生成搬到了 utils/uuid.ts（那里有完整说明）。
//
// 这段兜底本来是**长在这个文件里**的，而且注释写得很准 ——
// "有人用局域网 IP 访问的话，整个播放器会因为一个埋点字段而崩"。
// 准到 2026-09-15 上云时它换了个形式又发生了一次：
// `http://<公网IP>` 同样不是安全上下文，于是**上传队列**里那句裸的
// `crypto.randomUUID()` 炸了，而这一处当时没有任何兜底。
//
// 搬走的理由不是整洁，是"兜底写在调用点上"这个做法本身会漏：
// 一个文件写了、另一个文件忘了，而忘掉的那个**只在线上才暴露**。
//
// **埋点绝不能让主功能挂掉** —— 这个原则在本文件里出现了三次，
// 这是第一次。它现在靠 newId() 的兜底来兑现。

/**
 * 采集一次播放。
 *
 * @param el       <video> 元素的 ref。可能是 null 并且会变 ——
 *                 VideoPlayer 用 `v-if="!failed"` 控制它，加载失败时元素会消失。
 * @param videoId  取视频 id。用函数而不是值，是因为详情页会在同一个组件实例里
 *                 从 /video/1 换到 /video/2（播放器整体被 :key 重建，
 *                 但这个 composable 的生命周期跟着播放器走，取值时机不确定）。
 */
export function useQoE(el: Ref<HTMLVideoElement | null>, videoId: () => number) {
  const state = shallowRef<QoEState>(freshState())
  const sessionId = newId()

  /**
   * 把 waiting/playing 配成对，算出一次卡顿。
   *
   * ---------- 为什么是 waiting 而不是 stalled ----------
   *
   * 这两个事件很容易混，语义完全不同：
   *
   *   waiting  —— 播放**停了，在等数据**。这是用户感受到的"卡了"。
   *   stalled  —— 浏览器**尝试取数据但超过 3 秒一点都没拿到**。
   *
   * stalled 更严重但**更少见**：网络慢到"有数据但不够流畅"时
   * waiting 一直触发而 stalled 根本不触发。用 stalled 计数会得到
   * 一个系统性偏低的卡顿率 —— 表现是"看板说很流畅，用户说很卡"。
   *
   * ---------- 首次缓冲不算卡顿 ----------
   *
   * `waiting` 在**第一次播放之前也会触发**（那是在做初始缓冲，
   * 也就是首帧等待期）。把它算成卡顿会同时污染两个指标：
   * 卡顿数 +1，而且 stallTotalMs 里塞进了整个首帧时长。
   *
   * 判据是 `firstPlayingAt` —— 只有"已经播出过画面了"之后的 waiting
   * 才算卡顿。这是本文件里最容易被后续改动破坏的一条不变量。
   */
  function onWaiting() {
    if (state.value.firstPlayingAt === null) return // 首次缓冲，不是卡顿
    if (state.value.stallStartedAt !== null) return // 已经在卡了，不重复计数
    state.value.stallStartedAt = performance.now()
  }

  function onPlaying() {
    const s = state.value
    const now = performance.now()

    if (s.firstPlayingAt === null) {
      s.firstPlayingAt = now
    }

    // 卡顿结束：结算这一次
    if (s.stallStartedAt !== null) {
      s.stallTotalMs += now - s.stallStartedAt
      s.stallCount += 1
      s.stallStartedAt = null
    }
  }

  function onTimeUpdate() {
    const v = el.value
    if (!v) return
    const s = state.value

    if (s.lastCurrentTime === null) {
      s.lastCurrentTime = v.currentTime
      return
    }

    const delta = v.currentTime - s.lastCurrentTime
    s.lastCurrentTime = v.currentTime

    // ---------- 为什么要卡 delta 的上界 ----------
    //
    // timeupdate 大约每 250ms 触发一次，所以正常的 delta 在 0.25 秒上下。
    // 一个**很大**的 delta 只可能来自拖进度条（seek）——
    // 用户把进度条从 10:00 拖到 1:00:00，delta 就是 3000 秒。
    // 直接累加会让 played_ms 变成一个远超视频时长的数，
    // 而 played_ms 是卡顿率的**分母** —— 它会静默地把卡顿率压到接近 0。
    //
    // 上界取 1 秒：远大于正常的 0.25，又远小于任何有意义的 seek。
    // 同时 delta 必须为正（seek 往回拖时是负的，也不该算）。
    if (delta > 0 && delta < 1) {
      s.playedMs += delta * 1000

      // 码率加权：这段时间是以 currentBitrateKbps 播的。
      // 直传 mp4 时它是 0，加进去是 0，结果自然就是 avg=0 —— 无需分支。
      s.bitrateMsWeighted += s.currentBitrateKbps * delta * 1000
    }
  }

  /**
   * 拖进度条后必须让 lastCurrentTime 重新对齐。
   * 不做的话，seek 之后的第一次 timeupdate 会算出一个巨大的 delta ——
   * 虽然上面那个 `< 1` 的上界已经挡住了它，但**当前时间会因此偏离**，
   * 后续几次 timeupdate 都会是被上界挡掉的无效值（表现为播放时长凭空少几秒）。
   * 显式重置比依赖上界兜底更准。
   */
  function onSeeking() {
    state.value.lastCurrentTime = null
  }

  function onPlay() {
    state.value.pressedPlay = true
  }

  function onError() {
    // 只为让 shouldReport 知道"这次会话有内容可报"。
    // 错误码在 flush 时直接读 v.error.code，不在这里存。
    state.value.pressedPlay = true
  }

  // ---------- 绑定 / 解绑 ----------

  let bound: HTMLVideoElement | null = null

  function bind(v: HTMLVideoElement) {
    bound = v
    // t0 取"更早的那个"：元素可能在我们绑上之前就已经开始加载了
    // （:src 一写上去浏览器就开始拉），所以 bind 时刻是个偏晚的值。
    // 同时记 loadstart 时刻，两个取 min —— 首帧时间宁可报长不报短，
    // 和 entity.go 里选 CEIL 而不是 ROUND 是同一个取向。
    state.value.t0 = performance.now()

    v.addEventListener('loadstart', onLoadStart)
    v.addEventListener('play', onPlay)
    v.addEventListener('playing', onPlaying)
    v.addEventListener('waiting', onWaiting)
    v.addEventListener('timeupdate', onTimeUpdate)
    v.addEventListener('seeking', onSeeking)
    v.addEventListener('error', onError)
  }

  function onLoadStart() {
    const now = performance.now()
    const s = state.value
    if (s.t0 === null || now < s.t0) s.t0 = now
  }

  function unbind() {
    const v = bound
    if (!v) return
    v.removeEventListener('loadstart', onLoadStart)
    v.removeEventListener('play', onPlay)
    v.removeEventListener('playing', onPlaying)
    v.removeEventListener('waiting', onWaiting)
    v.removeEventListener('timeupdate', onTimeUpdate)
    v.removeEventListener('seeking', onSeeking)
    v.removeEventListener('error', onError)
    bound = null
  }

  // 元素会随 v-if 出现/消失，也可能被整体替换（换视频时 :key 变），
  // 所以要 watch 而不是 onMounted 绑一次了事。
  // flush: 'post' 保证在 DOM 更新之后跑，那时 ref 已经是新元素。
  watch(
    el,
    (v) => {
      unbind()
      if (v) bind(v)
    },
    { immediate: true, flush: 'post' },
  )

  // ---------- 组装上报体 ----------

  /**
   * 什么情况下**不**上报。两条：
   *
   *  1. 用户从没按过播放。进详情页、看一眼、走了 —— 这时所有指标都是 0。
   *     上报它会让 total_sessions 里混进大量"零会话"，
   *     首帧分位数和卡顿率的分母全被稀释。**表里的每一行都应该是"一次真实的播放尝试"。**
   *
   *  2. 一条数据都没有。理论上 1 成立时 2 必然成立，但分开写是因为
   *     将来可能加别的触发点（比如"预加载失败"），那时 1 可能不成立而 2 成立。
   *     两道都留着，成本是两个布尔判断。
   */
  function shouldReport(): boolean {
    const s = state.value
    return s.pressedPlay && (s.playedMs > 0 || s.firstPlayingAt !== null || bound?.error != null)
  }

  function buildReport(): QoEReport {
    const s = state.value
    const v = bound

    // 首帧 = loadstart → 首次 playing。没播成就是 0（服务端分位数会过滤掉 0）。
    //
    // ⚠ 这个数**会被 preload 策略影响**，改 preload 时要连着看：
    // VideoPlayer 已经从 preload="auto" 改成了 "metadata" —— 之前浏览器在
    // 用户按播放键之前就开始拉**媒体数据**，现在只拉文件头（拿时长/尺寸）。
    // 所以实际数据要等按播放才开始拉，首帧会**变大**。
    //
    // 也就是说："preload 优化"会让首帧这个数变差、同时让白传的字节变少。
    // **前后对比时这两个数必须一起看**，只看首帧会得出"优化把首帧搞慢了"的
    // 错误结论 —— 而真相是它把"用户没看就走"的那部分浪费消掉了。
    //
    // ⚠⚠ 另有一条**已知的口径问题**（未修，因为它是"定义"不是"实现"）：
    // t0 取的是 bind（组件挂载）时刻，而 firstPlayingAt 是用户**按下播放**
    // 之后才有的。两者之间如果用户发呆 30 秒，startup_ms 就是 30 秒 ——
    // 它量的是"进详情页到看到画面"，而不是"按播放到看到画面"。
    // 用户按得快时这个偏差很小（所以之前没暴露），按得慢时它是纯噪声。
    // 要修就得改口径（起点换成 onPlay 时刻），而口径一旦改，
    // 已记录的数据就不可比了 —— 所以留给"要不要改"这个决定，不擅自改。
    // 阶段 E 的降级对比用的是 stall_count / stall_total_ms，不受这条影响。
    const startupMs =
      s.t0 !== null && s.firstPlayingAt !== null ? Math.round(s.firstPlayingAt - s.t0) : 0

    // 卡顿还没结束时就被刷出（比如播到一半切后台）—— 只算已经结束的那些。
    // 不去把"进行中"的那段结算掉：它的结束时刻未知，猜一个只会让数据更脏。
    // 下一次刷出（页面卸载）会带上完整的值，而服务端是 upsert 取最后一次。

    const avgBitrate =
      s.playedMs > 0 ? Math.round(s.bitrateMsWeighted / s.playedMs) : s.currentBitrateKbps

    const conn = (navigator as Navigator & { connection?: { effectiveType?: string; downlink?: number } })
      .connection

    return {
      session_id: sessionId,
      video_id: videoId(),

      stall_count: s.stallCount,
      stall_total_ms: Math.round(s.stallTotalMs),
      played_ms: Math.round(s.playedMs),

      avg_bitrate_kbps: avgBitrate,
      tier_switches: s.tierSwitches,

      startup_ms: startupMs,
      dropped_frames: v?.getVideoPlaybackQuality?.().droppedVideoFrames ?? 0,
      error_code: v?.error ? String(v.error.code) : '',

      // navigator.connection **只有 Chromium 系有**，其他浏览器上是 undefined。
      // 用可选链取值、缺了就给空串/0 —— 服务端的这两个列本来就是可空的。
      effective_type: conn?.effectiveType ?? '',
      downlink_mbps: conn?.downlink ?? 0,
    }
  }

  // ---------- 刷出 ----------

  /**
   * 上报一次。`onUnload` 为 true 时走"页面正在消失"的那条路
   * （keepalive fetch，见 api/qoe.ts 里为什么不用 sendBeacon）。
   *
   * **没有"只发一次"的守卫**，这是刻意的：
   *
   * 页面隐藏（切标签页）时发一次，用户切回来接着看，最后卸载时再发一次 ——
   * 第二次携带的数据更完整，服务端 upsert 后写覆盖先写，正好是对的。
   * 如果加一个 `sent` 标志，第二次就被吞掉了，**用户切回来看的那段时间
   * 全部丢失**。
   *
   * 代价是同一次播放会产生 1~3 个请求。比起丢数据，这个代价可以接受 ——
   * 而且限流是 120/分钟，正常播放远远够用。
   */
  function flush(onUnload = false) {
    // 整个 flush 包在 try 里：埋点失败**绝不能**影响播放器卸载。
    // 这个函数会在 onUnmounted 里被调用，抛出去会打断后面的清理
    // （VideoPlayer 的 onUnmounted 里还有释放解码器的三步）。
    try {
      if (!shouldReport()) return
      const body = buildReport()
      if (onUnload) {
        reportOnUnload(body)
      } else {
        void reportQoE(body).catch(() => {
          /* 埋点失败静默。用户正在离开这一页，弹错误没有任何意义 */
        })
      }
    } catch {
      /* 同上：采集器自身的 bug 不该影响播放 */
    }
  }

  // ---------- 三个刷出时机 ----------

  function onVisibilityChange() {
    // 只在"变成隐藏"时刷。变成可见时刷没有意义（数据还没积累）
    if (document.visibilityState === 'hidden') flush()
  }

  function onPageHide() {
    // 页面真的要走 —— 这条路用 keepalive
    flush(true)
  }

  document.addEventListener('visibilitychange', onVisibilityChange)
  window.addEventListener('pagehide', onPageHide)

  onUnmounted(() => {
    // 顺序要紧：**先刷再解绑**。
    // 反过来的话 bound 已经是 null，buildReport 读不到播放器状态
    // （dropped_frames / error_code 全丢），而且 t0 也没了。
    flush()
    unbind()
    document.removeEventListener('visibilitychange', onVisibilityChange)
    window.removeEventListener('pagehide', onPageHide)
  })

  /**
   * HLS 档位变化时调用（阶段 D 接 hls.js 的 LEVEL_SWITCHED）。
   *
   * 为什么"切换次数"要在这里 +1 而不是在 flush 时比较首尾档位：
   * 一段播放里可能来回切很多次最后回到原档位，**首尾比较会得到 0 次**
   * —— 而"反复横跳"正是 ABR 最该被批评的行为，恰恰会被这个口径抹掉。
   */
  function setLevel(bitrateKbps: number) {
    const s = state.value
    if (s.currentBitrateKbps === bitrateKbps) return
    if (s.currentBitrateKbps !== 0) s.tierSwitches += 1
    s.currentBitrateKbps = bitrateKbps
  }

  return {
    /** 供阶段 D 的 hls.js 调用 */
    setLevel,
    /** 供调试用：手动触发一次上报 */
    flush,
    /** 只读，方便调试面板看 */
    sessionId,
  }
}
