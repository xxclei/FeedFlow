<template>
  <div class="shell">
    <!-- 顶栏：B 站那种「左 logo / 中搜索 / 右操作」，全站 sticky -->
    <header class="topbar">
      <div class="topbar-inner">
        <RouterLink class="brand" to="/feed">
          <span class="brand-dot" aria-hidden="true"></span>
          <span class="brand-name">myfeed</span>
        </RouterLink>

        <form class="search" role="search" @submit.prevent="onSearch">
          <svg viewBox="0 0 24 24" width="15" height="15" aria-hidden="true">
            <circle cx="11" cy="11" r="6.5" fill="none" stroke="currentColor" stroke-width="2" />
            <path d="m16 16 4.5 4.5" stroke="currentColor" stroke-width="2" stroke-linecap="round" />
          </svg>
          <input v-model="q" type="search" placeholder="搜标签，输入 日常 或 #日常 回车" />
        </form>

        <div class="top-actions">
          <template v-if="auth.isLoggedIn">
            <RouterLink class="btn post-btn" to="/video">投稿</RouterLink>
            <RouterLink class="me" to="/home" title="账号">
              <span class="avatar" aria-hidden="true">{{ initial }}</span>
              <span class="me-name">{{ auth.claims?.username }}</span>
            </RouterLink>
            <button class="btn btn-ghost sm" type="button" @click="onLogout">登出</button>
          </template>
          <template v-else>
            <RouterLink class="btn btn-ghost sm" to="/login">登录</RouterLink>
            <RouterLink class="btn post-btn" to="/register">注册</RouterLink>
          </template>
        </div>
      </div>
    </header>

    <!-- 二级频道条：桌面端专属。移动端没有它，改用内容区的分段控件 -->
    <nav class="channels">
      <div class="channels-inner">
        <template v-for="c in channels" :key="c.label">
          <span v-if="c.soon" class="channel off" :title="c.soon">{{ c.label }}</span>
          <RouterLink v-else class="channel" :class="{ on: isActive(c) }" :to="c.to">
            {{ c.label }}
          </RouterLink>
        </template>
      </div>
    </nav>

    <main class="content">
      <slot />
    </main>
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import { logout } from '../api/account'
import { useAuthStore } from '../stores/auth'

const auth = useAuthStore()
const route = useRoute()
const router = useRouter()

const q = ref('')

const initial = computed(() => auth.claims?.username?.slice(0, 1).toUpperCase() ?? '?')

interface Channel {
  label: string
  to: string
  tab?: string
  soon?: string
}

const channels: Channel[] = [
  { label: '最新', to: '/feed', tab: 'latest' },
  { label: '点赞榜', to: '/feed?tab=likes', tab: 'likes' },
  { label: '热门榜', to: '/feed?tab=popularity', tab: 'popularity' },
  { label: '关注', to: '/following', soon: '阶段6接入（依赖 social 模块）' },
]

// 频道高亮：既要在 /feed 上，tab 也要对得上，否则「最新」会在所有 tab 下都亮
const activeTab = computed(() => (route.query.tab as string) || 'latest')
function isActive(c: Channel): boolean {
  return route.path === '/feed' && c.tab === activeTab.value
}

// 没有搜索接口，搜索直接落到标签流（#标签 就是我们的检索维度）
function onSearch() {
  const name = q.value.trim().replace(/^#+/, '')
  if (!name) return
  router.push(`/tag/${encodeURIComponent(name)}`)
}

async function onLogout() {
  try {
    await logout()
  } finally {
    auth.clearTokens()
    router.push('/login')
  }
}
</script>

<style scoped>
.shell {
  min-height: 100dvh;
}

/* ---------- 顶栏 ---------- */
.topbar {
  position: sticky;
  top: 0;
  z-index: 30;
  background: rgba(15, 15, 18, 0.86);
  backdrop-filter: blur(14px);
  border-bottom: 1px solid var(--border);
}
.topbar-inner {
  display: flex;
  align-items: center;
  gap: 24px;
  height: var(--nav-h);
  max-width: var(--page-max);
  margin: 0 auto;
  padding: 0 24px;
}
.brand {
  display: flex;
  align-items: center;
  gap: 10px;
  font-weight: 700;
  font-size: 1.06rem;
  letter-spacing: -0.02em;
  flex: 0 0 auto;
}

.search {
  flex: 1 1 auto;
  max-width: 460px;
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 0 14px;
  height: 38px;
  border: 1px solid var(--border);
  border-radius: 999px;
  background: var(--surface-2);
  color: var(--ink-muted);
  transition: border-color 0.2s var(--ease-out);
}
.search:focus-within {
  border-color: var(--accent);
  box-shadow: 0 0 0 2px var(--accent-soft);
}
.search input {
  flex: 1;
  min-width: 0;
  border: none;
  outline: none;
  background: transparent;
  color: var(--ink);
  font-family: inherit;
  font-size: 0.9rem;
}
.search input::placeholder {
  color: var(--ink-muted);
}

.top-actions {
  display: flex;
  align-items: center;
  gap: 12px;
  flex: 0 0 auto;
  /* 搜索框有 max-width，宽屏下会剩一段空隙。
     不写这条的话空隙留在整行的末尾，这组按钮就浮在中间偏右而不是贴着右边缘。
     margin-left: auto 把剩余空间全吃掉，按钮被顶到最右边。 */
  margin-left: auto;
}
.post-btn {
  padding: 0 18px;
  min-height: 38px;
  display: inline-flex;
  align-items: center;
  font-size: 0.9rem;
}
.btn.sm {
  padding: 0 14px;
  min-height: 38px;
  display: inline-flex;
  align-items: center;
  font-size: 0.88rem;
}
.me {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 0.9rem;
  max-width: 160px;
}
.me-name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.avatar {
  flex: 0 0 auto;
  width: 32px;
  height: 32px;
  border-radius: 50%;
  display: grid;
  place-items: center;
  background: var(--surface-2);
  border: 1px solid var(--accent-line);
  color: var(--accent);
  font-weight: 700;
  font-size: 0.85rem;
}

/* ---------- 频道条 ---------- */
.channels {
  position: sticky;
  top: var(--nav-h);
  z-index: 20;
  background: rgba(15, 15, 18, 0.86);
  backdrop-filter: blur(14px);
  border-bottom: 1px solid var(--border);
}
.channels-inner {
  display: flex;
  align-items: center;
  gap: 26px;
  height: var(--channel-h);
  max-width: var(--page-max);
  margin: 0 auto;
  padding: 0 24px;
}
.channel {
  position: relative;
  display: inline-flex;
  align-items: center;
  height: 100%;
  color: var(--ink-muted);
  font-size: 0.95rem;
  font-weight: 600;
}
.channel:hover {
  color: var(--ink);
}
/* 激活态：文字变赤陶 + 底部一条 2px 指示线（B 站的频道条就是这个形态） */
.channel.on {
  color: var(--accent);
}
.channel.on::after {
  content: '';
  position: absolute;
  left: 50%;
  bottom: -1px;
  width: 22px;
  height: 2px;
  border-radius: 2px;
  background: var(--accent);
  transform: translateX(-50%);
}
.channel.off {
  color: var(--ink-muted);
  opacity: 0.45;
  cursor: not-allowed;
}

.content {
  /* 占满剩余高度，短页面（登录/账号）也不会让页脚跳 */
  min-height: calc(100dvh - var(--nav-h) - var(--channel-h));
  padding: 24px;
}
</style>
