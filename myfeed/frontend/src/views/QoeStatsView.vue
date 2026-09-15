<template>
  <div class="page">
    <div class="head">
      <div class="head-left">
        <RouterLink class="back" to="/home">← 回到我的</RouterLink>
        <h1>
          <span class="q-mark">QoE</span>
          <span class="q">播放质量看板</span>
        </h1>
        <p class="lead mono">POST /qoe/stats · 只读聚合分布，不含任何用户维度</p>
      </div>
    </div>

    <!-- 过滤条。**做成表单而不是进页面就读一次**：这个页面存在的意义就是
         改动前后对比（缓存头、ABR、阶段 E 的降级），所以"换一组条件再看一遍"
         是主要动作。视频 ID 留空 / 填 0 = 全部视频。 -->
    <form class="box" @submit.prevent="apply">
      <label class="fld">
        <span class="fld-k">视频 ID</span>
        <input
          v-model="videoIdText"
          type="search"
          inputmode="numeric"
          placeholder="留空 = 全部视频"
        />
      </label>

      <div class="days" role="group" aria-label="时间范围">
        <button
          v-for="opt in DAY_OPTIONS"
          :key="opt.value"
          class="d"
          :class="{ on: days === opt.value }"
          type="button"
          @click="pickDays(opt.value)"
        >
          {{ opt.label }}
        </button>
      </div>

      <button class="btn" type="submit">查询</button>
    </form>

    <!-- 当前生效的条件，**永远显示**。看板的数字不写清口径就没有意义：
         同一批数据"最近 7 天"和"全部时间"能差出一个数量级，
         而这上面只有一个被点亮的小按钮，很容易看串。 -->
    <p class="scope mono">
      {{ videoLabel }} · {{ daysLabel }}
      <template v-if="stats"> · {{ stats.total_sessions }} 次会话</template>
    </p>

    <p v-if="error" class="text-error">{{ error }}</p>
    <p v-else-if="!loaded" class="text-muted">读一次看板…</p>

    <template v-else-if="stats">
      <!-- 空表要单独说清楚，因为**它和"看板坏了"长得一模一样**（全 0）。
           区别在原因：这里说的是"这段时间真的没有数据"，
           而数据来自 useQoE.ts 的三条刷出路径 —— 没播过就没数据。 -->
      <p v-if="stats.total_sessions === 0" class="bad">
        这段时间里一次播放都没报上来。
        <br />
        看板读的是 <code>qoe_events</code> 表，数据由播放页的埋点写入 ——
        先去<RouterLink to="/feed">随便播一条</RouterLink>，再回来刷新。
        另外播放器卸载时那条上报走的是 <code>fetch keepalive</code>，
        如果后端没在跑，它会静默失败（埋点丢一条不影响播放，这是刻意的）。
      </p>

      <template v-else>
        <div class="cards">
          <div class="stat">
            <span class="k">会话数</span>
            <strong class="v">{{ fmtInt(stats.total_sessions) }}</strong>
            <span class="sub">
              其中游客 {{ fmtInt(stats.guests) }}（{{ pct(guestsRatio) }}）——
              游客占比异常升高说明鉴权接线出问题了
            </span>
          </div>

          <div class="stat">
            <span class="k">卡顿率 · 会话口径</span>
            <strong class="v">{{ pct(sessionStallRatio) }}</strong>
            <span class="sub">
              {{ fmtInt(stats.stalled_sessions) }} 次会话至少卡过一次。
              时长口径 {{ pct(stats.stall_ratio) }} —— <strong>两个数不一致本身是信息</strong>，
              见下
            </span>
          </div>

          <div class="stat">
            <span class="k">首帧 · p50 / p95 / p99</span>
            <strong class="v">{{ ms(stats.startup.p50_ms) }}</strong>
            <span class="sub">
              p95 {{ ms(stats.startup.p95_ms) }} · p99 {{ ms(stats.startup.p99_ms) }}
              <br />
              {{ fmtInt(stats.startup.n) }} 个样本（首帧 = loadstart → 首次 playing）
            </span>
          </div>

          <div class="stat">
            <span class="k">平均码率</span>
            <strong class="v">{{ fmtInt(stats.avg_bitrate_kbps) }} kbps</strong>
            <span class="sub">
              按 {{ fmtInt(stats.bitrate_sessions) }} 个 HLS 会话算 ——
              直传那条路的 avg_bitrate_kbps 是 0，混进来会把均值拉成假数
            </span>
          </div>
        </div>

        <!-- 两种口径并列解释。这一段不是装饰：单看一个数会得出相反结论 -->
        <p class="bad">
          <strong>卡顿率为什么给两个口径：</strong>
          会话口径高、时长口径低 = 很多次很短的卡顿（烦人但不难受）；
          会话口径低、时长口径高 = 少数几次很长的卡顿（难受，而且这些人大概率直接流失了）。
          合成一个数就看不出这个区别了。
        </p>

        <div class="card rise">
          <h3>码率分布</h3>
          <p class="text-muted small">
            <!-- 字段名叫 tiers，但装的其实是**码率分桶**而不是档位
                 （1080p/720p/480p）—— 后端按 avg_bitrate_kbps 切了四段。
                 档位那个标签客户端根本没上报，所以看板只能到分桶这一层。 -->
            后端按 <code>avg_bitrate_kbps</code> 切的四段（不是 1080p/720p 那些档位标签，
            客户端没上报档位名）。只统计 HLS 会话。
          </p>
          <ul class="bars">
            <li v-for="t in stats.tiers" :key="t.label" :class="{ zero: t.count === 0 }">
              <span class="bl mono">{{ t.label }}</span>
              <span class="bar"><i :style="{ width: barWidth(t.count) }"></i></span>
              <span class="bc mono">{{ fmtInt(t.count) }}</span>
            </li>
          </ul>
        </div>

        <div class="cards">
          <div class="stat">
            <span class="k">丢帧累计</span>
            <strong class="v">{{ fmtInt(stats.total_dropped_frames) }}</strong>
            <span class="sub">
              <code>getVideoPlaybackQuality()</code>。只在**解码跟不上**时增长 ——
              网络卡顿不会让它涨，那是另一回事（看卡顿率）
            </span>
          </div>

          <div class="stat">
            <span class="k">切档累计</span>
            <strong class="v">{{ fmtInt(stats.total_tier_switches) }}</strong>
            <span class="sub">
              hls.js 的 <code>LEVEL_SWITCHED</code>，ABR 每次换档 +1。
              这是 ABR 效果的度量：手动选档只会 +1，自动模式会持续增长
            </span>
          </div>
        </div>
      </template>

      <!-- 原始 JSON。刻意不做成"好看的表格"：这个页面所有的数字都是从这里
           挑出来的，把原文留着，「我看到的数」和「服务端真给的数」不一致时
           一眼就能发现（比如接口加了字段、或者某个数字没被格式化对）。 -->
      <details class="raw">
        <summary class="mono">原始响应</summary>
        <pre class="mono">{{ rawJson }}</pre>
      </details>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import { getQoEStats, type QoEStats } from '../api/qoe'

