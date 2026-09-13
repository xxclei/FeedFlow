<template>
  <section class="card author">
    <div class="top">
      <span class="avatar" aria-hidden="true">
        <img v-if="avatarURL" :src="avatarURL" alt="" />
        <template v-else>{{ initial }}</template>
      </span>

      <div class="who">
        <!-- 作者名**只**来自 /account/findByID。videos.username 是发布时抄的快照，
             改名后不会更新 —— 拿它当占位就会看到名字先旧后新地翻一下。 -->
        <template v-if="author">
          <strong class="name">{{ author.username }}</strong>
          <span class="uid mono">UID {{ author.id }}</span>
        </template>
        <template v-else>
          <span class="sk-line shimmer" style="width: 96px"></span>
          <span class="sk-line shimmer" style="width: 58px"></span>
        </template>
      </div>
    </div>

    <p v-if="author?.bio" class="bio">{{ author.bio }}</p>
    <p v-else-if="author" class="bio text-muted">这个账号还没写简介。</p>

    <!-- 关注：后端要到阶段6才有 social 模块。禁用态 + 可见角标，
         和桌面频道条的「关注 · 阶段6」、以及操作栏的「阶段4」是同一套说法 -->
    <button
      v-if="!isOwner"
      class="follow"
      type="button"
      aria-disabled="true"
      aria-label="关注（阶段6接入）"
      title="阶段6接入（依赖 social 模块，后端还没注册路由）"
      @click="noop"
    >
      关注 <span class="soon">阶段6</span>
    </button>
    <p v-else class="self text-muted">这是你自己的作品。</p>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'

import { staticURL } from '../../api/video'
import type { AccountInfo } from '../../api/account'

const props = defineProps<{
  author: AccountInfo | null
  isOwner: boolean
}>()

const initial = computed(() => props.author?.username?.slice(0, 1).toUpperCase() ?? '?')
const avatarURL = computed(() => (props.author?.avatar_url ? staticURL(props.author.avatar_url) : ''))

function noop() {
  /* 接口还不存在 */
}
</script>

<style scoped>
.author {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 18px;
  border-radius: 18px;
}

.top {
  display: flex;
  align-items: center;
  gap: 12px;
  min-width: 0;
}

.avatar {
  flex: 0 0 auto;
  width: 46px;
  height: 46px;
  border-radius: 50%;
  overflow: hidden;
  display: grid;
  place-items: center;
  background: var(--surface-2);
  border: 1px solid var(--accent-line);
  color: var(--accent);
  font-weight: 700;
  font-size: 1.1rem;
}
.avatar img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
}

.who {
  display: flex;
  flex-direction: column;
  gap: 3px;
  min-width: 0;
}
.name {
  font-size: 0.98rem;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.uid {
  color: var(--ink-muted);
}

.sk-line {
  display: block;
  height: 11px;
  border-radius: 6px;
}

.bio {
  margin: 0;
  font-size: 0.83rem;
  line-height: 1.6;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}

.follow {
  align-self: flex-start;
  display: inline-flex;
  align-items: center;
  gap: 7px;
  min-height: 38px;
  padding: 0 16px;
  border: 1px solid var(--border);
  border-radius: 11px;
  background: var(--surface-2);
  color: var(--ink-muted);
  font-family: inherit;
  font-size: 0.88rem;
  font-weight: 600;
  cursor: not-allowed;
}

.soon {
  padding: 1px 6px;
  border-radius: 999px;
  border: 1px solid var(--border);
  background: var(--surface);
  font-size: 0.68rem;
  font-weight: 600;
}

.self {
  margin: 0;
  font-size: 0.82rem;
}
</style>
