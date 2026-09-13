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
  /**
   * 结构化标签（批量投稿的 chip 编辑器产出）。**和描述里手写的 #xxx 取并集**，
   * 不是二选一 —— 所以老路径不传它时行为一个字不变。
   *
   * 它比"往描述里写 #标签"可靠的地方：不带 # 也能成标签（不用记语法）、
   * 按 rune 截到 100（不会撑爆 varchar(100) 变成 500）、按小写去重
   * （tags.name 是 _ci 排序规则，"Go" 和 "go" 是同一个标签）。
   */
  tag_names?: string[]
}) {
  return postJson<VideoItem>('/video/publish', payload, { authRequired: true })
}

export function listByAuthorID(authorID: number) {
  return postJson<VideoItem[]>('/video/listByAuthorID', { author_id: authorID })
}

export function deleteVideo(id: number) {
  return postJson<{ message: string }>('/video/delete', { id }, { authRequired: true })
}

/** 批量删除的响应：**删除成功和部分成功要分开报**（见下面 deleteVideoBatch） */
export interface DeleteBatchResult {
  deleted: number
  deleted_ids: number[]
  /** 没能删掉的 id：不是你的，或者已经不存在了。后端无法区分这两者，也不该区分 */
  skipped_ids: number[]
}

/**
 * 批量删除。
 *
 * ---------- 为什么响应里要有 skipped_ids ----------
 *
 * "3 条里删掉 2 条"是**正常结果**，不是错误：勾选列表可能是旧的（另一个标签页
 * 先删过了，或者勾的时候还在、提交时已经没了）。所以这个接口不会因为"有一条删不掉"
 * 而整体失败 —— 那种语义更难用：一次手滑多勾一条就整批白删。
 *
 * 代价是**前端必须如实转达**：skipped_ids 非空时不能笼统报"删除成功"，
 * 否则用户以为 3 条都没了，回头看见一条还在会觉得见了鬼。
 *
 * 上限 100 条（后端定的，管的是请求体大小和事务持有时间，不是业务规则）。
 */
export function deleteVideoBatch(ids: number[]) {
  return postJson<DeleteBatchResult>(
    '/video/deleteBatch',
    { ids },
    { authRequired: true },
  )
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
 *   2. 这里填 `is_liked: false` **不是**默认值偷懒：videos 表里根本没有"我赞过没有"
 *      这个信息（它是 likes 表和"我"的交叉，不属于视频），`/video/*` 也就返回不了。
 *      要真实状态只能另外调 `/like/isLiked` —— 详情页正是这么做的。
 *      注意这已经和 `/feed/*` 不同了：那边阶段4 起返回的是真值。
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
