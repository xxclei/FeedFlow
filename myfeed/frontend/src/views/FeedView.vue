<template>
  <div class="page">
    <!--
      这个视图只有一份。两个端的差异全部下沉到叶子组件：
      桌面用 desktop/VideoGrid（满宽 4 列），移动用 mobile/VideoGrid（双列）。
      数据、游标、错误处理在这里写一次。
      数据本身活在 useFeedStream 的模块级单例里 —— 点卡片进播放页会卸载本视图，
      状态要是建在 setup 里就全丢了。
    -->
    <div class="head">
      <div class="head-left">
        <h1>{{ curTab.label }}</h1>
        <p class="lead mono">{{ curTab.hint }}</p>
      </div>
      <button class="btn btn-ghost sm" type="button" :disabled="cur.loading" @click="refresh">
        刷新
      </button>
    </div>

    <!-- 移动端的壳里没有频道条，所以在这里补一个分段控件 -->
    <nav v-if="!isDesktop" class="seg" aria-label="时间线">
      <button
        v-for="t in FEED_TABS"
        :key="t.key"
        type="button"
        :class="{ on: active === t.key }"
        @click="go(t.key)"
      >
        {{ t.label }}
      </button>
    </nav>

    <p v-if="!auth.isLoggedIn" class="guest">
      游客模式 —— 这四个接口挂的是 <code>SoftJWTAuth</code>，没登录也能刷，只是
      <code>is_liked</code> 恒为 false。
    </p>

    <component
      :is="Grid"
      :items="cur.items"
      :loading="cur.loading"
      :loaded="cur.loaded"
      :has-more="cur.hasMore"
      :error="cur.error"
      @load-more="loadMore"
    >
      <template #empty>
        <svg viewBox="0 0 64 64" width="52" height="52" aria-hidden="true" class="empty-icon">
          <rect x="8" y="16" width="48" height="34" rx="5" fill="none" stroke="currentColor" stroke-width="2" />
          <path d="M26 27.5v11l10-5.5z" fill="currentColor" />
          <path d="M20 10h24" stroke="currentColor" stroke-width="2" stroke-linecap="round" opacity="0.45" />
        </svg>
        <p class="empty-title">这条时间线还是空的</p>
        <p class="text-muted">去「投稿」发一条，它会立刻出现在这里。</p>
        <RouterLink class="btn" to="/video">去投稿</RouterLink>
      </template>
    </component>

    <DebugPanel
      :rows="cursorRows"
      :loaded="cur.items.length"
      :dup-count="cur.dupCount"
      :hint="curTab.hint"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import DebugPanel from '../components/DebugPanel.vue'
import DesktopVideoGrid from '../components/desktop/VideoGrid.vue'
import MobileVideoGrid from '../components/mobile/VideoGrid.vue'
import { FEED_TABS, isTabKey, useFeedStream, type TabKey } from '../composables/useFeedStream'
import { useDevice } from '../composables/useDevice'
import { useAuthStore } from '../stores/auth'

const auth = useAuthStore()
const route = useRoute()
const router = useRouter()
const { isDesktop } = useDevice()

const { active, cur, load, setTab } = useFeedStream(12)

// 端不同 → 网格组件不同，其余全一样
const Grid = computed(() => (isDesktop.value ? DesktopVideoGrid : MobileVideoGrid))
const curTab = computed(() => FEED_TABS.find((t) => t.key === active.value)!)

// ---------- 路由 ↔ 当前流 双向同步 ----------
// 用 query 而不是组件内状态：桌面频道条在壳里、移动分段控件在视图里，
// 两边都要能改，还要能深链（/feed?tab=popularity 直接分享）

async function syncFromRoute() {
  const t = route.query.tab
  await setTab(isTabKey(t) ? t : 'latest')
}

onMounted(syncFromRoute)
watch(() => route.query.tab, syncFromRoute)

function go(key: TabKey) {
  router.replace({ path: '/feed', query: key === 'latest' ? {} : { tab: key } })
}

function loadMore() {
  load(active.value, false)
}

function refresh() {
  // 刷新 = 丢掉当前流的书签，从第一页重新拉
  load(active.value, true)
}

// ---------- 调试面板：游标原值 ----------

function show(v: number | string | undefined): string {
  if (v === undefined || v === null || v === '') return '—（首页：不传）'
  return String(v)
}

const cursorRows = computed<{ k: string; v: string }[]>(() => {
  const st = cur.value
  if (active.value === 'latest') {
    return [{ k: 'latest_time', v: st.cTime ? String(st.cTime) : '0（首页）' }]
  }
  if (active.value === 'likes') {
    return [
      { k: 'likes_count_before', v: show(st.cLikes) },
      { k: 'id_before', v: show(st.cID) },
    ]
  }
  return [
    { k: 'latest_popularity', v: show(st.cPopularity) },
    { k: 'latest_before', v: show(st.cBefore) },
    { k: 'latest_id_before', v: show(st.cLatestID) },
  ]
})

</script>

<style scoped>
.head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 16px;
}
.head-left {
  min-width: 0;
}
h1 {
  margin: 0;
  font-size: 1.25rem;
  letter-spacing: -0.02em;
}
.lead {
  margin: 4px 0 0;
  font-size: 0.76rem;
  color: var(--ink-muted);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.btn.sm {
  padding: 0 14px;
  min-height: 36px;
  display: inline-flex;
  align-items: center;
  font-size: 0.85rem;
  flex: 0 0 auto;
}

.seg {
  margin-bottom: 14px;
}

.guest {
  margin: 0 0 14px;
  padding: 9px 13px;
  border-radius: 11px;
  border: 1px solid var(--border);
  background: var(--surface);
  color: var(--ink-muted);
  font-size: 0.82rem;
}

.empty-icon {
  color: var(--ink-muted);
  opacity: 0.7;
}

@media (max-width: 767px) {
  h1 {
    font-size: 1.1rem;
  }
  .lead {
    display: none; /* 手机上接口路径是噪音，调试面板里还有 */
  }
}
</style>
