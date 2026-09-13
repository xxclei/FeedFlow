// 封面管线：无论来源是什么，最终都变成 1280×720 的 JPEG File
//
// 三条来源，按优先级：
//   1. 同名图片（`a.mp4` 旁边的 `a.jpg/.png/.webp`）—— 用户已经挑好的，最准
//   2. 从视频里抽一帧                                   —— 兜底，几乎总是有
//   3. 画一张占位图                                     —— 抽帧失败时的最后一道，不能失败
// 第 3 条必须存在：publish 要求 cover_url 非空，抽帧却会因为文件损坏/编码不支持而失败，
// 不兜底的话整批上传会卡在"视频传完了但发不出去"。

const COVER_W = 1280
const COVER_H = 720
const EXTRACT_TIMEOUT = 8000

/** 16:9 居中裁剪 → JPEG。所有封面都从这里出去 */
export function drawCover(src: CanvasImageSource, w: number, h: number): Promise<File> {
  const canvas = document.createElement('canvas')
  canvas.width = COVER_W
  canvas.height = COVER_H
  const ctx = canvas.getContext('2d')
  if (!ctx) return Promise.reject(new Error('画布不可用'))

  const sw = Math.min(w, (h * COVER_W) / COVER_H)
  const sh = Math.min(h, (w * COVER_H) / COVER_W)
  ctx.drawImage(src, (w - sw) / 2, (h - sh) / 2, sw, sh, 0, 0, COVER_W, COVER_H)

  return new Promise((res, rej) =>
    canvas.toBlob(
      (b) => (b ? res(new File([b], 'cover.jpg', { type: 'image/jpeg' })) : rej(new Error('封面生成失败'))),
      'image/jpeg',
      0.9,
    ),
  )
}

// ---------- 串行闸门 ----------
//
// 同一时刻**只允许一个 <video> 在解码**。
// 原因：一个 500MB 的视频元素会占住解码器和 canvas，N 个并行就是 N 份内存；
// 而且浏览器对同时活跃的 video 元素有硬上限（Chrome 约 75 个），超了直接 onerror。
// 批量上传时文件级并发是 2，抽帧却在全局串行 —— 这是有意的：抽帧是 CPU 活，
// 并行只会互相拖慢，还会和网络阶段抢主线程。

let chain: Promise<unknown> = Promise.resolve()

function serial<T>(fn: () => Promise<T>): Promise<T> {
  const next = chain.then(fn, fn)
  chain = next.catch(() => {})
  return next
}

// ---------- 来源 2：抽帧 ----------

/**
 * 从本地视频文件里截一帧（第 1 秒；太短就取中点）。
 *
 * finally 里的两步清理不能省：清 src 之后要显式 load() 才会真正释放解码器，
 * 只 revokeObjectURL 的话在移动端 Safari 上元素会一直攥着解码资源直到 GC。
 */
export function autoCoverFromVideo(file: File, timeoutMs = EXTRACT_TIMEOUT): Promise<File> {
  return serial(async () => {
    const url = URL.createObjectURL(file)
    const video = document.createElement('video')
    try {
      video.src = url
      video.muted = true
      video.playsInline = true
      video.preload = 'metadata'

      await withTimeout(
        new Promise<void>((res, rej) => {
          video.onloadeddata = () => res()
          video.onerror = () => rej(new Error('视频无法解码'))
        }),
        timeoutMs,
        '抽帧超时',
      )

      const at = Math.min(1, (video.duration || 1) / 2)
      await withTimeout(
        new Promise<void>((res, rej) => {
          video.onseeked = () => res()
          video.onerror = () => rej(new Error('定位关键帧失败'))
          video.currentTime = at
        }),
        timeoutMs,
        '定位关键帧超时',
      )

      if (!video.videoWidth || !video.videoHeight) throw new Error('视频尺寸未知')
      return await drawCover(video, video.videoWidth, video.videoHeight)
    } finally {
      video.removeAttribute('src')
      video.load()
      URL.revokeObjectURL(url)
    }
  })
}

// ---------- 来源 1：同名图片 ----------

const IMAGE_EXT = ['.jpg', '.jpeg', '.png', '.webp']

