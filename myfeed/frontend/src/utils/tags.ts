// 标签解析。规则和后端 video.ExtractTags 必须一致：
// # 后面跟 字母/数字/下划线（含中文，靠 Unicode 属性 \p{L}）
const TAG_RE = /#([\p{L}\p{N}_]+)/gu

export function extractTags(...texts: (string | undefined)[]): string[] {
  const out: string[] = []
  for (const text of texts) {
    if (!text) continue
    for (const m of text.matchAll(TAG_RE)) {
      const name = m[1]
      if (!out.includes(name)) out.push(name)
    }
  }
  return out
}