const route = useRoute()
const router = useRouter()

/**
 * 时间范围的三个选项。
 *
 * ⚠ **`0` 不能出现在这里**：后端把 `days = 0` 解释成"用默认值 7 天"
 * （Go 的零值既表示"没传"也表示"我就要 0"，那边选了"零值即默认"）。
 * 所以"全部时间"只能传**负数**，这里用 -1。
 * 顺带：那也意味着**任何想表达"不限时间"的地方都不能传 0** ——
 * 传了就是一个静默的 7 天，看板上完全看不出来。
 */
const DAY_OPTIONS = [
  { label: '7 天', value: 7 },
  { label: '30 天', value: 30 },
  { label: '全部', value: -1 },
] as const

const stats = ref<QoEStats | null>(null)
const loaded = ref(false)
const error = ref('')

/** 输入框里的文本。**不直接绑 number**：v-model.number 在输入框被清空时
 *  会给出空串（不是 0），而 `<input type="number">` 又会把 "12abc" 变成空。
 *  用文本 + 解析，行为可控：非数字一律当 0（全部视频），
 *  而"当前口径"那一行永远把它显示出来，错了也看得见。 */
const videoIdText = ref('')
const days = ref<number>(7)

const videoId = computed(() => {
  const n = Number.parseInt(videoIdText.value.trim(), 10)
  return Number.isFinite(n) && n > 0 ? n : 0
})

