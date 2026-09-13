<template>
  <li class="row" :class="item.state">
    <div class="thumb">
      <img v-if="item.coverPreview" :src="item.coverPreview" alt="" />
      <span v-else class="ph mono">{{ Math.round(item.percent * 100) }}%</span>
      <span v-if="item.coverSource" class="src" :title="sourceLabel(item.coverSource)">{{
        sourceBadge(item.coverSource)
      }}</span>
    </div>

    <div class="body">
      <p class="name" :title="item.relPath">{{ item.title }}</p>
      <p class="sub mono text-muted">
        {{ humanSize(item.size) }}
        <template v-if="item.videoId"> · #{{ item.videoId }}</template>
      </p>
      <p class="phase" :class="{ err: item.state === 'failed' }">
        {{ item.error || item.phaseLabel }}
      </p>
      <div class="bar" role="progressbar" :aria-valuenow="Math.round(item.percent * 100)">
        <span
          v-if="determinate"
          class="fill"
          :class="{ done: item.state === 'done' }"
          :style="{ transform: `scaleX(${item.percent})` }"
        />
        <!-- 无法估算进度的阶段（服务端收尾/发布会话）走不定态微光，而不是一个骗人的假百分比 -->
        <span v-else class="indeterminate" />
      </div>
    </div>

    <div class="ops">
      <button
        v-if="canCancel"
        class="mini"
        type="button"
        title="取消（服务端已收下的分片不会回收）"
        @click="emit('cancel', item.id)"
      >
        取消
      </button>
      <button v-if="canRetry" class="mini accent" type="button" @click="emit('retry', item.id)">
        重试
      </button>
      <button
        v-if="item.state === 'done' || item.state === 'canceled'"
        class="mini"
        type="button"
        @click="emit('remove', item.id)"
      >
        移除
      </button>
    </div>
  </li>
</template>

<script setup lang="ts">
import { computed } from 'vue'

import type { QueueItem } from '../../stores/uploadQueue'

const props = defineProps<{ item: QueueItem }>()
const emit = defineEmits<{
  (e: 'cancel', id: string): void
  (e: 'retry', id: string): void
  (e: 'remove', id: string): void
}>()

// 只有「本地在读」和「分片在传」的能量化：收尾、传封面、写库都是服务端或一次性的动作
const determinate = computed(
  () => props.item.state === 'hashing' || props.item.state === 'uploading',
)
const canCancel = computed(
  () => !['done', 'failed', 'canceled'].includes(props.item.state),
)
const canRetry = computed(() => props.item.state === 'failed' || props.item.state === 'canceled')

function humanSize(n: number): string {
  if (n >= 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`
  return `${Math.max(1, Math.round(n / 1024))} KB`
}

function sourceBadge(s: string): string {
  return { sibling: '同名图', frame: '抽帧', fallback: '占位' }[s] ?? ''
}
function sourceLabel(s: string): string {
  return (
    {
      sibling: '用了旁边同名的图片',
      frame: '从视频里截了一帧',
      fallback: '视频无法截帧，本地生成的占位封面',
    }[s] ?? ''
  )
}
</script>

<style scoped>
.row {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 12px;
  border: 1px solid var(--border);
  border-radius: 14px;
  background: var(--surface-2);
}
.row.done {
  border-color: rgba(74, 183, 132, 0.32);
}
.row.failed {
  border-color: rgba(229, 83, 75, 0.4);
}
.row.duplicate {
  border-style: dashed;
}

.thumb {
  position: relative;
  flex: 0 0 auto;
  width: 78px;
  aspect-ratio: 16 / 9;
  border-radius: 7px;
  overflow: hidden;
  background: var(--surface-3);
  display: grid;
  place-items: center;
}
.thumb img {
  width: 100%;
  height: 100%;
  object-fit: cover;
}
.ph {
  font-size: 0.72rem;
  color: var(--ink-muted);
}
.src {
  position: absolute;
  left: 3px;
  bottom: 3px;
  padding: 0 5px;
  border-radius: 4px;
  background: rgba(15, 15, 18, 0.8);
  color: var(--ink);
  font-size: 0.6rem;
}

.body {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 3px;
}
.name {
  margin: 0;
  font-size: 0.88rem;
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.sub {
  margin: 0;
  font-size: 0.74rem;
}
.phase {
  margin: 0;
  font-size: 0.76rem;
  color: var(--ink-muted);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.phase.err {
  color: var(--danger);
}

.bar {
  position: relative;
  height: 3px;
  margin-top: 4px;
  border-radius: 2px;
  background: var(--surface-3);
  overflow: hidden;
}
.fill {
  position: absolute;
  inset: 0;
  transform-origin: left;
  background: var(--accent);
  /* 只动 transform：宽度动画会触发布局重排，一整列进度条同时动就是卡顿的来源 */
  transition: transform 0.18s linear;
}
.fill.done {
  background: var(--ok);
}
.indeterminate {
  position: absolute;
  inset: 0;
  background: linear-gradient(90deg, transparent, var(--accent-line), transparent);
  animation: slide 1.3s linear infinite;
}
@keyframes slide {
  from {
    transform: translateX(-100%);
  }
  to {
    transform: translateX(100%);
  }
}

.ops {
  flex: 0 0 auto;
  display: flex;
  gap: 6px;
}
.mini {
  min-height: 32px;
  padding: 0 10px;
  border: 1px solid var(--border);
  border-radius: 9px;
  background: transparent;
  color: var(--ink);
  font-family: inherit;
  font-size: 0.78rem;
  cursor: pointer;
  transition: background 0.18s var(--ease-out);
}
.mini:hover {
  background: var(--surface-3);
}
.mini.accent {
  border-color: var(--accent-line);
  color: var(--accent);
}

@media (max-width: 767px) {
  .row {
    flex-wrap: wrap;
  }
  .thumb {
    width: 64px;
  }
  .ops {
    width: 100%;
    justify-content: flex-end;
  }
}
</style>
