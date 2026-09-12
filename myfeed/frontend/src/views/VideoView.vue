<template>
  <section class="video-page">
    <div class="card rise">
      <h2>发布视频</h2>
      <div class="grid">
        <div class="field">
          <label>视频文件（.mp4，≤200MB，>5MB 自动分片）</label>
          <input type="file" accept=".mp4,video/mp4" @change="onPickVideo" />
          <span v-if="playUrl" class="text-ok">✓ 已上传</span>
        </div>
        <div class="field">
          <label>封面（jpg/png/webp，≤10MB）</label>
          <input type="file" accept="image/*" @change="onPickCover" />
          <span v-if="coverUrl" class="text-ok">✓ 已上传</span>
        </div>
      </div>
      <div class="field">
        <label>标题（打 #xx 会自动高亮成标签）</label>
        <TagInput v-model="title" placeholder="例如：我的第一个视频 #日常" />
      </div>
      <div class="field">
        <label>描述</label>
        <TagInput v-model="description" multi placeholder="补充说明，也能带 #标签" />
      </div>
      <button class="btn" :disabled="busy" @click="onPublish">
        {{ busy ? progress || '发布中…' : '发布' }}
      </button>
      <p v-if="error" class="text-error">{{ error }}</p>
      <p v-if="okMsg" class="text-ok">{{ okMsg }}</p>
    </div>

    <div class="card rise">
      <h2>我的作品</h2>
      <p v-if="videos.length === 0" class="text-muted">还没有作品，发一个？</p>
      <div v-for="v in videos" :key="v.id" class="video-item">
        <video :src="v.play_url" :poster="v.cover_url" controls preload="none"></video>
        <div class="meta">
          <strong>#{{ v.id }} {{ v.title }}</strong>
          <span class="text-muted mono">{{ v.create_time?.slice(0, 19) }}</span>
        </div>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'

import TagInput from '../components/TagInput.vue'
import { listByAuthorID, publish, uploadCover, uploadVideoAuto, type VideoItem } from '../api/video'
import { useAuthStore } from '../stores/auth'

const auth = useAuthStore()

const videoFile = ref<File | null>(null)
const coverFile = ref<File | null>(null)
const playUrl = ref('')
const coverUrl = ref('')
const title = ref('')
const description = ref('')
const busy = ref(false)
const progress = ref('')
const error = ref('')
const okMsg = ref('')
const videos = ref<VideoItem[]>([])

function onPickVideo(e: Event) {
  const files = (e.target as HTMLInputElement).files
  videoFile.value = files?.[0] ?? null
  playUrl.value = ''
}
function onPickCover(e: Event) {
  const files = (e.target as HTMLInputElement).files
  coverFile.value = files?.[0] ?? null
  coverUrl.value = ''
}

async function loadMyVideos() {
  const accountID = auth.claims?.account_id
  if (!accountID) return
  videos.value = await listByAuthorID(accountID)
}
onMounted(loadMyVideos)

async function onPublish() {
  error.value = ''
  okMsg.value = ''
  if (!videoFile.value) {
    error.value = '请先选视频文件'
    return
  }
  if (!coverFile.value) {
    error.value = '请先选封面'
    return
  }
  if (!title.value.trim()) {
    error.value = '标题不能为空'
    return
  }
  busy.value = true
  try {
    // 自动策略：≤5MB 直传，>5MB 自动切片+断点续传（api/video.ts 的 uploadVideoAuto）
    const url = await uploadVideoAuto(videoFile.value, (msg) => (progress.value = msg))
    playUrl.value = url

    progress.value = '上传封面…'
    const c = await uploadCover(coverFile.value)
    coverUrl.value = c.cover_url

    progress.value = '发布中…'
    await publish({
      title: title.value,
      description: description.value,
      play_url: playUrl.value,
      cover_url: coverUrl.value,
    })
    okMsg.value = '发布成功！outbox 里躺了一封 pending 的信（阶段9寄出）'
    title.value = ''
    description.value = ''
    await loadMyVideos()
  } catch (e) {
    error.value = e instanceof Error ? e.message : '发布失败'
  } finally {
    busy.value = false
    progress.value = ''
  }
}
</script>

<style scoped>
.video-page {
  display: flex;
  flex-direction: column;
  gap: 24px;
  max-width: 720px;
}
h2 {
  margin: 0 0 16px;
  letter-spacing: -0.02em;
}
.grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 16px;
}
.grid .field {
  min-width: 0; /* 网格子项允许收缩，防输入框撑破 */
}
.field {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin-bottom: 14px;
}
.field label {
  font-size: 0.85rem;
  font-weight: 600;
  color: var(--ink-muted);
}
.field input[type='file'] {
  padding: 8px;
  border: 1px solid var(--border);
  border-radius: 12px;
  background: var(--surface-2);
  color: var(--ink);
  width: 100%;
  min-width: 0;
}
.tip {
  font-size: 0.85rem;
  margin: 12px 0 0;
}
.video-item {
  display: flex;
  gap: 16px;
  align-items: center;
  padding: 12px 0;
  border-top: 1px solid var(--border);
}
.video-item video {
  width: 180px;
  border-radius: 8px;
  background: #000;
}
.meta {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}
</style>
