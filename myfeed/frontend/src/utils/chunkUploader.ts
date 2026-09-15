// 一个文件从本地上传到「服务端已给出 play_url」的完整流程
//
// 后端的分片协议（internal/video/chunk_handler.go）：
//   init      → 开一个会话，按 (accountID, file_hash) 索引；同哈希会**复用**已有会话。
//               同时把最终文件预分配到 file_size，半成品叫 xxx.mp4.part
//   upload×N  → 每片单独 MD5 校验，然后**直接写到 index*chunk_size 的偏移上**
//   complete  → 全部到齐 → 把 .part 改名成 .mp4 → 销毁会话 → 返回 play_url
//
// 没有"服务端拼装"这一步：分片到达时就落在自己该在的位置上，complete 只是一次 rename。
// 所以分片乱序、并发都无所谓，也不会出现"传完了还要再等一次全量拷贝"的收尾停顿。
//
// 这套协议有几个会咬人的地方，代码里都标了 [坑N]。

import { ApiError } from '../api/client'
import { chunkCount, chunkSizeAt, chunkSizeFor } from './hash'
import type { Semaphore } from './pool'
import { abortError, isAbort, withRetry } from './retry'
import { completeChunk, initChunk, uploadChunk } from '../api/video'

/** 单个文件内部的分片并发。
 *  浏览器对同源的 HTTP/1.1 连接上限是 6，而我们还有 JSON 请求（发布/刷新）要挤进去，
 *  所以 3 是「能跑满上行又不让其它请求排队假死」的值。 */
export const MAX_CHUNKS_PER_FILE = 3

const MAX_ATTEMPTS = 3

export type Phase = 'initing' | 'uploading' | 'merging'

export interface UploadHooks {
  /** 全局分片闸门，由队列创建并跨文件共享：多文件并行时总在途请求仍受控 */
  gate: Semaphore
  signal?: AbortSignal
  onPhase?: (phase: Phase) => void
  /** 已发送字节数的**增量**（可正可负：重试会把这一片已计入的字节退回来） */
  onBytes?: (delta: number) => void
  /** 服务端说这些片它已经有了 → UI 把进度条直接推到断点位置 */
  onResume?: (bytes: number, chunks: number) => void
}

export interface UploadOutcome {
  playUrl: string
  /** 实际用于 init 的哈希。加过盐时和文件真实 MD5 不同，续传时要原样带回来 */
  activeHash: string
  uploadId: string
  resumedBytes: number
}

/** 会话不存在或已销毁（后端重启 / 被别人 complete 清掉）。重新 init 即可，不算故障 */
function isSessionGone(e: unknown): boolean {
  return e instanceof ApiError && e.status === 404
}

/**
 * [坑3] 会话可能**永久无法完成**：位图说这一片传过了（于是 upload 幂等短路直接回 200，
 * 再也不写盘），可磁盘上的文件却没了 —— 半成品 .part 被人删了、目录被清了、磁盘写失败过。
 * 此时 complete 永远失败，服务端不会自愈。
 *
 * 唯一的出路是换一个 upload_id，而 init 按 (accountID, file_hash) 复用会话，
 * 所以只能改 file_hash。好在服务端只把它当索引键，从不校验它等于真实内容 MD5。
 */
function isPoisoned(e: unknown): boolean {
  if (!(e instanceof ApiError)) return false
  if (e.status >= 500) return true // 收尾阶段文件丢了/改名失败，重试同一个会话没意义
  return typeof e.message === 'string' && e.message.includes('missing')
}

/**
 * 上传一个文件（批量上传和单文件上传共用这一条路径）。
 *
 * 不再为小文件走直传分支：直传省不下多少（分片路径对 ≤5MB 的文件本来就只有一片），
 * 却要额外维护一条没有断点续传、没有逐片校验的代码路径。统一走分片，
 * 换来的是「所有上传都可续传、都可校验、都能被 duplicate 检测合并」。
 */
