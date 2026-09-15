// 一个"任何环境下都能跑"的唯一 id。
//
// ---------- 为什么需要这个文件 ----------
//
// `crypto.randomUUID()` **只在安全上下文里存在**。
// 安全上下文 = `https://`、`localhost`、`127.0.0.1`、`file://`。
// 除此之外（包括 `http://192.168.x.x` 和 `http://<公网IP>`），
// `crypto` 这个对象**在**，但 `randomUUID` 这个方法**根本没挂上去**，
// 调用就是：
//
//     TypeError: crypto.randomUUID is not a function
//
// ---------- 这个项目已经因此炸过两次 ----------
//
//   ① useQoE 的 session_id（播放埋点）—— 当时就写了兜底，注释里那句
//      "有人用局域网 IP 访问的话整个播放器会因为一个埋点字段而崩"写得很准
//   ② 上传队列的 item.id（uploadQueue.ts）—— **这次忘了**，2026-09-15 上云时炸的
//
// 两次的报错都跟"网络"毫无关系，但都是**一换地址就出现**，
// 所以第一反应很容易跑去查 nginx 和后端（这次就查了半天）。
//
// 为什么上云才第一次真的炸：本地开发跑在 `localhost` 上，而 localhost
// 被浏览器**特批为安全上下文**。所以这一条在本机**永远测不出来** ——
// 它是"上云"这个动作本身带出来的，不是代码新引入的。
//
// ---------- 为什么要抽成一个文件 ----------
//
// 这条兜底逻辑本来是长在 useQoE.ts 里的。但"第三处漏了"这件事本身
// 说明了问题：**兜底写在每个调用点上，就是会漏**。一个文件写了、
// 另一个文件忘了，而忘了的那个只在非安全上下文里才暴露 ——
// 也就是只在线上暴露。
//
// 收到这里之后，可以随时跑这条检查：
//
//     grep -rn 'crypto\.randomUUID' frontend/src/
//
// 应该**只剩本文件一处**。多出来的每一处都是下一个待炸点。

/**
 * 生成一个唯一 id。
 *
 * 优先 `crypto.randomUUID()`（格式标准、随机源好）；
 * 拿不到就退到 16 字节随机数的十六进制串。
 *
 * 为什么不强求 UUID 格式：调用方（session_id / 队列 item.id）需要的只是
 * "在这一批里不重复"，不需要密码学强度、也不要求 8-4-4-4-12 的形状。
 * 为了凑格式去手搓版本位和变体位，是给未来的 bug 留门。
 */
export function newId(): string {
  const c = globalThis.crypto
  if (c?.randomUUID) return c.randomUUID()

  const bytes = new Uint8Array(16)

  // `getRandomValues` **不受**安全上下文限制（它不涉及密钥派生，所以没被闸住）
  // —— 也就是说在 http 下这条路依然是通的，真正兜底的只有 Math.random 那一支。
  if (c?.getRandomValues) {
    c.getRandomValues(bytes)
  } else {
    for (let i = 0; i < bytes.length; i++) bytes[i] = Math.floor(Math.random() * 256)
  }

  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
}
