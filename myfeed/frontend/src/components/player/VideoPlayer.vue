<template>
  <div class="player">
    <!-- preload="metadata" 而不是 "auto"。
         "auto" 是让浏览器**立刻开始下载整个文件**，哪怕用户根本没点播放 ——
         而下面第 27 行那段注释（和这个组件的整个交互设计）说的是"点了才放"。
         这两件事一直是矛盾的：用户进详情页、看一眼、没点就走，
         这中间已经白拉走了一段 38 MB 的文件（素材平均 38.2 MB，
         按 12 Mbps 上行算是 25.5 秒的整条上行带宽）。

         "metadata" 只拿文件头（时长/尺寸），不碰媒体数据。
         本项目的素材 111/111 都是 faststart（moov 在文件头），
         所以这一次请求很小 —— 换来的代价是用户按下播放后首帧会**变慢**
         （之前已经预下载了一段，现在要从头拉）。

         ⚠ 所以这是一次**有明确代价的取舍**，不是纯优化：
         白传的字节 ↓，首帧 ↑。两个数必须一起看，
         只看首帧会得出"这个改动把播放搞慢了"的错误结论。
         基准数据见 scripts/ 下的 QoE 记录。 -->
    <video
      v-if="!failed"
      ref="el"
      class="video"
      controls
      playsinline
      webkit-playsinline
      preload="metadata"
      :src="useHls ? undefined : staticURL(playUrl)"
      :poster="staticURL(coverUrl)"
      @playing="started = true"
      @error="failed = true"
    ></video>

    <!-- 播放器自己的错误态。文件是真可能不在的：.run/uploads 是本地目录，
         而且 deleteVideo **只删数据库记录、不动磁盘**，反过来也可能有人手工清了文件 -->
    <div v-else class="broken">
      <p class="broken-title">视频文件打不开</p>
      <p class="broken-hint">
        浏览器拿不到能解码的流。可能是文件被删了、编码不被支持，或者后端没在跑。
      </p>
      <code class="broken-url">{{ staticURL(playUrl) }}</code>
    </div>

    <!-- 大播放键浮层。**不自动播放**：Chrome 的静音自动播放策略看的是"文档有没有被用户交互过"，
         从卡片点进来时文档有交互历史，play() 往往真的会成功 —— 于是同一个 URL
         点进来会自己放、直接刷新或从别人分享的链接进来却停在浮层上，两种行为，
         调起来极其难受。统一成"点了才放"，行为确定。
         （代价是少一点"进来就播"的顺滑，换来的是同一地址永远同一种表现。） -->
    <button v-if="!failed && !started" class="overlay" type="button" @click="play">
      <span class="glyph" aria-hidden="true">
        <svg viewBox="0 0 24 24" width="28" height="28">
          <path d="M8 5.2v13.6L19 12z" fill="currentColor" />
        </svg>
      </span>
      <span class="overlay-title">{{ title }}</span>
    </button>

    <!-- 清晰度选择。**只在真的有得选的时候出现**（判据见 script 里的 canPickLevel）。
         放右上角而不是底部：<video controls> 的原生控件在底部，
         自己再叠一排上去会打架（挡住进度条），而且移动端那一排更高。 -->
    <div v-if="!failed && canPickLevel" class="levels" role="group" aria-label="清晰度">
      <button class="lv" :class="{ on: pickedLevel === -1 }" type="button" @click="pickLevel(-1)">
        自动
      </button>
      <button
        v-for="lv in levels"
        :key="lv.index"
        class="lv"
        :class="{ on: pickedLevel === lv.index }"
        type="button"
        @click="pickLevel(lv.index)"
      >
        {{ lv.label }}
      </button>
    </div>
  </div>
</template>

<script setup lang="ts">
import type HlsType from 'hls.js'
import { computed, onUnmounted, ref } from 'vue'

import { staticURL } from '../../api/video'
import { useQoE } from '../../composables/useQoE'

