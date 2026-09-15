# 阶段12 私信模块 —— 验收记录

日期：2026-09-14
范围：`internal/message/`（4 个文件）+ `db.go` AutoMigrate + `router.go` 挂载
SSE 那一半在阶段9 已完成并验证过，本次**不重复测**（见 `bench-notes-mq-all.md` 第五、六节）。

---

## 一、代码结构

原项目把四件套挤在 `entity.go` + `handler.go` 两个文件里，`Repository`/`Service`
只是 handler.go 里的瘦壳（handler 甚至直接摸 `service.repo`）。
本项目按约定拆成四个文件：

| 文件 | 内容 | 行数级 |
|---|---|---|
| `entity.go` | `Message` + 3 个请求/响应类型 | ~100 |
| `repo.go` | `Send` / `List`，各一条 SQL | ~110 |
| `service.go` | `Send`（校验收件人存在）/ `List` | ~95 |
| `handler.go` | `Send` / `List` | ~165 |

接线（`router.go`）：
```
messageRepository ← db
messageService    ← messageRepository + accountRepository
messageHandler    ← messageService
```
全项目**依赖最少**的模块，没有任何一环是可选的，也没有缓存/MQ/跨模块数据。

**路由**：`/message` 整个组套 `jwt.JWTAuth`，**没有一条公开路由** ——
这是唯一一个这样的模块。like/comment/social 都是"公开读 + 鉴权写"，
因为读的是公开内容；私信读的是私有数据，匿名读一个字节都是漏洞。
**鉴权结构是数据属性的映射，不是模块风格。**

---

## 二、对原项目的 4 处改进

| # | 改进 | 原项目 | 本项目 |
|---|---|---|---|
| 1 | `/message/send` 校验收件人存在 | 给不存在的 `to_id` 发信 → 200 + 一条永远没人能读到的孤儿数据 | `FindByID` → 404 `record not found` |
| 2 | `peer_id == 0` | 静默返回空列表，和"没聊过"长得一样 | 400 `peer_id is required` |
| 3 | `content` 全是空格 | 存进去（收件人看到空白气泡，且没有删除接口） | `TrimSpace` 后判空 → 400 |
| 4 | 四件套拆文件 | entity+handler 两文件挤完 | 四个文件 |

改进 #1 值得单独说：**这是"全库不用外键"的直接代价。**
有 `FOREIGN KEY` 的话那条 INSERT 会被数据库自己拒掉；项目为了 AutoMigrate
简单而不用外键，于是**引用完整性从数据库的活变成了 service 的活**，
忘一次就有一个入口能写脏数据。这已经是第三次（social 两端 FindByID、
comment/like 的 video 存在性、现在的 message 收件人）。

---

## 三、验收清单（逐条实测）

测试账号：`msgta`(id=6) / `msgtb`(id=7)，密码均为 `pass123456`。

### 3.1 功能
| # | 用例 | 结果 |
|---|---|---|
| 1 | A(6) → B(7) 发送 | ✅ 200 `{"id":1,"from_id":6,"to_id":7,"content":...,"is_read":false,"created_at":"2026-09-14T17:52:02.222+08:00"}` |
| 2 | B(7) `list {peer_id:6}` 看到 A 发的 | ✅ 200，1 条 |
| 3 | B 回一条 | ✅ 200 id=2 |
| 4 | A `list {peer_id:7}` 看到两条（**双向**） | ✅ count=2，`7→6` 在前、`6→7` 在后 |

### 3.2 边界与兜底
| # | 用例 | 结果 |
|---|---|---|
| 5 | `to_id=0` | ✅ 400 `{"error":"to_id and content are required"}` |
| 6 | `content=""` | ✅ 400 同上 |
| 7 | `content="   "`（全空格） | ✅ 400 同上 |
| 8 | 无 token | ✅ 401 `missing authorization header` |
| 9 | 假 token | ✅ 401 `invalid or expired token` |
| 10 | 空会话 `{peer_id:99999}` | ✅ 200 `{"messages":[]}` ← **不是 null** |
| 11 | `to_id=99999`（不存在） | ✅ 404 `record not found`（改进 #1） |
| 12 | `peer_id=0` | ✅ 400 `peer_id is required`（改进 #2） |
| 13 | `content="  padded  "` | ✅ 回显 `'padded'`（两端空格已 trim） |

