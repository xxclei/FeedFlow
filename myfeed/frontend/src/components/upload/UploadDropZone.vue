<template>
  <div
    class="drop"
    :class="{ over: dragging, busy }"
    @dragenter.prevent="dragging = true"
    @dragover.prevent="dragging = true"
    @dragleave.prevent="onDragLeave"
    @drop.prevent="onDrop"
  >
    <input ref="dirInput" class="hidden-input" type="file" webkitdirectory multiple @change="onInput" />
    <input ref="fileInput" class="hidden-input" type="file" accept=".mp4,video/mp4" multiple @change="onInput" />

    <svg class="icon" viewBox="0 0 48 48" width="42" height="42" aria-hidden="true">
      <path
        d="M8 32V12a3 3 0 0 1 3-3h9l4 5h13a3 3 0 0 1 3 3v15a3 3 0 0 1-3 3H11a3 3 0 0 1-3-3Z"
        fill="none"
        stroke="currentColor"
        stroke-width="2.2"
        stroke-linejoin="round"
      />
      <path d="M24 30V20m0 0-4 4m4-4 4 4" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" />
    </svg>

    <p class="title">{{ busy ? '正在读取文件夹…' : '把文件夹拖到这里' }}</p>
    <p class="hint text-muted">
      会自动递归子目录，只收 .mp4（≤{{ MAX_FILE_MB }}MB）；<code>.DS_Store</code> /
      <code>Thumbs.db</code> 之类会被跳过。
    </p>

    <div class="actions">
      <button class="btn" type="button" :disabled="busy" @click="dirInput?.click()">选择文件夹</button>
      <button class="btn btn-ghost" type="button" :disabled="busy" @click="fileInput?.click()">
        选几个文件
      </button>
    </div>

    <p v-if="err" class="text-error err">{{ err }}</p>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'

import { entriesFromDrop, pickedFromInput, walkEntries, MAX_FILE_MB, type PickedFile } from '../../utils/folder'

const emit = defineEmits<{ (e: 'picked', files: PickedFile[]): void }>()

const dirInput = ref<HTMLInputElement | null>(null)
const fileInput = ref<HTMLInputElement | null>(null)
const dragging = ref(false)
const busy = ref(false)
const err = ref('')

function onDragLeave(e: DragEvent) {
  // dragleave 会在子元素之间反复触发，relatedTarget 还在容器里就不算真的离开
  const to = e.relatedTarget as Node | null
  if (to && (e.currentTarget as HTMLElement).contains(to)) return
  dragging.value = false
}

async function onDrop(e: DragEvent) {
  dragging.value = false
  err.value = ''

  // 必须在任何 await 之前同步取出 entry。
  // dataTransfer.items 只在事件派发期间有效，await 一次之后它就空了 —— 拖拽读取最经典的坑。
  const entries = entriesFromDrop(e)

  if (entries.length === 0) {
    err.value =
      '没有读到文件。这个浏览器可能不支持拖拽文件夹，请改用「选择文件夹」或「选几个文件」。'
    return
  }

  busy.value = true
  try {
    const picked = await walkEntries(entries)
    if (picked.length === 0) err.value = '拖进来的目录里没有文件'
    else emit('picked', picked)
  } catch (e) {
    err.value = e instanceof Error ? e.message : '读取文件夹失败'
  } finally {
    busy.value = false
  }
}

function onInput(e: Event) {
  const input = e.target as HTMLInputElement
  const picked = pickedFromInput(input)
  // 清空 value：否则再选同一个文件夹时 change 不会触发
  input.value = ''
  if (picked.length) emit('picked', picked)
}

defineExpose({ openFolder: () => dirInput.value?.click() })
</script>

<style scoped>
.drop {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
  padding: 44px 24px;
  border: 1.5px dashed var(--border-strong);
  border-radius: 20px;
  background: var(--surface);
  text-align: center;
  transition:
    border-color 0.2s var(--ease-out),
    background 0.2s var(--ease-out),
    transform 0.2s var(--ease-spring);
}
.drop.over {
  border-color: var(--accent);
  background: var(--accent-soft);
  transform: scale(1.008);
}
.drop.busy {
  opacity: 0.75;
}
.hidden-input {
  display: none;
}
.icon {
  color: var(--ink-muted);
}
.drop.over .icon {
  color: var(--accent);
}
.title {
  margin: 0;
  font-size: 1.06rem;
  font-weight: 600;
}
.hint {
  margin: 0;
  max-width: 460px;
  font-size: 0.82rem;
  line-height: 1.6;
}
.actions {
  display: flex;
  flex-wrap: wrap;
  justify-content: center;
  gap: 10px;
  margin-top: 6px;
}
.err {
  margin: 4px 0 0;
  font-size: 0.85rem;
}
@media (max-width: 767px) {
  .drop {
    padding: 30px 16px;
  }
  .title {
    font-size: 0.98rem;
  }
}
</style>
