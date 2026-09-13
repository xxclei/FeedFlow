# 复刻进度追踪

> 对照 `howto_feed-rebuild/README.md` 的复刻总路线图，完成一个阶段就把状态列改掉。
> 规则：按顺序推进，每个阶段验收清单全过、`git commit` 之后才算完成。

- 当前进度：**阶段 3 代码完成，待前端验收**（2026-09-13）
- 新项目代码：`myfeed/`（后端）+ `myfeed/frontend/`（Vue3 前端，随模块生长）
- 参考答案：`origin-feed-project_example/feedsystem_video_go/`

## 路线图

| # | 阶段 | 文档 | 状态 | 验收要点 |
|---|---|---|---|---|
| 0 | 架构与环境准备 | [00](howto_feed-rebuild/00-架构与环境准备.md) | ✅ 2026-09-10 | `go run ./cmd` 起服务，MySQL 表自动创建 |
| 1 | 账号模块 | [01](howto_feed-rebuild/01-账号模块.md) | ✅ 2026-09-10 | Postman 走完注册→登录→带 token 访问 |
| 2 | 视频模块 | [02](howto_feed-rebuild/02-视频模块.md) | ✅ 2026-09-12 | 直传/分片上传/发布事务/outbox；前端发布页可传可播 |
| 3 | Feed 模块 | [03](howto_feed-rebuild/03-Feed模块.md) | 🔨 代码完成待验收 | 三种游标翻页不重不漏；游客可刷流 |
| 4 | 点赞模块 | [04](howto_feed-rebuild/04-点赞模块.md) | ⬜ 未开始 | 计数正确，重复点赞被拦截 |
| 5 | 评论模块 | [05](howto_feed-rebuild/05-评论模块.md) | ⬜ 未开始 | 只有作者能删评论 |
| 6 | 关注模块 | [06](howto_feed-rebuild/06-关注模块.md) | ⬜ 未开始 | 关注后关注流出现对方视频 |
| 7 | Redis 缓存 | [07](howto_feed-rebuild/07-Redis缓存.md) | ⬜ 未开始 | 停 Redis 业务不挂；redis-cli 能看到 key |
| 8 | 热榜 | [08](howto_feed-rebuild/08-热榜.md) | ⬜ 未开始 | 翻页榜单不抖；停 Redis 降级 MySQL |
| 9 | RabbitMQ 与 Worker | [09](howto_feed-rebuild/09-RabbitMQ与Worker.md) | ⬜ 未开始 | 点赞秒回异步落库；停 MQ 直写兜底 |
| 10 | Docker 部署 | [10](howto_feed-rebuild/10-Docker部署.md) | ⬜ 未开始 | `docker compose up -d --build` 全部起来 |
| 11 | 前端总装 | [11](howto_feed-rebuild/11-前端.md) | 🔶 随行完成约 65%（骨架/登录/发布页/分片上传/批量投稿/发现页/标签流/端上分流/播放页已就绪） | 浏览器完整走一遍用户旅程 |
| 12 | 私信与 SSE 实时通知 | [12](howto_feed-rebuild/12-私信与SSE实时通知.md) | ⬜ 未开始 | 两个账号互发私信；点赞触发实时通知 |

## 前端随行记录（DESIGN.md 风格）

- ✅ 骨架：Vite+Vue3+TS、路由守卫、pinia auth store、fetch 封装（401 自动续期单飞）
- ✅ 登录/注册/首页
- ✅ 视频发布页：+ **统一走分片上传（断点续传 + 逐片 MD5 校验）** + 发布 + 作品列表页内播放
- ✅ **批量投稿**：拖文件夹 / 选文件夹（`webkitdirectory`）→ 递归取 mp4 → 全批先算指纹 → 队列上传
- ✅ **端上分流**：同一个 SPA 按 `matchMedia('(max-width: 767px)')` 切 `DesktopShell` / `MobileShell`
- ✅ TagInput 组件：#标签实时高亮框 + 徽章（正则与后端 ExtractTags 一致）
- ✅ 发现页 `/feed`：最新/点赞榜/热门榜三标签 + IntersectionObserver 无限滚动 + 骨架屏 + 空态
- ✅ 调试抽屉 `DebugPanel`：游标原值 + **跨页重复探针**（重复出现即证明游标失效），默认收起
- ✅ 卡片组件：`components/{desktop,mobile}/VideoCard.vue`，封面/标题点进**播放页**（`RouterLink`，不是就地播放）、#标签可点进标签流
- ✅ 标签流页 `/tag/:name`
- ✅ **播放页 `/video/:id`**：播放器 + 操作栏 + 作者卡 + 相关推荐 + 评论占位，桌面两列 / 移动单列
- ✅ 收尾：`index.html` 加 `viewport-fit=cover` / `interactive-widget`、`lang="zh-CN"`；删掉未用的 `axios` 和三个脚手架死资源
- ⬜ 阶段4+：点赞交互 / 评论抽屉 / 热榜页 / SSE通知