### 3.3 分页与排序（灌 60 条）
```
起始 5 条 → 连发 60 条（耗时 0.69s，≈87 req/s，单线程 python 客户端，无 429）
A 视角返回 50 条  ← LIMIT 50 生效（共 65 条，砍掉最旧 15 条）
首条 seq-059 / 末条 seq-010   ← created_at 倒序正确
B 视角 50 条，id 序列与 A 视角完全一致  ← 双向对称
```

### 3.4 中文与 emoji
```
sent  : '你好 B，这是一条 UTF-8 中文私信 🎉 emoji 也要活下来'
echoed: （完全一致）      send-echo equal: True
list-readback equal: True
DB HEX: ...E4B8ADE69687E7A781E4BFA120F09F8E89   ← 合法 UTF-8，4 字节 emoji 存活
```
→ 列是真 `utf8mb4`，4 字节 emoji 不炸。

> ⚠ 注意：**用 shell 里内联中文的 curl 测会得到乱码**，那是 Git Bash 按 GBK
> 编码了请求体，不是后端的问题。判断方法一句话：`SELECT HEX(content)`，
> 合法 UTF-8 才继续查后端。详见 memory `windows-curl-mangles-chinese`。

---

## 四、EXPLAIN：一个"想当然"被实测推翻的地方

**预期**：跨列 OR 会走 `index_merge(union)`，两个方向索引各出一次。

**实测**（`repo.go` 里已按这个结果改正了注释）：

| 行数 | type | key | Extra |
|---|---|---|---|
| 3 | `range` | `idx_message_from` | Using index condition; **Using filesort** |
| 65 | **`ALL`** | **NULL** | Using where; **Using filesort** |
| 65 + `FORCE INDEX(...)` | `range` | `idx_message_from` | 仍然**不是** index_merge |

三条结论：

1. **两个索引的作用是"让 index_merge 成为一种可能"，不是"保证被用到"。**
   `possible_keys` 里出现它们 = 结构对了；`key: NULL` = 优化器选择不用。
   `show index from messages` 验的是前者。
   （已验：`PRIMARY(id)` / `idx_message_from(from_id)` / `idx_message_to(to_id)`，
   两个方向**各一个单列索引**，和 entity.go 的设计一致。）
2. **在几百行的表上验证不出索引有没有用。** 65 行时优化器算出来"扫全表更便宜"
   （这是它该做的判断）。messages 表的真实量级（个人私信）永远到不了十万行，
   所以**生产形态就是全表扫 + filesort，且完全可接受**。
3. `Order by created_at DESC LIMIT 50` **无论如何都不走索引**（filesort）。
   想让排序也走索引要升级成复合索引
   `(from_id, to_id, created_at)` / `(to_id, from_id, created_at)`。
   **不升级** —— 换索引要重建表，收益在当前量级是 0。
   知道升级路径存在就够了。

---

## 五、明确**没有**测的

- 50 条以上的历史翻页 —— **接口根本不支持**（`Limit(50)` 硬编码，无游标参数）。
  想加必须照 feed 的游标协议做完整，不是"加个参数"（见 `repo.go` 注释）。
- `IsRead` —— **原项目的死字段，本项目也照原样不写**。
  它缺的不是一行 UPDATE 而是一整套语义（何时算已读/谁标/批量还是逐条），
  见 `entity.go` 注释。
- 消息删除 / 撤回 / 已读回执 —— 都没有接口。
- 并发发送同一会话 —— 无竞争点（各插各的行，无唯一索引可撞）。
- 内容长度上限 —— 没有 `MaxBytesReader`，text 列能塞几十 MB（全项目现状）。

---

## 六、测试数据现状

- `messages` 表：3 条干净会话（6→7、7→6、6→7），已验证倒序 + UTF-8。
  `AUTO_INCREMENT` 已重置。
- 测试账号 `msgta`(6) / `msgtb`(7) **保留**，供前端联调；
  token 存在 `.run/tok_msgta.txt` / `.run/tok_msgtb.txt`。
- 旧的 `seq-000..059` 压测数据已删。

---

## 七、阶段12 前端