/** 在一批已选文件里找 `foo.mp4` 的同名图片；找不到返回 undefined */
export function findSiblingImage(video: { relPath: string }, all: PickedLike[]): File | undefined {
  const base = video.relPath.replace(/\.mp4$/i, '').toLowerCase()
  const byPath = new Map(all.map((p) => [p.relPath.toLowerCase(), p.file]))

  // 先认严格同名：`a.mp4` ↔ `a.jpg` / `a.jpeg` / `a.png` / `a.webp`
  for (const ext of IMAGE_EXT) {
    const hit = byPath.get(base + ext)
    if (hit) return hit
  }

  // 再认"同目录 + 以主名开头"（`a.mp4` + `a.封面.png`），但目录必须完全一致，
  // 否则同一个文件夹里 `01 - 开场.mp4` 会认领 `02 - 开场.jpg`
  const slash = base.lastIndexOf('/')
  const dir = base.slice(0, slash + 1)
  const stem = base.slice(slash + 1)
  return all.find((p) => {
    const path = p.relPath.toLowerCase()
    if (!IMAGE_EXT.some((e) => path.endsWith(e))) return false
    const s = path.lastIndexOf('/')
    return path.slice(0, s + 1) === dir && path.slice(s + 1).startsWith(stem)
  })?.file
}

interface PickedLike {
  relPath: string
  file: File
}

export function coverFromImage(file: File): Promise<File> {
  const url = URL.createObjectURL(file)
  const img = new Image()
  img.src = url
  return new Promise<File>((res, rej) => {
    img.onload = () => {
      drawCover(img, img.naturalWidth, img.naturalHeight).then(res, rej)
    }
    img.onerror = () => rej(new Error('图片无法解码'))
  }).finally(() => URL.revokeObjectURL(url))
}

// ---------- 来源 3：占位图 ----------

/**
 * 画一张本地占位封面（赤陶色条 + 标题），零网络、不可能失败。
 * 视觉上贴着设计系统，所以出现在信息流里也不会显得是坏图。
 */
export function renderFallbackCover(title: string): Promise<File> {
  return serial(async () => {
    const canvas = document.createElement('canvas')
    canvas.width = COVER_W
    canvas.height = COVER_H
    const ctx = canvas.getContext('2d')
    if (!ctx) throw new Error('画布不可用')

    ctx.fillStyle = '#0f0f12'
    ctx.fillRect(0, 0, COVER_W, COVER_H)
    ctx.fillStyle = '#e8674a'
    ctx.fillRect(0, COVER_H - 14, COVER_W, 6)

    ctx.fillStyle = '#f2f2f0'
    ctx.font = '600 56px "Noto Sans SC", "Microsoft YaHei", system-ui, sans-serif'
    ctx.textBaseline = 'middle'
    wrapText(ctx, title, 96, COVER_H / 2, COVER_W - 192, 74, 3)

    return new Promise((res, rej) =>
      canvas.toBlob(
        (b) => (b ? res(new File([b], 'cover.jpg', { type: 'image/jpeg' })) : rej(new Error('占位封面生成失败'))),
        'image/jpeg',
        0.88,
      ),
    )
  })
}

function wrapText(
  ctx: CanvasRenderingContext2D,
  text: string,
  x: number,
  cy: number,
  maxWidth: number,
  lineHeight: number,
  maxLines: number,
) {
  const lines: string[] = []
  let line = ''
  for (const ch of Array.from(text)) {
    if (ctx.measureText(line + ch).width > maxWidth && line) {
      lines.push(line)
      line = ch
      if (lines.length === maxLines) break
    } else {
      line += ch
    }
  }
  if (lines.length < maxLines && line) lines.push(line)
  else if (lines.length === maxLines) lines[maxLines - 1] = lines[maxLines - 1].slice(0, -1) + '…'

  const total = lines.length * lineHeight
  let y = cy - total / 2 + lineHeight / 2
  for (const l of lines) {
    ctx.fillText(l, x, y)
    y += lineHeight
  }
}

function withTimeout<T>(p: Promise<T>, ms: number, msg: string): Promise<T> {
  return new Promise<T>((res, rej) => {
    const timer = setTimeout(() => rej(new Error(msg)), ms)
    p.then(
      (v) => {
        clearTimeout(timer)
        res(v)
      },
      (e) => {
        clearTimeout(timer)
        rej(e)
      },
    )
  })
}

/** 供单文件上传页复用的默认行为：同名图片 → 抽帧 */
export async function resolveCover(
  file: File,
  title: string,
  sibling?: File,
): Promise<{ blob: File; source: 'sibling' | 'frame' | 'fallback' }> {
  if (sibling) {
    try {
      return { blob: await coverFromImage(sibling), source: 'sibling' }
    } catch {
      /* 同名图片坏了，继续往下走 */
    }
  }
  try {
    return { blob: await autoCoverFromVideo(file), source: 'frame' }
  } catch {
    return { blob: await renderFallbackCover(title), source: 'fallback' }
  }
}