### 批量上传踩到的三个后端行为（都已在代码里标注）

| # | 后端行为 | 前端对策 |
|---|---|---|
| 1 | 断点续传按 `(accountID, file_hash)` 索引，**内容相同的两份文件会塌进同一个会话**（谁先 complete 谁销毁会话） | 开传**之前**全批算一份指纹，同哈希的只让第一个真传，其余标 `duplicate` 复用它的 `play_url` |
| 2 | 没有 TTL / 没有清理：放弃的会话留在内存，半成品 `.mp4.part` 留在磁盘 | 不提供"清理"按钮（后端没有对应接口），界面上如实写明；`init` 不在没指纹时预调 |
| 3 | 会话可能**永久无法完成**（位图说传过、磁盘文件却没了 → `upload` 幂等短路不写盘、`complete` 永远失败） | 逃生舱：`file_hash` 只是索引键、**从不校验内容**，`complete` 失败时用 `${hash}-r1` 重新 `init` 换一个全新会话，只加一次盐 |

### 分片上传的两处改造（2026-09-13）

**① 去掉「合并」这一步：分片按偏移直写最终文件**

老实现是「每片落 `tmp/<upload_id>/<index>` → `complete` 时按序 `io.Copy` 拼成 mp4」，
`complete` 对 500MB 的文件要读 500MB 再写 500MB，用户得盯着进度条多等一整个磁盘往返。

新实现：

- `init` 就把输出路径定下来（`videos/<账号>/<日期>/<随机>.mp4`），并把 `xxx.mp4.part` **预分配**到 `file_size`（`Truncate` 只移动 EOF，不写零，不费 IO）
- `upload` 用 `io.NewOffsetWriter(f, index*chunk_size)` 直接写到该片的位置上 → 分片乱序/并发到达都无所谓，也没有临时目录可残留
- `complete` 只剩一次 `os.Rename(.part → .mp4)`：同目录内原子，不存在"半个 mp4"，也不再需要清理 tmp

配套的防御：`session.chunkLen(index)` 校验每片实际字节数（400）；写不满 `wantLen` 返回 5xx（可重试）；
`complete` 前 `Stat` 确认文件还在（错误信息里带 `missing`，前端才认得出来走逃生舱）。

**② 分片大小自适应（5 / 10 / 20 MB 分档）**

前端 `utils/hash.ts` 的 `chunkSizeFor(fileSize)`：`<50MB → 5MB`、`<200MB → 10MB`、`≥200MB → 20MB`。
口径按文件大小分档：往返次数、位图大小、MD5 次数都随分片变大而变优，代价是断点续传粒度变粗、
单片重试更贵、在途内存 `chunk_size × 并发数` 变大。

它是**纯函数**，刻意不做参数透传 —— 只要所有分片运算都从 `chunkSizeFor(file.size)` 取尺子，
就不可能"指纹按 5MB 切、上传按 20MB 切"。后端从"完全不校验 `chunk_size`"改成
`validateChunkPlan`：断言 `total_chunks == ceil(file_size / chunk_size)`，并给 `chunk_size ≥ 1MB`、
`total_chunks ≤ 4096` 兜底（否则 `chunk_size=1` 配 500MB 文件就是一个 5 亿长度的 `[]bool` = 一次 500MB 内存）。
续传时若分片几何变了，**丢掉旧会话重开**，而不是返回错误把文件钉死。

**顺带**：`maxUploadSize = 500 << 20` 提到 `chunk_entity.go`，直传和分片 init 共用同一个常量，改一处两边都跟着变。
（单文件上限从 200MB 提到 500MB，是原项目之外的自主改动；`howto_feed-rebuild/02-视频模块.md` 描述的是原项目，保持 200MB 不动。）

> `howto_feed-rebuild/02-视频模块.md:181` 记的缺口「服务端**不校验** `chunk_size × total_chunks == file_size`，
> 恶意/出错声明会到 complete 缺片时才暴露」——就是上面 ② 里的 `validateChunkPlan`，已补上。
> 注意：分片一改成"按偏移直写"，这个缺口就从"合出来的文件短一截"升级成"整个文件写歪"，
> 所以它不是可选项，是直写方案的**前置条件**。

