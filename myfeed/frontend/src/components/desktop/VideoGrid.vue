<template>
  <div class="wrap">
    <p v-if="error" class="alert">{{ error }}</p>

    <!-- 首屏骨架：与真实卡片同形（B 站的骨架也是封面块 + 两行标题），不用转圈 -->
    <div v-if="loading && items.length === 0" class="grid">
      <div v-for="i in skeleton" :key="`sk-${i}`" class="sk">
        <div class="sk-cover shimmer"></div>
        <div class="sk-line shimmer" style="width: 90%"></div>
        <div class="sk-line shimmer" style="width: 52%"></div>
      </div>
    </div>

    <div v-else-if="loaded && items.length === 0" class="empty-block">
      <slot name="empty" />
    </div>

    <div v-else class="grid">
      <VideoCard
        v-for="(it, i) in items"
        :key="`${it.id}-${i}`"
        class="rise"
        :style="{ animationDelay: `${Math.min(i, 11) * 45}ms` }"
        :item="it"
      />
    </div>

    <!-- 哨兵：滚进视口就自动翻下一页 -->
    <div ref="sentinel" class="sentinel">
      <button
        v-if="hasMore"
        class="btn btn-ghost"
        type="button"
        :disabled="loading"
        @click="$emit('load-more')"
      >
        {{ loading ? '加载中…' : '加载更多' }}
      </button>
      <p v-else-if="items.length" class="text-muted end-note">到底了 · 共 {{ items.length }} 条</p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'

import VideoCard from './VideoCard.vue'
import type { FeedVideoItem } from '../../api/feed'
import { useInfiniteScroll } from '../../composables/useInfiniteScroll'

const props = withDefaults(
  defineProps<{
    items: FeedVideoItem[]
    loading: boolean
    loaded: boolean
    hasMore: boolean
    error: string
    skeleton?: number
  }>(),
  { skeleton: 10 },
)

const emit = defineEmits<{ (e: 'load-more'): void }>()

const sentinel = ref<HTMLElement | null>(null)

useInfiniteScroll(
  sentinel,
  () => props.hasMore && !props.loading,
  () => emit('load-more'),
)
</script>

<style scoped>
.wrap {
  display: flex;
  flex-direction: column;
  gap: 18px;
}

/* auto-fill + minmax：列数随容器宽度自己变，不用写一堆媒体查询。
   250px 这个下界决定了满宽时的列数：`--page-max` 1180 减去 `.content` 的 48px padding
   只剩 1132px 可用，而 4×250+3×18 = 1054 ≤ 1132（排得下 4 列）、
   5×250+4×18 = 1322 > 1132（排不下 5 列）—— 所以宽屏正好 4 个。

   不写死 repeat(4, 1fr)：那样 768px 宽的桌面窗口会被挤成 4 列各 170px，标题两行全成省略号。
   给下界就能自动退成 3 列（≈1000px）、2 列（≈800px）。 */
.grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(250px, 1fr));
  gap: 20px 18px;
}

.sk {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.sk-cover {
  width: 100%;
  aspect-ratio: 16 / 9;
  border-radius: 10px;
}
.sk-line {
  height: 11px;
  border-radius: 6px;
}

.sentinel {
  display: flex;
  justify-content: center;
  padding: 10px 0 4px;
  min-height: 52px;
}
.end-note {
  margin: 0;
  font-size: 0.85rem;
}
</style>
