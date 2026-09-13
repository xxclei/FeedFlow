<template>
  <section class="debug">
    <button class="head" type="button" :aria-expanded="open" @click="open = !open">
      <svg class="chev" :class="{ open }" viewBox="0 0 24 24" width="14" height="14" aria-hidden="true">
        <path d="m9 6 6 6-6 6" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" />
      </svg>
      <span class="head-title">调试 · 游标</span>
      <span class="head-hint mono">{{ hint }}</span>
      <span class="pill" :class="dupCount ? 'bad' : 'ok'">
        {{ dupCount ? `重复 ${dupCount}` : '无重复' }}
      </span>
    </button>

    <div v-if="open" class="body">
      <p class="note">
        服务端每页把「本页最后一条的排序键」还给你，下一页原样带回去。客户端从不解释它的含义。
      </p>

      <dl class="cursor">
        <template v-for="row in rows" :key="row.k">
          <dt class="mono">{{ row.k }}</dt>
          <dd class="mono">{{ row.v }}</dd>
        </template>
      </dl>

      <div class="stats">
        <span>已加载 <strong>{{ loaded }}</strong> 条</span>
        <span :class="dupCount ? 'text-error' : 'text-ok'">
          {{ dupCount ? `跨页重复 ${dupCount} 条` : '跨页无重复' }}
        </span>
      </div>

      <p v-if="dupCount" class="text-error note">
        抓到了跨页重复！说明游标没起作用，检查 ORDER BY 与 WHERE 的键是否镜像。
      </p>
    </div>
  </section>
</template>

<script setup lang="ts">
import { ref } from 'vue'

defineProps<{
  rows: { k: string; v: string }[]
  loaded: number
  dupCount: number
  hint: string
}>()

const open = ref(false) // 默认收起：B 站风的页面不该常驻一个教学面板
</script>

<style scoped>
.debug {
  margin-top: 26px;
  border: 1px solid var(--border);
  border-radius: 14px;
  background: var(--surface);
  overflow: hidden;
}

.head {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  min-height: 46px;
  padding: 0 16px;
  border: none;
  background: transparent;
  color: var(--ink-muted);
  font-family: inherit;
  font-size: 0.85rem;
  cursor: pointer;
  text-align: left;
}
.head:hover {
  background: var(--surface-2);
}
.chev {
  flex: 0 0 auto;
  transition: transform 0.22s var(--ease-out);
}
.chev.open {
  transform: rotate(90deg);
}
.head-title {
  font-weight: 600;
  color: var(--ink);
}
.head-hint {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 0.76rem;
}

.pill {
  flex: 0 0 auto;
  padding: 2px 10px;
  border-radius: 999px;
  font-size: 0.74rem;
  font-weight: 600;
}
.pill.ok {
  color: var(--ok);
  border: 1px solid rgba(74, 183, 132, 0.4);
  background: rgba(74, 183, 132, 0.08);
}
.pill.bad {
  color: var(--danger);
  border: 1px solid rgba(229, 83, 75, 0.4);
  background: rgba(229, 83, 75, 0.1);
}

.body {
  padding: 4px 16px 16px;
  border-top: 1px solid var(--border);
}
.note {
  margin: 10px 0;
  font-size: 0.8rem;
  line-height: 1.55;
  color: var(--ink-muted);
}

.cursor {
  display: grid;
  gap: 6px;
  margin: 12px 0;
}
.cursor dt {
  margin: 0;
  font-size: 0.7rem;
  color: var(--ink-muted);
}
.cursor dd {
  margin: 0;
  padding: 5px 9px;
  border-radius: 8px;
  background: var(--surface-2);
  border: 1px solid var(--border);
  color: var(--accent);
  font-size: 0.74rem;
  overflow-wrap: anywhere;
}

.stats {
  display: flex;
  justify-content: space-between;
  gap: 12px;
  font-size: 0.8rem;
  color: var(--ink-muted);
  padding-top: 10px;
  border-top: 1px solid var(--border);
}
.stats strong {
  color: var(--ink);
}
</style>