**遗留**：`.run/uploads/tmp/` 是改造前的旧目录结构，现在的代码不再写它，可以手工删掉。

### 播放页 `/video/:id`（2026-09-13）

点卡片封面/标题 → 进 `views/VideoDetailView.vue`。**后端零改动** —— 两个公开接口
（`POST /video/getDetail`、`POST /video/listByAuthorID`）+ `feed/listByTag` 就够撑起这一页。

**五个新文件**：`views/VideoDetailView.vue`、`components/player/VideoPlayer.vue`、
`components/video/{ActionBar,AuthorCard,RelatedList}.vue`。

#### 唯一的真坑：`/video/*` 和 `/feed/*` 是两套字段形状

| | `/video/getDetail`、`listByAuthorID` | `/feed/*` |
|---|---|---|
| 作者 | `username` 扁平字符串（+ `author_id`） | `author: {id, username}` 嵌套 |
| 时间 | `create_time` = **RFC3339 字符串** | Unix **秒** |
| 额外 | `popularity` | `is_liked` |

而 `utils/time.ts` 的 `formatTime(sec)` 收的是秒。所以 `api/video.ts` 里加了
`normalizeVideo()`：扁平 → 嵌套、`isoToUnixSeconds()` 转秒、补 `is_liked: false`。
**归一放在 API 边界**，下游（卡片、相关推荐列表）只认一种形状。
（`is_liked: false` 不算撒谎，`/feed/*` 今天也硬编码 false，阶段4 一起回填。）

#### 顺带查出来的两个真问题

**① `/video/:id` 千万不能带 `requiresAuth`。** 上面那条 `/video`（投稿页）有它，
抄路由时特别容易连 `meta` 一起抄 —— 抄了就把所有未登录用户从发现页一脚踢到登录页，
而 `getDetail` 本来就是公开接口。已在路由里写明原因。

**② 路由参数是字符串，直接发给后端是 500 不是 404。** 后端是
`type GetDetailRequest struct { ID uint }`、**没有 `binding:"required"`**。
实测：

```bash
curl -X POST .../video/getDetail -d '{"id":"7"}'    # → 500
# {"error":"json: cannot unmarshal string into Go struct field GetDetailRequest.id of type uint"}
```
那个 500 会被原样渲染到页面上（`handleResponse` 直接透传 `{"error":...}`）。
所以视图里先 `Number(route.params.id)`，`Number.isInteger && > 0` 才发请求，
否则本地判"不存在"，一个请求都不发。顺带：`/video/abc` → `NaN` → `JSON.stringify` 出 `{"id":null}`
→ Go 收到 0 → `WHERE id IN (0)` → 404，所以 0 是安全的，1.5 和字符串才是雷。

#### 三个未接入的区块：摆出来，但如实标注

| 区块 | 角标 | 依据 |
|---|---|---|
| 点赞 | `阶段4` | 阶段4 有 likes 模块，现在 `router.go` 里还没有它的路由 |
| 收藏 | `无接口` | **12 个阶段里都没有收藏** —— 写"阶段4"才是撒谎 |
| 评论 | `阶段5` | 阶段5 有 comment 模块，后端目前没有 `internal/comment` |

禁用态用的是 **`aria-disabled` + 空操作，不是 `disabled` 属性**：移动端没有 hover，
只写 `title` 的话手机上什么都看不到；而 `disabled` 会把元素从 tab 顺序里摘掉，
键盘/读屏用户连 `title` 都够不着。角标是**可见文字**，`title` 只作补充。

#### 只摆真有的数据

`videos` 表里只有 `likes_count` 和 `popularity`。B 站那排「播放量 / 弹幕数」我们没有对应字段，
而 **`popularity` 是热度分、不是播放次数**，标成"播放"就是编数据。所以只显示
**发布时间 + 点赞数**。相关推荐的标题也跟着数据源变：有 `#标签` → `/feed/listByTag`，
标题「相关推荐」；没有 → `/video/listByAuthorID`，标题「TA 的其它作品」。
两条腿给出的确实不是一回事，含糊地都叫"相关推荐"是在骗人。

#### 播放器：五个刻意的选择

1. **不自动播放。** Chrome 的静音自动播策略看的是"文档有没有被用户交互过"：
   从卡片点进来文档有交互历史，`play()` 往往真会成功 —— 于是同一个 URL
   点进来会自己放、直接刷新或从别人分享的链接进来却停在浮层上，**两种行为**。
   统一成"点了才放"，行为确定。代价是少了点顺滑。