/** videoId 是给 QoE 埋点用的（哪条视频的播放质量）。
 *  注意**渲染完全不依赖它** —— 播放器播的是 playUrl，id 只用于上报。
 *  做成必填而不是可选，是为了让调用方无法"忘了传"：
 *  漏传的话埋点会全部记到 video_id=0 上，而那不会报任何错。 */
const props = defineProps<{
  videoId: number
  playUrl: string
  coverUrl: string
  title: string
  /** HLS master playlist 的**路径**（后端给的是 `/static/hls/<id>/master.m3u8`）。
   *  非空就走 hls.js，空就走直传 mp4 那条老路。
   *
   *  可选 + 默认空串：这样**所有老调用点一个字都不用改**，
   *  而且阶段 B 判成 `skipped` 的视频（和 127 条改造前的存量）天然走老路，
   *  不会因为"多了个 HLS 播放器"而回归。 */
  hlsUrl?: string
}>()

const el = ref<HTMLVideoElement | null>(null)
/** 用 playing 而不是 play：play 只表示"播放被请求了"，这时还没有任何一帧解出来，
 *  拿它去收浮层会露出一块黑。playing 才是"画面真的在走了" */
const started = ref(false)
const failed = ref(false)

/** 能不能走 HLS。**只认 hlsUrl 非空**，不再去看 transcode_status ——
 *  后端的不变量是"这一列只在 ready 时才非空"，判两次只会多出一个
 *  两边可能不一致的分支。详细理由见 `api/feed.ts` 的 FeedVideoItem.hls_url。 */
const useHls = computed(() => (props.hlsUrl ?? '').length > 0)

/** 一档清晰度。index 是它在 hls.js `levels` 数组里的**下标**。
 *
 *  这里存的必须是下标，不能是"第几档"或者码率：hls.js 的 `nextLevel` / `currentLevel`
 *  要的都是下标，而 `levels` 的顺序是按 master 里的出现顺序（hls.js 解析后
 *  一般按 BANDWIDTH 升序排，但那是它的实现，不是接口承诺）。
 *  将来 master 里增删一档，所有下标都会平移 —— 所以**只在同一次解析的结果内使用**。 */
interface LevelOption {
  index: number
  label: string
}

/** master 解析出来的档位。空数组 = 还不能选（没按过播放，或者源不是 HLS）。 */
const levels = ref<LevelOption[]>([])

/** 用户**手动**选的档位，`-1` = 自动（交回 ABR 自己决定）。
 *
 *  它和"当前正在播的那一档"是**两件不同的事**，这是这个选择器最容易写错的地方：
 *  自动模式下 ABR 会不停切档，如果高亮跟的是"当前档"，"自动"那颗按钮就会
 *  随着每次切档一闪一闪（而且用户点"自动"之后立刻又跳走，看起来像没点上）。
 *  所以高亮跟的是**用户的意愿** pickedLevel，不是实际状态。 */
const pickedLevel = ref(-1)

/** 选择器只在**真的有得选**时出现。
 *
 *  判据是 `>= 2` 而不是 `> 0`，因为阶段 E 的过载降级是**服务端**把 master
 *  缩成一档 —— 那种情况下只剩一个选项，"选"这个动作没有意义，
 *  正确的表现是**整排按钮消失**，而不是显示一排只能点一个的按钮
 *  （后者会让人以为"只有低清"，而其实是服务端在降级）。 */
const canPickLevel = computed(() => useHls.value && levels.value.length >= 2)

// 播放质量采集。返回值的 setLevel 接在下面 hls.js 的 LEVEL_SWITCHED 上 ——
// avg_bitrate_kbps / tier_switches 这两个数就是 ABR 效果的度量。
//
// 用 props.videoId 的取值函数而不是值本身：详情页会在同一个组件实例里
// 从 /video/1 换到 /video/2。虽然父组件把 :key 绑在 play_url 上、
// 换视频时这个组件整体重建（所以实际拿到的永远是当次的值），
// 但传函数不会依赖那个实现细节 —— 万一将来 :key 去掉，这里依然是对的。
const qoe = useQoE(el, () => props.videoId)

