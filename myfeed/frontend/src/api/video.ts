// 视频模块的 API

import SparkMD5 from 'spark-md5'

import { postForm, postJson } from './client'

const CHUNK_SIZE = 5 * 1024 * 1024 // 5MB，和后端 ChunkSize 约定一致

export interface VideoItem {
  id: number
  author_id: number
  username: string
  title: string
  description?: string
  play_url: string
  cover_url: string
  create_time: string
  likes_count: number
  popularity: number
}

export function uploadVideo(file: File) {
  const fd = new FormData()
  fd.append('file', file)
  return postForm<{ play_url: string }>('/video/uploadVideo', fd, { authRequired: true })
}

export function uploadCover(file: File) {
  const fd = new FormData()
  fd.append('file', file)
  return postForm<{ cover_url: string }>('/video/uploadCover', fd, { authRequired: true })
}

export function publish(payload: {
  title: string
  description: string
  play_url: string
  cover_url: string
}) {
  return postJson<VideoItem>('/video/publish', payload, { authRequired: true })
}

export function listByAuthorID(authorID: number) {
  return postJson<VideoItem[]>('/video/listByAuthorID', { author_id: authorID })
}

export function deleteVideo(id: number) {
  return postJson<{ message: string }>('/video/delete', { id }, { authRequired: true })
}

// 后端返回的是绝对地址 http://localhost:8080/static/...，
// 换成同源相对路径（经 vite 代理），播放/画布截帧都不受跨域影响
export function staticURL(url: string): string {
  try {
    const u = new URL(url, window.location.origin)
    return u.pathname
  } catch {
    return url
  }
}

// ---------- 分片上传 ----------

export interface InitChunkResult {
  upload_id: string
  uploaded_chunks: number[]
}

export function initChunk(
  filename: string,
  fileSize: number,
  totalChunks: number,
  fileHash: string,
) {
  return postJson<InitChunkResult>('/video/chunk/init', {
    filename,
    file_size: fileSize,
    chunk_size: CHUNK_SIZE,
    total_chunks: totalChunks,
    file_hash: fileHash,
  }, { authRequired: true })
}

export function uploadChunk(uploadID: string, chunkIndex: number, chunkHash: string, blob: Blob) {
  const fd = new FormData()
  fd.append('upload_id', uploadID) // 后端用 form tag 绑定，multipart 字段即可
  fd.append('chunk_index', String(chunkIndex))
  fd.append('chunk_hash', chunkHash)
  fd.append('file', blob)
  return postForm<{ chunk_index: number }>('/video/chunk/upload', fd, { authRequired: true })
}

export function completeChunk(uploadID: string) {
  return postJson<{ play_url: string }>('/video/chunk/complete', { upload_id: uploadID }, { authRequired: true })
}

// ---------- 指纹计算（spark-md5 增量算法，内存里永远只有当前一片） ----------

export async function md5Blob(blob: Blob): Promise<string> {
  const spark = new SparkMD5.ArrayBuffer()
  spark.append(await blob.arrayBuffer())
  return spark.end()
}

export async function md5File(file: File, onProgress?: (done: number, total: number) => void): Promise<string> {
  const spark = new SparkMD5.ArrayBuffer()
  let done = 0
  for (let off = 0; off < file.size; off += CHUNK_SIZE) {
    spark.append(await file.slice(off, Math.min(off + CHUNK_SIZE, file.size)).arrayBuffer())
    done++
    onProgress?.(done, Math.ceil(file.size / CHUNK_SIZE))
  }
  return spark.end()
}

// 自动策略：小文件直传，大文件自动切片+断点续传（调用方无感）
export async function uploadVideoAuto(
  file: File,
  onProgress?: (msg: string) => void,
): Promise<string> {
  if (file.size <= CHUNK_SIZE) {
    onProgress?.('直传中…')
    return (await uploadVideo(file)).play_url
  }

  onProgress?.('计算文件指纹…')
  const fileHash = await md5File(file)
  const total = Math.ceil(file.size / CHUNK_SIZE)

  onProgress?.('初始化分片会话…')
  const init = await initChunk(file.name, file.size, total, fileHash)
  const uploaded = new Set(init.uploaded_chunks) // 断点续传：服务端记得的片直接跳过

  for (let i = 0; i < total; i++) {
    if (uploaded.has(i)) continue
    onProgress?.(`上传分片 ${i + 1}/${total}`)
    const blob = file.slice(i * CHUNK_SIZE, Math.min((i + 1) * CHUNK_SIZE, file.size))
    const chunkHash = await md5Blob(blob)
    await uploadChunk(init.upload_id, i, chunkHash, blob)
  }

  onProgress?.('合并分片…')
  const done = await completeChunk(init.upload_id)
  return done.play_url
}

export { CHUNK_SIZE }
