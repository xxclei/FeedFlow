<template>
  <div class="app">
    <nav class="nav rise">
      <span class="brand">
        <span class="brand-dot" aria-hidden="true"></span>
        <span class="brand-name">myfeed</span>
      </span>
      <div class="nav-links">
        <RouterLink to="/home">首页</RouterLink>
        <template v-if="auth.isLoggedIn">
          <RouterLink to="/video">视频</RouterLink>
          <span class="who">
            你好，<strong>{{ auth.claims?.username }}</strong>
            <span class="mono">#{{ auth.claims?.account_id }}</span>
          </span>
          <button class="btn btn-ghost" @click="onLogout">登出</button>
        </template>
        <template v-else>
          <RouterLink to="/login">登录</RouterLink>
          <RouterLink to="/register">注册</RouterLink>
        </template>
      </div>
    </nav>
    <main class="main">
      <RouterView />
    </main>
  </div>
</template>

<script setup lang="ts">
import { RouterLink, RouterView, useRouter } from 'vue-router'

import { logout } from './api/account'
import { useAuthStore } from './stores/auth'

const auth = useAuthStore()
const router = useRouter()

async function onLogout() {
  try {
    await logout()
  } finally {
    auth.clearTokens() // 无论后端结果如何，本地凭证都清掉
    router.push('/login')
  }
}
</script>

<style scoped>
.nav {
  position: sticky;
  top: 0;
  z-index: 10;
  display: flex;
  justify-content: space-between;
  align-items: center;
  max-width: 1080px;
  margin: 0 auto;
  padding: 14px 24px;
  background: rgba(15, 15, 18, 0.82);
  backdrop-filter: blur(12px);
  border-bottom: 1px solid var(--border);
}
.brand {
  display: flex;
  align-items: center;
  gap: 10px;
  font-weight: 700;
  letter-spacing: -0.02em;
}
.nav-links {
  display: flex;
  align-items: center;
  gap: 20px;
  font-size: 0.95rem;
}
.who {
  color: var(--ink-muted);
}
.who strong {
  color: var(--ink);
}
.main {
  max-width: 1080px;
  margin: 0 auto;
  padding: 32px 24px;
  min-height: calc(100dvh - 61px);
}
</style>
