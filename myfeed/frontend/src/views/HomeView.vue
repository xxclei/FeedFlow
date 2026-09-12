<template>
  <section class="home">
    <div class="card rise">
      <h2>
        已登录：<strong>{{ auth.claims?.username }}</strong>
        <span class="mono">#{{ auth.claims?.account_id }}</span>
      </h2>
      <p class="text-muted">
        这段身份信息直接来自 JWT 载荷——前端只负责展示，验签是后端中间件的事。
      </p>
      <button class="btn btn-ghost" @click="onRename">改名试试（会签发新 token）</button>
      <p v-if="renameMsg" :class="renameOk ? 'text-ok' : 'text-error'">{{ renameMsg }}</p>
    </div>

    <div class="card rise">
      <h3>接下来这里会长出什么</h3>
      <ul class="roadmap">
        <li><span class="phase">阶段 2</span>视频上传与发布</li>
        <li><span class="phase">阶段 3</span>Feed 滑动流</li>
        <li><span class="phase">阶段 4/5</span>点赞与评论</li>
        <li><span class="phase">阶段 8</span>热榜</li>
        <li><span class="phase">阶段 9+</span>实时通知</li>
      </ul>
    </div>
  </section>
</template>

<script setup lang="ts">
import { ref } from 'vue'

import { rename } from '../api/account'
import { useAuthStore } from '../stores/auth'

const auth = useAuthStore()
const renameMsg = ref('')
const renameOk = ref(false)

async function onRename() {
  const name = window.prompt('输入新用户名（改名后旧 token 立即失效，你会拿到新 token）：')
  if (!name) return
  try {
    const data = await rename(name)
    auth.setToken(data.token) // 换证：store 里的 token 被覆盖
    renameOk.value = true
    renameMsg.value = `改名成功，新 token 已生效（你现在是 ${name}）`
  } catch (e) {
    renameOk.value = false
    renameMsg.value = e instanceof Error ? e.message : '改名失败'
  }
}
</script>

<style scoped>
.home {
  display: flex;
  flex-direction: column;
  gap: 24px;
  max-width: 640px;
}
h2,
h3 {
  margin: 0 0 8px;
  letter-spacing: -0.02em;
}
.roadmap {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.roadmap li {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 14px;
  border: 1px solid var(--border);
  border-radius: 12px;
  background: var(--surface-2);
}
.phase {
  font-family: var(--font-mono);
  font-size: 0.8rem;
  color: var(--accent);
}
</style>