2. **`playsinline` + `webkit-playsinline` 必须写**，否则 iOS Safari 一点播放就强制全屏。
3. **`preload="auto"`**（不是 `metadata`）：整页只有一个播放器，而我们的 mp4 没有
   `-movflags +faststart`（`complete` 就是一次 rename，没有转码步骤），moov 在文件尾部，
   只取 metadata 在 iOS 上可能是黑首帧。
4. **`object-fit: contain`**：卡片上用 `cover` 裁掉一点看不出来，播放器上裁掉的是画面本身。
5. **卸载时必须 `pause()` + `removeAttribute('src')` + `load()`** —— 三步缺一不可，
   只靠组件销毁元素会一直攥着解码器。这是 `utils/cover.ts` 已经踩过的教训。
   父组件把 `:key` 绑在 `play_url` 上，换视频时旧播放器整个卸载，正好走这里放手。

**顺带把最后一批裸 `<video>` 清了**：`VideoView.vue` 的「我的作品」以前每个作品挂一个
`<video preload="none">`，作品一多就是几十个解码器常驻，而 Chrome 对同时活跃的 video 元素
有硬上限（约 75 个，`utils/cover.ts` 里写着）。现在整个应用只剩**一个**真 `<video>` 元素
（详情页那个），加上抽帧时临时创建的一个。

#### 另外两处一起修的（都已验证）

**① 批量上传：算完指纹的文件在干等 —— 真缺陷**

`uploadQueue.ts` 的 `start()` 原来是 `await hashPass(); wake()`，而 `runHashPass()` 是
顺序遍历、循环体里**没有** `wake()`。后果：完全没有流水线 —— 第 1 个文件要等**最后一个**
文件的指纹算完才开始传（20 个 500MB 的文件 = 先顺序读完 10GB），而且每个算完指纹的行
都被置成 `queued` / `排队中` 然后干等，就是"算完了还显示排队"的来源。

原来那么写是为了按内容指纹判重（[坑1] 同哈希会塌进同一个 upload_id）。但
**判重只要求"自己算完"，不要求"别人也算完"**：改成边算边往 `Map<fileHash, item>` 里登记，
算出这一个的瞬间就能定它是不是重复，然后立刻 `wake()`。
判重发生在置成 `queued` **之前**，所以 `drain()` 不可能先抢走一个本该是 duplicate 的文件。

配套加了一条**护城河**：`markDuplicates()` 现在遇到 `state === 'duplicate'` 直接 `continue`。
不加的话，一个已被增量判成 duplicate 的 item 会在收尾那趟里被登记成 leader，
于是它和它真正的 leader 互相指认 → **环形等待，两个都不传**。
（这是增量判重**才会**出现的问题，老的"一趟定生死"写法碰不到。）

**② 桌面网格一行 4 个**：`minmax(200px, 1fr)` → `minmax(250px, 1fr)`。
`--page-max` 1180 − `.content` 48px padding = 1132px 可用：4 列要 `4×250+3×18 = 1054 ≤ 1132` ✓、
5 列要 `5×250+4×18 = 1322 > 1132` ✗，所以满宽正好 4 个，收窄自动退成 3 列、2 列。
不写死 `repeat(4, 1fr)`：那样 768px 宽的桌面窗口会挤成 4 列各 170px，标题全变省略号。

**③ 发现页的状态不再随跳出而丢失**：`useFeedStream` 的三个流原来建在函数作用域里，
而 `App.vue` 里没有 `<KeepAlive>` —— 点卡片进播放页会卸载 `FeedView`，游标、已加载的
每一页、`DebugPanel` 的重复探针全部归零，返回要重新从第一页刷。现在提到**模块作用域单例**，
并给路由加了 `scrollBehavior`（`savedPosition ?? {top: 0}`），返回时连滚动位置一起还原。
> 注意：**别用 `<KeepAlive>` 解决这个问题** —— 被 deactivate 的组件不会触发 `onUnmounted`，
> 详情页播放器那三步释放就静默不跑了，正好是 `utils/cover.ts` 记的那个泄漏。

**④ 「缺少文件指纹」—— `queued` 这个状态有两个来源，`drain()` 分不清**

现象：某一行直接落 `失败 / 缺少文件指纹`（错误是 `runItem` 里
`if (!fp) throw new Error('缺少文件指纹')` 抛的，`uploadQueue.ts:418`）。

