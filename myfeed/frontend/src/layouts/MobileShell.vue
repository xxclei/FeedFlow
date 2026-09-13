<template>
  <div class="shell">
    <header class="topbar">
      <div class="bar">
        <RouterLink class="brand" to="/feed">
          <span class="brand-dot" aria-hidden="true"></span>
          <span class="brand-name">myfeed</span>
        </RouterLink>

        <div class="actions">
          <button class="icon-btn" type="button" aria-label="搜索" @click="toggleSearch">
            <svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true">
              <circle cx="11" cy="11" r="6.5" fill="none" stroke="currentColor" stroke-width="2" />
              <path d="m16 16 4.5 4.5" stroke="currentColor" stroke-width="2" stroke-linecap="round" />
            </svg>
          </button>
          <RouterLink class="icon-btn" :to="auth.isLoggedIn ? '/video' : '/login'" aria-label="投稿">
            <svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true">
              <path
                d="M12 7.5v9M7.5 12h9"
                fill="none"
                stroke="currentColor"
                stroke-width="2.2"
                stroke-linecap="round"
              />
            </svg>
          </RouterLink>
        </div>
      </div>

      <!-- 搜索是「点开才出现」的一行，不占常驻空间（移动端宽度金贵） -->
      <form v-if="searchOpen" class="search-row" @submit.prevent="onSearch">
        <input
          ref="searchInput"
          v-model="q"
          type="search"
          placeholder="搜视频；带 # 则进标签流"
          @keydown.esc="searchOpen = false"
        />
        <button class="go" type="submit">搜索</button>
      </form>
    </header>

    <main class="content">
      <slot />
    </main>

    <TabBar />
  </div>
</template>

<script setup lang="ts">
import { nextTick, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'

import TabBar from '../components/mobile/TabBar.vue'
import { useAuthStore } from '../stores/auth'

const auth = useAuthStore()
const router = useRouter()

const searchOpen = ref(false)
const searchInput = ref<HTMLInputElement | null>(null)
const q = ref('')

async function toggleSearch() {
  searchOpen.value = !searchOpen.value
  if (searchOpen.value) {
    await nextTick()
    searchInput.value?.focus()
  }
}

/**
 * 搜索框的分流：`#` 开头走标签流（指名一个标签），其余走全文检索。
 * 理由和桌面端 DeskopShell 里那段一模一样 —— 一个框认两种意图。
 */
function onSearch() {
  const raw = q.value.trim()
  if (!raw) return
  searchOpen.value = false
  q.value = ''
  if (raw.startsWith('#')) {
    const name = raw.replace(/^#+/, '').trim()
    if (!name) return
    router.push(`/tag/${encodeURIComponent(name)}`)
    return
  }
  router.push({ path: '/search', query: { q: raw } })
}
</script>

<style scoped>
.shell {
  min-height: 100dvh;
}

.topbar {
  position: sticky;
  top: 0;
  z-index: 30;
  background: rgba(15, 15, 18, 0.9);
  backdrop-filter: blur(14px);
  border-bottom: 1px solid var(--border);
}
.bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  height: 52px;
  padding: 0 10px 0 14px;
}
.brand {
  display: flex;
  align-items: center;
  gap: 8px;
  font-weight: 700;
  font-size: 1rem;
  letter-spacing: -0.02em;
}
.actions {
  display: flex;
  align-items: center;
  gap: 2px;
}
.icon-btn {
  display: grid;
  place-items: center;
  width: 44px;
  height: 44px;
  border: none;
  border-radius: 12px;
  background: transparent;
  color: var(--ink);
  cursor: pointer;
  transition: background 0.18s var(--ease-out);
}
.icon-btn:hover {
  background: var(--surface-2);
}

.search-row {
  display: flex;
  gap: 8px;
  padding: 0 12px 10px;
}
.search-row input {
  flex: 1;
  min-width: 0;
  height: 40px;
  padding: 0 14px;
  border: 1px solid var(--border);
  border-radius: 999px;
  background: var(--surface-2);
  color: var(--ink);
  font-family: inherit;
  font-size: 0.92rem;
  outline: none;
}
.search-row input:focus {
  border-color: var(--accent);
  box-shadow: 0 0 0 2px var(--accent-soft);
}
.go {
  flex: 0 0 auto;
  height: 40px;
  padding: 0 16px;
  border: none;
  border-radius: 999px;
  background: var(--accent);
  color: #fff;
  font-family: inherit;
  font-size: 0.88rem;
  font-weight: 600;
  cursor: pointer;
}

.content {
  min-height: calc(100dvh - 52px);
  /* 底部留出 Tab 栏 + iPhone 安全区，最后一张卡片不会被压住 */
  padding: 12px 12px calc(var(--tabbar-h) + env(safe-area-inset-bottom, 0px) + 16px);
}
</style>
