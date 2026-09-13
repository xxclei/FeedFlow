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
      <h3>我的内容</h3>
      <div class="links">
        <RouterLink class="btn btn-ghost" to="/likes">我的点赞</RouterLink>
        <RouterLink class="btn btn-ghost" to="/video">我的投稿</RouterLink>
        <RouterLink class="btn btn-ghost" :to="`/profile/${myID}`">我的主页</RouterLink>
      </div>
      <p class="text-muted">
        「我的点赞」读的是 <code>POST /like/listMyLikedVideos</code> ——
        一个只认 token 里那个人的接口，连请求体都没有。
        而「我的主页」走的是完全公开的 <code>POST /account/getProfile</code>，
        它就是别人点你名字时看到的那一页，没有"自己人版本"。
      </p>
    </div>

    <div class="card rise">
      <h3>接下来这里会长出什么</h3>
      <ul class="roadmap">
        <li class="done"><span class="phase">阶段 2</span>视频上传与发布</li>
        <li class="done"><span class="phase">阶段 3</span>Feed 游标滑动流 —— <RouterLink to="/feed">去看看</RouterLink></li>
        <li class="done"><span class="phase">阶段 4</span>点赞 —— <RouterLink to="/likes">我的点赞</RouterLink></li>
        <li class="done">
          <span class="phase">阶段 5</span>评论与 @提及通知 ——
          <RouterLink to="/feed">随便点开一条视频</RouterLink>
        </li>
        <li class="done">
          <span class="phase">阶段 6</span>关注流与个人主页 ——
          <RouterLink to="/feed?tab=following">关注流</RouterLink> ·
          <RouterLink :to="`/profile/${myID}`">个人主页</RouterLink>
        </li>
        <!-- 这一轮的扩展，不在 00-12 那条路线上（howto 那 13 篇文档没提检索），
             所以刻意不给它编个"阶段 N" —— 编了就等着和阶段 7（Redis 缓存）
             对不上号。标签写「扩展」两个字，反而是最诚实的 -->
        <li class="done">
          <span class="phase">扩展</span>模糊检索（MySQL FULLTEXT + ngram）——
          <RouterLink to="/search?q=日常">试试搜「日常」</RouterLink>
        </li>
        <li><span class="phase">阶段 7</span>三级缓存与限流</li>
        <li><span class="phase">阶段 8</span>热门榜 Redis 快照</li>
        <li><span class="phase">阶段 9+</span>实时通知</li>
      </ul>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'

import { rename } from '../api/account'
import { useAuthStore } from '../stores/auth'

const auth = useAuthStore()
const renameMsg = ref('')
const renameOk = ref(false)

// 这一页有 requiresAuth 守卫，claims 必然存在；`?? 0` 只是为了 TS 收窄
const myID = computed(() => auth.claims?.account_id ?? 0)

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
.links {
  display: flex;
  flex-wrap: wrap;
  gap: 10px;
  margin-bottom: 12px;
}
.links .btn {
  min-height: 38px;
  padding: 0 16px;
  font-size: 0.86rem;
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
  /* 标签文字长度不一（「阶段 2」5 字符、「扩展」2 字符、「阶段 9+」6 字符），
     不给个下限的话右边那列标题会跟着左右错开。这条纯粹为了对齐好看 */
  min-width: 4.4em;
}
.roadmap li.done {
  border-color: rgba(74, 183, 132, 0.35);
}
.roadmap li.done .phase {
  color: var(--ok);
}
.roadmap a {
  color: var(--accent);
}
@media (max-width: 767px) {
  .card {
    border-radius: 18px;
    padding: 20px;
  }
  h2 {
    font-size: 1.05rem;
  }
}
</style>
