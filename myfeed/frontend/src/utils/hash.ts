// 文件指纹：一次顺序读，同时算出「整文件 MD5」和「每片 MD5」
//
// 为什么要一次读完两样？
//   整文件 MD5 是后端断点续传会话的索引键（init 传 file_hash）；
//   每片 MD5 是后端逐片校验完整性的凭据（upload 传 chunk_hash）。
// 如果分两趟读，一个 500MB 的文件就要从磁盘读两遍，重试时还要再读。

import SparkMD5 from 'spark-md5'

/**
 * 分片大小按文件大小分档（自适应）。
 *
 * 调大买到什么：更少的 HTTP 往返（请求头/握手开销摊到更多字节上）、更小的位图、
 * 更少的 MD5 调用次数、更少的 WriteAt 系统调用。
 *
 * 代价是什么：
 *   - 断点续传的粒度变粗 —— 传了 19MB 断线，那 19MB 全废（5MB 分片只废 4MB）
 *   - 单片重试的代价变大（重传一次就是 20MB）
 *   - 并发在途内存 = chunk_size × 并发数，分档调大等于把峰值也调大
 * 所以这是权衡，不是无脑越大越好。小文件本来就一片传完，给它 20MB 纯属浪费。
 *
 * 分档必须和后端的几何校验一起看：后端现在会断言
 * `total_chunks == ceil(file_size / chunk_size)`，落盘偏移就是 `index * chunk_size`，
 * 声明错了会直接把文件写歪（不是拼歪——没有"拼"这一步了）。
 */
const TIERS: ReadonlyArray<{ upTo: number; chunk: number }> = [
  { upTo: 50 * 1024 * 1024, chunk: 5 * 1024 * 1024 }, // < 50MB  → 5MB
  { upTo: 200 * 1024 * 1024, chunk: 10 * 1024 * 1024 }, // < 200MB → 10MB
  { upTo: Infinity, chunk: 20 * 1024 * 1024 }, //          ≥ 200MB → 20MB
]

/**
 * 分片大小**只由文件大小决定**，是个纯函数。
 *
 * 这是刻意的：只要不把 chunk size 当参数传来传去，就不可能出现
 * "指纹按 5MB 切、上传按 20MB 切"这种自相矛盾。所有分片运算都从这里取尺子。
 */
export function chunkSizeFor(fileSize: number): number {
  return (TIERS.find((t) => fileSize < t.upTo) ?? TIERS[TIERS.length - 1]).chunk
}

/** 第 index 片的字节数（最后一片可能不满） */
export function chunkSizeAt(fileSize: number, index: number): number {
  const cs = chunkSizeFor(fileSize)
  return Math.min(cs, fileSize - index * cs)
}

export function chunkCount(fileSize: number): number {
  return Math.max(1, Math.ceil(fileSize / chunkSizeFor(fileSize)))
}

export interface FileFingerprint {
  fileHash: string
  /** chunkHashes[i] 对应字节区间 [i*cs, (i+1)*cs)，cs = chunkSizeFor(fileSize) */
  chunkHashes: string[]
}

/**
 * 顺序扫描文件，产出全部指纹。
 *
 * 内存峰值只有一片（≤20MB）：外层用增量哈希累积整文件摘要，内层用同一个 ArrayBuffer
 * 顺手算出这一片的摘要。**不要把分片 Blob 缓存起来**——那是每个在传文件 500MB 的常驻内存。
 */
export async function fingerprintFile(
  file: File,
  onProgress?: (done: number, total: number) => void,
  signal?: AbortSignal,
): Promise<FileFingerprint> {
  const outer = new SparkMD5.ArrayBuffer()
  const chunkHashes: string[] = []
  const cs = chunkSizeFor(file.size) // 整趟只解析一次档位
  const total = chunkCount(file.size)

  for (let i = 0; i < total; i++) {
    if (signal?.aborted) throw new DOMException('aborted', 'AbortError')
    const start = i * cs
    const buf = await file.slice(start, Math.min(start + cs, file.size)).arrayBuffer()
    outer.append(buf)

    const inner = new SparkMD5.ArrayBuffer()
    inner.append(buf)
    chunkHashes.push(inner.end())

    onProgress?.(i + 1, total)
  }

  return { fileHash: outer.end(), chunkHashes }
}
