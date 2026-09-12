// 统一的 API 请求封装（基于 fetch，不用 axios——原项目就是 fetch）
// 职责：自动带 token、401 时用 refreshToken 自动续期并重试一次、统一错误对象

import { useAuthStore } from '../stores/auth'

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

// 走 vite proxy：浏览器请求 /api/xxx → vite 转发到 127.0.0.1:8080/xxx
const API_BASE = (import.meta.env.VITE_API_BASE as string | undefined) ?? '/api'

let isRefreshing = false
let refreshPromise: Promise<string | null> | null = null

// 用 refreshToken 换新 accessToken。
// isRefreshing/refreshPromise：并发多个 401 时只发一次 refresh，大家共享同一个 Promise（"单飞"）
async function tryRefresh(): Promise<string | null> {
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

export async function postJson<T>(path: string, body: unknown, options?: { authRequired?: boolean }): Promise<T> {
  const auth = useAuthStore()
  const token = auth.token

  if (options?.authRequired && !token) {
    throw new ApiError('需要先登录（缺少 token）', 401)
  }

  const headers = buildHeaders(token, true)
  const payload = JSON.stringify(body ?? {})

  let res = await fetch(`${API_BASE}${path}`, { method: 'POST', headers, body: payload })

  // token 过期（401）→ 自动续期 → 用新 token 重放原请求
  if (res.status === 401 && path !== '/account/refresh') {
    const newToken = await tryRefresh()
    if (newToken) {
      headers.Authorization = `Bearer ${newToken}`
      res = await fetch(`${API_BASE}${path}`, { method: 'POST', headers, body: payload })
    }
  }

  return handleResponse<T>(res)
}

// 文件上传用（头像/视频）：body 是 FormData，Content-Type 让浏览器自动带 boundary
export async function postForm<T>(path: string, body: FormData, options?: { authRequired?: boolean }): Promise<T> {
  const auth = useAuthStore()
  const token = auth.token

  if (options?.authRequired && !token) {
    throw new ApiError('需要先登录（缺少 token）', 401)
  }

  const headers = buildHeaders(token, false)

  let res = await fetch(`${API_BASE}${path}`, { method: 'POST', headers, body })

  if (res.status === 401 && path !== '/account/refresh') {
    const newToken = await tryRefresh()
    if (newToken) {
      headers.Authorization = `Bearer ${newToken}`
      res = await fetch(`${API_BASE}${path}`, { method: 'POST', headers, body })
    }
  }

  return handleResponse<T>(res)
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
