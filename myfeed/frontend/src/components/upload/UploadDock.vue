<template>
  <!--
    全局上传浮窗。挂在 App.vue 上、RouterView 之外 —— 这样从投稿页切到发现页，
    视图组件被卸载了，队列还活着，进度条也还看得见。
  -->
  <aside class="dock" :class="{ collapsed }">
    <button class="head" type="button" :aria-expanded="!collapsed" @click="collapsed = !collapsed">
      <span class="dot" :class="tone" aria-hidden="true"></span>
      <span class="title">
        {{ q.running ? '正在上传' : q.finished ? '上传结束' : '上传队列' }}
      </span>
      <span class="pct mono">{{ Math.round(q.overallPct * 100) }}%</span>
      <svg class="chev" :class="{ up: !collapsed }" viewBox="0 0 24 24" width="14" height="14" aria-hidden="true">
        <path d="m6 15 6-6 6 6" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" />
      </svg>
    </button>

    <div class="bar" role="progressbar" :aria-valuenow="Math.round(q.overallPct * 100)">
      <span class="fill" :style="{ transform: `scaleX(${q.overallPct})` }" />
    </div>

    <div v-if="!collapsed" class="body">
      <ul class="tally">
        <li><strong>{{ q.counts.done }}</strong> 已发布</li>
        <li v-if="q.counts.active"><strong>{{ q.counts.active }}</strong> 处理中</li>
        <li v-if="q.counts.queued"><strong>{{ q.counts.queued }}</strong> 排队</li>
        <li v-if="q.counts.duplicate"><strong>{{ q.counts.duplicate }}</strong> 内容重复</li>
        <li v-if="q.counts.failed" class="bad"><strong>{{ q.counts.failed }}</strong> 失败</li>
        <li v-if="q.counts.canceled"><strong>{{ q.counts.canceled }}</strong> 已取消</li>
      </ul>

      <p class="note text-muted">
        共 {{ humanSize(q.doneBytes) }} / {{ humanSize(q.totalBytes) }} ·
        队列活在 Pinia 里，切页面不会中断。
      </p>

      <div class="ops">
        <button v-if="q.running && !q.paused" class="mini" type="button" @click="q.pause()">暂停</button>
        <button v-else-if="q.paused" class="mini accent" type="button" @click="q.resume()">继续</button>
        <RouterLink class="mini accent" to="/video">看详情</RouterLink>
        <button v-if="q.finished" class="mini" type="button" @click="q.clearFinished()">清除已完成</button>
        <button v-else class="mini" type="button" @click="q.cancelAll()">全部取消</button>
      </div>

      <p v-if="q.paused" class="note text-muted">
        暂停只是停发新分片。已经传到服务端的分片留在会话里，点「继续」会从断点接着传。
      </p>
    </div>
  </aside>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { RouterLink } from 'vue-router'

import { useUploadQueueStore } from '../../stores/uploadQueue'

const q = useUploadQueueStore()
const collapsed = ref(false)

const tone = computed(() => {
  if (q.counts.failed) return 'bad'
  if (q.finished) return 'ok'
  return 'busy'
})

function humanSize(n: number): string {
  if (n >= 1024 * 1024 * 1024) return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`
  if (n >= 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`
  return `${Math.max(1, Math.round(n / 1024))} KB`
}
</script>

<style scoped>
.dock {
  position: fixed;
  right: 20px;
  /* 移动端要给底部 Tab 栏让位 */
  bottom: calc(20px + var(--tabbar-h) + env(safe-area-inset-bottom, 0px));
  z-index: 45;
  width: 320px;
  max-width: calc(100vw - 24px);
  border: 1px solid var(--border-strong);
  border-radius: 16px;
  background: rgba(23, 23, 28, 0.96);
  backdrop-filter: blur(16px);
  box-shadow: var(--shadow);
  overflow: hidden;
  animation: rise-in 0.32s var(--ease-out) both;
}

.head {
  display: flex;
  align-items: center;
  gap: 9px;
  width: 100%;
  min-height: 44px;
  padding: 0 14px;
  border: none;
  background: transparent;
  color: var(--ink);
  font-family: inherit;
  font-size: 0.88rem;
  font-weight: 600;
  cursor: pointer;
  text-align: left;
}
.head:hover {
  background: var(--surface-2);
}
.dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  flex: 0 0 auto;
}
.dot.busy {
  background: var(--accent);
  animation: breathe 1.6s var(--ease-out) infinite;
}
.dot.ok {
  background: var(--ok);
}
.dot.bad {
  background: var(--danger);
}
.title {
  flex: 1;
  min-width: 0;
}
.pct {
  color: var(--ink-muted);
  font-size: 0.78rem;
}
.chev {
  color: var(--ink-muted);
  transition: transform 0.22s var(--ease-out);
}
.chev.up {
  transform: rotate(180deg);
}

.bar {
  position: relative;
  height: 3px;
  background: var(--surface-3);
}
.fill {
  position: absolute;
  inset: 0;
  transform-origin: left;
  background: var(--accent);
  transition: transform 0.25s var(--ease-out);
}

.body {
  padding: 12px 14px 14px;
  border-top: 1px solid var(--border);
}
.tally {
  display: flex;
  flex-wrap: wrap;
  gap: 4px 14px;
  margin: 0 0 8px;
  padding: 0;
  list-style: none;
  font-size: 0.78rem;
  color: var(--ink-muted);
}
.tally strong {
  color: var(--ink);
}
.tally .bad strong {
  color: var(--danger);
}
.note {
  margin: 0 0 10px;
  font-size: 0.72rem;
  line-height: 1.55;
}
.ops {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}
.mini {
  display: inline-flex;
  align-items: center;
  min-height: 34px;
  padding: 0 12px;
  border: 1px solid var(--border);
  border-radius: 9px;
  background: transparent;
  color: var(--ink);
  font-family: inherit;
  font-size: 0.8rem;
  cursor: pointer;
  transition: background 0.18s var(--ease-out);
}
.mini:hover {
  background: var(--surface-2);
}
.mini.accent {
  border-color: var(--accent-line);
  color: var(--accent);
}

@media (max-width: 767px) {
  .dock {
    right: 12px;
    left: 12px;
    width: auto;
  }
}
</style>