const videoLabel = computed(() => (videoId.value > 0 ? `视频 #${videoId.value}` : '全部视频'))
const daysLabel = computed(() => (days.value < 0 ? '全部时间' : `最近 ${days.value} 天`))

const rawJson = computed(() => (stats.value ? JSON.stringify(stats.value, null, 2) : ''))

const guestsRatio = computed(() => {
  const s = stats.value
  return s && s.total_sessions > 0 ? s.guests / s.total_sessions : 0
})
const sessionStallRatio = computed(() => {
  const s = stats.value
  return s && s.total_sessions > 0 ? s.stalled_sessions / s.total_sessions : 0
})

/** 分桶条的最大值。**用四桶里的最大值而不是总会话数做分母** ——
 *  四桶加起来才是 HLS 会话数，拿总数当分母的话每根都短得看不出来。 */
const barMax = computed(() => {
  const t = stats.value?.tiers ?? []
  return Math.max(1, ...t.map((x) => x.count))
})

function barWidth(count: number): string {
  return `${Math.round((count / barMax.value) * 100)}%`
}

function fmtInt(n: number): string {
  return n.toLocaleString('zh-CN')
}

function pct(x: number): string {
  // 两位小数：卡顿率（时长口径）在正常数据上经常是 0.x% 甚至更小，
  // 只留一位的话「0.4%」和「0.04%」会显示成同一个数
  return `${(x * 100).toFixed(2)}%`
}

function ms(v: number): string {
  return v >= 1000 ? `${(v / 1000).toFixed(2)} s` : `${v} ms`
}

/** 拉一次看板。**两个参数都显式给**，不吃后端的默认值：
 *  依赖默认值的话，今天"默认 7 天"就是页面上写着的那 7 天，
 *  哪天后端把 defaultStatsDays 改成 14，这一页显示的仍然写着 7 天但拿的是 14 天的数。 */
async function load() {
  error.value = ''
  try {
    stats.value = await getQoEStats(videoId.value, days.value)
    loaded.value = true
  } catch (e) {
    error.value = e instanceof Error ? e.message : '看板读取失败'
  }
}

/** 把条件和 URL 同步。**这是刻意的**：这一页的主要用途是"改动前后各看一次"，
 *  能直接分享/收藏"这个条件的这一屏"比截图强 —— 阶段 E 验收要对比的
 *  「降级开 vs 关」两组数字，就是同一串 query 换一个 d 或 v。 */
function syncQuery() {
  void router.replace({
    path: '/qoe',
    query: {
      ...(videoId.value > 0 ? { v: String(videoId.value) } : {}),
      // 默认的 7 天不进 URL：让地址栏短一点，也让"没有参数"= 默认这件事成立
      ...(days.value !== 7 ? { d: String(days.value) } : {}),
    },
  })
}

function apply() {
  syncQuery()
  void load()
}

function pickDays(v: number) {
  days.value = v
  apply()
}

onMounted(() => {
  // 从 URL 恢复条件。**只读一次、不 watch route**：进这一页的唯一入口是
  // /home 的链接，没有第二个组件会在这页活着的时候改 query
  // （SearchView 之所以要 watch，是因为顶栏搜索框会在组件复用时改它 —— 这里没有那个入口）。
  const v = Number.parseInt(String(route.query.v ?? ''), 10)
  if (Number.isFinite(v) && v > 0) videoIdText.value = String(v)
  const d = Number.parseInt(String(route.query.d ?? ''), 10)
  if (Number.isFinite(d) && d !== 0) days.value = d
  void load()
})
</script>

<style scoped>
.page {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
}
.back {
  display: inline-block;
  font-size: 0.83rem;
  color: var(--ink-muted);
  margin-bottom: 8px;
}
h1 {
  margin: 0;
  font-size: 1.35rem;
  letter-spacing: -0.02em;
  display: flex;
  align-items: baseline;
  gap: 8px;
}
.q-mark {
  color: var(--accent);
  font-size: 0.9rem;
}
.lead {
  margin: 4px 0 0;
  font-size: 0.76rem;
  color: var(--ink-muted);
}
h3 {
  margin: 0 0 6px;
  font-size: 1rem;
  letter-spacing: -0.02em;
}