export async function uploadOneFile(
  file: File,
  fileHash: string,
  chunkHashes: string[],
  hooks: UploadHooks,
): Promise<UploadOutcome> {
  const total = chunkCount(file.size)
  const { signal } = hooks

  let activeHash = fileHash
  let reinitPlain = 0 // 会话丢了，重开一个同名会话
  let reinitSalted = 0 // 会话坏了，加盐强制开新会话

  for (;;) {
    hooks.onPhase?.('initing')
    const init = await withRetry(() => initChunk(file.name, file.size, total, activeHash), {
      attempts: MAX_ATTEMPTS,
      signal,
    })

    // ⚠ `?? []` 看着像多余的防御，但它买到的是"最坏情况退化成重传一遍"，
    // 而不是"整个上传功能炸掉"。
    //
    // 契约上这个字段是列表，后端也已经在 UploadedChunks() 里补了
    // （Go 的 nil slice 会编成 null 而不是 []，2026-09-15 上云当天在这里崩过：
    //   Cannot read properties of null (reading 'reduce')）。
    // 留着兜底是因为这里**没有中间地带**：null 进来，reduce 抛异常，
    // 整个文件的上传当场死掉，而且报错信息（"reading 'reduce'"）指不到
    // 是哪个字段、更指不到后端 —— 排查成本远大于这一行。
    const uploadedChunks = init.uploaded_chunks ?? []
    const uploaded = new Set(uploadedChunks)
    const resumedBytes = uploadedChunks.reduce((n, i) => n + chunkSizeAt(file.size, i), 0)
    if (resumedBytes > 0) hooks.onResume?.(resumedBytes, uploadedChunks.length)

    const missing: number[] = []
    for (let i = 0; i < total; i++) if (!uploaded.has(i)) missing.push(i)

    try {
      if (missing.length > 0) {
        hooks.onPhase?.('uploading')
        await uploadChunks(file, init.upload_id, missing, chunkHashes, hooks)
      }

      hooks.onPhase?.('merging')
      // complete 的 4xx 是终局判断而不是瞬时故障，
      // isRetryable 对 4xx 本来就返回 false，所以这里不用额外写判断
      const done = await withRetry(() => completeChunk(init.upload_id), {
        attempts: MAX_ATTEMPTS,
        signal,
      })

      return { playUrl: done.play_url, activeHash, uploadId: init.upload_id, resumedBytes }
    } catch (e) {
      if (isAbort(e) || signal?.aborted) throw e

      if (isSessionGone(e) && reinitPlain < 1) {
        reinitPlain++
        continue
      }
      if (isPoisoned(e) && reinitSalted < 1) {
        // 只加一次盐：第二次还坏就说明不是会话的问题，报错让人去看后端
        reinitSalted++
        activeHash = `${fileHash}-r1`
        continue
      }
      throw e
    }
  }
}

/**
 * N 条泳道抢一个游标，每条泳道的每一片都要先过全局闸门。
 *
 * 为什么不是把 missing 切成 N 份分给泳道：某条泳道里有一片卡着重试时，
 * 那条泳道后面的活就全堵住了，别的泳道闲着也不顶用。游标是共享的，
 * 谁先拿到谁传，长短片自然均衡。
 */
async function uploadChunks(
  file: File,
  uploadID: string,
  missing: number[],
  chunkHashes: string[],
  hooks: UploadHooks,
) {
  const { gate } = hooks
  // 本文件的私有信号：任意一片彻底失败 → 掐掉同文件的其它泳道，别让它们空跑
  const ac = new AbortController()
  const onOuterAbort = () => ac.abort()
  hooks.signal?.addEventListener('abort', onOuterAbort, { once: true })

  const cs = chunkSizeFor(file.size) // 本文件统一的分片大小
  let cursor = 0

  const pump = async () => {
    for (;;) {
      if (ac.signal.aborted) throw abortError()
      const slot = cursor++
      if (slot >= missing.length) return

      const index = missing[slot]
      const start = index * cs
      const blob = file.slice(start, start + chunkSizeAt(file.size, index))

      // 这一片已经计入的字节数。重试时归零，避免进度被同一片推过 100%
      let counted = 0

      await gate.acquire(ac.signal)
      try {
        await withRetry(
          () =>
            uploadChunk(uploadID, index, chunkHashes[index], blob, {
              signal: ac.signal,
              onUploadProgress: (sent) => {
                if (sent > counted) {
                  hooks.onBytes?.(sent - counted)
                  counted = sent
                }
              },
            }),
          {
            attempts: MAX_ATTEMPTS,
            signal: ac.signal,
            onRetry: () => {
              // 服务端对已收下的片会幂等短路直接回 200，所以重发一片通常只花一个来回
              if (counted > 0) hooks.onBytes?.(-counted)
              counted = 0
            },
          },
        )
      } finally {
        gate.release()
      }
    }
  }

  try {
    const lanes = Math.min(MAX_CHUNKS_PER_FILE, missing.length)
    await Promise.all(Array.from({ length: lanes }, pump))
  } catch (e) {
    ac.abort()
    throw e
  } finally {
    hooks.signal?.removeEventListener('abort', onOuterAbort)
  }
}
