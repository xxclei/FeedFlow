<template>
  <div class="player">
    <video
      v-if="!failed"
      ref="el"
      class="video"
      controls
      playsinline
      webkit-playsinline
      preload="auto"
      :src="staticURL(playUrl)"
      :poster="staticURL(coverUrl)"
      @playing="started = true"
      @error="failed = true"
    ></video>

    <!-- 播放器自己的错误态。文件是真可能不在的：.run/uploads 是本地目录，
         而且 deleteVideo **只删数据库记录、不动磁盘**，反过来也可能有人手工清了文件 -->
    <div v-else class="broken">
      <p class="broken-title">视频文件打不开</p>
      <p class="broken-hint">
        浏览器拿不到能解码的流。可能是文件被删了、编码不被支持，或者后端没在跑。
      </p>
      <code class="broken-url">{{ staticURL(playUrl) }}</code>
    </div>

    <!-- 大播放键浮层。**不自动播放**：Chrome 的静音自动播放策略看的是"文档有没有被用户交互过"，
         从卡片点进来时文档有交互历史，play() 往往真的会成功 —— 于是同一个 URL
         点进来会自己放、直接刷新或从别人分享的链接进来却停在浮层上，两种行为，
         调起来极其难受。统一成"点了才放"，行为确定。
         （代价是少一点"进来就播"的顺滑，换来的是同一地址永远同一种表现。） -->
    <button v-if="!failed && !started" class="overlay" type="button" @click="play">
      <span class="glyph" aria-hidden="true">
        <svg viewBox="0 0 24 24" width="28" height="28">
          <path d="M8 5.2v13.6L19 12z" fill="currentColor" />
        </svg>
      </span>
      <span class="overlay-title">{{ title }}</span>
    </button>
  </div>
</template>

<script setup lang="ts">
import { onUnmounted, ref } from 'vue'

import { staticURL } from '../../api/video'

defineProps<{ playUrl: string; coverUrl: string; title: string }>()

const el = ref<HTMLVideoElement | null>(null)
/** 用 playing 而不是 play：play 只表示"播放被请求了"，这时还没有任何一帧解出来，
 *  拿它去收浮层会露出一块黑。playing 才是"画面真的在走了" */
const started = ref(false)
const failed = ref(false)

function play() {
  // play() 返回 Promise，被策略拒绝时是 reject 而不是抛异常 —— 不 catch 就是 unhandled rejection
  el.value?.play().catch(() => {
    /* 浮层留着，用户再点一次就行 */
  })
}

/**
 * 卸载时**必须**主动释放解码器。
 *
 * 光靠组件销毁不够：<video> 元素会一直攥着解码资源直到 GC，移动端 Safari 上尤其明显。
 * 这三步（pause → 清 src → load）是 utils/cover.ts 里抽帧那条路已经验证过的写法，
 * 在详情页同样必要 —— 而且父组件把 :key 绑在 play_url 上，从 /video/1 换到 /video/2
 * 时旧播放器会先整个卸载，正好走这里放手。
 */
onUnmounted(() => {
  const v = el.value
  if (!v) return
  v.pause()
  // 清 src 之后要显式 load()，元素才会真正松手
  v.removeAttribute('src')
  v.load()
})
</script>

<style scoped>
.player {
  position: relative;
  width: 100%;
  aspect-ratio: 16 / 9;
  background: #000;
  border: 1px solid var(--border);
  border-radius: 14px;
  overflow: hidden;
}

.video {
  display: block;
  width: 100%;
  height: 100%;
  background: #000;
  /* contain 而不是 cover：卡片上裁掉一点看不出来，播放器上裁掉的是画面本身 */
  object-fit: contain;
}

.overlay {
  position: absolute;
  inset: 0;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 16px;
  padding: 20px;
  border: none;
  background: linear-gradient(180deg, rgba(15, 15, 18, 0.1), rgba(15, 15, 18, 0.72));
  color: var(--ink);
  font-family: inherit;
  cursor: pointer;
}

.glyph {
  display: grid;
  place-items: center;
  width: 62px;
  height: 62px;
  padding-left: 4px; /* 三角形的视觉重心偏左，补一点回来 */
  border-radius: 50%;
  background: rgba(232, 103, 74, 0.94);
  box-shadow: var(--shadow);
  transition: transform 0.2s var(--ease-spring);
}
.overlay:hover .glyph {
  transform: scale(1.07);
}

.overlay-title {
  max-width: min(80%, 560px);
  font-size: 0.95rem;
  font-weight: 600;
  text-align: center;
  text-shadow: 0 2px 12px rgba(0, 0, 0, 0.7);
}

.broken {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
  height: 100%;
  padding: 24px;
  text-align: center;
}
.broken-title {
  margin: 0;
  font-size: 1rem;
  font-weight: 600;
}
.broken-hint {
  margin: 0;
  max-width: 460px;
  font-size: 0.83rem;
  color: var(--ink-muted);
}
.broken-url {
  margin-top: 4px;
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 0.72rem;
  color: var(--ink-muted);
}
</style>
