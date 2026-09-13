<template>
  <div class="chips-input" :class="{ focus }">
    <span v-for="(t, i) in chips" :key="t" class="chip">
      <span class="hash">#</span>{{ t }}
      <button
        type="button"
        class="x"
        :aria-label="`移除标签 ${t}`"
        :disabled="disabled"
        @click="removeAt(i)"
      >
        ×
      </button>
    </span>

    <input
      ref="field"
      :value="pending"
      :placeholder="chips.length ? '' : placeholder"
      :disabled="disabled"
      @input="onInput"
      @keydown.enter.prevent="commitPending"
      @keydown.backspace="onBackspace"
      @paste="onPaste"
      @blur="commitPending"
      @focus="focus = true"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'

import { normalizeTagNames } from '../utils/tags'

/**
 * 真正的标签 chip 编辑器。
 *
 * ---------- 它存在的唯一理由：消灭"打了字但没有标签"这种静默失败 ----------
 *
 * 上一个版本里，批量投稿的"统一标签"是一个裸 <input>，它的内容会**整个**变成
 * 每条视频的描述，后端再从描述里用正则抽 #xxx。于是：
 *
 *   打「日常 vlog」不带 # → 一个标签都没有，而且**没有任何提示**
 *   标签成了描述的一部分 → 吃掉 varchar(255) 的额度，还和真正的描述混在一起
 *
 * 这里把标签变成一个**结构化**的东西：输入即 chip、chip 即标签、没有语法要记。
 * 传出去的是 string[]，后端直接落 video_tags，不再经过"文本 → 正则 → 猜意图"。
 *
 * ---------- 交互细节都有理由 ----------
 *
 *   回车 / 空格 / 逗号 / 顿号 / #  成 chip —— 空格也成 chip 是刻意的：
 *        "日常 vlog" 这种写法的心智就是两个标签，不是"一个带空格的标签"
 *   粘 "#a #b #c"                   一次拆成 3 个
 *   输入框为空时按退格               删掉尾巴上那个 chip（和各家标签输入一致）
 *   失焦                            把没成 chip 的文字也提交（否则用户以为存了）
 *
 * 规范化统一走 utils/tags.ts（剥前导 #、按 rune 截到 100、按小写去重），
 * 和后端 normalizeTagNames 同一套规则 —— 所以这里显示的 chip 就是库里会有的标签。
 */
const props = withDefaults(
  defineProps<{
    modelValue: string[]
    placeholder?: string
    disabled?: boolean
  }>(),
  { placeholder: '输入后回车，例如 日常（不用打 #）', disabled: false },
)

const emit = defineEmits<{ (e: 'update:modelValue', v: string[]): void }>()

const pending = ref('')
const focus = ref(false)
const field = ref<HTMLInputElement | null>(null)

/** 上游存的一律是规范化过的；再走一遍 normalizeTagNames 是为了兜住"上游塞了脏值" */
const chips = computed(() => normalizeTagNames(props.modelValue))

/** 分隔符：空白、逗号、顿号、#。标签名里不允许出现它们（TAG_RE 也不认） */
const SEP_RE = /[\s,，#]+/

function emitNames(names: string[]) {
  // 一定走 normalizeTagNames：去重必须发生在**提交时**，
  // 否则用户连打两次同一个标签会看到两个 chip，而库里只有一个
  const next = normalizeTagNames([...chips.value, ...names])
  if (next.length !== chips.value.length || next.some((n, i) => n !== chips.value[i])) {
    emit('update:modelValue', next)
  }
}

function addMany(text: string) {
  emitNames(text.split(SEP_RE))
}

function commitPending() {
  if (!pending.value.trim()) {
    pending.value = ''
    syncDOM()
    return
  }
  const names = pending.value.split(SEP_RE)
  pending.value = ''
  syncDOM()
  emitNames(names)
}

function onInput(e: Event) {
  const raw = (e.target as HTMLInputElement).value
  // 输入里只要出现了分隔符，就把**最后一个分隔符之前**的部分立刻变成 chip，
  // 剩下的是用户正在打的半截词。贪心 .* 取的就是最后一个分隔符
  const m = raw.match(/^([\s\S]*)[\s,，#]+([^\s,，#]*)$/)
  if (m) {
    emitNames(m[1].split(SEP_RE))
    pending.value = m[2]
  } else {
    pending.value = raw
  }
  syncDOM()
}

/**
 * 把 DOM 里的输入框强行拉回和 pending 一致。
 *
 * `:value` 绑定的更新只在**值变了**的时候才写回 DOM，而这里会出现
 * "pending 本来就是空、用户又打了个空格"这种值没变、但输入框里多了个字符的情况 ——
 * 不显式同步的话，那个空格会一直留在框里，看起来像组件卡住了。
 */
function syncDOM() {
  if (field.value && field.value.value !== pending.value) {
    field.value.value = pending.value
  }
}

function onBackspace() {
  // 只在输入框空的时候吞掉这次退格，否则会让用户想删文字却删掉整个 chip
  if (pending.value !== '' || chips.value.length === 0) return
  const next = chips.value.slice(0, -1)
  emit('update:modelValue', next)
}

function onPaste(e: ClipboardEvent) {
  const text = e.clipboardData?.getData('text') ?? ''
  if (!text) return
  // 带分隔符的粘贴直接切成多个 chip；不带分隔符的走浏览器默认插入，
  // 因为用户可能只是想把一个词粘进来接着编辑
  if (SEP_RE.test(text)) {
    e.preventDefault()
    addMany(text)
    pending.value = ''
    syncDOM()
  }
  // SEP_RE 刻意**不带 g**：带 g 的正则有 lastIndex 状态，同一个正则对象在多次
  // .test() 之间会给出时真时假的结果。这里的 SEP_RE 是模块级常量、被反复调用，
  // 加 g 就是一个只在"第二次粘贴"时才现形的 bug。
}

function removeAt(i: number) {
  const next = chips.value.slice()
  next.splice(i, 1)
  emit('update:modelValue', next)
  field.value?.focus()
}
</script>

<style scoped>
.chips-input {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px;
  min-height: 43px;
  padding: 7px 10px;
  border: 1px solid var(--border);
  border-radius: 12px;
  background: var(--surface-2);
  transition: border-color 0.2s var(--ease-out);
}
.chips-input.focus {
  border-color: var(--accent);
  box-shadow: 0 0 0 2px var(--accent-soft);
}
.chips-input input {
  flex: 1 1 90px;
  min-width: 90px;
  padding: 4px 2px;
  border: none;
  background: transparent;
  color: var(--ink);
  font-family: inherit;
  font-size: 0.9rem;
  outline: none;
}

.chip {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 2px 4px 2px 9px;
  border: 1px solid rgba(232, 103, 74, 0.45);
  border-radius: 999px;
  background: rgba(232, 103, 74, 0.08);
  color: var(--accent);
  font-family: var(--font-mono);
  font-size: 0.78rem;
  line-height: 1.5;
  max-width: 100%;
}
.hash {
  opacity: 0.7;
}
.x {
  width: 17px;
  height: 17px;
  display: grid;
  place-items: center;
  padding: 0;
  border: none;
  border-radius: 50%;
  background: transparent;
  color: var(--accent);
  font-size: 0.95rem;
  line-height: 1;
  cursor: pointer;
  opacity: 0.65;
}
.x:hover:not(:disabled) {
  opacity: 1;
  background: rgba(232, 103, 74, 0.16);
}
.x:disabled {
  cursor: default;
  opacity: 0.3;
}
</style>