### 7.1 新增 / 修改

| 文件 | 说明 |
|---|---|
| `api/message.ts` | `sendMessage` / `listMessages`（`res?.messages ?? []` 兜 null；注释写明 created_at 是 RFC3339、后端倒序、`is_read` 是死字段别拿它渲染） |
| `api/notification.ts` | 4 个接口 + `notificationTarget` 分流 + `createNotificationStream` |
| `stores/notification.ts` | `items`/`unread`/`connected`，`MAX_KEEP=100` 防只增不减 |
| `components/NotificationBell.vue` | 铃铛 + 下拉面板，两壳共用 |
| `views/MessageView.vue` | 两栏私信页 |
| `api/client.ts` | 只把 `tryRefresh` 加 `export`（**不抄第二份续期逻辑**） |
| `App.vue` | SSE 的 connect/disconnect 挂这一层 |
| `layouts/{Desktop,Mobile}Shell.vue` | 顶栏铃铛入口 |
| `router/index.ts` | `/messages`、`/messages/:peerId`，都 `requiresAuth` |
| `views/ProfileView.vue` | "私信"按钮（`!isSelf && isLoggedIn`） |

**为什么 SSE 挂在 `App.vue` 而不是铃铛组件里**：壳会随 768px 断点重新挂载，
挂铃铛里的话**每拖一次窗口就断开重连一次**，而"断开到重连"那几秒的推送
正好落在后端注册表里没有这条连接的窗口（按设计直接丢）。挂 App 这一层，壳怎么换都不影响。

**`notificationTarget()` 必须按 type 分流**：`follow` 的 `target_id` 是**账号 ID**，
like/comment/mention 是**视频 ID**。不分流不会 404 —— 账号 3 和视频 3 都真实存在，
只会打开一条风马牛不相及的视频。**这种 bug 最难发现。**

### 7.2 前端实测（经 vite 开发代理，即浏览器的真实路径）

| 项 | 结果 |
|---|---|
| `npx vue-tsc --noEmit` | ✅ 0 错误 |
| `npx vite build` | ✅ 通过，`MessageView-*.js 6.98 kB` |
| `/api/message/list` 经代理 | ✅ 200，3 条，UTF-8 正常 |
| SSE 响应头 | ✅ `text/event-stream` + `X-Accel-Buffering: no` + chunked |
| 端到端推送延迟 | ✅ 关注动作 @3.02s → 通知帧 @3.03s（**10ms**） |
| keepalive | ✅ @30.06s 收到 `: keepalive` |

---

## 八、两个实测出来的真缺陷（都已修）

### 8.1 SSE 经 vite 代理要等 30 秒才出响应头

**现象**（同一段代码，只换请求地址）：

| 路径 | 首字节（响应头）到达时间 |
|---|---|
| 直连 `127.0.0.1:8080` | **0.07s** |
| 经 `localhost:5173/api`（vite） | **30.02s** ← 卡了整整一个 keepalive 周期 |

**根因**：Node 的 `res.writeHead()` **只把状态行和头记在内存里，不落到 socket**，
真正发出去是在第一次 `write()` 时（那一刻它才知道该用 Content-Length 还是 chunked）。
后端 `c.Writer.Flush()` 只把头交给 gin/net-http，代理侧没收到**任何 body 字节**
就没有那次 write，于是头一直被攒着 —— 直到 30 秒后那条 keepalive 提供了"第一个字节"。

**后果**（比看起来轻，但确实存在）：前端 `onopen` 要等 30 秒才触发，
面板上一直挂着"实时推送未连上"。**推送本身是通的** ——
4.02s 发出的关注通知在 4.03s 就到了，因为它就是那个"第一个字节"。

**修法**（`internal/worker/ssehub.go`）：Flush 之前先写一个**注释帧** `: connected\n\n`。
注释帧（`:` 开头）是 SSE 标准的一部分，浏览器会忽略它、不进 `onmessage`、不产生数据，
所以它只干一件事：**提供那个"第一个字节"，把响应头和连接状态一次性顶出去**。
任何"攒到有 body 才转发"的中间层（Node 代理、部分 nginx 配置、CDN）都会立刻放行。

