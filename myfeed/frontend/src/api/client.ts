// 统一的 API 请求封装（基于 fetch，不用 axios——原项目就是 fetch）
// 职责：自动带 token、401 时用 refreshToken 自动续期并重试一次、统一错误对象、可取消

import { useAuthStore } from '../stores/auth'
import { abortError, isAbort } from '../utils/retry'

export class ApiError extends Error {
  status: number
  payload?: unknown

  constructor(message: string, status: number, payload?: unknown) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.payload = payload
  }
}

type ApiErrorBody = { error?: string }

export interface RequestOptions {
  authRequired?: boolean
  /** 取消上传/请求。abort 时抛 DOMException('AbortError')，不是 ApiError */
  signal?: AbortSignal
}

// 走 vite proxy：浏览器请求 /api/xxx → vite 转发到 127.0.0.1:8080/xxx
const API_BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api'

let isRefreshing = false
let refreshPromise: Promise<string | null> | null = null

// 用 refreshToken 换新 accessToken。
// isRefreshing/refreshPromise：并发多个 401 时只发一次 refresh，大家共享同一个 Promise（"单飞"）
//
// **导出**是给 notification.ts 用的：SSE 那条长连接不走 fetch，401 只能靠
// onerror 发现，所以它需要自己触发一次续期。让它复用这一份而不是抄一份 ——
// 同后端 jwt.QueryTokenAuth 那条纪律：**鉴权逻辑不能有第二份实现**，
// 抄一份的下场是两边对"什么算撤销"的判断慢慢分叉。
export async function tryRefresh(): Promise<string | null> {
  const auth = useAuthStore()
  if (!auth.refreshToken) return null
  if (isRefreshing) return refreshPromise
  isRefreshing = true
  refreshPromise = (async () => {
    try {
      const res = await fetch(`${API_BASE}/account/refresh`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ refresh_token: auth.refreshToken }),
      })
      if (!res.ok) {
        auth.clearTokens()
        return null
      }
      const data = await res.json()
      auth.setToken(data.token)
      return data.token as string
    } catch {
      auth.clearTokens()
      return null
    } finally {
      isRefreshing = false
    }
  })()
  return refreshPromise
}

function buildHeaders(token: string | null, hasBody: boolean): Record<string, string> {
  const headers: Record<string, string> = {}
  if (hasBody) headers['Content-Type'] = 'application/json'
  if (token) headers.Authorization = `Bearer ${token}`
  return headers
}

/** fetch 的网络异常是 TypeError，先归一化成 ApiError(0)，重试逻辑就只需要看一个字段 */
async function safeFetch(url: string, init: RequestInit): Promise<Response> {
  try {
    return await fetch(url, init)
  } catch (e) {
    if (isAbort(e)) throw e // 用户主动取消，不是故障
    throw new ApiError('网络不可达，请检查后端是否在跑', 0, e)
  }
}

/**
 * 401 → 单飞续期 → 用新 token 重放。
 * fetch 和 XHR 两条路都要这套逻辑，所以抽出来共用；`send` 只负责「用这个 token 发一次」。
 */
async function withAuthRetry<T>(
  path: string,
  token: string | null,
  send: (token: string | null) => Promise<Response>,
): Promise<T> {
  let res = await send(token)
  if (res.status === 401 && path !== '/account/refresh') {
    const fresh = await tryRefresh()
    if (fresh) res = await send(fresh)
  }
  return handleResponse<T>(res)
}

export function postJson<T>(path: string, body: unknown, options: RequestOptions = {}): Promise<T> {
  const auth = useAuthStore()
  const token = auth.token

  if (options.authRequired && !token) {
    return Promise.reject(new ApiError('需要先登录（缺少 token）', 401))
  }

  const payload = JSON.stringify(body ?? {})

  return withAuthRetry<T>(path, token, (tk) =>
    safeFetch(`${API_BASE}${path}`, {
      method: 'POST',
      headers: buildHeaders(tk, true),
      body: payload,
      signal: options.signal,
    }),
  )
}

// 文件上传用（头像/视频）：body 是 FormData，Content-Type 让浏览器自动带 boundary
export function postForm<T>(path: string, body: FormData, options: RequestOptions = {}): Promise<T> {
  const auth = useAuthStore()
  const token = auth.token

  if (options.authRequired && !token) {
    return Promise.reject(new ApiError('需要先登录（缺少 token）', 401))
  }

  return withAuthRetry<T>(path, token, (tk) =>
    safeFetch(`${API_BASE}${path}`, {
      method: 'POST',
      headers: buildHeaders(tk, false),
      body,
      signal: options.signal,
    }),
  )
}

/**
 * 带**字节级上传进度**的 FormData POST，走 XHR。
 *
 * 为什么不继续用 fetch：fetch 无法上报请求体发送进度（`ReadableStream` body +
 * `duplex: 'half'` 既过不了 vite 代理，Safari/Firefox 也不支持）。分片粒度上报的话，
 * 慢网络下一个 5~20MB 的分片 POST 会让进度条卡住好几秒 —— 那正是我们要避免的"转圈感"。
 */
export function postFormXHR<T>(
  path: string,
  body: FormData,
  options: RequestOptions & { onUploadProgress?: (sent: number, total: number) => void } = {},
): Promise<T> {
  const auth = useAuthStore()
  const token = auth.token

  if (options.authRequired && !token) {
    return Promise.reject(new ApiError('需要先登录（缺少 token）', 401))
  }

  const send = (tk: string | null) => xhrSend(path, body, tk, options)

  return withAuthRetry<T>(path, token, send)
}

function xhrSend(
  path: string,
  body: FormData,
  token: string | null,
  options: RequestOptions & { onUploadProgress?: (sent: number, total: number) => void },
): Promise<Response> {
  return new Promise<Response>((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    xhr.open('POST', `${API_BASE}${path}`)
    if (token) xhr.setRequestHeader('Authorization', `Bearer ${token}`)

    if (options.onUploadProgress) {
      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable) options.onUploadProgress?.(e.loaded, e.total)
      }
    }

    xhr.onload = () => {
      // 204 不允许带 body，构造 Response 时会抛 RangeError
      const text = xhr.responseText
      resolve(
        new Response(text ? text : null, { status: xhr.status, statusText: xhr.statusText }),
      )
    }
    xhr.onerror = () => reject(new ApiError('网络不可达，请检查后端是否在跑', 0))
    xhr.ontimeout = () => reject(new ApiError('请求超时', 408))
    xhr.onabort = () => reject(abortError())

    if (options.signal) {
      if (options.signal.aborted) {
        xhr.abort()
        return
      }
      options.signal.addEventListener('abort', () => xhr.abort(), { once: true })
    }

    xhr.send(body)
  })
}

async function handleResponse<T>(res: Response): Promise<T> {
  const auth = useAuthStore()
  const text = await res.text()
  let data: unknown = null
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = text
    }
  }

  if (!res.ok) {
    if (res.status === 401) auth.clearTokens() // 确认无效，清凭证回登录页
    const msg =
      data && typeof data === 'object' && (data as ApiErrorBody).error
        ? String((data as ApiErrorBody).error)
        : `请求失败 (${res.status})`
    throw new ApiError(msg, res.status, data)
  }

  return data as T
}
