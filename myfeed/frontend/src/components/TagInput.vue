<template>
  <div class="taginput">
    <div class="wrap" :class="{ multi }">
      <!-- 背景渲染层：同样的文字，#标签 被套上高亮框 -->
      <div ref="back" class="back" aria-hidden="true">
        <span v-html="highlighted"></span><br v-if="multi" /><span>&nbsp;</span>
      </div>
      <!-- 前景输入层：文字透明只留光标，视觉上就是"输入的文字被高亮" -->
      <textarea
        v-if="multi"
        ref="field"
        :value="modelValue"
        :placeholder="placeholder"
        v-bind="$attrs"
        @input="onInput"
        @scroll="syncScroll"
      ></textarea>
      <input
        v-else
        ref="field"
        :value="modelValue"
        :placeholder="placeholder"
        v-bind="$attrs"
        @input="onInput"
      />
    </div>
    <div v-if="tags.length" class="chips">
      <span v-for="t in tags" :key="t" class="chip">#{{ t }}</span>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { TAG_RE, extractTags } from '../utils/tags'

// 本组件原本自带一份 TAG_RE 的副本，本轮收敛到 utils/tags.ts 里唯一的那一份。
// 注意：**这个正则带 g 标志，是"有状态"的**（lastIndex 会被 retain），
// 所以不能拿同一个正则对象交替跑 matchAll 和 test ——
// utils/tags.ts 里的 extractTags 每次调用都从 matchAll 重新开始，是安全的。
// 这里的 highlighted 用的 replace 也是从头扫的，同样安全。

const props = defineProps<{
  modelValue: string
  multi?: boolean
  placeholder?: string
}>()

const emit = defineEmits<{ (e: 'update:modelValue', v: string): void }>()

const back = ref<HTMLElement>()
const field = ref<HTMLElement>()

function escapeHtml(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}

const highlighted = computed(() =>
  escapeHtml(props.modelValue).replace(
    TAG_RE,
    '<mark>#<span>$1</span></mark>',
  ),
)

// 下面这排 chip 是"用户写下的 #标签会被落库成什么"的预览，
// 所以直接用 extractTags（和后端 ExtractTags 同一套规则），不再自己遍历一遍
const tags = computed(() => extractTags(props.modelValue))

function onInput(e: Event) {
  emit('update:modelValue', (e.target as HTMLInputElement).value)
}

// 多行时让背景层跟着输入层滚动
function syncScroll() {
  if (back.value && field.value) {
    back.value.scrollTop = field.value.scrollTop
  }
}
</script>

<style scoped>
.wrap {
  position: relative;
  width: 100%;
  min-width: 0;
}
.back,
.wrap textarea,
.wrap input {
  /* 两层完全对齐：同字体、同内边距、同换行规则 */
  padding: 10px 12px;
  border: 1px solid var(--border);
  border-radius: 12px;
  background: var(--surface-2);
  font-family: inherit;
  font-size: 0.95rem;
  line-height: 1.6;
  letter-spacing: normal;
  width: 100%;
  min-width: 0;
}
.back {
  position: absolute;
  inset: 0;
  pointer-events: none;
  color: var(--ink);
  overflow: hidden;
  white-space: pre; /* 单行不换行 */
}
.wrap.multi .back {
  white-space: pre-wrap;
  word-break: break-word;
  overflow: hidden;
}
.wrap textarea,
.wrap input {
  color: transparent; /* 文字隐形，视觉来自背景层 */
  caret-color: var(--ink); /* 光标可见 */
  outline: none;
  transition: border-color 0.2s var(--ease-out);
}
.wrap textarea {
  resize: vertical;
}
.wrap textarea:focus,
.wrap input:focus {
  border-color: var(--accent);
  box-shadow: 0 0 0 2px rgba(232, 103, 74, 0.35);
}
.back :deep(mark) {
  background: rgba(232, 103, 74, 0.16);
  color: var(--accent);
  border: 1px solid rgba(232, 103, 74, 0.55);
  border-radius: 5px;
  padding: 0 3px;
}
.chips {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-top: 6px;
}
.chip {
  font-family: var(--font-mono);
  font-size: 0.78rem;
  color: var(--accent);
  border: 1px solid rgba(232, 103, 74, 0.45);
  border-radius: 999px;
  padding: 1px 10px;
  background: rgba(232, 103, 74, 0.08);
}
</style>