**修后实测**：直连 0.06s / 经 vite **0.03s**。端到端复测：
`: connected` @0.05s → 关注 @3.02s → 通知帧 @3.03s → keepalive @30.06s。

**代价是两个换行符，收益是 `onopen` 从 30s 变成 0s。**
这个坑在直连测试里**永远看不到** —— 只有走浏览器那条路才会暴露。

### 8.2 前端 SSE 会"悄无声息地永久死掉"

`api/notification.ts` 里的退避计数 `attempt` 原本只在 `onmessage` 里归零。
但 `attempt` 的语义是"**连续**几次重建失败"，所以只要连上了就该归零 ——
只放在 `onmessage` 里的漏洞是：**重连成功、但此后一直没收到任何通知**的用户，
`attempt` 会一直停在上次的值，每次断线 +1，攒到 `maxRetry` 就永久不再重连。
而这条路上的断开本来就很零星（切网络、休眠唤醒），攒够 3 次可能要几周 ——
然后红点**悄无声息地永远不动了**，正是这个文件存在的唯一理由所要防的那件事。

**修法**：`es.onopen` 里加 `attempt = 0`。

配套的第二个洞（`stores/notification.ts`）：`connect()` 原本用 `if (es) return` 判重。
一旦内部退避用尽放弃重建，`es` 仍然非 null 但那连接已经 CLOSED —— 之后每次
`connect()` 都被这一行挡回去，**再也没有任何路能接回来**（只能登出再登录）。
**修法**：判据改成 `if (es && es.readyState !== EventSource.CLOSED) return`。

---

## 九、通知接口验收（阶段9 已测，本次复测越权那条）

```
6 的通知条数=1   id=18 type=follow is_read=false        ← 见下方 async 缝隙
未读=2

越权：B(id=7) 去标 A(id=6) 的第 18 条
  返回 {'message':'ok'} HTTP=200      ← 返回成功
  A 的未读仍=2                        ← 但**不生效**（WHERE 带 recipient_id 条件）

单条标已读 id=18 → A 未读 2→1，list 里 is_read=[False(19), True(18)]  ✅
空 body 全标   → A 未读 →0，list 里 is_read=[True, True]            ✅
```

**越权那条返回 200 但不变**，是刻意的：`UPDATE ... WHERE id=? AND recipient_id=?`
影响 0 行，而 handler 不报告影响行数（固定返回 `{"message":"ok"}`）——
**不报错、不泄露"这条存在但不属于你"**。

**上表第一行和第三行的矛盾值得记**：list 立刻查只有 1 条，心跳一下之后再查未读是 2。
这不是 bug —— 是 **`social.follow` 的 HTTP 响应先返回，`NotificationWorker`
才把通知行插进去**（MQ 往返，几毫秒）。见 memory `rabbitmq-publish-is-async`。
**同一个接口隔几毫秒查两次得到不同的数，在异步链路上是正常的。**

---

## 十、构建状态

```
go build ./...             OK
go vet ./...               OK
gofmt -l internal/ cmd/    空
go test ./internal/...     video ok / rabbitmq ok，其余无测试文件

npx vue-tsc --noEmit       0 错误
npx vite build             通过
```
`myfeed.exe` 与 `worker.exe` 均在运行（API 已用带 8.1 修复的版本重启）。
真实浏览器里点一遍**没做** —— 见下。

---

## 十一、本次仍然**没有**验证的

- **没有在真浏览器里点过。** 全部前端验证都是"构建 + 通过 vite 代理用
  python 打接口"。没验证到的：
  - 401 那一下 `readyState` 是否真落到 `CLOSED`（Chrome/Firefox 按规范是的，但没实测）
  - 铃铛下拉面板的交互（点外关闭 / Esc 关闭 / 移动端 44px 触控区）
  - MessageView 两栏布局在 768px 断点两侧的表现
  - **8.2 那两个修复的回归**：修的是"几周后才显形"的路径，没法用一次点击验证
- `docker stop/start rabbitmq` 后的**后端自愈** —— 阶段9 已查明是个真缺陷
  （整个 rabbitmq 包的 `amqp.Dial` 只在启动时出现一次，`NewChannel()` 拿到的一直是
  死连接，所有"5 秒后重连"的循环都在空转），本次**没重测**，也没修。