/**
 * hls.js 实例。**延迟到第一次按播放才创建** —— 这是这个文件里最要紧的一个决定。
 *
 * ---------- 为什么不能挂载时就建 ----------
 *
 * hls.js 的默认行为是 attachMedia + loadSource 之后**立刻开始拉清单和分片**。
 * 那和这个组件通篇的设计直接冲突：上面那段 preload 的注释花了 16 行说明
 * "用户进详情页、看一眼、没点就走"这条路不该白拉字节 ——
 * 直传那条路为此把 preload 从 auto 降到了 metadata。
 *
 * 如果 HLS 这条路上挂载即加载，那**恰好是 preload=auto 的复现**，
 * 而且更难发现（它不是 <video> 的属性，是 hls.js 的默认值）。
 * 直传那条路白拉一份 moov，HLS 这条路白拉 master + 若干分片，后者更贵。
 *
 * 用 `autoStartLoad: false` + 按播放再 startLoad() 也是 hls.js 的正规写法，
 * 但它**是否真的不拉 master** 取决于版本实现（清单往往在 loadSource 时就取了）。
 * 延迟构造没有这层不确定性：不构造 = 一个字节都不会发出去。
 * 结构上保证，而不是靠一个开关的语义。见 §转码记录里的同一条判断。
 *
 * 代价：按播放之后要先等一次 master 的 RTT 才开始拉分片，首帧比直传那条路晚。
 * 这是明确的取舍 —— 和 preload 那次一样，**白传的字节 ↓、首帧 ↑**。
 */
let hls: HlsType | null = null
/** 动态 import 是异步的，两次快点播放会同时进来 —— 没有这个标记就会建两个实例，
 *  第一个的定时器和 XHR 再也没人 destroy（DevTools 里看到分片拉了两遍）。 */
let starting = false

