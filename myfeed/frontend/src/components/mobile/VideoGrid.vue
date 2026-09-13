<template>
  <div class="wrap">
    <p v-if="error" class="alert">{{ error }}</p>

    <div v-if="loading && items.length === 0" class="grid">
      <div v-for="i in skeleton" :key="`sk-${i}`" class="sk">
        <div class="sk-cover shimmer"></div>
        <div class="sk-line shimmer" style="width: 92%"></div>
        <div class="sk-line shimmer" style="width: 58%"></div>
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
        :style="{ animationDelay: `${Math.min(i, 9) * 40}ms` }"
        :item="it"
      />
    </div>

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
  { skeleton: 8 },
)

const emit = defineEmits<{ (e: 'load-more'): void }>()

const sentinel = ref<HTMLElement | null>(null)

useInfiniteScroll(
  sentinel,
  () => props.hasMore && !props.loading,
  () => emit('load-more'),
  '600px 0px', // 移动端滚得快，提前量给大一点
)
</script>

<style scoped>
.wrap {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

/* 固定双列（B 站移动端首页就是这个密度），不用 auto-fill：
   手机宽度区间窄，两列永远是最优解，留 auto-fill 反而会在小屏塌成一列 */
.grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 14px 9px;
}

.sk {
  display: flex;
  flex-direction: column;
  gap: 5px;
}
.sk-cover {
  width: 100%;
  aspect-ratio: 16 / 9;
  border-radius: 7px;
}
.sk-line {
  height: 9px;
  border-radius: 5px;
}

.sentinel {
  display: flex;
  justify-content: center;
  padding: 8px 0;
  min-height: 48px;
}
.end-note {
  margin: 0;
  font-size: 0.78rem;
}
</style>