.box {
  display: flex;
  align-items: flex-end;
  gap: 10px;
  flex-wrap: wrap;
}
.fld {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 160px;
}
.fld-k {
  font-size: 0.72rem;
  color: var(--ink-muted);
}
.fld input {
  padding: 10px 14px;
  border: 1px solid var(--border);
  border-radius: 12px;
  background: var(--surface-2);
  color: var(--ink);
  font-family: inherit;
  font-size: 0.9rem;
  outline: none;
}
.fld input:focus {
  border-color: var(--accent);
  box-shadow: 0 0 0 2px var(--accent-soft);
}

.days {
  display: flex;
  gap: 2px;
  padding: 3px;
  border: 1px solid var(--border);
  border-radius: 999px;
  background: var(--surface-2);
}
.d {
  padding: 6px 14px;
  border: none;
  border-radius: 999px;
  background: transparent;
  color: var(--ink-muted);
  font-family: inherit;
  font-size: 0.82rem;
  font-weight: 600;
  cursor: pointer;
  transition: background 0.15s var(--ease-out);
}
.d.on {
  background: var(--accent);
  color: #fff;
}

.scope {
  margin: 0;
  font-size: 0.76rem;
  color: var(--ink-muted);
}

/* 中性信息面板（和 SearchView 的 .bad 同一个用意）：这里说的是"没有数据"/"口径说明"，
   不是"服务坏了"，用红色报错会让人以为看板坏了 */
.bad {
  margin: 0;
  padding: 10px 14px;
  border: 1px dashed var(--border);
  border-radius: 12px;
  background: var(--surface-2);
  font-size: 0.84rem;
  color: var(--ink-muted);
  line-height: 1.7;
}
.bad a {
  color: var(--accent);
}
.bad code {
  font-family: var(--font-mono);
  font-size: 0.78rem;
}

.cards {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(210px, 1fr));
  gap: 12px;
}
.stat {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 14px 16px;
  border: 1px solid var(--border);
  border-radius: 14px;
  background: var(--surface-2);
}
.stat .k {
  font-size: 0.74rem;
  color: var(--ink-muted);
}
.stat .v {
  font-size: 1.5rem;
  letter-spacing: -0.02em;
  font-variant-numeric: tabular-nums;
}
.stat .sub {
  font-size: 0.74rem;
  color: var(--ink-muted);
  line-height: 1.6;
}
.stat code {
  font-family: var(--font-mono);
}

.small {
  font-size: 0.78rem;
  line-height: 1.7;
}
.small code {
  font-family: var(--font-mono);
  font-size: 0.74rem;
}

.bars {
  list-style: none;
  margin: 12px 0 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.bars li {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 0.78rem;
}
.bl {
  min-width: 9.6em;
  color: var(--ink-muted);
}
.bar {
  flex: 1;
  min-width: 0;
  height: 8px;
  border-radius: 999px;
  background: var(--surface-2);
  overflow: hidden;
}
.bar i {
  display: block;
  height: 100%;
  border-radius: 999px;
  background: var(--accent);
}
.bc {
  min-width: 3em;
  text-align: right;
  color: var(--ink-muted);
}
/* 计数为 0 的桶整行压暗。**不隐藏** —— 四段是固定铺开的，
   少一行会让人以为"这个桶不存在"，而事实是"这段时间没有会话落在这一段"，
   后者恰恰是个有用的读数（比如 3000+ 一档全是 0，说明降级生效了）。 */
.bars li.zero {
  opacity: 0.42;
}

.raw {
  border: 1px solid var(--border);
  border-radius: 12px;
  background: var(--surface-2);
  padding: 10px 14px;
}
.raw summary {
  font-size: 0.78rem;
  color: var(--ink-muted);
  cursor: pointer;
}
.raw pre {
  margin: 10px 0 0;
  max-height: 320px;
  overflow: auto;
  font-size: 0.72rem;
  line-height: 1.6;
  color: var(--ink-muted);
}

@media (max-width: 767px) {
  .lead {
    display: none;
  }
  .fld {
    min-width: 0;
    flex: 1;
  }
  .box .btn {
    flex: 1;
  }
  /* 窄屏上四个桶名太长，换成上下排 —— 横排会把条形挤到几乎看不见 */
  .bars li {
    flex-wrap: wrap;
    gap: 4px 10px;
  }
  .bar {
    flex-basis: 100%;
    order: 3;
  }
  .bc {
    margin-left: auto;
  }
}
</style>
