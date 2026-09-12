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
          <label>封面（不选则自动从视频截取一帧）</label>
          <input type="file" accept="image/*" @change="onPickCover" />
          <span v-if="coverNote" class="text-ok">✓ {{ coverNote }}</span>
        </div>
      </div>

      <!-- 封面预览：固定 16:9 区域，所见即所得 -->
      <div class="cover-box" :class="{ empty: !coverPreview }">
        <img v-if="coverPreview" :src="coverPreview" alt="封面预览" />
        <span v-else class="text-muted">封面预览（1280×720，自动居中裁剪）</span>
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
        <video :src="staticURL(v.play_url)" :poster="staticURL(v.cover_url)" controls preload="none"></video>
        <div class="meta">
          <strong>#{{ v.id }} {{ v.title }}</strong>
          <span class="text-muted mono">{{ v.create_time?.slice(0, 19) }}</span>
          <button class="btn btn-ghost del" @click="onDelete(v)">删除</button>
        </div>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'

import TagInput from '../components/TagInput.vue'
import {
  deleteVideo,
  listByAuthorID,
  publish,
  staticURL,
  uploadCover,
  uploadVideoAuto,
  type VideoItem,
} from '../api/video'
import { useAuthStore } from '../stores/auth'

const auth = useAuthStore()

const videoFile = ref<File | null>(null)
const coverBlob = ref<File | null>(null) // 处理后的封面（1280×720 JPEG），上传用它
const coverPreview = ref('') // 预览地址
const coverNote = ref('')
const playUrl = ref('')
const title = ref('')
const description = ref('')
const busy = ref(false)
const progress = ref('')
const error = ref('')
const okMsg = ref('')
const videos = ref<VideoItem[]>([])

// ---------- 封面管线：一切封面最终都变成 1280×720 的 JPEG ----------

function drawCover(src: CanvasImageSource, w: number, h: number): Promise<File> {
  const canvas = document.createElement('canvas')
  canvas.width = 1280
  canvas.height = 720
  const ctx = canvas.getContext('2d')
  if (!ctx) return Promise.reject(new Error('画布不可用'))
  // 16:9 居中裁剪源图（多出的部分左右/上下对称裁掉）
  const sw = Math.min(w, (h * 1280) / 720)
  const sh = Math.min(h, (w * 720) / 1280)
  ctx.drawImage(src, (w - sw) / 2, (h - sh) / 2, sw, sh, 0, 0, 1280, 720)
  return new Promise((res, rej) =>
    canvas.toBlob(
      (b) => (b ? res(new File([b], 'cover.jpg', { type: 'image/jpeg' })) : rej(new Error('封面生成失败'))),
      'image/jpeg',
      0.9,
    ),
  )
}

// 选了图片：读图 → 居中裁成 16:9
async function onPickCover(e: Event) {
  const file = (e.target as HTMLInputElement).files?.[0]
  if (!file) return
  const url = URL.createObjectURL(file)
  try {
    const img = new Image()
    img.src = url
    await new Promise<void>((res, rej) => {
      img.onload = () => res()
      img.onerror = () => rej(new Error('图片无法解码'))
    })
    coverBlob.value = await drawCover(img, img.naturalWidth, img.naturalHeight)
    coverPreview.value = URL.createObjectURL(coverBlob.value)
    coverNote.value = '已按 16:9 裁剪'
    error.value = ''
  } catch (err) {
    error.value = err instanceof Error ? err.message : '封面处理失败'
  } finally {
    URL.revokeObjectURL(url)
  }
}

// 没选图：从本地视频文件里截一帧（取第1秒，太短就取中点）
async function autoCoverFromVideo(file: File): Promise<File> {
  const url = URL.createObjectURL(file)
  try {
    const video = document.createElement('video')
    video.src = url
    video.muted = true
    await new Promise<void>((res, rej) => {
      video.onloadeddata = () => res()
      video.onerror = () => rej(new Error('视频无法解码，自动截帧失败'))
    })
    const at = Math.min(1, (video.duration || 1) / 2)
    await new Promise<void>((res, rej) => {
      video.onseeked = () => res()
      video.onerror = () => rej(new Error('截帧失败'))
      video.currentTime = at
    })
    return await drawCover(video, video.videoWidth, video.videoHeight)
  } finally {
    URL.revokeObjectURL(url)
  }
}

// ---------- 列表 / 删除 ----------

async function loadMyVideos() {
  const accountID = auth.claims?.account_id
  if (!accountID) return
  videos.value = await listByAuthorID(accountID)
}
onMounted(loadMyVideos)

async function onDelete(v: VideoItem) {
  if (!window.confirm(`确认删除「${v.title}」？（数据库记录删除，磁盘文件保留）`)) return
  try {
    await deleteVideo(v.id)
    await loadMyVideos()
  } catch (e) {
    error.value = e instanceof Error ? e.message : '删除失败'
  }
}

// ---------- 发布 ----------

function onPickVideo(e: Event) {
  const files = (e.target as HTMLInputElement).files
  videoFile.value = files?.[0] ?? null
  playUrl.value = ''
}

async function onPublish() {
  error.value = ''
  okMsg.value = ''
  if (!videoFile.value) {
    error.value = '请先选视频文件'
    return
  }
  if (!title.value.trim()) {
    error.value = '标题不能为空'
    return
  }
  busy.value = true
  try {
    // ① 视频：≤5MB 直传，>5MB 自动切片+断点续传
    const url = await uploadVideoAuto(videoFile.value, (msg) => (progress.value = msg))
    playUrl.value = url

    // ② 封面：选了图用裁剪结果；没选就自动截帧
    if (!coverBlob.value) {
      progress.value = '自动截取封面…'
      try {
        coverBlob.value = await autoCoverFromVideo(videoFile.value)
        coverPreview.value = URL.createObjectURL(coverBlob.value)
        coverNote.value = '已自动截取第1秒画面'
      } catch {
        throw new Error('自动截帧失败，请手动选择一张封面图片')
      }
    }

    progress.value = '上传封面…'
    const c = await uploadCover(coverBlob.value)

    progress.value = '发布中…'
    await publish({
      title: title.value,
      description: description.value,
      play_url: url,
      cover_url: c.cover_url,
    })
    okMsg.value = '发布成功！outbox 里躺了一封 pending 的信（阶段9寄出）'
    title.value = ''
    description.value = ''
    coverBlob.value = null
    coverPreview.value = ''
    coverNote.value = ''
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
  min-width: 0;
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
.cover-box {
  width: 320px;
  aspect-ratio: 16 / 9;
  border: 1px dashed var(--border);
  border-radius: 12px;
  overflow: hidden;
  display: flex;
  align-items: center;
  justify-content: center;
  margin-bottom: 14px;
  background: var(--surface-2);
}
.cover-box img {
  width: 100%;
  height: 100%;
  object-fit: cover; /* 固定区域展示，比例永远 16:9 */
}
.cover-box.empty {
  font-size: 0.8rem;
  text-align: center;
  padding: 8px;
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
.del {
  align-self: flex-start;
  color: var(--danger);
  border-color: rgba(229, 83, 75, 0.4);
  min-height: 32px;
  padding: 4px 12px;
}
</style>
