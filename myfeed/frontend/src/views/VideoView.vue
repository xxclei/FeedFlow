<template>
  <section class="page">
    <div class="head">
      <div>
        <h1>投稿</h1>
        <p class="lead text-muted">
          单个走一遍完整流水线，批量把整个文件夹一次做完 —— 两条路底层是同一套分片协议。
        </p>
      </div>
    </div>

    <nav class="seg" aria-label="投稿方式">
      <button type="button" :class="{ on: mode === 'single' }" @click="go('single')">单个投稿</button>
      <button type="button" :class="{ on: mode === 'batch' }" @click="go('batch')">
        批量投稿<template v-if="q.hasActivity"> · {{ q.items.length }}</template>
      </button>
    </nav>

    <!-- ================= 单个 ================= -->
    <template v-if="mode === 'single'">
      <div class="card">
        <div class="grid">
          <div class="field">
            <label>视频文件（.mp4，≤{{ MAX_FILE_MB }}MB，统一走分片上传）</label>
            <input type="file" accept=".mp4,video/mp4" @change="onPickVideo" />
          </div>
          <div class="field">
            <label>封面（留空则同名图片优先，其次自动抽帧）</label>
            <input type="file" accept="image/*" @change="onPickCover" />
            <span v-if="coverNote" class="text-ok tag-note">✓ {{ coverNote }}</span>
          </div>
        </div>

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

        <div class="submit-row">
          <button class="btn" :disabled="busy" @click="onPublish">
            {{ busy ? progress || '发布中…' : '发布' }}
          </button>
          <button v-if="busy" class="btn btn-ghost" type="button" @click="cancelSingle">取消上传</button>
        </div>

        <p v-if="error" class="text-error">{{ error }}</p>
        <p v-if="okMsg" class="text-ok">{{ okMsg }}</p>
      </div>
    </template>

    <!-- ================= 批量 ================= -->
    <template v-else>
      <UploadDropZone @picked="onPicked" />

      <div v-if="q.items.length" class="card opts">
        <div class="grid">
          <div class="field">
            <label>标题前缀（拼在自动标题前面，可留空）</label>
            <input v-model="q.options.titlePrefix" placeholder="例如：2026 合辑 · " />
          </div>
          <div class="field">
            <label>统一描述（可留空）</label>
            <input v-model="q.options.description" placeholder="例如：周末随手拍" />
          </div>
        </div>
        <div class="field">
          <label>统一标签（回车成标签，不用打 <code>#</code>）</label>
          <TagChipsInput v-model="q.options.tags" placeholder="例如：日常 vlog（空格或回车分隔）" />
        </div>
        <p class="text-muted note">
          标题从文件名推：去扩展名、去掉开头的 <code>01 - </code> 这类序号。
          <strong>三个选项都是发布那一刻才生效的，改完立刻对已经入队的文件也生效</strong>
          （老版本只对之后新选的文件生效，改完前缀得全删重选）。
        </p>
        <p v-if="q.options.tags.length" class="text-muted note">
          标签会同时写进描述文本 —— 词法搜索只覆盖 title/description（FULLTEXT 索引建在这两列上），
          不写进去的话按标签搜不到这些视频，而单独投稿的能搜到。
        </p>
      </div>

      <div v-if="q.rejected.length" class="card rejected">
        <details>
          <summary>已跳过 {{ q.rejected.length }} 个文件</summary>
          <ul>
            <li v-for="r in q.rejected" :key="r.name" class="mono">
              {{ r.name }} <span class="text-muted">— {{ r.reason }}</span>
            </li>
          </ul>
        </details>
      </div>

      <div v-if="q.items.length" class="card">
        <div class="run-head">
          <div>
            <h2>{{ q.running ? '正在上传' : q.finished ? '上传结束' : '待开始' }}</h2>
            <p class="text-muted note">
              {{ q.counts.done }} 已发布 · {{ q.counts.active }} 处理中 · {{ q.counts.queued }} 排队 ·
              {{ q.counts.duplicate }} 内容重复
              <template v-if="q.counts.failed"> · <span class="text-error">{{ q.counts.failed }} 失败</span></template>
              <template v-if="q.counts.canceled"> · {{ q.counts.canceled }} 已取消</template>
            </p>
          </div>
          <div class="run-ops">
            <button v-if="!q.started" class="btn" type="button" @click="q.start()">
              开始上传（{{ q.items.length }} 个）
            </button>
            <template v-else>
              <button v-if="q.paused" class="btn" type="button" @click="q.resume()">继续</button>
              <button v-else-if="q.running" class="btn btn-ghost" type="button" @click="q.pause()">
                暂停
              </button>
              <button class="btn btn-ghost" type="button" @click="q.cancelAll()">全部取消</button>
            </template>
            <button class="btn btn-ghost" type="button" @click="onResetBatch">清空</button>
          </div>
        </div>

        <div class="agg" role="progressbar" :aria-valuenow="Math.round(q.overallPct * 100)">
          <span class="fill" :style="{ transform: `scaleX(${q.overallPct})` }" />
        </div>
        <p class="text-muted note">
          {{ humanSize(q.doneBytes) }} / {{ humanSize(q.totalBytes) }} ·
          文件级并行 2，每文件 3 条分片泳道，全局在途上限 4 —— 留 2 个连接给刷新/发布，否则它们会被分片堵到假死。
        </p>

        <ul class="list">
          <UploadItemRow
            v-for="it in q.items"
            :key="it.id"
            :item="it"
            :title="q.effectiveTitle(it)"
            :tags="q.effectiveTags(it)"
            @cancel="q.cancelItem"
            @retry="q.retryItem"
            @remove="q.removeItem"
          />
        </ul>
      </div>

      <div v-else class="card empty-note">
        <h3>还没有选文件</h3>
        <p class="text-muted">
          拖一个文件夹进来，里面的 mp4 会被递归找出来。<strong>内容完全相同的文件只会真传一份</strong>，
          其余直接复用它的地址 —— 后端按 <code>(账号, 文件哈希)</code> 索引上传会话，两份同样的文件同时传会互相踩。
        </p>
      </div>

      <div class="card hint-card">
        <h3>这个阶段的两个已知缺口</h3>
        <ul class="bullets text-muted">
          <li>
            <strong>没有 TTL。</strong>被放弃的上传会话会一直留在服务端内存里，
            已经收下的分片（<code>.run/uploads/videos/.../*.mp4.part</code> 半成品文件）也不回收，
            重启后端才能释放。所以「取消」只停发，不清理。
          </li>
          <li>
            <strong>续传靠服务端。</strong>失败后重新选同一个文件夹，<code>init</code> 会返回同一个上传会话和
            已收分片列表，只有缺的那几片会被重发 —— 不需要本地记任何东西。
          </li>
        </ul>
      </div>
    </template>

    <!-- ================= 我的作品 ================= -->
    <div class="card">
      <h2>我的作品</h2>
      <p v-if="videos.length === 0" class="text-muted">还没有作品，发一个？</p>

      <!-- 批量操作条。只勾了就出现 —— 空选时挂一条"已选 0 条"的禁用条是纯噪音 -->
      <div v-if="videos.length" class="bulk">
        <label class="pick">
          <input type="checkbox" :checked="allSelected" :indeterminate.prop="someSelected" @change="toggleAll" />
          <span class="text-muted">
            全选（{{ videos.length }} 条）<template v-if="selected.size"> · 已选 {{ selected.size }} 条</template>
          </span>
        </label>
        <button
          v-if="selected.size"
          class="btn btn-ghost del"
          type="button"
          :disabled="batchBusy"
          @click="onDeleteSelected"
        >
          {{ batchBusy ? '删除中…' : `删除选中（${selected.size}）` }}
        </button>
      </div>

      <p v-if="batchMsg" class="text-ok note">{{ batchMsg }}</p>
      <p v-if="listError" class="text-error note">{{ listError }}</p>

      <!-- 列表里**不再内嵌 <video>**，改成缩略图 + 跳播放页。
           以前每个作品都挂一个真 <video> 元素：作品一多就是几十个解码器常驻，
           而 Chrome 对同时活跃的 video 元素有硬上限（约 75 个，见 utils/cover.ts 的注释），
           超了直接 onerror。要播就进 /video/:id，那里只允许有一个播放器存在。 -->
      <div v-for="v in videos" :key="v.id" class="video-item" :class="{ picked: selected.has(v.id) }">
        <input
          class="box"
          type="checkbox"
          :checked="selected.has(v.id)"
          :aria-label="`选择 ${v.title}`"
          @change="toggleOne(v.id)"
        />
        <RouterLink class="thumb" :to="`/video/${v.id}`" :aria-label="`打开 ${v.title}`">
          <img :src="staticURL(v.cover_url)" :alt="v.title" loading="lazy" />
          <span class="glyph" aria-hidden="true">
            <svg viewBox="0 0 24 24" width="16" height="16">
              <path d="M8 5.2v13.6L19 12z" fill="currentColor" />
            </svg>
          </span>
        </RouterLink>
        <div class="meta">
          <strong>
            <RouterLink :to="`/video/${v.id}`">#{{ v.id }} {{ v.title }}</RouterLink>
          </strong>
          <span class="text-muted mono">{{ v.create_time?.slice(0, 19) }}</span>
          <button class="btn btn-ghost del" :disabled="batchBusy" @click="onDelete(v)">删除</button>
        </div>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'

import TagInput from '../components/TagInput.vue'
import TagChipsInput from '../components/TagChipsInput.vue'
import UploadDropZone from '../components/upload/UploadDropZone.vue'
import UploadItemRow from '../components/upload/UploadItemRow.vue'
import {
  deleteVideo,
  deleteVideoBatch,
  listByAuthorID,
  publish,
  staticURL,
  uploadCover,
  type VideoItem,
} from '../api/video'
import { useAuthStore } from '../stores/auth'
import { useUploadQueueStore } from '../stores/uploadQueue'
import { uploadOneFile } from '../utils/chunkUploader'
import { coverFromImage, resolveCover } from '../utils/cover'
import { chunkCount, fingerprintFile } from '../utils/hash'
import { Semaphore } from '../utils/pool'
import { isAbort, withRetry } from '../utils/retry'
import { MAX_FILE_MB, type PickedFile } from '../utils/folder'

const auth = useAuthStore()
const q = useUploadQueueStore()
const route = useRoute()
const router = useRouter()

const mode = computed(() => (route.query.mode === 'batch' ? 'batch' : 'single'))
function go(m: 'single' | 'batch') {
  router.replace({ path: '/video', query: m === 'single' ? {} : { mode: 'batch' } })
}

// ---------- 我的作品 ----------

const videos = ref<VideoItem[]>([])

/** 勾选状态。用 Set 而不是数组：判断"这一条勾了没有"在每个 checkbox 的渲染里都要做，
 *  数组的 includes 是 O(n)，一页 200 条就是每次渲染 200×200 次比较 */
