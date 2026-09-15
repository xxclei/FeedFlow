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
import { normalizeTagNames } from '../utils/tags'
import { newId } from '../utils/uuid'
import { useAuthStore } from './auth'

/** 文件级并行度。每个文件内部还有 3 条分片泳道，总在途由全局闸门压住 */
const MAX_FILES_IN_FLIGHT = 2
/** 全局分片闸门。浏览器同源连接上限 6，留 2 个给 JSON 请求，否则刷新/发布会被分片堵在后面 */
const MAX_CHUNKS_IN_FLIGHT = 4

/** description 列宽（varchar(255)，单位是**字符**不是字节）。见 effectiveDescription */
const MAX_DESCRIPTION_RUNES = 255
/** 批量面板最多几个标签。后端不限个数，这里封顶纯粹是防"粘进来一大段" */
const MAX_BATCH_TAGS = 20

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

/**
 * 批量面板上的三个选项。
 *
 * ---------- 它们**都在发布的那一刻**才生效，不是入队时 ----------
 *
 * 老版本把 titlePrefix 拼进 item.title（入队时算一次），把 extraTags 塞进描述，
 * 于是"先选文件、再改选项"只对**之后**选的文件生效 —— 用户改完前缀发现前面那些没变，
 * 只能全删重选。这是同一个 bug 的两个实例，所以本轮三个选项统一改成
 * **在 runItem 里现算**（见 effectiveTitle / effectiveDescription / effectiveTags）。
 *
 * ---------- extraTags（string）为什么变成 tags（string[]）----------
 *
 * 老的 extraTags 是一个裸输入框，它的内容会**整个**变成每条视频的描述，
 * 后端再从描述里用正则抽 #xxx。于是"打 日常 vlog 不带 #"= 一个标签都没有，
 * 而且不报错、不提示。现在标签有专门的 chip 编辑器（TagChipsInput），
 * 传出去的是结构化数组，后端直接落 video_tags —— 不再经过"文本 → 正则 → 猜意图"。
 */
export interface BatchOptions {
  titlePrefix: string
  /** 统一描述（可选）。标签**不再**从这里走，见 tags */
  description: string
  /** 统一标签。发布时作为 tag_names 送出去，和描述里手写的 #xxx 取并集 */
  tags: string[]
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
  const options = ref<BatchOptions>({ titlePrefix: '', description: '', tags: [] })
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
        // 用 utils/uuid.ts 的 newId()，**不要**直接写 crypto.randomUUID()：
        // 那个方法只在安全上下文（https / localhost）里存在，
        // 而这个站现在是 http://<公网IP> 跑着的 —— 2026-09-15 上云当天
        // 就是这一行报的 "crypto.randomUUID is not a function"，
        // 表现是"选完文件点上传，队列里一个条目都不出现"。
        id: newId(),
        filename: p.file.name,
        relPath: p.relPath,
        size: p.file.size,
        // **基础标题**：只从文件名推，不含前缀。前缀在发布时现拼（见 effectiveTitle）——
        // 存进来的标题带上前缀的话，"改前缀"就只能对后来者生效了
        title: titleFromFilename(p.file.name),
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

  // ---------- 三个"发布时才现算"的派生值 ----------
  //
  // 放在这里而不是入队时算进 item 里，是为了让"改选项"对**已经入队的文件**也生效。
  // 它们都是普通函数而不是 computed：调用点在 runItem 里（非响应式上下文），
  // 而模板里调用时每次渲染都会重新求值 —— 两边都成立。

  /** 最终标题 = 前缀 + 基础标题 */
  function effectiveTitle(it: QueueItem): string {
    const p = options.value.titlePrefix.trim()
    return p ? `${p}${it.title}` : it.title
  }

  /** 最终标签。走 normalizeTagNames，和后端 normalizeTagNames 同一套规则 */
  function effectiveTags(_it: QueueItem): string[] {
    // _it 参数当前用不上（标签是整批统一的），但保留它让三个 effective* 签名一致 ——
    // 将来"每个文件单独调标签"时只改实现，不用改所有调用点
    return normalizeTagNames(options.value.tags).slice(0, MAX_BATCH_TAGS)
  }

  /**
   * 最终描述 = 统一描述 + 标签（渲染成 `#a #b`）。
   *
   * ---------- 为什么标签要**同时**写进描述文本 ----------
   *
   * 两件事都指向同一个结论：
   *
   *  1. **词法搜索只覆盖 title + description**（FULLTEXT 索引 ft_videos_title_desc 就建在
   *     这两列上），video_tags 不参与词法检索。标签不落到文本里的话，批量投稿的视频
   *     按标签搜不到 —— 而单独投稿的（TagInput 把 #标签 写进描述）能搜到。
   *     同一个功能两种行为，用户没法理解。
   *  2. 卡片上的标签 chip 是从 title/description 里正则解析出来的
   *     （后端响应目前不返回 tags），不写进文本就**不显示**。
   *
   * 所以这是一处刻意的冗余：**结构化标签（tag_names）是权威来源，描述里的文本是投影**。
   * 等 /feed 响应带上 tags、FULLTEXT 也覆盖 video_tags 之后，这一行就可以删掉。
   *
   * 长度按 rune 截到 255（description 是 varchar(255)，MySQL 的 N 是**字符数**）：
   * 超了会撞 1406 → 被兜成 500，而用户只会看到"发布失败"，看不出是描述太长。
   * 老版本的 extraTags 是原样发出去的，一直有这个隐患。
   */
  function effectiveDescription(it: QueueItem): string {
    const parts: string[] = []
    const desc = options.value.description.trim()
    if (desc) parts.push(desc)
    const tags = effectiveTags(it)
    if (tags.length) parts.push(tags.map((t) => `#${t}`).join(' '))
    const text = parts.join(' ')
    const runes = Array.from(text)
    return runes.length > MAX_DESCRIPTION_RUNES
      ? runes.slice(0, MAX_DESCRIPTION_RUNES).join('')
      : text
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
      // 三个字段都是**此刻**现算的（不是入队时算好存进 item 的）——
      // 所以用户在上传途中改前缀/描述/标签，连正在传的这个都会用上新的
      const video = await withRetry(
        () =>
          publish({
            title: effectiveTitle(it),
            description: effectiveDescription(it),
            tag_names: effectiveTags(it),
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
      // 用**最终标题**画在封面上：占位图里写着标题，用户看到的就是发出去的那条
      const blob = await renderFallbackCover(effectiveTitle(it))
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
    // 三个派生值要暴露给模板：列表里显示"这个文件最终会带上什么"，
    // 以及让"改选项立即对所有已入队文件生效"这件事在界面上看得见
    effectiveTitle,
    effectiveDescription,
    effectiveTags,
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