根因：`queued` 既是"指纹算完了可以传"，也是 `addPicked` 创建时的初始状态（**那时指纹还不存在**）。
`drain()` 只认 `state`，于是没算过指纹的 item 也会被捞进上传阶段。三条能造出来的路径：

| | 触发 |
|---|---|
| α | **算指纹/上传途中再补选文件**：`addPicked` 里 `hashPass()` 撞上"已有一轮在跑"，老写法 `if (hashRun) return hashRun` 直接把**那一轮**的 promise 还了回来；而那一轮是 `for (const it of items.value)`，数组对象在循环开始就定死，`concat` 是**换新数组** → 新文件对它彻底隐形。随后 `.then(wake)` 就把它们放去上传了 |
| β | **暂停 → 继续**：`pause()` 会 `hashAc.abort()`，循环里每个 item 立刻抛 AbortError → 全部退回 `queued`（**一个指纹都没有**）；而 `resume()` 只 `wake()`，没重跑哈希 → 必现整批失败 |
| γ | **对循环已越过的 item 点重试**：`retryItem` 置 `queued` 后 `await hashPass()`，撞上同一轮陈旧 promise，而它按数组顺序早被跳过了 |

四处改动（都在 `uploadQueue.ts`）：

1. `runHashPass` 改成 `for(;;)` + **每轮重新 `find` 活数组**，判据从 `state === 'queued'`
   换成 `state === 'queued' && !prints.has(id)` —— 只有 `prints` 能回答"这一个到底算过没有"。
   两个防死循环的出口：暂停/abort 时 `break`（否则每轮都捞到同一个没算完的）、
   `!file` 时把 item 挪出 `queued`（否则 `find` 永远返回它）。
2. `hashPass()` 从"有在跑就返回它"改成**串成一条链**（`hashChain = hashChain.then(run, run)`）：
   新一轮排在旧的后面，而不是被旧的顶替。每轮开头先扫一遍待办，多排一轮是廉价空转。
3. `resume()` 走 `hashPass().then(wake)` —— 暂停掐断的是半途，放回 `queued` 的文件**没有指纹**。
4. `drain()` 加兜底闸门：捞到的 `queued` 若没有指纹，不跑它，改成 `void hashPass()` 推回哈希通道。

另：`runHashPass` 的 duplicate 分支也补了 `wake()` —— leader 早就 `done` 的话这一个立刻能借
它的 `play_url` 发布，不必等整轮哈希跑完。这四条都是"别让没算过指纹的 item 进入上传阶段"的同一个修法。

#### 本页故意没做的

- **播放量 / 弹幕数**：没有字段，不编。
- **移动端悬浮小窗**：播放器随页面滚走。
- **hover 静音预览**：B 站有，我们要的是"点进详情页"，不做两套播放路径。
- **移动端 TabBar 在详情页不高亮任何 tab**：「投稿」用 `r.path === '/video'` 全等匹配，
  `/video/:id` 不会误亮；四个 tab 都不亮是**有意的** —— 详情页是被"推进去"的一层，
  不属于任何一个 tab（B 站 App 在视频页也收起底栏）。

## 备注

- 本机环境：Go 1.25 ✅ / Docker Desktop + WSL2 ✅（注意：Smart App Control 已关，否则拦编译产物）/ MySQL·Redis·RabbitMQ 容器 ✅ / npm 源已换 npmmirror
- 阶段7回填点：chunk 会话迁 Redis、GetDetail 防击穿缓存、限流中间件、token 缓存快路径
- 进阶实验清单：视频转码流水线（阶段9解锁，见任务#14）

## 进阶实验待办（按解锁条件排序）

1. **视频转码流水线**（阶段9解锁）：上传完成后发转码消息 → Worker 消费 → ffmpeg 多码率/抽帧/HLS 切片 → 回写 video 表（对齐 B站/YouTube 工业级）
2. **Feed 推拉结合改造**（阶段9解锁）：全局时间线改造成"每用户收件箱 ZSET"；普通用户发布 fan-out 推送进粉丝收件箱，大V（粉丝超阈值）标记为 Pull 源；刷新 = 收件箱 + 实时查大V合并（微博/Twitter 同款方案）
3. **对象存储 + CDN + 预签名直传**（阶段10后最合适）：视频/封面存 OSS/S3，数据库只存 URL；上传走服务端签发的预签名凭证直传对象存储，500MB 流量不再经过 API 进程；播放 URL 走 CDN 边缘节点 + 签名防盗链
