<template>
  <div class="page">
    <div class="head">
      <div class="head-left">
        <RouterLink class="back" to="/feed">← 回到发现</RouterLink>
        <h1>
          <span class="q-mark">搜</span>
          <span class="q">{{ q }}</span>
        </h1>
        <p class="lead mono">POST /feed/search · MySQL FULLTEXT + ngram（本轮词法那一路）</p>
      </div>
      <RouterLink v-if="q" class="btn btn-ghost tagjump" :to="`/tag/${encodeURIComponent(q)}`">
        也看看标签 #{{ q }} →
      </RouterLink>
    </div>

    <!-- 表单（搜索页自己也要能改查询词，否则用户只能回顶栏再搜一次） -->
    <form class="box" role="search" @submit.prevent="submit">
      <input v-model="draft" type="search" placeholder="搜标题和描述，支持多个词" />
      <button class="btn" type="submit">搜索</button>
    </form>

    <!-- 400 的提示。**刻意不是红色报错**：它说的是"这个查询本身没意义"
         （只打了标点、或者游标坏了），不是"服务出故障了"。
         后端也刻意回 400 而不是空列表 —— 空列表的语义是"没有这个视频"，
         那种情况下用户会以为语料里真没有，然后一直换词试 -->
    <p v-if="badQuery" class="bad">
      {{ badQuery }}
      <template v-if="q">—— 试着去掉标点，或者换个具体的词。</template>
    </p>

    <component
      :is="Grid"
      :items="items"
      :loading="loading"
      :loaded="loaded"
      :has-more="hasMore"
      :error="gridError"
      @load-more="loadMore"
    >
      <template #empty>
        <p class="empty-title">没有找到相关视频</p>
        <p class="text-muted">
          {{ q ? `「${q}」没有命中任何标题或描述。` : '输入关键词开始搜索。' }}
          换个词试试，或者点右上角去标签流里翻。
        </p>
      </template>
    </component>

    <div ref="sentinel" class="sentinel" aria-hidden="true"></div>

    <DebugPanel
      :rows="debugRows"
      :loaded="items.length"
      :dup-count="dupCount"
      :hint="q ? `query=${q}` : '（翻页中，召回未重跑）'"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import DebugPanel from '../components/DebugPanel.vue'
import DesktopVideoGrid from '../components/desktop/VideoGrid.vue'
import MobileVideoGrid from '../components/mobile/VideoGrid.vue'
import { ApiError } from '../api/client'
import { searchVideos, type SearchArms, type SearchMode } from '../api/search'
import type { FeedVideoItem } from '../api/feed'
import { useDevice } from '../composables/useDevice'
import { useInfiniteScroll } from '../composables/useInfiniteScroll'

const route = useRoute()
const router = useRouter()
const { isDesktop } = useDevice()

const Grid = computed(() => (isDesktop.value ? DesktopVideoGrid : MobileVideoGrid))

const q = ref(String(route.query.q ?? ''))
const draft = ref(q.value)

const items = ref<FeedVideoItem[]>([])
const loading = ref(false)
const loaded = ref(false)
const hasMore = ref(false)
const error = ref('')
/** 后面几页的游标。第一页不传它，见 load() */
const cursor = ref('')

const mode = ref<SearchMode | null>(null)
const arms = ref<SearchArms>({ lexical: 0, vector: 0 })
const total = ref(0)

const sentinel = ref<HTMLElement | null>(null)

// ---------- 跨页重复检测 ----------
//
// 不是装饰性的：搜索用的是一个**冻结排名的令牌**（next_cursor 里冻着整份 id 列表
// 和偏移量），所以翻页理论上不可能重复或遗漏。真出现了，说明游标那条路坏了 ——
// 而"第二页悄悄重复了一条"这种问题，只有把它显示出来才会有人发现。
// 顺带它还能抓到"同一条视频在融合结果里出现两次"（两路都召回了它、却没合并）。
const dupCount = computed(() => {
  const seen = new Set<number>()
  let dup = 0
  for (const it of items.value) {
    if (seen.has(it.id)) dup++
    else seen.add(it.id)
  }
  return dup
})

/**
 * **只把"加载/服务端"的错误交给网格显示**，400 不进去。
 *
 * 400 的含义是"查询里没有可检索的词"或"游标无效"，这两种都不是故障：
 * 网格会把 error 渲染成一条红色横幅，而用户只需要知道"换个词"。
 */
