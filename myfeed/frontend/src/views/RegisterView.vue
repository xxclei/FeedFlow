<template>
  <section class="card auth rise">
    <h2>注册</h2>
    <p class="text-muted subtitle">领一张你的工位牌</p>
    <form @submit.prevent="onSubmit">
      <div class="field">
        <label for="username">用户名</label>
        <input id="username" v-model="username" autocomplete="username" required />
      </div>
      <div class="field">
        <label for="password">密码</label>
        <input id="password" v-model="password" type="password" autocomplete="new-password" required />
      </div>
      <button class="btn" type="submit" :disabled="loading">
        {{ loading ? '注册中…' : '注册' }}
      </button>
      <p v-if="error" class="text-error">{{ error }}</p>
      <p v-if="ok" class="text-ok">注册成功，正在跳转登录…</p>
    </form>
    <p class="text-muted tip">已有账号？<RouterLink to="/login">去登录</RouterLink></p>
  </section>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'

import { register } from '../api/account'

const username = ref('')
const password = ref('')
const loading = ref(false)
const error = ref('')
const ok = ref(false)
const router = useRouter()

async function onSubmit() {
  error.value = ''
  ok.value = false
  loading.value = true
  try {
    await register(username.value, password.value)
    ok.value = true
    setTimeout(() => router.push('/login'), 800)
  } catch (e) {
    error.value = e instanceof Error ? e.message : '注册失败'
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
