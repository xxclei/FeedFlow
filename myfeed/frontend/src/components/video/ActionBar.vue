<template>
  <div class="bar">
    <!--
      点赞从阶段4 起是真的可用（后端 /like/like · /like/unlike 已注册）。

      它是一个**开关按钮**，所以用 aria-pressed 而不是 aria-disabled ——
      pressed 语义就是"按下去的状态"，读屏会念成"切换按钮，已按下"，
      而 aria-disabled 的语义是"此按钮不可用"，两回事。
      busy 时才用 aria-disabled（请求在飞、点了也没用），并且照样保留在
      tab 顺序里：disabled 属性会把元素摘出去，键盘和读屏用户就够不着了。

      收藏仍然是禁用态 + 角标文案，理由和之前一样：
        1. 移动端没有 hover，只写 title 的话手机上什么都看不到；
        2. disabled 会摘掉元素，读屏用户连说明都读不到。
    -->
    <button
      class="act like"
      :class="{ on: isLiked }"
      type="button"
      :aria-pressed="isLiked"
      :aria-disabled="liking"
      :aria-label="isLiked ? `取消点赞（当前 ${likesCount}）` : `点赞（当前 ${likesCount}）`"
      :title="isLiked ? '取消点赞' : '点赞'"
      @click="onLike"
    >
      <svg viewBox="0 0 24 24" width="17" height="17" aria-hidden="true">
        <!-- 已赞时把图标填实（B 站的点赞图标也是实心/空心两态）：颜色之外再给一层
             不依赖颜色的区分，色觉障碍用户也看得出来按没按下去 -->
        <path
          d="M7 10.5v9H4.6a1 1 0 0 1-1-1v-7a1 1 0 0 1 1-1zm0 0 4.3-7a1.6 1.6 0 0 1 2.9 1.3l-.9 3.7h5.2a1.7 1.7 0 0 1 1.7 2.1l-1.5 7a1.7 1.7 0 0 1-1.7 1.4H7"
          :fill="isLiked ? 'currentColor' : 'none'"
          stroke="currentColor"
          stroke-width="1.7"
          stroke-linejoin="round"
        />
      </svg>
      <span>{{ likesCount }}</span>
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

const props = defineProps<{
  likesCount: number
  /** 我赞过没有。由父组件提供 —— 它可能来自 /like/isLiked，也可能是乐观更新的结果 */
  isLiked: boolean
  /** 点赞请求在飞。用来防连点：连点两下会发出 like + unlike，白白多一轮往返 */
  liking?: boolean
  isOwner: boolean
  deleting?: boolean
}>()

const emit = defineEmits<{
  (e: 'toggle-like'): void
  (e: 'delete'): void
}>()

const copied = ref(false)
const manual = ref('')
const manualEl = ref<HTMLInputElement | null>(null)

/**
 * 组件**不改状态、不发请求**，只把"用户点了"这件事报上去。
 *
 * 点赞的状态是"这个视频 + 我"的联合状态，父组件（详情页）才是它的所有者；
 * 组件自己存一份的话，路由切到另一个视频时那份状态会留下来变成幽灵。
 * 连点保护在这里做（纯 UI 关注点），对账和回滚在父组件做（业务关注点）。
 */
function onLike() {
  if (props.liking) return
  emit('toggle-like')
}

function noop() {
  /* 收藏仍然是禁用态：后端没有对应接口 */
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
/* 已赞：颜色 + 实心图标两重信号。注意样式挂在 .on 而不是 :aria-pressed 上 ——
   aria-pressed 是给辅助技术读的语义，样式用类名，两者不混。 */
.act.like.on {
  color: var(--accent);
  border-color: var(--accent-line);
  background: var(--accent-soft);
}
.act.like.on:hover {
  border-color: var(--accent);
}

.act.danger {
  color: var(--danger);
  border-color: rgba(229, 83, 75, 0.4);
}
.act.danger:hover {
  background: rgba(229, 83, 75, 0.1);
}

/* 阶段角标：用来标"这个功能后端还没有"。它现在只剩「收藏」一个使用者 ——
   点赞（阶段4）、关注（阶段6）、评论（阶段5）都已经变成真按钮了。
   阶段7+ 再冒出未接入的功能时，继续复用这套形态 */
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
