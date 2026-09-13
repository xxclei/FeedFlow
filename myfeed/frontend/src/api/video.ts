// 视频模块的 API

import { postForm, postFormXHR, postJson, type RequestOptions } from './client'
import type { FeedVideoItem } from './feed'
import { chunkSizeFor } from '../utils/hash'
import { isoToUnixSeconds } from '../utils/time'

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

/**
 * 直传（小文件）。
 *
 * 批量上传走的是分片路径，不再调它 —— 但保留导出，因为它是理解"没有断点续传、
 * 没有逐片校验"这条基线的对照实现：服务端完全信任传输结果，只校验扩展名和大小。
 * 分片路径才是需要完整性保证时的答案。
 */
export function uploadVideo(file: File, options: RequestOptions = {}) {
  const fd = new FormData()
  fd.append('file', file)
  return postForm<{ url: string; play_url: string }>('/video/uploadVideo', fd, {
    authRequired: true,
    ...options,
  })
}

export function uploadCover(file: File, options: RequestOptions = {}) {
  const fd = new FormData()
  fd.append('file', file)
  return postForm<{ url: string; cover_url: string }>('/video/uploadCover', fd, {
    authRequired: true,
    ...options,
  })
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

/**
 * 公开接口（没挂 JWT 中间件），游客也能看详情页。
 *
 * `id` 必须是**数字**。后端是 `ID uint`、而且没有 `binding:"required"`：
 * 把路由参数（字符串）原样发过去的话反序列化就失败了，`ShouldBindJSON` 报错 →
 * 归类成 **500**（不是 404），页面上会挂一条 Go 的英文报错。所以调用方先转数字。
 */
export function getDetail(id: number) {
  return postJson<VideoItem>('/video/getDetail', { id })
}

/**
 * `/video/*` 的返回形状 → `/feed/*` 的返回形状。
 *
 * 同一个视频，两组接口给的是两套字段：
 *
 * |            | /video/getDetail、listByAuthorID | /feed/*            |
 * |------------|----------------------------------|--------------------|
 * | 作者        | `username` 扁平字符串 + author_id  | `author: {id, name}` |
 * | create_time | RFC3339 字符串                    | Unix 秒             |
 * | 额外        | `popularity`                      | `is_liked`          |
 *
 * 归一放在 **API 边界**，下游（卡片、相关推荐列表）就只需要认识一种形状，
 * 不用到处写 typeof 分支去猜这个字段到底是秒还是字符串。
 *
 * 注意两点：
 *   1. `username` 是 videos 表里的**快照**（发视频时抄下来的），改名后不会更新。
 *      所以展示作者名要以 /account/findByID 的实时结果为准，别用这个字段 ——
 *      否则同一个页面上"标题旁的作者"和"作者卡里的作者"会是两个名字。
 *   2. `is_liked: false` 不算撒谎：`/feed/*` 今天也是硬编码 false（阶段4 回填）。
 */
export function normalizeVideo(v: VideoItem): FeedVideoItem {
  return {
    id: v.id,
    author: { id: v.author_id, username: v.username },
    title: v.title,
    description: v.description,
    play_url: v.play_url,
    cover_url: v.cover_url,
    create_time: isoToUnixSeconds(v.create_time),
    likes_count: v.likes_count,
    is_liked: false,
  }
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
//
// 四个端点是一条会话流水线：init 开会话 → upload × N → complete 收尾并销毁会话。
// status 用来对账（比如 complete 的响应丢了、想确认服务端到底收了多少片）。

export interface InitChunkResult {
  upload_id: string
  uploaded_chunks: number[]
}

export interface ChunkStatusResult {
  upload_id: string
  uploaded_chunks: number[]
  total_chunks: number
}

export function initChunk(
  filename: string,
  fileSize: number,
  totalChunks: number,
  fileHash: string,
) {
  return postJson<InitChunkResult>(
    '/video/chunk/init',
    {
      filename,
      file_size: fileSize,
      // 服务端**会**校验这个值了：断言 total_chunks == ceil(file_size / chunk_size)，
      // 并把每片的落盘偏移算成 index * chunk_size。声明错了不是"拼歪"而是"写歪"
      chunk_size: chunkSizeFor(fileSize),
      total_chunks: totalChunks,
      // 后端只把它当会话索引键（(accountID, file_hash) → upload_id），**从不校验它等于内容 MD5**。
      // 会话损坏时靠给这个值加盐强制开新会话，见 utils/chunkUploader.ts
      file_hash: fileHash,
    },
    { authRequired: true },
  )
}

export function uploadChunk(
  uploadID: string,
  chunkIndex: number,
  chunkHash: string,
  blob: Blob,
  options: RequestOptions & { onUploadProgress?: (sent: number, total: number) => void } = {},
) {
  const fd = new FormData()
  fd.append('upload_id', uploadID) // 后端用 form tag 绑定，multipart 字段即可
  fd.append('chunk_index', String(chunkIndex))
  fd.append('chunk_hash', chunkHash)
  fd.append('file', blob)
  // 只有分片走 XHR：它需要真实的上传字节进度
  return postFormXHR<{ chunk_index: number }>('/video/chunk/upload', fd, {
    authRequired: true,
    ...options,
  })
}

export function completeChunk(uploadID: string) {
  return postJson<{ url: string; play_url: string }>(
    '/video/chunk/complete',
    { upload_id: uploadID },
    { authRequired: true },
  )
}

export function chunkStatus(uploadID: string) {
  return postJson<ChunkStatusResult>(
    '/video/chunk/status',
    { upload_id: uploadID },
    { authRequired: true },
  )
}
