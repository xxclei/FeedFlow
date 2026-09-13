// 标签解析。规则和后端 video.ExtractTags 必须一致：
// # 后面跟 字母/数字/下划线（含中文，靠 Unicode 属性 \p{L}）
//
// **这个正则导出，不许再抄第二份。** 它曾经在 tags.ts 和 TagInput.vue 里
// 各有一份镜像（后端 Go 那份按语言边界留着）。两份镜像的代价不是多打几个字，
// 而是改一处漏一处时**两边都还能跑**，只是行为悄悄分叉：
// 比如给标签名加上 "-" 支持，只改了 TagInput，于是输入框高亮出 3 个标签、
// 后端只落库 2 个 —— 没有任何报错，只是"标签对不上"。
export const TAG_RE = /#([\p{L}\p{N}_]+)/gu

/** 标签名长度上限，对应后端 video.maxTagNameRunes（tags.name 是 varchar(100)） */
export const MAX_TAG_NAME_RUNES = 100

/**
 * 规范化单个标签：去首尾空白 → 剥掉前导 # → 按 rune 截断。返回空串表示它不该成为标签。
 *
 * 和后端 normalizeTagNames 的每一步一一对应：
 *   strings.TrimSpace / strings.TrimLeft("#") / []rune 截断 / 去空
 */
export function normalizeTagName(raw: string): string {
  let name = raw.trim()
  // 只剥**前导**的 #（一个或多个），和后端的 strings.TrimLeft(name, "#") 一致
  name = name.replace(/^#+/, '').trim()
  if (!name) return ''

  // 按 rune 数截断，不是 name.slice(0, 100)：
  // 中文一个字是 1 个 rune、3 个 UTF-16 码元，slice 会把它拦腰截断成半个字符，
  // 而后端是按 rune 截的 —— 两边不一致时前端显示的 chip 和后端存的名字会对不上
  const runes = Array.from(name)
  return runes.length > MAX_TAG_NAME_RUNES
    ? runes.slice(0, MAX_TAG_NAME_RUNES).join('')
    : name
}

/**
 * 规范化一串标签：剥 #、截断、丢空、**按小写去重（保序）**。
 *
 * 去重要按小写，是后端那段最容易被忽略的一条：tags.name 在 MySQL 里是
 * utf8mb4_unicode_ci 排序规则，**"Go" 和 "go" 是同一个标签**。
 * 前端不去重的话用户会看到两个 chip（还以为设了两个标签），后端只落一个。
 */
export function normalizeTagNames(names: readonly string[]): string[] {
  const seen = new Set<string>()
  const out: string[] = []
  for (const raw of names) {
    const name = normalizeTagName(raw)
    if (!name) continue
    const key = name.toLowerCase()
    if (seen.has(key)) continue
    seen.add(key)
    out.push(name)
  }
  return out
}

/**
 * 从文本里解析 #标签（和后端 video.ExtractTags 一致：去重保序）。
 *
 * **它刻意不过 normalizeTagNames**，因为两者的用途不同：
 * 这个函数是**显示用**的解析器，回答"这段文字里出现了哪些 #标签"，
 * 所以在 VideoCard / VideoDetailView 上渲染的是**用户写下的原文**。
 * normalizeTagNames 是**写入用**的规范化器，会剥 #、截断、按小写合并 ——
 * 那是落库前的加工，拿它去处理展示文本会让页面上的 chip 和正文字对不上
 * （比如正文里一个超长 #标签，chip 被截短了，正文没变）。
 *
 * 代价是：用户在描述里同时写了 #Go 和 #go，这里会渲染成两个 chip，
 * 而库里其实是同一个标签。这是显示层的近似，不是数据错误。
 */
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