const selected = ref(new Set<number>())
const allSelected = computed(() => videos.value.length > 0 && selected.value.size === videos.value.length)
const someSelected = computed(() => selected.value.size > 0 && !allSelected.value)

/** 每批最多删多少。**和后端 maxBatchDeleteIDs 一致** —— 后端超了回 400 */
const MAX_BATCH_DELETE = 100

const batchBusy = ref(false)
const batchMsg = ref('')
/** 「我的作品」自己的错误位。**刻意不复用下面那个 error** ——
 *  那个属于单个投稿卡片，共用的话一条删除失败会在两个卡片里各显示一次 */
const listError = ref('')

function toggleOne(id: number) {
  // 必须换一个新的 Set：ref(new Set()) 里改元素**不会触发**响应式更新
  // （Vue 的 reactive 不认识 Set 的原地修改），而只改 Set 再赋值自己也不行 ——
  // 引用没变，Vue 认为没变化。这里每次都造新对象
  const next = new Set(selected.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  selected.value = next
}

function toggleAll() {
  selected.value = allSelected.value ? new Set() : new Set(videos.value.map((v) => v.id))
}

async function loadMyVideos() {
  const accountID = auth.claims?.account_id
  if (!accountID) return
  videos.value = await listByAuthorID(accountID)
  // 列表换了，勾选里可能有已经不存在的 id（另一个标签页删过了）。
  // 留着它会让"已选 3 条"里的 1 条永远删不掉、skipped_ids 里反复出现它 ——
  // 那种"每次都少一条"的现象比直接清空勾选更难理解
  const alive = new Set(videos.value.map((v) => v.id))
  selected.value = new Set([...selected.value].filter((id) => alive.has(id)))
}
onMounted(loadMyVideos)

// 批量发布完，回到这一页时列表要是新的
watch(mode, () => {
  if (q.counts.done > 0) void loadMyVideos()
})
watch(
  () => q.counts.done,
  (n, old) => {
    if (n > old) void loadMyVideos()
  },
)

async function onDelete(v: VideoItem) {
  if (!window.confirm(`确认删除「${v.title}」？（数据库记录删除，磁盘文件保留）`)) return
  try {
    await deleteVideo(v.id)
    await loadMyVideos()
  } catch (e) {
    listError.value = e instanceof Error ? e.message : '删除失败'
  }
}

/**
 * 批量删除。
 *
 * ---------- 三种结果要分开说，不能笼统报"删除成功" ----------
 *
 * 后端刻意回 deleted_ids / skipped_ids 而不是一个计数，就是为了让这里能如实转达。
 * 勾选列表可能是旧的（另一个标签页先删过了，或者勾的时候还在、提交时已经没了），
 * 所以"3 条里删掉 2 条"是**正常结果**，不是错误。
 *
 *   · 全删掉   → "已删除 3 条"
 *   · 部分成功 → "3 条删掉 2 条，1 条没删掉（不是你的，或已经删过了）"，
 *                并把没删掉的那条**留在勾选态里**让用户能重试
 *   · 全失败   → 走 catch（比如 403/网络断了），那是真错误
 *
 * 如果这里只报"删除成功"，用户回头刷一下发现还有一条在，只会认为删除功能是坏的。
 */
async function onDeleteSelected() {
  const ids = [...selected.value]
  if (ids.length === 0) return

  // 超限时**拒绝**而不是悄悄截断前 100 条：截断的话用户以为都删了，
  // 而实际上后面的还在（而且他还得重新勾一遍）
  if (ids.length > MAX_BATCH_DELETE) {
    listError.value = `一次最多删 ${MAX_BATCH_DELETE} 条，当前勾了 ${ids.length} 条，请分批删`
    return
  }
  if (!window.confirm(`确认删除选中的 ${ids.length} 条作品？（数据库记录删除，磁盘文件保留）`)) return

  listError.value = ''
  batchMsg.value = ''
  batchBusy.value = true
  try {
    const res = await deleteVideoBatch(ids)
    if (res.skipped_ids?.length) {
      batchMsg.value =
        `已删除 ${res.deleted} 条，${res.skipped_ids.length} 条没删掉` +
        `（#${res.skipped_ids.join(' #')} —— 不是你的，或已经删过了）。它们仍留在勾选里，可以重试。`
      // 没删掉的留在勾选态：用户想重试就不用重新找一遍
      selected.value = new Set(res.skipped_ids)
    } else {
      batchMsg.value = `已删除 ${res.deleted} 条`
      selected.value = new Set()
    }
    await loadMyVideos()
  } catch (e) {
    listError.value = e instanceof Error ? e.message : '批量删除失败'
  } finally {
    batchBusy.value = false
  }
}

// ---------- 单个投稿 ----------

const videoFile = ref<File | null>(null)
const coverBlob = ref<File | null>(null) // 手动处理过的封面（1280×720 JPEG）
const coverPreview = ref('')
const coverNote = ref('')
const title = ref('')
const description = ref('')
const busy = ref(false)
const progress = ref('')
const error = ref('')
const okMsg = ref('')

// 单文件上传用不着跨文件调度，闸门给满 3 —— 和批量路径共用同一套分片代码
const singleGate = new Semaphore(3)
let singleAc: AbortController | null = null

function cancelSingle() {
  singleAc?.abort()
}

function onPickVideo(e: Event) {
  videoFile.value = (e.target as HTMLInputElement).files?.[0] ?? null
  // 换文件就清掉上一条的封面，否则会把上一个视频的封面发出去
  coverBlob.value = null
  coverPreview.value = ''
  coverNote.value = ''
  const v = videoFile.value
  if (v && !title.value.trim()) title.value = v.name.replace(/\.[^.]+$/, '')
}

async function onPickCover(e: Event) {
  const file = (e.target as HTMLInputElement).files?.[0]
  if (!file) return
  try {
    coverBlob.value = await coverFromImage(file)
    coverPreview.value = URL.createObjectURL(coverBlob.value)
    coverNote.value = '已按 16:9 裁剪'
    error.value = ''
  } catch (err) {
    error.value = err instanceof Error ? err.message : '封面处理失败'
  }
}

async function onPublish() {
  error.value = ''
  okMsg.value = ''
  const file = videoFile.value
  if (!file) return void (error.value = '请先选视频文件')
  if (!title.value.trim()) return void (error.value = '标题不能为空')

  busy.value = true
  const ac = new AbortController()
  singleAc = ac

  try {
    progress.value = '计算文件指纹…'
    const fp = await fingerprintFile(
      file,
      (d, t) => (progress.value = `计算指纹 ${d}/${t}`),
      ac.signal,
    )
    const total = chunkCount(file.size)
    let sent = 0

    const outcome = await uploadOneFile(file, fp.fileHash, fp.chunkHashes, {
      gate: singleGate,
      signal: ac.signal,
      onPhase: (p) =>
        (progress.value =
          p === 'initing' ? '建立上传会话…' : p === 'uploading' ? `上传中 0/${total}` : '服务端收尾…'),
      onResume: (bytes, chunks) =>
        (progress.value = `服务端已有 ${chunks} 片，从断点续传（${humanSize(bytes)}）`),
      onBytes: (delta) => {
        sent = Math.max(0, Math.min(sent + delta, file.size))
        progress.value = `上传中 ${Math.min(total, Math.ceil(sent / (file.size / total)))}/${total}`
      },
    })

    if (!coverBlob.value) {
      progress.value = '准备封面…'
      // 单文件选不到"同目录的同名图片"（它不在选择集里），所以这里只会走抽帧 → 占位图这条梯子。
      // 同名图片只有批量投稿（整个文件夹都在手上）才认得出来。
      const resolved = await resolveCover(file, title.value)
      coverBlob.value = resolved.blob
      coverPreview.value = URL.createObjectURL(resolved.blob)
      coverNote.value =
        { sibling: '用了同名图片', frame: '自动截取第 1 秒画面', fallback: '截帧失败，用了占位封面' }[
          resolved.source
        ]
    }

    progress.value = '上传封面…'
    const cover = await uploadCover(coverBlob.value, { signal: ac.signal })

    progress.value = '发布中…'
    await withRetry(
      () =>
        publish({
          title: title.value,
          description: description.value,
          play_url: outcome.playUrl,
          cover_url: cover.cover_url,
        }),
      { signal: ac.signal },
    )

    okMsg.value = '发布成功！outbox 里躺了一封 pending 的信（阶段9寄出）'
    title.value = ''
    description.value = ''
    coverBlob.value = null
    coverPreview.value = ''
    coverNote.value = ''
    videoFile.value = null
    await loadMyVideos()
  } catch (e) {
    error.value = isAbort(e) ? '已取消上传' : e instanceof Error ? e.message : '发布失败'
  } finally {
    busy.value = false
    progress.value = ''
    singleAc = null
  }
}

// ---------- 批量投稿 ----------

function onPicked(files: PickedFile[]) {
  q.addPicked(files)
}

function onResetBatch() {
  if (q.running && !window.confirm('还有文件在传，确认清空队列？（已上传的分片不会从服务端回收）')) return
  q.reset()
}

function humanSize(n: number): string {
  if (n >= 1024 * 1024 * 1024) return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`
  if (n >= 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`
  return `${Math.max(1, Math.round(n / 1024))} KB`
}
</script>

<style scoped>
.head {
  margin-bottom: 16px;
}
h1 {
  margin: 0;
  font-size: 1.35rem;
  letter-spacing: -0.02em;
}
.lead {
  margin: 4px 0 0;
  font-size: 0.84rem;
}
h2 {
  margin: 0 0 6px;
  font-size: 1.05rem;
  letter-spacing: -0.02em;
}
h3 {
  margin: 0 0 6px;
  font-size: 0.98rem;
}

.page {
  display: flex;
  flex-direction: column;
  gap: 18px;
  align-items: stretch;
}
.seg {
  margin-bottom: 2px;
}

.card {
  border-radius: 18px;
  padding: 22px;
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
.field input:not([type]) {
  padding: 11px 14px;
  border: 1px solid var(--border);
  border-radius: 12px;
  background: var(--surface-2);
  color: var(--ink);
  font-family: inherit;
  font-size: 0.95rem;
  outline: none;
}
.field input:not([type]):focus {
  border-color: var(--accent);
  box-shadow: 0 0 0 2px var(--accent-soft);
}
.tag-note {
  font-size: 0.8rem;
}

.cover-box {
  width: 320px;
  max-width: 100%;
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
  object-fit: cover;
}
.cover-box.empty {
  font-size: 0.8rem;
  text-align: center;
  padding: 8px;
}

.submit-row {
  display: flex;
  flex-wrap: wrap;
  gap: 10px;
}

.note {
  margin: 0;
  font-size: 0.78rem;
  line-height: 1.6;
}
.opts .field:last-of-type {
  margin-bottom: 0;
}

.rejected summary {
  cursor: pointer;
  font-size: 0.85rem;
  color: var(--ink-muted);
}
.rejected ul {
  margin: 10px 0 0;
  padding-left: 18px;
  font-size: 0.76rem;
  max-height: 200px;
  overflow: auto;
}
.rejected li {
  margin-bottom: 3px;
}

.run-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  flex-wrap: wrap;
  margin-bottom: 12px;
}
.run-ops {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}
.run-ops .btn {
  min-height: 38px;
  padding: 0 16px;
  font-size: 0.88rem;
}

.agg {
  position: relative;
  height: 5px;
  border-radius: 3px;
  background: var(--surface-3);
  overflow: hidden;
  margin-bottom: 8px;
}
.fill {
  position: absolute;
  inset: 0;
  transform-origin: left;
  background: var(--accent);
  transition: transform 0.25s var(--ease-out);
}

.list {
  list-style: none;
  margin: 14px 0 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.empty-note h3 {
  margin-top: 0;
}
.hint-card {
  border-style: dashed;
}
.bullets {
  margin: 0;
  padding-left: 18px;
  font-size: 0.82rem;
  line-height: 1.7;
}

.bulk {
  display: flex;
  align-items: center;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: 12px;
  padding: 4px 0 12px;
}
.pick {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 0.84rem;
  cursor: pointer;
}
.pick input,
.box {
  width: 16px;
  height: 16px;
  flex: 0 0 auto;
  accent-color: var(--accent);
  cursor: pointer;
}
.bulk .btn {
  min-height: 34px;
  padding: 0 14px;
  font-size: 0.84rem;
}

.video-item {
  display: flex;
  gap: 16px;
  align-items: center;
  padding: 12px 0;
  border-top: 1px solid var(--border);
}
/* 勾中的行给一点底色，否则在多选一排里看不出自己勾了哪几条 */
.video-item.picked {
  background: rgba(232, 103, 74, 0.05);
  border-radius: 10px;
}
.thumb {
  position: relative;
  flex: 0 0 auto;
  width: 180px;
  max-width: 42vw;
  aspect-ratio: 16 / 9;
  border-radius: 8px;
  overflow: hidden;
  background: #000;
  border: 1px solid var(--border);
}
.thumb img {
  width: 100%;
  height: 100%;
  object-fit: cover;
  display: block;
  transition: transform 0.3s var(--ease-out);
}
.thumb:hover img {
  transform: scale(1.04);
}
.glyph {
  position: absolute;
  inset: 0;
  margin: auto;
  width: 34px;
  height: 34px;
  display: grid;
  place-items: center;
  padding-left: 2px;
  border-radius: 50%;
  background: rgba(15, 15, 18, 0.62);
  backdrop-filter: blur(6px);
  color: var(--ink);
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

@media (max-width: 767px) {
  .grid {
    grid-template-columns: 1fr;
  }
  .card {
    border-radius: 16px;
    padding: 18px;
  }
  .run-ops {
    width: 100%;
  }
  .run-ops .btn {
    flex: 1 1 auto;
  }
}
</style>
