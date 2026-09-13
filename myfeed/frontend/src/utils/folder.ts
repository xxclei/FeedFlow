// 取一个文件夹里的所有视频
//
// 两条入口，语义不一样，UI 上要如实说明：
//   <input webkitdirectory>  —— 浏览器负责递归，每个 File 自带 webkitRelativePath
//   拖拽文件夹               —— 我们自己递归，且**只**递归拖进来的目录（散文件也能拖）
// 所以「要不要递归子文件夹」这个开关对 input 路线毫无意义，干脆不做。

/** 和后端保持一致的上限（直传和分片 init 都是 500 << 20） */
export const MAX_FILE_SIZE = 500 * 1024 * 1024
/** 给界面文案用。从上面那个常量推出来，改一处两边都跟着变，不会写着 200 实际卡 500 */
export const MAX_FILE_MB = MAX_FILE_SIZE / 1024 / 1024
export const MAX_BATCH_FILES = 200
const MAX_DEPTH = 8

const SKIP_DIRS = new Set([
  '__MACOSX',
  '.git',
  '.svn',
  'node_modules',
  'System Volume Information',
  '$RECYCLE.BIN',
])

/** Windows 系统文件 + 类 Unix 隐藏文件。注意 `._foo.mp4` 也命中 startsWith('.')，
 *  那是 macOS 的 AppleDouble 边车文件，4KB 大小却带 .mp4 后缀，不挡掉就会被当视频传 */
function isJunkName(name: string): boolean {
  if (name.startsWith('.')) return true
  const lower = name.toLowerCase()
  return lower === 'thumbs.db' || lower === 'desktop.ini' || name.startsWith('~$')
}

export interface PickedFile {
  file: File
  /** 相对路径，统一用 / 分隔，例如 "我的合集/01 - 开场.mp4" */
  relPath: string
}

export interface Rejected {
  name: string
  reason: string
}

export interface PickResult {
  accepted: PickedFile[]
  rejected: Rejected[]
}

// ---------- 入口 A：<input type="file" webkitdirectory> ----------

export function pickedFromInput(input: HTMLInputElement): PickedFile[] {
  return Array.from(input.files ?? []).map((file) => ({
    file,
    // webkitdirectory 下这个属性一定有值；没有就退回文件名
    relPath: (file as File & { webkitRelativePath?: string }).webkitRelativePath || file.name,
  }))
}

// ---------- 入口 B：拖拽 ----------

/**
 * 必须在 drop 事件里**同步**调用。
 * dataTransfer.items 只在事件派发期间有效，await 一次之后就空了 —— 这是拖拽读取最经典的坑。
 */
export function entriesFromDrop(e: DragEvent): FileSystemEntry[] {
  const items = e.dataTransfer?.items
  if (!items) return []
  const out: FileSystemEntry[] = []
  for (const it of Array.from(items)) {
    if (it.kind !== 'file') continue
    const entry = it.webkitGetAsEntry?.()
    if (entry) out.push(entry)
  }
  return out
}

export async function walkEntries(entries: FileSystemEntry[]): Promise<PickedFile[]> {
  const out: PickedFile[] = []
  for (const entry of entries) await walkEntry(entry, '', out, 0)
  return out
}

async function walkEntry(
  entry: FileSystemEntry,
  prefix: string,
  out: PickedFile[],
  depth: number,
): Promise<void> {
  if (depth > MAX_DEPTH) return

  if (entry.isFile) {
    const file = await fileFromEntry(entry as FileSystemFileEntry)
    // 拖拽拿到的 File 没有 webkitRelativePath（那个属性只服务于 input），路径只能自己拼
    out.push({ file, relPath: prefix + entry.name })
    return
  }
  if (!entry.isDirectory || SKIP_DIRS.has(entry.name)) return

  const reader = (entry as FileSystemDirectoryEntry).createReader()
  for (;;) {
    const batch = await readEntries(reader)
    // readEntries **一次最多返回 100 条**，并且以「空数组」而非「不足 100 条」表示读完。
    // 只调一次的话，300 个文件的文件夹会被静默截断成 100 个。
    if (batch.length === 0) break
    for (const child of batch) await walkEntry(child, `${prefix}${entry.name}/`, out, depth + 1)
  }
}

