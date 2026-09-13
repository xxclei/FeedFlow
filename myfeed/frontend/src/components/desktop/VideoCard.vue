<template>
  <article class="vcard">
    <!-- 封面：B 站那种 16:9 大图。点它**进播放页**，不在这里就地播 ——
         卡片尺寸（250×140）播 16:9 的片子本来也看不清，真正的播放场景是 /video/:id。
         用 RouterLink 而不是按钮：它渲染成真的 <a href>，Ctrl+点击、中键、
         "在新标签页打开"这些浏览器原生的动作都免费拿到。 -->
    <RouterLink class="cover" :to="`/video/${item.id}`" :aria-label="`打开 ${item.title}`">
      <img :src="staticURL(item.cover_url)" :alt="item.title" loading="lazy" />
      <span class="play" aria-hidden="true">
        <svg viewBox="0 0 24 24" width="17" height="17"><path d="M8 5.2v13.6L19 12z" fill="currentColor" /></svg>
      </span>
      <!-- 角标位：B 站放时长，我们还没有 duration 字段，放点赞数 -->
      <span class="badge mono">{{ item.likes_count }}</span>
    </RouterLink>

    <div class="meta">
      <h3 :title="item.title">
        <RouterLink :to="`/video/${item.id}`">{{ item.title }}</RouterLink>
      </h3>

      <RouterLink v-if="tags.length" class="tag" :to="`/tag/${tags[0]}`">#{{ tags[0] }}</RouterLink>

      <div class="foot">
        <span class="avatar" aria-hidden="true">{{ initial }}</span>
        <span class="author">{{ item.author.username }}</span>
        <span class="dot" aria-hidden="true">·</span>
        <time class="mono" :title="fullTime(item.create_time)">{{ formatTime(item.create_time) }}</time>
      </div>
    </div>
  </article>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink } from 'vue-router'

import { staticURL } from '../../api/video'
import type { FeedVideoItem } from '../../api/feed'
import { extractTags } from '../../utils/tags'
import { formatTime, fullTime } from '../../utils/time'

const props = defineProps<{ item: FeedVideoItem }>()

const initial = computed(() => props.item.author.username?.slice(0, 1).toUpperCase() || '?')
const tags = computed(() => extractTags(props.item.title, props.item.description))
</script>

<style scoped>
.vcard {
  display: flex;
  flex-direction: column;
  gap: 8px;
  min-width: 0;
}

/* ---------- 封面 ---------- */
.cover {
  position: relative;
  display: block;
  width: 100%;
  aspect-ratio: 16 / 9;
  padding: 0;
  border: 1px solid var(--border);
  border-radius: 10px;
  overflow: hidden;
  background: #000;
  cursor: pointer;
  transition: border-color 0.2s var(--ease-out);
}
.cover img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
  transition: transform 0.32s var(--ease-out);
}
.vcard:hover .cover img {
  transform: scale(1.035);
}
.vcard:hover .cover {
  border-color: var(--accent-line);
}

.play {
  position: absolute;
  inset: 0;
  margin: auto;
  width: 42px;
  height: 42px;
  display: flex;
  align-items: center;
  justify-content: center;
  border-radius: 50%;
  background: rgba(15, 15, 18, 0.6);
  backdrop-filter: blur(6px);
  color: var(--ink);
  padding-left: 2px;
  opacity: 0;
  transform: scale(0.86);
  transition:
    opacity 0.2s var(--ease-out),
    transform 0.2s var(--ease-spring);
}
.vcard:hover .play {
  opacity: 1;
  transform: scale(1);
}

.badge {
  position: absolute;
  right: 6px;
  bottom: 6px;
  padding: 1px 6px;
  border-radius: 5px;
  background: rgba(15, 15, 18, 0.78);
  color: var(--ink);
  font-size: 0.7rem;
}

/* ---------- 文字 ---------- */
.meta {
  display: flex;
  flex-direction: column;
  gap: 5px;
  min-width: 0;
}
h3 {
  margin: 0;
  font-size: 0.92rem;
  font-weight: 600;
  line-height: 1.42;
  letter-spacing: -0.01em;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
  transition: color 0.18s var(--ease-out);
}
/* 标题里的链接要继承 h3 的颜色。不写这条的话全局的 `a { color: var(--ink) }`
   会盖掉继承，"悬停变赤陶"就失效了。 */
h3 a {
  color: inherit;
}
.vcard:hover h3 {
  color: var(--accent);
}

.tag {
  align-self: flex-start;
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  padding: 1px 8px;
  border-radius: 999px;
  border: 1px solid var(--border);
  background: var(--surface-2);
  color: var(--ink-muted);
  font-size: 0.72rem;
}
.tag:hover {
  color: var(--accent);
  border-color: var(--accent-line);
}

.foot {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 0.78rem;
  color: var(--ink-muted);
  min-width: 0;
}
.avatar {
  flex: 0 0 auto;
  width: 19px;
  height: 19px;
  border-radius: 50%;
  display: grid;
  place-items: center;
  background: var(--surface-2);
  border: 1px solid var(--border);
  color: var(--accent);
  font-size: 0.64rem;
  font-weight: 700;
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
