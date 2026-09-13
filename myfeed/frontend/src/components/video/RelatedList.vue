<template>
  <section class="card related">
    <h2>{{ title }}</h2>

    <ul v-if="items.length" class="list">
      <!-- RouterLink 进了同一个路由但换了参数：父视图 watch route.params.id 重新拉数据，
           所以这里不用 @click 手动做什么 -->
      <li v-for="it in items" :key="it.id">
        <RouterLink class="row" :to="`/video/${it.id}`">
          <span class="thumb">
            <img :src="staticURL(it.cover_url)" :alt="it.title" loading="lazy" />
          </span>
          <span class="info">
            <span class="rt" :title="it.title">{{ it.title }}</span>
            <span class="rmeta">
              <span class="rauthor">{{ it.author.username }}</span>
              <span aria-hidden="true">·</span>
              <time class="mono">{{ formatTime(it.create_time) }}</time>
            </span>
          </span>
        </RouterLink>
      </li>
    </ul>

    <p v-else-if="loading" class="text-muted hint">加载中…</p>
    <p v-else class="text-muted hint">这一侧暂时没有别的视频。</p>
  </section>
</template>

<script setup lang="ts">
import { RouterLink } from 'vue-router'

import { staticURL } from '../../api/video'
import type { FeedVideoItem } from '../../api/feed'
import { formatTime } from '../../utils/time'

defineProps<{
  title: string
  items: FeedVideoItem[]
  loading: boolean
}>()
</script>

<style scoped>
.related {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 18px;
  border-radius: 18px;
}

h2 {
  margin: 0;
  font-size: 0.95rem;
  letter-spacing: -0.01em;
}

.list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.row {
  display: flex;
  gap: 10px;
  min-width: 0;
}

.thumb {
  flex: 0 0 auto;
  width: 104px;
  aspect-ratio: 16 / 9;
  border-radius: 7px;
  overflow: hidden;
  background: var(--surface-2);
  border: 1px solid var(--border);
}
.thumb img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}

.info {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}

.rt {
  font-size: 0.83rem;
  font-weight: 500;
  line-height: 1.4;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
  transition: color 0.18s var(--ease-out);
}
.row:hover .rt {
  color: var(--accent);
}

.rmeta {
  display: flex;
  align-items: center;
  gap: 5px;
  font-size: 0.72rem;
  color: var(--ink-muted);
  min-width: 0;
}
.rauthor {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.hint {
  margin: 0;
  font-size: 0.82rem;
}
</style>
