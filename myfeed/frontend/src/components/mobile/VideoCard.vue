<template>
  <article class="vcard">
    <RouterLink class="cover" :to="`/video/${item.id}`" :aria-label="`打开 ${item.title}`">
      <img :src="staticURL(item.cover_url)" :alt="item.title" loading="lazy" />
      <span class="badge mono">{{ item.likes_count }}</span>
    </RouterLink>

    <h3 :title="item.title">
      <RouterLink :to="`/video/${item.id}`">{{ item.title }}</RouterLink>
    </h3>
    <div class="foot">
      <span class="author">{{ item.author.username }}</span>
      <span class="dot" aria-hidden="true">·</span>
      <time class="mono">{{ formatTime(item.create_time) }}</time>
    </div>
  </article>
</template>

<script setup lang="ts">
import { RouterLink } from 'vue-router'

import { staticURL } from '../../api/video'
import type { FeedVideoItem } from '../../api/feed'
import { formatTime } from '../../utils/time'

defineProps<{ item: FeedVideoItem }>()
</script>

<style scoped>
.vcard {
  display: flex;
  flex-direction: column;
  gap: 5px;
  min-width: 0;
}

.cover {
  position: relative;
  display: block;
  width: 100%;
  aspect-ratio: 16 / 9;
  padding: 0;
  border: 1px solid var(--border);
  border-radius: 7px;
  overflow: hidden;
  background: #000;
  cursor: pointer;
}
.cover img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}

.badge {
  position: absolute;
  right: 4px;
  bottom: 4px;
  padding: 0 5px;
  border-radius: 4px;
  background: rgba(15, 15, 18, 0.78);
  color: var(--ink);
  font-size: 0.63rem;
}

h3 {
  margin: 0;
  font-size: 0.8rem;
  font-weight: 500;
  line-height: 1.36;
  /* 移动端标题固定两行：高度一致，双列网格才不会参差 */
  display: -webkit-box;
  -webkit-line-clamp: 2;
  line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
  min-height: 2.72em;
}

.foot {
  display: flex;
  align-items: center;
  gap: 5px;
  font-size: 0.68rem;
  color: var(--ink-muted);
  min-width: 0;
}
.author {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  min-width: 0;
}
.dot {
  opacity: 0.5;
}
time {
  flex: 0 0 auto;
}
</style>