async function startHls(v: HTMLVideoElement) {
  const url = staticURL(props.hlsUrl ?? '')

  // ---------- 为什么用动态 import 而不是顶部 import ----------
  //
  // hls.js 打包后约 150 kB（gzip ~50 kB）。顶部 import 的话它会被算进
  // **VideoDetailView 这个路由 chunk**，于是**每一条**视频的详情页都要
  // 先下完它才能开始播 —— 包括 127 条改造前的存量和门禁判成 skipped 的那些，
  // 它们永远不会走 HLS 这条路（实测：改造后这个 chunk 从 21 kB 涨到 597 kB）。
  //
  // 这和这个文件开头的 preload 那一段是同一个判断：**没有用到的东西不该先付钱**。
  // 区别只是那次省的是媒体字节，这次省的是 JS 字节。
  //
  // ⚠ 代价：按播放之后多一次 chunk 下载的 RTT（本机 / 局域网是毫秒级，
  // 首次访问 + 冷缓存时会明显一些）。**只影响 HLS 这条路**，直传不受影响。
  const { default: Hls } = await import('hls.js')

  // 等 chunk 的这段时间里组件可能已经卸载了（用户点了播放又立刻退出）
  if (!el.value || el.value !== v) return

  // Safari / iOS 原生支持 HLS，**优先用它**而不是 hls.js：
  // 原生那条路走硬解、省电，而且 hls.js 在 iOS 上本来就只能靠 MSE 兜。
  if (!Hls.isSupported()) {
    if (v.canPlayType('application/vnd.apple.mpegurl')) {
      v.src = url
      return
    }
    // 两条路都不行。**这时必须切直传**，而不是显示"打不开" ——
    // 直传的 mp4 一直都在（转码不删源文件），它是永远可用的兜底。
    v.src = staticURL(props.playUrl)
    return
  }

  hls = new Hls({
    // 起手就按真实上行去试，而不是 hls.js 默认那个 500 kbps 的经验值 ——
    // 本项目的场景是局域网/本机，默认值会让它在 480p 上磨蹭好几秒才升档。
    // 但**不设固定的 startLevel**：服务端过载降级时 master 里只剩一档，
    // 那时候任何"从中间档开始"的策略都会失效。
    abrEwmaDefaultEstimate: 5_000_000,
  })

  hls.on(Hls.Events.MANIFEST_PARSED, (_evt, data) => {
    // **按了播放之后选择器才会出现** —— 因为 levels 只有解析完 master 才有内容。
    // 这是上面"延迟构造 hls.js"那个决定的连带结果，不打算为它破例：
    // 想提前知道有哪几档，就得在挂载时先拉一次 master，
    // 那等于把刚省掉的那次请求原样加回来（还多搭进去 hls.js 的 150 kB chunk）。
    //
    // 用 data.levels 而不是 hls.levels：data 是这次解析的结果，不依赖模块级
    // 那个可变变量此刻指向谁（虽然实际同一个，但少一个能空指针的入口）。
    levels.value = data.levels.map((l, i) => ({ index: i, label: levelLabel(l) }))
  })

  hls.on(Hls.Events.LEVEL_SWITCHED, (_evt, data) => {
    // bitrate 是 bps，埋点表里存的是 kbps —— 口径转换只在这里做一次。
    const level = hls?.levels?.[data.level]
    if (level) qoe.setLevel(Math.round(level.bitrate / 1000))
  })

  hls.on(Hls.Events.ERROR, (_evt, data) => {
    // **只有 fatal 才算失败。** 非 fatal 的（比如某个分片重试成功了）
    // 每次网络抖动都会来一条，拿它切错误态会让播放器无缘无故变成"打不开"。
    if (!data.fatal) return
    failed.value = true
  })

  hls.loadSource(url)
  hls.attachMedia(v)
}

function play() {
  const v = el.value
  if (!v) return

  // ---------- HLS 这条路上 play() 必须等 attachMedia 之后再调 ----------
  //
  // 元素在这一刻**还没有任何 src**（上面 :src 在 HLS 模式下是 undefined），
  // 所以这时候调 play() 只会被浏览器拒掉（"no supported sources"），
  // 而 hls.js 自己**不会**帮你起播（autoStartLoad 管的是加载，不是播放）。
  // 早先的写法是先 startHls 再无条件 play，结果是按了播放键画面不动 ——
  // 浮层收了、进度条不走，看起来像"卡住了"，其实一行请求都没发。
  if (useHls.value && hls === null) {
    if (starting) return // 已经在拉了，别建第二个实例
    starting = true
    // 动态 import 可能失败（chunk 拉不下来）。**失败了要能继续播** ——
    // 切直传，而不是留一个点不动的播放键。
    void startHls(v)
      .then(() => replay(v))
      .catch(() => {
        v.src = staticURL(props.playUrl)
        replay(v)
      })
      .finally(() => {
        starting = false
      })
    return
  }

  replay(v)
}

/** play() 返回 Promise，被策略拒绝时是 reject 而不是抛异常 —— 不 catch 就是 unhandled rejection */
function replay(v: HTMLVideoElement) {
  v.play().catch(() => {
    /* 浮层留着，用户再点一次就行 */
  })
}

/** 一档的显示名：优先用分辨率高度（`1080p`），拿不到才退到码率（`1200k`）。
 *
 *  为什么不用 `l.name`：那个 NAME 是**转码时自己填进 master 的**，
 *  而本项目的 master 是服务端现场生成的（阶段 E 降级时只发低档），
 *  name 可能是空的、也可能几档写成同一个。
 *  `height` 是 hls.js 从 master 的 RESOLUTION 属性解析出来的数字，比 name 可靠；
 *  真的连 RESOLUTION 都没有时（有些转码器会省掉），至少还有码率可显示 ——
 *  这一档**不会返回空串**，否则按钮会变成一个看不出来是什么的空框。 */
