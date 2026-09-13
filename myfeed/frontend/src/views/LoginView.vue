<template>
  <section class="card auth rise">
    <h2>登录</h2>
    <p class="text-muted subtitle">回到你的剪辑室</p>
    <form @submit.prevent="onSubmit">
      <div class="field">
        <label for="username">用户名</label>
        <input id="username" v-model="username" autocomplete="username" required />
      </div>
      <div class="field">
        <label for="password">密码</label>
        <input id="password" v-model="password" type="password" autocomplete="current-password" required />
      </div>
      <button class="btn" type="submit" :disabled="loading">
        {{ loading ? '登录中…' : '登录' }}
      </button>
      <p v-if="error" class="text-error">{{ error }}</p>
    </form>
    <p class="text-muted tip">没有账号？<RouterLink to="/register">去注册</RouterLink></p>
  </section>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'

import { login } from '../api/account'
import { useAuthStore } from '../stores/auth'

const username = ref('')
const password = ref('')
const loading = ref(false)
const error = ref('')
const auth = useAuthStore()
const router = useRouter()

async function onSubmit() {
  error.value = ''
  loading.value = true
  try {
    const data = await login(username.value, password.value)
    auth.setTokens(data.token, data.refresh_token)
    router.push('/feed')
  } catch (e) {
    error.value = e instanceof Error ? e.message : '登录失败'
  } finally {
    loading.value = false
  }
}
</script>

<style scoped>
.auth {
  max-width: 400px;
  margin: 64px auto 0;
  display: flex;
  flex-direction: column;
  gap: 20px;
}
h2 {
  margin: 0;
  letter-spacing: -0.02em;
}
.subtitle {
  margin: -12px 0 0;
  font-size: 0.9rem;
}
form {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.tip {
  margin: 0;
  font-size: 0.9rem;
}
@media (max-width: 767px) {
  .auth {
    margin-top: 12px;
    border-radius: 18px;
    padding: 22px;
  }
}
</style>