const gridError = computed(() => (error.value && !badQuery.value ? error.value : ''))
const badQuery = ref('')

const debugRows = computed(() => [
  { k: 'mode · 实际跑成的形态', v: mode.value ?? '—' },
  { k: 'arms.lexical · 词法召回', v: String(arms.value.lexical) },
  { k: 'arms.vector · 向量召回', v: `${arms.value.vector}（本轮未接线）` },
  { k: 'total · 可翻候选数', v: String(total.value) },
  { k: 'next_cursor · 冻结令牌', v: cursor.value ? `${cursor.value.slice(0, 44)}…` : '（没有下一页）' },
])

function submit() {
  const next = draft.value.trim()
  if (!next) return
  // 换查询词必须**重置一切**：游标、列表、计数。带着旧游标去搜新词，
  // 后端会照旧切片（它不看查询词），于是你会看到上一个词的下一页 —— 而且不报错
  if (next === q.value) {
    void load(true)
    return
  }
  router.replace({ path: '/search', query: { q: next } })
}

/** 从头搜一次 */
async function load(reset = false) {
  if (loading.value) return
  loading.value = true
  error.value = ''
  badQuery.value = ''
  try {
    const res = await searchVideos({
      query: q.value,
      cursor: reset ? '' : cursor.value,
      limit: 20,
    })
    items.value = reset ? res.video_list ?? [] : items.value.concat(res.video_list ?? [])
    cursor.value = res.next_cursor ?? ''
    hasMore.value = res.has_more
    mode.value = res.mode
    arms.value = res.arms ?? { lexical: 0, vector: 0 }
    total.value = res.total
    loaded.value = true
  } catch (e) {
    if (e instanceof ApiError && e.status === 400) {
      // **这里不把消息塞进 error**：网格会把 error 渲染成"出故障了"的红色横幅，
      // 而 400 说的是"这个查询本身没意义" —— 两件事，两种提示
      badQuery.value = e.message || '换个关键词试试'
      items.value = []
      hasMore.value = false
    } else {
      error.value = e instanceof Error ? e.message : '搜索失败'
    }
  } finally {
    loading.value = false
  }
}

function loadMore() {
  if (!hasMore.value) return
  void load(false)
}

useInfiniteScroll(
  sentinel,
  () => !!q.value && !loading.value && hasMore.value,
  loadMore,
)

onMounted(() => {
  if (q.value) void load(true)
  else loaded.value = true
})

// 顶栏搜一次之后路由参数变了，但组件被复用 → 必须手动重查（和 TagView 同一个坑）
watch(
  () => route.query.q,
  (v) => {
    const next = String(v ?? '')
    if (next === q.value) return
    q.value = next
    draft.value = next
    items.value = []
    cursor.value = ''
    hasMore.value = false
    loaded.value = false
    mode.value = null
    total.value = 0
    // 等一帧：游标和列表的清理要和 load 的读取顺序对上，否则可能在清空之前就发出了请求
    void nextTick(() => load(true))
  },
)
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
  min-width: 0;
}
.q-mark {
  color: var(--accent);
  font-size: 0.9rem;
}
.q {
  overflow-wrap: anywhere;
}
.lead {
  margin: 4px 0 0;
  font-size: 0.76rem;
  color: var(--ink-muted);
}
.tagjump {
  min-height: 34px;
  padding: 0 14px;
  font-size: 0.84rem;
}

.box {
  display: flex;
  gap: 10px;
}
.box input {
  flex: 1;
  min-width: 0;
  padding: 11px 14px;
  border: 1px solid var(--border);
  border-radius: 12px;
  background: var(--surface-2);
  color: var(--ink);
  font-family: inherit;
  font-size: 0.95rem;
  outline: none;
}
.box input:focus {
  border-color: var(--accent);
  box-shadow: 0 0 0 2px var(--accent-soft);
}

/* 这一条是 400 的提示，刻意做成中性信息面板而不是红色报错 */
.bad {
  margin: 0;
  padding: 10px 14px;
  border: 1px dashed var(--border);
  border-radius: 12px;
  background: var(--surface-2);
  font-size: 0.84rem;
  color: var(--ink-muted);
}
.sentinel {
  height: 1px;
}
.empty-title {
  font-size: 1rem;
  font-weight: 600;
  margin: 0 0 4px;
}

@media (max-width: 767px) {
  .lead {
    display: none;
  }
  .box {
    flex-wrap: wrap;
  }
}
</style>
