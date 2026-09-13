// 批量上传队列
//
// 放在 Pinia 而不是页面组件里，是因为「上传中跑去刷发现页」是这个项目里最正常的行为：
// 换路由会卸载视图组件，进度就没了。队列活在 app 级，人在哪一页它都在传。

import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

import { publish, uploadCover } from '../api/video'
import { uploadOneFile } from '../utils/chunkUploader'
import { autoCoverFromVideo, coverFromImage, findSiblingImage, renderFallbackCover } from '../utils/cover'
import { filterPicked, titleFromFilename, type PickedFile, type Rejected } from '../utils/folder'
import { chunkCount, fingerprintFile, type FileFingerprint } from '../utils/hash'
import { Semaphore } from '../utils/pool'
import { isAbort, withRetry } from '../utils/retry'
import { useAuthStore } from './auth'

/** 文件级并行度。每个文件内部还有 3 条分片泳道，总在途由全局闸门压住 */
const MAX_FILES_IN_FLIGHT = 2
/** 全局分片闸门。浏览器同源连接上限 6，留 2 个给 JSON 请求，否则刷新/发布会被分片堵在后面 */
const MAX_CHUNKS_IN_FLIGHT = 4

export type ItemState =
  | 'queued' // 已入队，等调度
  | 'hashing' // 本地算指纹
  | 'uploading' // 分片上传中
  | 'publishing' // 传封面 + 写库
  | 'duplicate' // 内容和批内更早的文件一模一样，等它先传完，共用 play_url
  | 'done'
  | 'failed'
  | 'canceled'

export interface QueueItem {
  id: string
  filename: string
  relPath: string
  size: number
  title: string
  state: ItemState
  /** 给人看的一句话状态，比如 "上传中 3/8" */
  phaseLabel: string
  bytesDone: number
  hashDone: number
  hashTotal: number
  percent: number
  error: string
  coverSource: '' | 'sibling' | 'frame' | 'fallback'
  coverPreview: string
  playUrl: string
  uploadId: string
  activeHash: string
  leaderId: string
  videoId: number
  /** 调度器占用标记，不是 UI 状态 */
  claimed: boolean
}

export interface BatchOptions {
  titlePrefix: string
  extraTags: string
}

// File 和指纹不进响应式图：Vue 的 reactive 会深度遍历，500MB 的 File 没必要被代理，
// 挂在 item 上还会让整个 store 不可序列化。用模块级 Map 按 id 侧存。
const files = new Map<string, File>()
const prints = new Map<string, FileFingerprint>()
const siblingImages = new Map<string, File>()
const controllers = new Map<string, AbortController>()
/** 中断是因为「暂停」还是「取消」？决定它回队列还是落 canceled。
 *  用独立的 Set 而不是 item 上的字段：item 是响应式的，字段会被 TS 收窄，
 *  而且它本质上是调度器状态，不该出现在 UI 的数据模型里。 */
const cancelIntent = new Set<string>()

