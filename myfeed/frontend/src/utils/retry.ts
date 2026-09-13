// 失败分级 + 指数退避重试

import { ApiError } from '../api/client'

export function abortError(): DOMException {
  return new DOMException('aborted', 'AbortError')
}

export function isAbort(e: unknown): boolean {
  return e instanceof DOMException && e.name === 'AbortError'
}

/**
 * 这个错误值不值得重试？
 *
 * 只判「是不是瞬时的」，不判「业务上对不对」——业务错误（分片哈希不匹配、
 * 会话不存在）由调用方单独处理，因为它们的补救动作不是「再试一次」。
 */
export function isRetryable(e: unknown): boolean {
  if (isAbort(e)) return false
  if (e instanceof ApiError) {
    if (e.status === 0) return true // 网络不可达（fetch 的 TypeError 已被归一化成 status 0）
    return e.status === 408 || e.status === 429 || e.status >= 500
  }
  return true // 未知异常，当作瞬时故障
}

/**
 * 500ms · 2^(n-1) ±25% 抖动 → 约 500 / 1000 / 2000ms，封顶 8s。
 *
 * 抖动是必要的：分片并发是 3~4 路，后端一重启这 4 个请求会同时失败，
 * 没有抖动它们就会同时重试、再次撞在一起。
 */
export function backoffMs(attempt: number): number {
  const base = Math.min(500 * 2 ** (attempt - 1), 8000)
  return Math.round(base * (0.75 + Math.random() * 0.5))
}

/** 可被 signal 打断的 sleep —— 取消上传时不能让退避把取消卡住好几秒 */
export function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(abortError())
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    const onAbort = () => {
      clearTimeout(timer)
      reject(abortError())
    }
    signal?.addEventListener('abort', onAbort, { once: true })
  })
}

export interface RetryOptions {
  attempts?: number
  signal?: AbortSignal
  /** 每次重试前回调（用来把进度回退、打印日志） */
  onRetry?: (attempt: number, error: unknown, delayMs: number) => void
  /** 覆盖默认的「该不该重试」判断 */
  retryable?: (e: unknown) => boolean
}

export async function withRetry<T>(fn: () => Promise<T>, opts: RetryOptions = {}): Promise<T> {
  const { attempts = 3, signal, onRetry, retryable = isRetryable } = opts

  for (let attempt = 1; ; attempt++) {
    try {
      return await fn()
    } catch (e) {
      if (attempt >= attempts || !retryable(e)) throw e
      const delay = backoffMs(attempt)
      onRetry?.(attempt, e, delay)
      await sleep(delay, signal)
    }
  }
}
