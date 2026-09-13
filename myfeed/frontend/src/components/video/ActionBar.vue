<template>
  <div class="bar">
    <!--
      点赞/收藏是**禁用态 + 可见的角标文案**，不是 disabled 属性。
      两个理由：
        1. 移动端没有 hover，只写 title 的话手机上什么都看不到，只剩两个灰按钮；
        2. disabled 会把元素从 tab 顺序里摘掉，键盘/读屏用户连 title 都够不着。
      用 aria-disabled + 空操作，元素仍可聚焦、仍能读到 aria-label 里的说明。
    -->
    <button
      class="act"
      type="button"
      aria-disabled="true"
      :aria-label="`点赞 ${likesCount}（阶段4接入）`"
      title="阶段4接入（依赖 likes 模块，后端还没注册路由）"
      @click="noop"
    >
      <svg viewBox="0 0 24 24" width="17" height="17" aria-hidden="true">
        <path
          d="M7 10.5v9H4.6a1 1 0 0 1-1-1v-7a1 1 0 0 1 1-1zm0 0 4.3-7a1.6 1.6 0 0 1 2.9 1.3l-.9 3.7h5.2a1.7 1.7 0 0 1 1.7 2.1l-1.5 7a1.7 1.7 0 0 1-1.7 1.4H7"
          fill="none"
          stroke="currentColor"
          stroke-width="1.7"
          stroke-linejoin="round"
        />
      </svg>
      <span>{{ likesCount }}</span>
      <span class="soon">阶段4</span>
    </button>

    <button
      class="act"
      type="button"
      aria-disabled="true"
      aria-label="收藏（后端没有对应接口）"
      title="后端没有收藏接口 —— 12 个阶段里都没有它"
      @click="noop"
    >
      <svg viewBox="0 0 24 24" width="17" height="17" aria-hidden="true">
        <path
          d="m12 4.4 2.4 5 5.4.7-4 3.8 1 5.4-4.8-2.6-4.8 2.6 1-5.4-4-3.8 5.4-.7z"
          fill="none"
          stroke="currentColor"
          stroke-width="1.7"
          stroke-linejoin="round"
        />
      </svg>
      <span>收藏</span>
      <span class="soon">无接口</span>
    </button>

    <button class="act" type="button" @click="share">
      <svg viewBox="0 0 24 24" width="17" height="17" aria-hidden="true">
        <path
          d="M13 5.5 20 12l-7 6.5v-4C9 14.5 6 16 4 19c0-5 3-8.5 9-9.3z"
          fill="none"
          stroke="currentColor"
          stroke-width="1.7"
          stroke-linejoin="round"
        />
      </svg>
      <span>分享</span>
    </button>

    <button
      v-if="isOwner"
      class="act danger"
      type="button"
      :aria-disabled="deleting"
      @click="$emit('delete')"
    >
      <svg viewBox="0 0 24 24" width="17" height="17" aria-hidden="true">
        <path
          d="M5 7h14M10 7V5.4a1 1 0 0 1 1-1h2a1 1 0 0 1 1 1V7m-7.6 0 .8 11.1a1 1 0 0 0 1 .9h5.6a1 1 0 0 0 1-.9L17.6 7"
          fill="none"
          stroke="currentColor"
          stroke-width="1.7"
          stroke-linecap="round"
          stroke-linejoin="round"
        />
      </svg>
      <span>{{ deleting ? '删除中…' : '删除' }}</span>
    </button>

    <!-- 分享回执。剪贴板失败时退化成"把地址摆出来让你手动复制" -->
    <span v-if="copied" class="note text-ok" role="status">已复制链接</span>
    <span v-if="manual" class="manual">
      <input ref="manualEl" :value="manual" readonly aria-label="视频链接" @focus="selectAll" />
      <span class="note text-muted">剪贴板不可用（非 HTTPS），请手动复制</span>
    </span>
  </div>
</template>

<script setup lang="ts">
import { nextTick, ref } from 'vue'

defineProps<{
  likesCount: number
  isOwner: boolean
  deleting?: boolean
}>()

defineEmits<{ (e: 'delete'): void }>()

const copied = ref(false)
const manual = ref('')
const manualEl = ref<HTMLInputElement | null>(null)

function noop() {
  /* 禁用态：接口还不存在，什么都不做。文案在角标里 */
}

/**
 * 分享 = 复制当前地址。
 *
 * `navigator.clipboard` 在**非安全上下文**里是 undefined：localhost / 127.0.0.1 算安全，
 * 但手机上通过 `http://192.168.x.x:5173` 打开就不算 —— 那时 writeText 直接 TypeError。
 * 另外文档没有焦点时 writeText 会 reject。
 * 两种都退化成"把 URL 显示出来让用户自己复制"，而不是静默失败。
 */
async function share() {
  copied.value = false
  manual.value = ''
  const clip = navigator.clipboard
  try {
    if (!clip) throw new Error('剪贴板不可用')
    await clip.writeText(window.location.href)
    copied.value = true
    window.setTimeout(() => (copied.value = false), 2000)
  } catch {
    manual.value = window.location.href
    await nextTick()
    manualEl.value?.focus()
  }
}

function selectAll(e: FocusEvent) {
  ;(e.target as HTMLInputElement).select()
}
</script>

<style scoped>
.bar {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.act {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  min-height: 40px;
  padding: 0 13px;
  border: 1px solid var(--border);
  border-radius: 11px;
  background: var(--surface-2);
  color: var(--ink);
  font-family: inherit;
  font-size: 0.88rem;
  cursor: pointer;
  transition:
    background 0.18s var(--ease-out),
    border-color 0.18s var(--ease-out);
}
.act:hover {
  background: var(--surface-3);
  border-color: var(--border-strong);
}
.act[aria-disabled='true'] {
  color: var(--ink-muted);
  cursor: not-allowed;
}
.act[aria-disabled='true']:hover {
  background: var(--surface-2);
  border-color: var(--border);
}
.act.danger {
  color: var(--danger);
  border-color: rgba(229, 83, 75, 0.4);
}
.act.danger:hover {
  background: rgba(229, 83, 75, 0.1);
}

/* 阶段角标：和桌面频道条那个「关注 · 阶段6」是同一个说法的形态 */
.soon {
  padding: 1px 6px;
  border-radius: 999px;
  border: 1px solid var(--border);
  background: var(--surface);
  color: var(--ink-muted);
  font-size: 0.68rem;
  font-weight: 600;
}

.note {
  font-size: 0.8rem;
}

.manual {
  display: flex;
  align-items: center;
  gap: 8px;
  flex: 1 1 100%;
  min-width: 0;
}
.manual input {
  flex: 1 1 auto;
  min-width: 0;
  padding: 7px 10px;
  border: 1px solid var(--border);
  border-radius: 9px;
  background: var(--surface-2);
  color: var(--ink);
  font-family: var(--font-mono);
  font-size: 0.74rem;
  outline: none;
}
.manual input:focus {
  border-color: var(--accent);
}
</style>