function fileFromEntry(entry: FileSystemFileEntry): Promise<File> {
  return new Promise((resolve, reject) => entry.file(resolve, reject))
}

function readEntries(reader: FileSystemDirectoryReader): Promise<FileSystemEntry[]> {
  return new Promise((resolve, reject) => reader.readEntries(resolve, reject))
}

// ---------- 过滤 ----------

export function filterPicked(picked: PickedFile[]): PickResult {
  const accepted: PickedFile[] = []
  const rejected: Rejected[] = []
  const seenPaths = new Set<string>()

  for (const p of picked) {
    const name = p.file.name

    if (seenPaths.has(p.relPath)) {
      rejected.push({ name: p.relPath, reason: '同一路径重复选中' })
      continue
    }
    if (isJunkName(name)) {
      rejected.push({ name: p.relPath, reason: '隐藏/系统文件' })
      continue
    }
    // 扩展名是**唯一**的格式闸门：分片路径的后端完全不校验扩展名，
    // 还会把结果无条件命名成 filename + ".mp4"，所以这里放行了什么，就会被当成 mp4 提供。
    if (!/\.mp4$/i.test(name)) {
      rejected.push({ name: p.relPath, reason: '不是 .mp4' })
      continue
    }
    if (p.file.size <= 0) {
      rejected.push({ name: p.relPath, reason: '空文件' })
      continue
    }
    if (p.file.size > MAX_FILE_SIZE) {
      rejected.push({
        name: p.relPath,
        reason: `超过 ${MAX_FILE_SIZE / 1024 / 1024}MB`,
      })
      continue
    }
    if (accepted.length >= MAX_BATCH_FILES) {
      rejected.push({ name: p.relPath, reason: `超过单批 ${MAX_BATCH_FILES} 个` })
      continue
    }

    seenPaths.add(p.relPath)
    accepted.push(p)
  }

  return { accepted: accepted.sort((a, b) => naturalCompare(a.relPath, b.relPath)), rejected }
}

/** 自然序：`2 - b.mp4` 排在 `10 - c.mp4` 前面（按字符串比会反过来） */
export function naturalCompare(a: string, b: string): number {
  return a.localeCompare(b, 'zh-Hans-CN', { numeric: true, sensitivity: 'base' })
}

// ---------- 标题推导 ----------

// 开头是 1~4 位数字，后面跟一个分隔符（- _ . 、）或至少两个空格
const LEADING_ORDINAL = /^\s*(\d{1,4})\s*(?:[-–—_.、]|\s{2,})\s*/

/**
 * 全自动标题：去扩展名 → 去掉开头的序号 → trim。
 *
 * 刻意保守：
 *   `01 - 开场.mp4`    → `开场`
 *   `Ep01.mp4`         → `Ep01`（数字后面没有分隔符，不动）
 *   `2024 年度总结.mp4` → 不变（单个空格不算分隔符）
 *   `03.mp4`           → `03`（去掉序号后什么都不剩，那就留着）
 * 不做 `_` → 空格之类的"美化"：中文文件名里下划线往往是有意义的。
 */
export function titleFromFilename(name: string): string {
  const base = name.replace(/\.[^.]+$/, '')
  const stripped = base.replace(LEADING_ORDINAL, '').trim()
  return clampTitle(stripped || base.trim() || name)
}

/** title/description 在库里是 varchar(255)。按码点截断，别把 emoji 的代理对劈成两半 */
export function clampTitle(s: string, max = 255): string {
  const points = Array.from(s)
  return points.length <= max ? s : points.slice(0, max).join('')
}
