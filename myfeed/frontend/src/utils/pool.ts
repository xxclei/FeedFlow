// 并发闸门

import { abortError } from './retry'

interface Waiter {
  resolve: () => void
  reject: (e: unknown) => void
  signal?: AbortSignal
  onAbort?: () => void
}

/**
 * 可取消的 FIFO 信号量。
 *
 * 唯一容易写错的地方：**在排队时被取消，不能吞掉名额**。
 * 如果 abort 时只是 reject 而不把 waiter 从队列里摘掉，release() 就会把这个
 * 名额「交接」给一个已经死掉的 waiter —— 名额泄漏，整个批量上传会慢慢停住。
 */
export class Semaphore {
  private free: number
  private queue: Waiter[] = []

  constructor(permits: number) {
    this.free = permits
  }

  acquire(signal?: AbortSignal): Promise<void> {
    if (signal?.aborted) return Promise.reject(abortError())

    if (this.free > 0) {
      this.free--
      return Promise.resolve()
    }

    return new Promise<void>((resolve, reject) => {
      const waiter: Waiter = { resolve, reject, signal }

      if (signal) {
        waiter.onAbort = () => {
          const i = this.queue.indexOf(waiter)
          if (i >= 0) this.queue.splice(i, 1) // 摘掉自己，别让名额漏给死人
          reject(abortError())
        }
        signal.addEventListener('abort', waiter.onAbort, { once: true })
      }

      this.queue.push(waiter)
    })
  }

  release(): void {
    const next = this.queue.shift()
    if (!next) {
      this.free++
      return
    }
    // 名额直接转交给队首，free 保持不变
    if (next.signal && next.onAbort) next.signal.removeEventListener('abort', next.onAbort)
    next.resolve()
  }
}