function levelLabel(l: { height: number; bitrate: number }): string {
  if (l.height > 0) return `${l.height}p`
  return `${Math.round(l.bitrate / 1000)}k`
}

/**
 * 手动选档。`-1` = 交回给 ABR —— 这是 hls.js 自己的约定，不是这里发明的哨兵值。
 *
 * ---------- 三个 setter 的区别，别按名字猜 ----------
 *
 * hls.js 有三个长得极像的入口，语义完全不同（下面这段是照
 * `node_modules/hls.js/dist/hls.d.ts` 的原话写的，不是凭印象 ——
 * 我第一版注释就把 nextLevel 和 loadLevel 说反了）：
 *
 *	currentLevel  立刻换。**会 flush 当前缓冲区**去尽快换档，
 *	              文档原话 "playback will interrupt at least shortly to re-buffer"
 *	nextLevel     换下一段加载的数据。尽快生效、**不打断播放**，
 *	              可能中止正在进行的加载、flush 掉播放点之外的缓冲
 *	loadLevel     最保守：不 flush，但会打断当前加载 ——
 *	              生效点是"已缓冲的部分播完之后"，可能几十秒，太钝
 *
 * ---------- 为什么这里用 nextLevel 而不是看起来最"跟手"的 currentLevel ----------
 *
 * 决定性理由不是体验，是**这个项目在给自己量卡顿**。
 * `currentLevel` 每次都会造成一次真实的重缓冲，而 useQoE.ts 把 `waiting` 记成
 * `stall_count` —— 也就是说用户每点一次清晰度，**看板上的卡顿率就自己涨一点**，
 * 而那次卡顿是播放器自己造的，跟网络、跟服务端都无关。
 * 一个用来判断"服务端改动有没有让播放变差"的指标，不能掺进这种自伤。
 *
 * 代价认下来：生效点在下一个分片边界，本项目分片 6 秒 —— 点完最长约 6 秒
 * 才看到画质变化，中间画面照常播（不黑、不卡）。按钮的**高亮立刻变**
 * （pickedLevel 是同步改的），所以不存在"点了没反应"的观感问题，
 * 只是画面慢一拍 —— 这个取舍比"立刻黑一下 + 埋点自己记一笔"好得多。
 *
 * ⚠ 阶段 E 的过载降级**不经过这里**：那是服务端把 master 缩成一档，
 * 前端只是收到一个更短的 levels，`canPickLevel` 因此为 false、整排按钮消失。
 * 换句话说这里**没有**"帮服务端降级"的逻辑，也不该有 —— 前端能做的选择
 * 只能在服务端给出的选项里选。 */
function pickLevel(index: number) {
  pickedLevel.value = index
  if (hls) hls.nextLevel = index
}

/**
 * 卸载时**必须**主动释放解码器。
 *
 * 光靠组件销毁不够：<video> 元素会一直攥着解码资源直到 GC，移动端 Safari 上尤其明显。
 * 这三步（pause → 清 src → load）是 utils/cover.ts 里抽帧那条路已经验证过的写法，
 * 在详情页同样必要 —— 而且父组件把 :key 绑在 play_url 上，从 /video/1 换到 /video/2
 * 时旧播放器会先整个卸载，正好走这里放手。
 *
 * hls.js 的 destroy() 同理且更硬：它自己开着一堆 XHR/Worker，
 * 不管的话组件销毁之后**分片还在后台拉**（DevTools 里能看到请求继续）。
 */
