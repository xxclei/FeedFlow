<template>
  <div class="page">
    <!-- 和详情页同理：移动端壳的顶栏没有返回键，被"推进去"的一层得自己给出口 -->
    <button v-if="!isDesktop" class="back" type="button" @click="goBack">
      <svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true">
        <path
          d="M14.5 5.5 8 12l6.5 6.5"
          fill="none"
          stroke="currentColor"
          stroke-width="2"
          stroke-linecap="round"
          stroke-linejoin="round"
        />
      </svg>
      返回
    </button>

    <div class="head">
      <div class="head-left">
        <h1>我的点赞</h1>
        <p class="lead mono">POST /like/listMyLikedVideos · 无请求体 · 无游标，LIMIT 200</p>
      </div>
      <button class="btn btn-ghost sm" type="button" :disabled="loading" @click="load">
        刷新
      </button>
    </div>

    <!-- 这里是列表页不是详情页，但请求失败的处理是同一件事：说清楚 + 给条出路 -->
    <p v-if="error" class="alert">{{ error }}</p>

    <p v-else-if="!loading && items.length" class="lead-muted">
      共 {{ items.length }} 条，按<strong>点赞时间</strong>倒序（不是视频发布时间）。
      取消点赞在视频详情页里做。
    </p>

    <component
      :is="Grid"
      :items="items"
      :loading="loading"
      :loaded="loaded"
      :has-more="false"
      :error="error"
    >
      <template #empty>
        <svg viewBox="0 0 64 64" width="52" height="52" aria-hidden="true" class="empty-icon">
          <path
            d="M18 27v19h-5.5a2 2 0 0 1-2-2V29a2 2 0 0 1 2-2zm0 0 10.7-17.4a4 4 0 0 1 7.2 3.2L33.7 22h13a4.2 4.2 0 0 1 4.2 5.2l-3.7 17.5a4.2 4.2 0 0 1-4.2 3.3H18"
            fill="none"
            stroke="currentColor"
            stroke-width="2"
            stroke-linejoin="round"
          />
        </svg>
        <p class="empty-title">还没有赞过任何视频</p>
        <p class="text-muted">在视频详情页点一下点赞，它就会出现在这里。</p>
        <RouterLink class="btn" to="/feed">去发现页</RouterLink>
      </template>
    </component>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'

import DesktopVideoGrid from '../components/desktop/VideoGrid.vue'
import MobileVideoGrid from '../components/mobile/VideoGrid.vue'
import { ApiError } from '../api/client'
import type { FeedVideoItem } from '../api/feed'
import { listMyLikedVideos } from '../api/like'
import { normalizeVideo } from '../api/video'
import { useDevice } from '../composables/useDevice'

const router = useRouter()
const { isDesktop } = useDevice()

// 两个端的差异全在叶子组件里，这个视图只有一份（和 FeedView 同一套写法）
const Grid = computed(() => (isDesktop.value ? DesktopVideoGrid : MobileVideoGrid))

const items = ref<FeedVideoItem[]>([])
const loading = ref(false)
const loaded = ref(false)
const error = ref('')

async function load() {
  loading.value = true
  error.value = ''
  try {
    const raw = await listMyLikedVideos()
    // 返回的是 **videos 表的原始形状**（扁平 username + RFC3339 时间），
    // 不是 FeedVideoItem —— 必须过一遍 normalizeVideo，否则时间会渲染成 Invalid Date
    items.value = raw.map((v) => ({
      ...normalizeVideo(v),
      // normalizeVideo 里 is_liked 只能是 false（/video/* 没有这个字段），
      // 但这一页的每一条**按定义就是赞过的** —— 这里覆盖成 true，
      // 卡片上的「已赞」标记才是诚实的
      is_liked: true,
    }))
  } catch (e) {
    // 401 单列一句：这个页面有 requiresAuth 守卫，走到这里说明 token 是在
    // 页面打开之后才过期的（client.ts 已经 clearTokens，但 view 不会自动重挂）
    if (e instanceof ApiError && e.status === 401) {
      error.value = '登录状态已失效，请重新登录后再看。'
    } else {
      error.value = e instanceof Error ? e.message : '加载失败'
    }
  } finally {
    loading.value = false
    // loaded 和 loading 分开：VideoGrid 靠 loaded 决定"空"是首屏未加载还是真的没有，
    // 失败时置 true 会让它显示"空列表"，那就是在撒谎了
    loaded.value = !error.value
  }
}

onMounted(load)

function goBack() {
  if (window.history.state?.back) router.back()
  else router.replace('/home')
}
</script>

<style scoped>
.head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 14px;
  margin-bottom: 18px;
}
.head-left {
  min-width: 0;
}
h1 {
  margin: 0 0 4px;
  font-size: 1.3rem;
  letter-spacing: -0.02em;
}
.lead {
  margin: 0;
  font-size: 0.72rem;
  color: var(--ink-muted);
  overflow-wrap: anywhere;
}
.lead-muted {
  margin: 0 0 16px;
  font-size: 0.82rem;
  color: var(--ink-muted);
}

/* .page 不是 flex 容器（就是 max-width + margin auto），所以这里不用 align-self */
.back {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  min-height: 40px;
  margin-bottom: 6px;
  padding: 0 12px 0 8px;
  border: none;
  border-radius: 10px;
  background: transparent;
  color: var(--ink-muted);
  font-family: inherit;
  font-size: 0.86rem;
  font-weight: 600;
  cursor: pointer;
}
.back:hover {
  color: var(--ink);
  background: var(--surface-2);
}

@media (max-width: 767px) {
  h1 {
    font-size: 1.1rem;
  }
  /* 移动端头部把刷新按钮挤到第二行，标题独占一行更好读 */
  .head {
    flex-direction: column;
    align-items: stretch;
    gap: 10px;
  }
}
</style>
