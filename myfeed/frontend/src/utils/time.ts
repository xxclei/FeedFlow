// 时间格式化。
// 注意：Feed 卡片上的 create_time 是 Unix **秒**（后端 buildFeedVideos 用的是 .Unix()），
// 而翻页游标 next_time 是 **毫秒**（.UnixMilli()）。两个单位，两处换算，别搞混。

export function formatTime(sec: number): string {
  if (!sec) return '—'
  const d = new Date(sec * 1000)
  const diff = Date.now() - d.getTime()

  if (diff < 0) return d.toLocaleString('zh-CN') // 时钟漂移，直接显原始时间

  const min = Math.floor(diff / 60_000)
  if (min < 1) return '刚刚'
  if (min < 60) return `${min} 分钟前`

  const hour = Math.floor(min / 60)
  if (hour < 24) return `${hour} 小时前`

  const day = Math.floor(hour / 24)
  if (day < 30) return `${day} 天前`

  return d.toLocaleDateString('zh-CN')
}

// 完整时间，给 title 属性用
export function fullTime(sec: number): string {
  if (!sec) return ''
  return new Date(sec * 1000).toLocaleString('zh-CN')
}

/**
 * RFC3339 字符串 → Unix **秒**。
 *
 * 第三套时间形状：`/feed/*` 给的是 `.Unix()` 秒，`/video/*`（getDetail、listByAuthorID）
 * 给的是 `time.Time` 直接序列化出来的 RFC3339 字符串（"2026-09-13T10:30:00+08:00"），
 * 而热门榜的游标 `next_latest_before` 又是另一种用法（原样回传，不能自己解析再格式化）。
 * formatTime/fullTime 只认秒，所以 `/video/*` 的返回值必须先过这里。
 *
 * 解析不出来返回 0：formatTime 对 0 显示 "—"，比显示 "Invalid Date" 体面。
 */
export function isoToUnixSeconds(iso: string): number {
  const ms = Date.parse(iso)
  return Number.isFinite(ms) ? Math.floor(ms / 1000) : 0
}
