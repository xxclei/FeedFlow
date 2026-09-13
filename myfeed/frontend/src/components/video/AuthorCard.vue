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
          <!-- 从阶段6 起名字可以点：/profile/:id 是公开页面，游客也进得去。
               详情页是个人主页最主要的入口 —— 评论区那几条名字只能带你去评论者 -->
          <RouterLink class="name" :to="`/profile/${author.id}`" title="打开 TA 的主页">
            {{ author.username }}
          </RouterLink>
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

    <!-- 关注：从阶段6 起是真按钮了（原来是禁用态 + 「阶段6」角标）。
         按钮本体和它那一整套状态机在 FollowButton.vue 里 ——
         个人主页（ProfileView）用的是同一个组件。
         作者信息还没到时（author === null）先不渲染：vloggerId 是必填的 prop，
         而此时我们根本不知道要关注谁 -->
    <p v-if="isOwner" class="self text-muted">这是你自己的作品。</p>
    <FollowButton v-else-if="author" :vlogger-id="author.id" />
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink } from 'vue-router'

import FollowButton from './FollowButton.vue'
import { staticURL } from '../../api/video'
import type { AccountInfo } from '../../api/account'

const props = defineProps<{
  author: AccountInfo | null
  isOwner: boolean
}>()

const initial = computed(() => props.author?.username?.slice(0, 1).toUpperCase() ?? '?')
const avatarURL = computed(() => (props.author?.avatar_url ? staticURL(props.author.avatar_url) : ''))
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
  font-weight: 700;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
/* 名字是链到 /profile/:id 的。别的链接默认是主色，但这里"名字"本身就是
   页面上的主标识，染成橙色反而像个次要操作 —— 保持正文颜色，hover 才给反馈 */
a.name {
  color: var(--ink);
  text-decoration: none;
}
a.name:hover {
  color: var(--accent);
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

/* .follow 和 .soon 的样式随按钮一起搬去了 FollowButton.vue（scoped 样式不跨组件） */

.self {
  margin: 0;
  font-size: 0.82rem;
}
</style>