onUnmounted(() => {
  hls?.destroy()
  hls = null
  // 档位一起清掉。**当前观察不到差别**（父组件把 :key 绑在 play_url 上，
  // 换视频时整个组件重建，这两个 ref 跟着新实例重新初始化）。
  // 留着是因为 `hls` 是**模块级**的：它的生命周期和组件实例并不绑定。
  // 一旦那个 :key 被去掉、组件被复用，残留的 levels 会和新的 hls 实例对不上 ——
  // 那时候按钮上写着 480p，按下去设的是新 master 的第 4 档。宁可现在多两行。
  levels.value = []
  pickedLevel.value = -1

  const v = el.value
  if (!v) return
  v.pause()
  // 清 src 之后要显式 load()，元素才会真正松手
  v.removeAttribute('src')
  v.load()
})
</script>

<style scoped>
.player {
  position: relative;
  width: 100%;
  aspect-ratio: 16 / 9;
  background: #000;
  border: 1px solid var(--border);
  border-radius: 14px;
  overflow: hidden;
}

.video {
  display: block;
  width: 100%;
  height: 100%;
  background: #000;
  /* contain 而不是 cover：卡片上裁掉一点看不出来，播放器上裁掉的是画面本身 */
  object-fit: contain;
}

.overlay {
  position: absolute;
  inset: 0;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 16px;
  padding: 20px;
  border: none;
  background: linear-gradient(180deg, rgba(15, 15, 18, 0.1), rgba(15, 15, 18, 0.72));
  color: var(--ink);
  font-family: inherit;
  cursor: pointer;
}

.glyph {
  display: grid;
  place-items: center;
  width: 62px;
  height: 62px;
  padding-left: 4px; /* 三角形的视觉重心偏左，补一点回来 */
  border-radius: 50%;
  background: rgba(232, 103, 74, 0.94);
  box-shadow: var(--shadow);
  transition: transform 0.2s var(--ease-spring);
}
.overlay:hover .glyph {
  transform: scale(1.07);
}

.overlay-title {
  max-width: min(80%, 560px);
  font-size: 0.95rem;
  font-weight: 600;
  text-align: center;
  text-shadow: 0 2px 12px rgba(0, 0, 0, 0.7);
}

.levels {
  position: absolute;
  top: 10px;
  right: 10px;
  display: flex;
  gap: 2px;
  padding: 3px;
  border-radius: 999px;
  /* 半透明底而不是实底：这是**叠在画面上的**控件，全屏时那块底色越不显眼越好。
     backdrop-filter 拿不到支持时（老 Safari）退化成一块深色底，同样能用。 */
  background: rgba(15, 15, 18, 0.62);
  backdrop-filter: blur(6px);
  /* 原生控件在底部，但**全屏/移动端**它们会浮得更高，而且有的浏览器
     会给控件层自己的 z-index —— 显式抬一层，别被盖住。 */
  z-index: 2;
}

.lv {
  min-width: 42px;
  padding: 4px 9px;
  border: none;
  border-radius: 999px;
  background: transparent;
  color: rgba(255, 255, 255, 0.72);
  font-family: inherit;
  font-size: 0.7rem;
  font-weight: 600;
  line-height: 1.5;
  cursor: pointer;
  transition: background 0.15s var(--ease-out);
}
.lv:hover {
  color: #fff;
}
/* 选中的那颗。**只有 pickedLevel 会点亮**，正在播的实际档位不点亮 ——
   自动模式下后者一直在变，跟着它会让高亮乱跳（理由见 script 里的 pickedLevel）。 */
.lv.on {
  background: rgba(232, 103, 74, 0.94);
  color: #fff;
}

.broken {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
  height: 100%;
  padding: 24px;
  text-align: center;
}
.broken-title {
  margin: 0;
  font-size: 1rem;
  font-weight: 600;
}
.broken-hint {
  margin: 0;
  max-width: 460px;
  font-size: 0.83rem;
  color: var(--ink-muted);
}
.broken-url {
  margin-top: 4px;
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 0.72rem;
  color: var(--ink-muted);
}
</style>
