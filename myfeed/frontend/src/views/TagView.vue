<template>
  <div class="page">
    <div class="head">
      <div class="head-left">
        <RouterLink class="back" to="/feed">← 回到发现</RouterLink>
        <h1><span class="hash">#</span>{{ name }}</h1>
        <p class="lead mono">POST /feed/listByTag · 双 JOIN（videos ↔ video_tags ↔ tags）</p>
      </div>
    </div>

    <component
      :is="Grid"
      :items="items"
      :loading="loading"
      :loaded="loaded"
      :has-more="false"
      :error="error"
      @load-more="() => {}"
    >
      <template #empty>
        <p class="empty-title">还没有视频打这个标签</p>
        <p class="text-muted">
          发布时在标题里写 <code>#{{ name }}</code>，就会出现在这儿。
        </p>
        <RouterLink class="btn" to="/video">去投稿</RouterLink>
      </template>
    </component>

    <p v-if="loaded && items.length" class="end-note text-muted">
      共 {{ items.length }} 条 · 本阶段标签流没有游标（阶段7 补 Redis 分页）
    </p>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'

import DesktopVideoGrid from '../components/desktop/VideoGrid.vue'
import MobileVideoGrid from '../components/mobile/VideoGrid.vue'
import { listByTag, type FeedVideoItem } from '../api/feed'
import { useDevice } from '../composables/useDevice'

const route = useRoute()
const { isDesktop } = useDevice()

const Grid = computed(() => (isDesktop.value ? DesktopVideoGrid : MobileVideoGrid))

const name = ref(String(route.params.name ?? ''))
const items = ref<FeedVideoItem[]>([])
const loading = ref(false)
const loaded = ref(false)
const error = ref('')

async function load() {
  loading.value = true
  error.value = ''
  try {
    const res = await listByTag(name.value, 20)
    items.value = res.video_list ?? []
    loaded.value = true
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
    items.value = []
  } finally {
    loading.value = false
  }
}

onMounted(load)
// 从卡片上点另一个标签时路由参数变了但组件复用，得手动重查
watch(
  () => route.params.name,
  (v) => {
    name.value = String(v ?? '')
    loaded.value = false
    items.value = []
    load()
  },
)
</script>

<style scoped>
.head {
  margin-bottom: 16px;
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
}
.hash {
  color: var(--accent);
}
.lead {
  margin: 4px 0 0;
  font-size: 0.76rem;
  color: var(--ink-muted);
}
.end-note {
  margin: 18px 0 0;
  font-size: 0.8rem;
  text-align: center;
}
@media (max-width: 767px) {
  .lead {
    display: none;
  }
}
</style>