export const useUploadQueueStore = defineStore('uploadQueue', () => {
  const auth = useAuthStore()

  const items = ref<QueueItem[]>([])
  const rejected = ref<Rejected[]>([])
  const options = ref<BatchOptions>({ titlePrefix: '', extraTags: '' })
  const paused = ref(false)
  const started = ref(false)
  const fatalError = ref('')

  const gate = new Semaphore(MAX_CHUNKS_IN_FLIGHT)
  let activeFiles = 0
  let draining = false
  let hashAc: AbortController | null = null
  const wakers: (() => void)[] = []

  // ---------- 派生数据 ----------

  const counts = computed(() => {
    const c = { queued: 0, active: 0, done: 0, failed: 0, canceled: 0, duplicate: 0 }
    for (const it of items.value) {
      if (it.state === 'done') c.done++
      else if (it.state === 'failed') c.failed++
      else if (it.state === 'canceled') c.canceled++
      else if (it.state === 'duplicate') c.duplicate++
      else if (it.state === 'queued') c.queued++
      else c.active++
    }
    return c
  })

  /** 失败/取消的不进分母，否则进度条永远到不了头，看着像卡死 */
  const counted = computed(() => items.value.filter((i) => i.state !== 'failed' && i.state !== 'canceled'))
  const totalBytes = computed(() => counted.value.reduce((n, i) => n + i.size, 0))
  const doneBytes = computed(() => counted.value.reduce((n, i) => n + Math.min(i.bytesDone, i.size), 0))
  const overallPct = computed(() => (totalBytes.value ? doneBytes.value / totalBytes.value : 0))

  const hasActivity = computed(() => items.value.length > 0)
  const running = computed(() =>
    items.value.some((i) => i.state === 'hashing' || i.state === 'uploading' || i.state === 'publishing'),
  )
  const finished = computed(
    () => hasActivity.value && !running.value && counts.value.queued === 0 && !paused.value,
  )

  // ---------- 入队 ----------

  /** coverPreview 是 object URL，丢弃 item 时必须 revoke，否则 40 个文件就是 40 张图常驻内存 */
  function revokePreview(it: QueueItem) {
    if (it.coverPreview) URL.revokeObjectURL(it.coverPreview)
  }

  function reset(opts?: BatchOptions) {
    cancelAll()
    for (const it of items.value) revokePreview(it)
    items.value = []
    rejected.value = []
    files.clear()
    prints.clear()
    siblingImages.clear()
    started.value = false
    paused.value = false
    fatalError.value = ''
    if (opts) options.value = opts
  }

  function addPicked(picked: PickedFile[]) {
    if (!auth.claims?.account_id) {
      fatalError.value = '需要先登录'
      return
    }

    const { accepted, rejected: rej } = filterPicked(picked)
    rejected.value = rej

    const created: QueueItem[] = []
    for (const p of accepted) {
      const item: QueueItem = {
        id: crypto.randomUUID(),
        filename: p.file.name,
        relPath: p.relPath,
        size: p.file.size,
        title: applyPrefix(titleFromFilename(p.file.name)),
        state: 'queued',
        phaseLabel: '等待中',
        bytesDone: 0,
        hashDone: 0,
        hashTotal: chunkCount(p.file.size),
        percent: 0,
        error: '',
        coverSource: '',
        coverPreview: '',
        playUrl: '',
        uploadId: '',
        activeHash: '',
        leaderId: '',
        videoId: 0,
        claimed: false,
      }
      files.set(item.id, p.file)
      // 在**原始**列表里找同名图片，不是过滤后的 accepted —— 后者只剩 mp4，
      // 图片早被 .mp4 那道闸门筛掉了，用它找等于永远找不到封面图
      const sib = findSiblingImage(p, picked)
      if (sib) siblingImages.set(item.id, sib)
      created.push(item)
    }

    items.value = items.value.concat(created)

    // 已经开始传了才追加文件 → 它还没指纹，得补一轮哈希，否则会被丢进上传阶段直接失败
    if (started.value && created.length) void hashPass().then(wake)
  }

  function applyPrefix(title: string): string {
    const p = options.value.titlePrefix.trim()
    return p ? `${p}${title}` : title
  }

  /** 统一标签走描述：后端从 title/description 里抽 #xxx，这样批量加 #vlog 不用逐个改标题 */
  function descriptionFor(): string {
    return options.value.extraTags.trim()
  }

  // ---------- 阶段一：逐个算指纹（算完一个就能开传） ----------
  //
  // 为什么要先算指纹：[坑1] 后端按 (accountID, file_hash) 索引会话，同一个文件夹里
  // 两份内容相同的 mp4 会塌进同一个 upload_id（谁先 complete 谁销毁会话，另一份的
  // 分片就没了着落）。所以要在 init 之前按内容指纹判重，让第二份只借用地址、不重复上传。
  //
  // 但判重**只要求"自己算完"，不要求"别人也算完"** —— 边算边往一张 Map 里登记就能
  // 在算出这一个的瞬间定它是不是重复。所以这里是**算完一个就 wake() 放它去传**，
  // 而不是等全批算完（老的写法是 `await hashPass(); wake()`，20 个 500MB 的文件
  // 变成"先顺序读完 10GB，才允许传第 1 个"，且每行都挂着"排队中"干等）。
  //
  // 顺序读而不是并行：并行 arrayBuffer 只会让磁盘寻道互相打架。
  // （代价：现在指纹读盘和分片切片读盘会交叠。SSD 上无所谓，机械盘上换来的是
  //   "第 1 个文件提前十分钟开始传" —— 这个交换值得。）

  // 哈希轮次串成一条链：同一时刻只有一轮在算（用户可能在上传中途又补选一批文件）。
  let hashChain: Promise<void> = Promise.resolve()

  /**
   * 把新一轮**排在上一次调用的后面**，而不是"已有一轮在跑就把它的 promise 还给你"。
   *
   * 这个区别是一个真 bug：老的写法 `if (hashRun) return hashRun` 会把正在跑的
   * 那一轮直接返回给调用者，而那一轮的扫描范围是**它启动那一刻的数组**（见下面
   * runHashPass 里"扫活数组"那段注释）—— 调用者刚补选进来的文件对它完全隐形。
   * 于是 `addPicked` 里那句 `hashPass().then(wake)` 的真实含义变成了
   * "等一轮看不见新文件的哈希跑完，然后把新文件放进上传通道"，
   * 上传阶段只能报「缺少文件指纹」。resume / retryItem 是同一个坑。
   *
   * 每轮一开始都先扫一遍待办再决定要不要干活，所以多排一轮只是廉价的空转。
   */
  function hashPass(): Promise<void> {
    hashChain = hashChain.then(runHashPass, runHashPass)
    return hashChain
  }

  async function runHashPass() {
    hashAc = new AbortController()
    const signal = hashAc.signal

    // 指纹索引：边算边建。种子是已经算好的那些（中途补选文件 / retryItem 的场景），
    // 这样"先传了一份、再补选含同一文件的文件夹"也能当场判重、直接复用已发布的 play_url。
    const byHash = new Map<string, QueueItem>()
    for (const it of items.value) {
      const fp = prints.get(it.id)
      if (fp && it.state !== 'duplicate') byHash.set(fp.fileHash, byHash.get(fp.fileHash) ?? it)
    }

    // 取待办的方式是"每轮重新扫一遍活数组"，而不是 `for (const it of items.value)`。
    // for-of 在循环开始的一瞬间就把数组对象定死了，而 addPicked 走的是
    // `items.value = items.value.concat(created)` —— 那是换了一个**新**数组，
    // 正在跑的这轮对刚补选的文件彻底隐形，可它已经是 'queued' 了。
    //
    // 判据用 prints 而不是 state：'queued' 有两个来源（addPicked 一创建就是 queued，
    // 但那时指纹还不存在），只有 prints 能回答"这一个到底算过没有"。
    for (;;) {
      // 暂停/取消时 hashAc 已经 abort，此后的每个 item 都会立刻抛 AbortError。
      // 不在这儿 break 的话，find 每轮都捞到同一个没算完的 item —— 死循环。
      if (paused.value || signal.aborted) break

      const it = items.value.find((x) => x.state === 'queued' && !prints.has(x.id))
      if (!it) break

      const file = files.get(it.id)
      if (!file) {
        // files 里没有它（比如 removeItem 清掉了文件条目）。必须把它挪出 'queued'，
        // 否则 find 每轮都捞到同一个它 —— 又是死循环。
        it.state = 'failed'
        it.error = '文件已不在队列里'
        it.phaseLabel = '失败'
        continue
      }

      it.state = 'hashing'
      it.phaseLabel = '读取文件…'
      try {
        const fp = await fingerprintFile(
          file,
          (done, total) => {
            it.hashDone = done
            it.hashTotal = total
            it.percent = total ? done / total : 1
            it.phaseLabel = `计算指纹 ${done}/${total}`
          },
          signal,
        )
        prints.set(it.id, fp)
        it.activeHash = fp.fileHash
        it.percent = 0

        // 哈希过程中被点了取消（cancelItem 不会打断哈希，它只是在等这一片读完）
        if (cancelIntent.has(it.id)) {
          it.state = 'canceled'
          it.phaseLabel = '已取消'
          continue
        }

        // 先算出来的那份当 leader，后面同哈希的直接标 duplicate。
        // 判重发生在置成 queued **之前**，所以 drain() 不可能先抢走一个本该是重复的文件 —— 无竞态
        const leader = byHash.get(fp.fileHash)
        if (leader && leader.id !== it.id) {
          it.state = 'duplicate'
          it.leaderId = leader.id
          it.phaseLabel = `与「${leader.filename}」内容相同，只传一份`
          // leader 要是早就 done 了，这一个立刻就能发（借它的 play_url），不必等整轮哈希
          wake()
        } else {
          byHash.set(fp.fileHash, it)
          it.state = 'queued'
          it.phaseLabel = '排队中'
          // 这一个已经可以传了，不用等后面的指纹 —— 流水线就在这一行
          wake()
        }
      } catch (e) {
        if (isAbort(e)) {
          it.state = paused.value ? 'queued' : 'canceled'
          it.phaseLabel = paused.value ? '已暂停' : '已取消'
        } else {
          it.state = 'failed'
          it.error = errText(e)
          it.phaseLabel = '指纹计算失败'
        }
      }
    }
    hashAc = null

    markDuplicates()
  }

  /**
   * 按内容指纹分组，同组里第一个当 leader，其余标 duplicate。
   *
   * 已经 done 的文件也参与当 leader —— 这样"先传了一半、再补选同一个文件夹"的场景下，
   * 新入队的重复文件会直接复用已发布那条的 play_url，不用再传一遍。
   *
   * runHashPass 现在已经在循环里逐个判重了，这里主要是收尾兜底（比如 leader 当时还没算完指纹）。
   */
  function markDuplicates() {
    const byHash = new Map<string, QueueItem>()

    for (const it of items.value) {
      if (it.state === 'failed' || it.state === 'canceled') continue
      // 已经判过重的不能再反过来当 leader：它和它真正的 leader 会互相指认 → 环形等待，
      // 两个都停在 duplicate 谁也不传。增量判重才需要这一行守卫。
      if (it.state === 'duplicate') continue
      const fp = prints.get(it.id)
      if (!fp) continue

      const leader = byHash.get(fp.fileHash)
      if (!leader) {
        byHash.set(fp.fileHash, it)
        continue
      }
      if (it.state === 'queued') {
        it.state = 'duplicate'
        it.leaderId = leader.id
        it.phaseLabel = `与「${leader.filename}」内容相同，只传一份`
      }
    }
  }

  // ---------- 阶段二：排空循环 ----------
  //
  // 用「捞一个、跑一个、回头再捞」而不是固定大小的工作池：待办列表是**运行时增长**的
  // （重复者可能被提级成真正的上传、失败重试会重新入队），固定池要额外套一层队列抽象。

  async function drain() {
    if (draining || paused.value) return
    draining = true
    try {
      for (;;) {
        if (paused.value) break

        promoteOrphans() // leader 挂了 → 它的重复者得自己上

        const next = items.value.find(
          (i) => !i.claimed && (i.state === 'queued' || (i.state === 'duplicate' && leaderReady(i))),
        )
        if (!next) break

        // 兜底闸门：'queued' 有两个来源 —— "指纹算完了"和 addPicked 刚创建（还没算）。
        // 正常时序下前者才可能被捞到，但异步路径多，漏一个就会以「缺少文件指纹」失败。
        // 与其让它带着这个错误落终态，不如把它推回哈希通道 —— 那轮算完会自己 wake。
        if (next.state === 'queued' && !prints.has(next.id)) {
          void hashPass()
          break
        }

        next.claimed = true
        activeFiles++
        void runItem(next)
          .catch(() => {
            /* 错误已经落到 item 上 */
          })
          .finally(() => {
            activeFiles--
            wakers.shift()?.()
            // 一个文件落终态，可能刚好解锁了它的重复者
            if (!draining) void drain()
          })

        if (activeFiles >= MAX_FILES_IN_FLIGHT) await new Promise<void>((r) => wakers.push(r))
      }
    } finally {
      draining = false
    }
  }

  function wake() {
    if (!draining) void drain()
  }

  function leaderOf(it: QueueItem): QueueItem | undefined {
    return it.leaderId ? items.value.find((x) => x.id === it.leaderId) : undefined
  }

  function leaderReady(it: QueueItem): boolean {
    const l = leaderOf(it)
    return !!l && l.state === 'done'
  }

  /** leader 失败/取消/被移除 → 重复者提级成真正的上传（指纹早就算好了，不用重算） */
  function promoteOrphans() {
    for (const it of items.value) {
      if (it.state !== 'duplicate' || it.claimed) continue
      const l = leaderOf(it)
      if (!l) {
        it.state = 'queued'
        it.leaderId = ''
        it.phaseLabel = '并传对象已移除，改为自己传'
      } else if (l.state === 'failed' || l.state === 'canceled') {
        it.state = 'queued'
        it.leaderId = ''
        it.phaseLabel = '并传对象失败，改为自己传'
      }
    }
  }

  async function runItem(it: QueueItem) {
    const file = files.get(it.id)
    if (!file) return

    cancelIntent.delete(it.id)
    const ac = new AbortController()
    controllers.set(it.id, ac)

    try {
      let playUrl: string

      if (it.state === 'duplicate') {
        // 同一个文件不必传第二遍：借 leader 的 play_url，然后**照常发一条自己的记录**
        // （同一个视频、两个标题 —— 这正是批量发布想要的效果，不安全的只是并发复用会话）
        playUrl = leaderOf(it)?.playUrl ?? ''
        if (!playUrl) throw new Error('并传对象的上传结果丢失')
        it.bytesDone = it.size
        it.percent = 1
        it.phaseLabel = '复用已有文件'
      } else {
        it.state = 'uploading'
        it.phaseLabel = '准备上传…'

        const fp = prints.get(it.id)
        if (!fp) throw new Error('缺少文件指纹')

        const total = chunkCount(it.size)
        const outcome = await uploadOneFile(file, fp.fileHash, fp.chunkHashes, {
          gate,
          signal: ac.signal,
          onPhase: (phase) => {
            if (phase === 'initing') it.phaseLabel = '建立上传会话…'
            else if (phase === 'uploading') it.phaseLabel = `上传中 0/${total}`
            else it.phaseLabel = '服务端收尾…'
          },
          onResume: (bytes, chunks) => {
            it.bytesDone = bytes
            it.phaseLabel = `服务端已有 ${chunks} 片，从断点续传`
          },
          onBytes: (delta) => {
            it.bytesDone = Math.max(0, Math.min(it.bytesDone + delta, it.size))
            it.percent = it.size ? it.bytesDone / it.size : 0
            if (it.phaseLabel.startsWith('上传中')) {
              const per = it.size / total
              it.phaseLabel = `上传中 ${Math.min(total, Math.ceil(it.bytesDone / per))}/${total}`
            }
          },
        })

        it.uploadId = outcome.uploadId
        it.activeHash = outcome.activeHash
        playUrl = outcome.playUrl
        it.bytesDone = it.size
        it.percent = 1
      }

      it.state = 'publishing'

      const coverBlob = await resolveCover(it, file)

      it.phaseLabel = '上传封面…'
      const cover = await withRetry(() => uploadCover(coverBlob, { signal: ac.signal }), {
        signal: ac.signal,
      })

      it.phaseLabel = '发布…'
      const video = await withRetry(
        () =>
          publish({
            title: it.title,
            description: descriptionFor(),
            play_url: playUrl,
            cover_url: cover.cover_url,
          }),
        { signal: ac.signal },
      )

      it.videoId = video.id
      it.playUrl = playUrl
      it.state = 'done'
      it.phaseLabel = `已发布 #${video.id}`
      it.percent = 1
    } catch (e) {
      if (isAbort(e) || ac.signal.aborted) {
        if (cancelIntent.has(it.id)) {
          it.state = 'canceled'
          it.phaseLabel = '已取消'
        } else {
          // 暂停：回队列。服务端记着已收的分片，恢复后 init 会带回同一个 upload_id
          it.state = 'queued'
          it.phaseLabel = '已暂停，恢复后从断点继续'
        }
      } else {
        it.state = 'failed'
        it.error = errText(e)
        it.phaseLabel = '失败'
      }
    } finally {
      controllers.delete(it.id)
      cancelIntent.delete(it.id)
      it.claimed = false
    }
  }

  async function resolveCover(it: QueueItem, file: File): Promise<File> {
    const sibling = siblingImages.get(it.id)
    if (sibling) {
      it.phaseLabel = '读取同名图片做封面…'
      try {
        const blob = await coverFromImage(sibling)
        it.coverSource = 'sibling'
        it.coverPreview = URL.createObjectURL(blob)
        return blob
      } catch {
        /* 同名图片坏了，继续往下走 */
      }
    }

    it.phaseLabel = '截取封面…'
    try {
      const blob = await autoCoverFromVideo(file)
      it.coverSource = 'frame'
      it.coverPreview = URL.createObjectURL(blob)
      return blob
    } catch {
      // publish 要求 cover_url 非空，抽帧却会因为文件损坏/编码不支持而失败。
      // 本地画一张占位图兜底：零网络、不可能失败，整批不会卡在最后一步。
      it.phaseLabel = '视频无法截帧，使用占位封面'
      const blob = await renderFallbackCover(it.title)
      it.coverSource = 'fallback'
      it.coverPreview = URL.createObjectURL(blob)
      return blob
    }
  }

  // ---------- 对外动作 ----------

  async function start() {
    if (started.value) return
    started.value = true
    paused.value = false
    await hashPass()
    wake()
  }

  function pause() {
    paused.value = true
    hashAc?.abort()
    for (const it of items.value) controllers.get(it.id)?.abort()
  }

  function resume() {
    paused.value = false
    started.value = true
    for (const it of items.value) {
      if (it.state === 'queued') it.phaseLabel = '排队中'
    }
    // 必须重新过一遍哈希，不能只 wake()：pause() 是在算指纹的半途 hashAc.abort() 掐断的，
    // 那些文件被放回了 'queued'，可它们**指纹并不存在**。直接 wake() 的话
    // drain 会把它们当成"已算完"捞去上传，整批报「缺少文件指纹」。
    // hashPass 只看 prints，已经有指纹的会被跳过，只有没算完的才重读。
    void hashPass().then(wake)
  }

  /** 从 canceled / failed 重新入队。服务端的分片还在，init 会带回断点 */
  async function retryItem(id: string) {
    const it = items.value.find((x) => x.id === id)
    if (!it || it.state === 'done') return

    cancelIntent.delete(id)
    it.error = ''
    it.claimed = false
    it.phaseLabel = '重新排队…'
    paused.value = false
    started.value = true

    if (prints.has(id)) {
      it.state = 'queued'
      wake()
      return
    }
    // 指纹都没算出来（哈希阶段就被取消/失败了），回到 hashPass 再跑一遍
    it.state = 'queued'
    it.hashDone = 0
    await hashPass()
    wake()
  }

  function cancelItem(id: string) {
    const it = items.value.find((x) => x.id === id)
    if (!it || it.state === 'done') return

    cancelIntent.add(id)
    const ac = controllers.get(id)
    if (ac) {
      ac.abort() // 在途的分片会被掐掉；服务端已经收下的部分无法撤回
    } else {
      it.state = 'canceled'
      it.phaseLabel = '已取消'
      it.claimed = false
    }
  }

  function cancelAll() {
    for (const it of items.value) cancelItem(it.id)
  }

  function removeItem(id: string) {
    cancelItem(id)
    const target = items.value.find((x) => x.id === id)
    if (target) revokePreview(target)
    items.value = items.value.filter((x) => x.id !== id)
    files.delete(id)
    prints.delete(id)
    siblingImages.delete(id)
  }

  function clearFinished() {
    const keep: QueueItem[] = []
    const dropped: string[] = []
    for (const i of items.value) {
      if (i.state === 'done' || i.state === 'canceled') dropped.push(i.id)
      else keep.push(i)
    }
    for (const i of items.value) {
      if (dropped.includes(i.id)) revokePreview(i)
    }
    for (const id of dropped) {
      files.delete(id)
      prints.delete(id)
      siblingImages.delete(id)
    }
    items.value = keep
  }

  function errText(e: unknown): string {
    if (e instanceof Error) return e.message
    return String(e)
  }

  return {
    items,
    rejected,
    options,
    paused,
    started,
    fatalError,
    counts,
    totalBytes,
    doneBytes,
    overallPct,
    hasActivity,
    running,
    finished,
    addPicked,
    reset,
    start,
    pause,
    resume,
    retryItem,
    cancelItem,
    cancelAll,
    removeItem,
    clearFinished,
  }
})
