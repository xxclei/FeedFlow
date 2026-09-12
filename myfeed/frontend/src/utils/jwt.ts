export interface JwtPayload {
  account_id: number
  username: string
  exp?: number
}

// 前端解码 JWT 载荷只为了"显示你是谁"（头像/昵称）。
// 不验签——验签是后端的事，前端读了也不能信，只作展示。
export function decodeJwtPayload(token: string): JwtPayload | null {
  try {
    const part = token.split('.')[1]
    const b64 = part.replace(/-/g, '+').replace(/_/g, '/')
    const json = decodeURIComponent(
      atob(b64)
        .split('')
        .map((c) => '%' + c.charCodeAt(0).toString(16).padStart(2, '0'))
        .join(''),
    )
    return JSON.parse(json) as JwtPayload
  } catch {
    return null
  }
}
