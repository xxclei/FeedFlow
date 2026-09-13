<template>
  <nav class="tabbar" aria-label="主导航">
    <RouterLink
      v-for="t in tabs"
      :key="t.key"
      class="tab"
      :class="{ on: t.match(route) }"
      :to="t.to"
    >
      <span class="icon" aria-hidden="true">
        <svg viewBox="0 0 24 24" width="21" height="21">
          <path
            :d="t.path"
            fill="none"
            stroke="currentColor"
            stroke-width="1.9"
            stroke-linecap="round"
            stroke-linejoin="round"
          />
        </svg>
      </span>
      <span class="label">{{ t.label }}</span>
    </RouterLink>
  </nav>
</template>

<script setup lang="ts">
import { RouterLink, useRoute, type RouteLocationNormalizedLoaded } from 'vue-router'

import { useAuthStore } from '../../stores/auth'

const auth = useAuthStore()
const route = useRoute() // 高亮当前 Tab：靠路径 + query 判断，不靠"最后点过谁"

interface Tab {
  key: string
  label: string
  to: string
  path: string
  match: (r: RouteLocationNormalizedLoaded) => boolean
}

// 移动端的四个 Tab。B 站是「首页/动态/会员购/我的」，我们映射成
// 「首页/热门/投稿/我的」——把"投稿"提到一级，因为那是这个项目的主线动作。
const tabs: Tab[] = [
  {
    key: 'home',
    label: '首页',
    to: '/feed',
    path: 'M3.5 10.5 12 3.8l8.5 6.7V20a.9.9 0 0 1-.9.9h-4.4v-6.2H9.8v6.2H4.4a.9.9 0 0 1-.9-.9z',
    match: (r) => r.path === '/feed' && r.query.tab !== 'popularity',
  },
  {
    key: 'popular',
    label: '热门',
    to: '/feed?tab=popularity',
    path: 'M4 16.5 9.6 11l3.6 3.6L20 7.5M20 7.5h-4.6M20 7.5v4.6',
    match: (r) => r.path === '/feed' && r.query.tab === 'popularity',
  },
  {
    key: 'post',
    label: '投稿',
    to: '/video',
    path: 'M12 8.2v7.6M8.2 12h7.6M4.5 5.5h15v13h-15z',
    match: (r) => r.path === '/video',
  },
  {
    key: 'me',
    label: '我的',
    // 未登录直接去登录页，省一次跳转守卫
    to: auth.isLoggedIn ? '/home' : '/login',
    path: 'M12 12.2a3.9 3.9 0 1 0 0-7.8 3.9 3.9 0 0 0 0 7.8M4.8 20.4c.9-3.6 3.8-5.6 7.2-5.6s6.3 2 7.2 5.6',
    // /likes 也算「我的」：它是从 /home 推进去的一层，属于这个 tab 的分支，
    // 所以这里保持高亮（不像 /video/:id —— 那是从发现页推出去的，四个 tab 都不亮）
    match: (r) =>
      r.path === '/home' || r.path === '/likes' || r.path === '/login' || r.path === '/register',
  },
]
</script>

<style scoped>
.tabbar {
  position: fixed;
  left: 0;
  right: 0;
  bottom: 0;
  z-index: 40;
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  align-items: center;
  /* 全局 box-sizing 是 border-box，所以安全区的内边距要**加**进高度里，
     直接写 height: var(--tabbar-h) 会把图标挤扁 */
  height: calc(var(--tabbar-h) + env(safe-area-inset-bottom, 0px));
  /* 给 iPhone 的 home indicator 留位置 */
  padding-bottom: env(safe-area-inset-bottom, 0px);
  background: rgba(23, 23, 28, 0.94);
  backdrop-filter: blur(16px);
  border-top: 1px solid var(--border);
}

.tab {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 3px;
  /* 触控目标：整格 58px 高，远超 DESIGN.md 的 44px 下限 */
  min-height: 46px;
  color: var(--ink-muted);
  transition: color 0.18s var(--ease-out);
}
.tab:hover {
  color: var(--ink);
}
.tab.on {
  color: var(--accent);
}
.icon {
  display: block;
  line-height: 0;
}
.label {
  font-size: 0.68rem;
  font-weight: 600;
  letter-spacing: 0.01em;
}
</style>
