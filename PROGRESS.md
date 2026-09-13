# 复刻进度追踪

> 对照 `howto_feed-rebuild/README.md` 的复刻总路线图，完成一个阶段就把状态列改掉。
> 规则：按顺序推进，每个阶段验收清单全过、`git commit` 之后才算完成。

- 当前进度：**阶段 0~6 全部已提交**（含本轮扩展：批量标签 / 批量删除 / 模糊检索 / 播放页），提交 `e960d69`，2026-09-13。
- 新项目代码：`myfeed/`（后端）+ `myfeed/frontend/`（Vue3 前端，随模块生长）
- 参考答案：`origin-feed-project_example/feedsystem_video_go/`

## 路线图

| # | 阶段 | 文档 | 状态 | 验收要点 |
|---|---|---|---|---|
| 0 | 架构与环境准备 | [00](howto_feed-rebuild/00-架构与环境准备.md) | ✅ 2026-09-10 | `go run ./cmd` 起服务，MySQL 表自动创建 |
| 1 | 账号模块 | [01](howto_feed-rebuild/01-账号模块.md) | ✅ 2026-09-10 | Postman 走完注册→登录→带 token 访问 |
| 2 | 视频模块 | [02](howto_feed-rebuild/02-视频模块.md) | ✅ 2026-09-12 | 直传/分片上传/发布事务/outbox；前端发布页可传可播 |
| 3 | Feed 模块 | [03](howto_feed-rebuild/03-Feed模块.md) | ✅ 已验收（提交 `c57fa91`） | 三种游标翻页不重不漏；游客可刷流 |
| 4 | 点赞模块 | [04](howto_feed-rebuild/04-点赞模块.md) | ✅ 已验收（提交 `e960d69`） | 计数正确，重复点赞被拦截 |
| 5 | 评论模块 | [05](howto_feed-rebuild/05-评论模块.md) | ✅ 已验收（提交 `e960d69`） | 只有作者能删评论；@提及写 notifications 行 |
| 6 | 关注模块 | [06](howto_feed-rebuild/06-关注模块.md) | ✅ 已验收（提交 `e960d69`） | 关注后关注流出现对方视频 |
| 7 | Redis 缓存 | [07](howto_feed-rebuild/07-Redis缓存.md) | ⬜ 未开始 | 停 Redis 业务不挂；redis-cli 能看到 key |
| 8 | 热榜 | [08](howto_feed-rebuild/08-热榜.md) | ⬜ 未开始 | 翻页榜单不抖；停 Redis 降级 MySQL |
| 9 | RabbitMQ 与 Worker | [09](howto_feed-rebuild/09-RabbitMQ与Worker.md) | ⬜ 未开始 | 点赞秒回异步落库；停 MQ 直写兜底 |
| 10 | Docker 部署 | [10](howto_feed-rebuild/10-Docker部署.md) | ⬜ 未开始 | `docker compose up -d --build` 全部起来 |
| 11 | 前端总装 | [11](howto_feed-rebuild/11-前端.md) | 🔶 随行完成约 75%（骨架/登录/发布页/分片上传/批量投稿/发现页/标签流/端上分流/播放页/检索页已就绪） | 浏览器完整走一遍用户旅程 |
| 12 | 私信与 SSE 实时通知 | [12](howto_feed-rebuild/12-私信与SSE实时通知.md) | ⬜ 未开始 | 两个账号互发私信；点赞触发实时通知 |

**扩展（不在上面这条路线上）**：模糊检索 —— `FULLTEXT + ngram` 词法那一路，2026-09-13。
howto 那 13 篇文档一份都没提检索（`grep -rln "FULLTEXT\|ngram" howto_feed-rebuild/*.md` 零命中），
所以它没有阶段编号，别按"阶段 7"去找它 —— 阶段 7 是 Redis 缓存。
另一半（向量召回 + RRF 融合 + 回填命令）**明确推迟**，见下面「已知缺口」。验收清单在 §「检索」。

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
- ✅ 点赞（阶段4）：`LikeButton` + `/likes` 我的点赞页
- ✅ 评论（阶段5）：`CommentSection` 抽屉 + `@提及` 高亮
- ✅ 关注与主页（阶段6）：`FollowButton` + `/profile/:id` 公开个人主页；关注流是 `/feed?tab=following`
- ✅ **检索页 `/search?q=`（本轮扩展）**：`FULLTEXT + ngram` 词法搜索 + `mode`/`arms` 进调试面板
- ✅ **批量删除（本轮）**：「我的作品」多选 + 全选 + 批量条，`skipped_ids` 如实提示
- ✅ **批量标签（本轮）**：`TagChipsInput` chip 编辑器，三个批量选项改成**发布那一刻才求值**
- ⬜ 阶段7+：三级缓存 / 热榜页 / SSE 实时通知

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

### 批量标签 · 批量删除 · 检索（本轮扩展，2026-09-13）

三件事来自同一句诉求：**"批量上传时能批量设标签、视频能批量删除、搜索要像搜索引擎那样模糊匹配"**。
①② 是小的、独立的；③ 是大头，且**只落了词法那一路**（向量那一路见「已知缺口」）。

#### ① 批量标签：`tag_names` 成为一等公民

老实现是坏的，不是"不够好"：`BatchOptions.extraTags` 是一个裸 `<input>`，它的内容**整个**变成
每条视频的 `description`，后端再从 `title + " " + description` 里 `ExtractTags` 抽 `#xxx`。三个问题：

- 打 `日常 vlog` 不带 `#` → **静默零标签**（最恶劣的那种失败：不报错、看起来生效了）；
- 标签成了描述的一部分，白吃 `varchar(255)` 的额度；
- 改 `extraTags` 只对**之后加入**的文件生效 —— 和 `titlePrefix` 同一个 bug。

现在：`PublishVideoRequest.TagNames []string`，服务端落库取 **`ExtractTags(文本) ∪ TagNames`**。
因为是并集，**所有既有行为原样保留**（单独发布页的 `#标签` 走的是老路径，不受影响）。

**顺手修掉的是一整类 bug，不是一个实例**：`titlePrefix` / 标签 / 统一描述 三个批量选项现在都在
**发布那一刻**求值（`uploadQueue.ts` 的 `effectiveTitle` / `effectiveDescription` / `effectiveTags`），
每行还把该文件**最终会带上的标题和标签**显示出来 —— "它对已入队的文件也生效"变成可见的，
而不是靠注释保证。

> **一处刻意的冗余**：批量标签**同时**写进了描述文本（`#日常 `）。理由是 FULLTEXT 只索引
> `title + description` 两列（`video_tags` 不参与），标签只进 `video_tags` 的话，
> "只有标签、标题描述里没这个词"的视频会搜不到，而单独发布那条路把 `#tag` 写进了描述、**搜得到** ——
> 同一个功能两种行为，比冗余更糟。等 `/feed` 开始返回标签、FULLTEXT 覆盖到 `video_tags` 之后可以摘掉。

#### ② 批量删除：一次 `IN` + 清 `video_tags` + 401→403

老 `DeleteVideo` = `db.Delete(&Video{}, id)`：没有事务、没有 `AND author_id = ?`、**不清 `video_tags`**
（孤儿关联一直在攒）。而且删别人的视频返回 **401** —— `client.ts:214` 对**任何** 401 都 `clearTokens()`，
手滑点一下别人视频的删除键就被静默登出。评论模块当初为完全相同的理由改成了 403，这次对齐。

- `SelectOwnedIDsTx` **先问"哪些 id 真是我的"**，再删那批 → `deleted_ids` / `skipped_ids` 是先查出来的
  确定答案，不靠 `RowsAffected` 推断（GORM 的软删除语义会让它的含义随版本漂移；
  而且"不是我的"和"不存在"在 `WHERE id IN AND author_id` 下**无法区分**，但响应里要如实报出来）。
- 单条 `Delete` 改成 `DeleteBatch` 的 **1 元素特例**，不再有两份删除逻辑。
- handler 校验：空 → 400；`> 100` → 400（给事务和请求体封顶）；**去重后再进 service**（`[3,3,3]` 不算 3 条）。

#### ③ 检索：`FULLTEXT + ngram`，融合那一步留着但**恒等**

```
query ──→ sanitize ──→ buildBoolean ──→ MySQL MATCH...AGAINST ──→ 冻结令牌游标 ──→ GetByIDs → 按序重排
```

数据流里**仍有 `FuseRRF(k=60, lists...)` 这一步**，只是这轮只喂给它一路。看起来像白跑，
但它在单路上是**恒等变换**：`score(d) = 1/(k+rank)` 对 rank **严格递减**，一路的融合结果就是原序。
留着它的理由是接线只差传参 —— 而不是"以后再补一层排序"（那会改掉排序语义，是另一回事）。

**(a) 第一处手写 DDL。** 到这一轮为止 GORM 结构体 tag 是**唯一**的 schema 声明方式（仓库里 0 个 `.sql`）。
`WITH PARSER ngram` 没有对应的结构体 tag，所以 `internal/video/search_index.go` 里必然出现第一段裸 SQL。
它在 `AutoMigrate` 里被调用，幂等靠查 `information_schema.STATISTICS`（MySQL 没有 `ADD FULLTEXT INDEX IF NOT EXISTS`）。

```sql
ALTER TABLE videos ADD FULLTEXT INDEX ft_videos_title_desc (title, description) WITH PARSER ngram;
```

**(b) 四个真的坑**（前三个不处理就是静默错误）：

| 坑 | 后果 | 处理 |
|---|---|---|
| `ngram_token_size = 2` | **单字查询（"日"）永远匹配不到**，静默返回空 | rune 数 < 2 → **LIKE 降级**。按 **rune** 判，不是 byte（中文一字 3 字节） |
| 索引期丢停用词 | 含停用词的 n-gram **在索引期就没了**，事后改变量对已建索引无效 | 建索引前确认 `innodb_ft_enable_stopword`，只能重建 |
| BOOLEAN 模式下引号短语（Bug #118238） | ngram + utf8mb4 + CJK 标点 → **返回空集**，不带引号反而能命中 | 构造查询用**裸 token**，不加引号 |
| 给已有数据的表加 ngram 索引 | 结果不一致 | 补 `SET GLOBAL innodb_optimize_fulltext_only=1` + `OPTIMIZE TABLE videos`（只重建全文索引，不重建整表） |

**(c) 「搜索引擎」的那半句 = AND 失败再 OR 一次。** `+tok1 +tok2` 等价于"整串子串包含"（严格）；
返回条数 < limit 时用 `tok1 tok2` 重试。跑的是哪个模式**如实报给前端**（`mode` 字段，
`ngram` / `ngram-or` / `like`）—— 这一页最有价值的可观测性就是"搜索变差了能一眼看出是哪一步退化的"。

**(d) sanitize 后 0 个 token → 400，不是空列表。** 只打标点 `+++` 时，空列表的语义是"没有这个视频"，
用户会以为语料里真没有、然后一直换词试。400 的文案说清"查询里没有可检索的词"。

**(e) 游标 = 服务端签发的不透明令牌，令牌里冻着整轮 id 列表 + offset。**
RRF 的输出不是一个能按游标切片的有序序列，所以项目里用了四次的"镜像游标"在这**不适用**。
offset 分页在一般情况下不安全（新插入会让翻页重复/遗漏），**这里安全，因为底层顺序被冻住了** ——
这正是"排序不可变时 offset 才是对的"这条误解的真正边界。同一轮翻页是一份**快照**（中途新发的视频不会插进来），
和最新流的行为不同，是有意的。令牌**不签名**：它是客户端持有的，改它只能换到另一个公开视频的切片，
不是安全边界 —— 加 HMAC 只会让人以为那里有什么可保护的。

> **最容易出的 bug 记在这里**：`feed.GetByIDs` 用的是 `WHERE id IN ?`，**返回 MySQL 的顺序，不是融合顺序**。
> 必须按 id 列表**在 Go 里重排**，否则相关度排序静默退化成一团乱序 —— 不报错，结果看起来也"有内容"。

**(f) 前端把两种失败分开**：400（查询本身没意义）走中性虚线提示面板，5xx 走网格的红色错误横幅，
靠 `ApiError.status` 区分。`SearchView` 还带**跨页重复探针** —— 冻结令牌下重复理论上不可能出现，
真出现就说明游标那条路坏了，而"第二页悄悄重复一条"只有显示出来才会有人发现。

**验收（你自己跑，见 §「验收命令」）**：`npx vue-tsc -b && npx vite build` 已过（tsc 与 build 都干净）；
`go build ./...` / `go vet ./...` / `gofmt` 已过（唯一例外见「已知缺口」）。

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

## 验收命令（批量标签 / 批量删除 / 检索 · 2026-09-13）

按约定这些都是你自己跑的。挑的是**不做就会静默错**的那几条，不是走一遍流程。

**建索引 / 迁移（出问题都在这里，先看这段）**

```sql
show index from videos;                  -- 要有 ft_videos_title_desc，INDEX_TYPE = FULLTEXT
SELECT @@ngram_token_size;               -- = 2，这就是单字要降级 LIKE 的原因
SELECT @@innodb_ft_enable_stopword;      -- 记下值；建索引后再改对已建索引无效
show index from video_tags;              -- 要的是**一个两列**复合唯一索引 idx_video_tag (video_id, tag_id)
```

> `video_tags` 那条**和阶段4 `likes`、阶段6 `socials` 同一个坑**，而且**插第二条时才炸**。
> AutoMigrate 加这个索引**之前**先跑一次重复行检查，有重复会直接迁移失败：
> ```sql
> SELECT video_id, tag_id, COUNT(*) c FROM video_tags GROUP BY 1,2 HAVING c>1;
> ```

**ngram 的边界（最值钱的一条，手工验）**

```sql
SELECT id FROM videos WHERE MATCH(title,description) AGAINST('+日常' IN BOOLEAN MODE);  -- 窄
SELECT id FROM videos WHERE MATCH(title,description) AGAINST('日常' IN BOOLEAN MODE);   -- 宽（OR）
```

确认 `+` 形式**真的比不带 `+` 窄**。两条都一样，说明 ngram 分词没生效、索引是坏的。

**批量标签**

1. 批量选几个文件 → chip 输入打 `日常`（**不带 `#`**）→ 回车 → 出现 `#日常` chip → 发布后
   `select * from tags` 有它、`video_tags` 有对应关联。
2. 粘 `#a #b #c` → 拆成 3 个 chip。
3. **改标签对已入队的文件也生效**：先加文件 → 再改标签 → 再发布（验证不是只对后来者生效）。
4. 打一个 300 字的标签 → **不会 500**（被按 rune 截到 100）。
5. 单独发布页（`TagInput`）的 `#标签` 行为**没变**（并集不破坏既有路径）。

**批量删除**

6. 选 3 条自己的视频删掉 → 3 条都消失，且 `select count(*) from video_tags where video_id in (…)` = 0。
7. 构造部分成功：混一个不属于你的 id 进去 → 响应里 `deleted_ids` 只有你那条、`skipped_ids` 里有它，
   前端**如实提示**而不是笼统报"删除成功"。
8. **删别人的视频 → 403 而不是 401，而且不被登出。** 删完立刻看是否还是登录态 ——
   401 会被 `client.ts` 的 `clearTokens()` 静默踢出去。这条要专门点。

**检索**

9. 搜多字中文词 → 有结果，且**相关度排序看得出来**（不是纯时间倒序）。
10. 调试面板显示 `mode=ngram`、两路各召回多少条。
11. 搜**单个汉字** → `mode=like` 且**有结果**。不做这步，单字搜索会静默返回空。
12. 只打标点 `+++` → 400 且文案是"没有可检索的词"，**不是空列表**。
13. 翻页：搜个宽泛的词 → 滚到底加载第二页 → **无重复无遗漏**；同一轮刷新第一页 → 顺序一致。
    重复探针（调试面板）会自己报数，非 0 就是游标那条路坏了。
14. **游客（登出）**能搜索、能翻页。
15. 顶栏搜索框分流：打 `#日常` → 进标签流；打 `日常` → 进检索页。

### 为什么删视频不删本地文件（2026-09-13 提问）

**先纠正一个可能的误解**：不是"忘了"。原项目的 `DeleteVideo` 就是一行

```go
func (vr *VideoRepository) DeleteVideo(ctx context.Context, id uint) error {
	return vr.db.WithContext(ctx).Delete(&Video{}, id).Error
}
```

（`origin-.../internal/video/video_repo.go:32`，原项目全库只有分片上传那几处 `os.Remove`）。
我们复刻了它，**但真正成立的理由是这个项目自己的两条性质**：

**① 一个磁盘文件可能对应多条视频行。** 批量上传的判重逻辑是"同哈希的只真传一次，
其余标 `duplicate`，**借 leader 的 `play_url`、然后照常发一条自己的记录**"
（`frontend/src/stores/uploadQueue.ts:516-517`）。所以库里两条 `videos` 行可以指向**同一个 `.mp4`**。
删掉其中任何一行的同时删文件，另一条视频立刻变成"记录还在、点开 404" ——
而且不报错、也不会有任何日志。这不是理论风险，是你批量投稿本来就会产生的状态。

**② `play_url` 是客户端给的，服务端没有归属凭证。**
`PublishVideoRequest.PlayURL` 直接来自请求体（`internal/video/entity.go:48`），
handler 原样搬进库（`video_handler.go:55`），只在 service 里 `TrimSpace` 一下。
于是"删 `play_url` 指向的文件"等于**让请求体决定服务端删哪个文件**：
最轻是删到别人的视频文件，不做路径校验的话（`/static/../../`）就是一个任意文件删除。
要做这件事，前提是 `play_url` 由服务端签发/校验归属 —— 那是另一个改动，不是删除逻辑里的一行。

**③ MySQL 事务和文件系统之间没有原子性。** 数据库删除可回滚，`os.Remove` 不可：
删在事务里 → 事务一回滚，文件没了、行还在（视频永久损坏）；删在提交之后 → 崩在两者中间就是泄漏。
两边都拿不到原子性。而失败模式**不对称**：漏删一个文件 = 浪费磁盘（可检测、可补跑），
错删一个文件 = 视频永久损坏（不可恢复）。所以边界不清时，正确的默认是**不删**。

**工业界的形态**（阶段9 的 Worker 就是它的位置）：数据库先删（它才是"什么该存在"的真相源），
文件交给**异步 GC**：定期扫 `.run/uploads`，把"磁盘上有、但 `videos` 表里没有任何
`play_url`/`cover_url` 引用它"的文件当成孤儿删掉。这样 ①② 自动被解决（引用计数由扫描得出，
不靠人记），③ 也不再是问题（GC 崩了下次重跑，删除是不可回滚的那一半、GC 是可重跑的那一半）。
对象存储的 lifecycle 规则是同一个思路。

> 封面（`cover_url`）**没有**① 的问题：它是前端 canvas 现画、当 `File` 单条上传的
> （`frontend/src/utils/cover.ts:150`），每条视频一张自己的。但它和 `.mp4` 一样在漏，
> GC 要一起扫。

### 删除的完整清理 + 文件 GC —— 存档，**到阶段9（MQ 与 Worker）再做**

2026-09-13 定下：上面那个问题（"为什么删视频不删文件"）的完整解法**推迟**，不是本轮做。
**本轮只存档，一行代码都没写。** 下面是全部结论，届时照它实施。

**三个已定的选择**

| # | 问题 | 选择 |
|---|---|---|
| 1 | 范围 | **文件 GC + DB 孤儿一起做**（两半共用同一套"删除后要清理什么"的骨架，分两轮等于写两遍） |
| 2 | 三个 URL 列的归属校验 | **发布时校验 + 删除时二次校验**。**这是行为变更**：现在「用别人的 `play_url` 发布」能过，之后会被拒（等于堵掉一个白嫖别人视频的口子） |
| 3 | 执行形态 | **独立 `cmd/` 命令**（幂等、可重跑），阶段9 有 Worker 后再搬成 ticker —— 只换调用点，逻辑不重写 |

选择 3 有项目内先例：`video_service.go:134-155` 已写明这条纪律 —— fire-and-forget 异步之所以敢做，
前提是"**有一个幂等的兜底命令**"。`cmd/gc` 就是 `cmd/backfill` 的兄弟（那个也还没做）。

**要两套机制，不是一套**——这是读完代码后推翻的第一个判断：

| 孤儿类别 | 有 DB 行能指到它吗 | 谁来收 |
|---|---|---|
| 视频行被删，`.mp4`/封面留下 | 有（删除时同事务写记录） | **待清理表 + Worker** |
| 换头像后旧头像被顶掉 | 有（更新时同事务写记录） | 同上 |
| **上传会话被放弃的 `.part`** | **没有** —— `init` 写了盘但从没 publish，DB 里没有任何行 | **会话 TTL + reaper** |
| 因分片几何变化被 `discard` 的旧 `.part` | 没有 | 已有：`chunk_handler.go:203` 唯一的 `os.Remove` |
| `init` 并发竞态里输掉的那个 `.part` | **没有，连内存 map 都没引用**（`put` 覆盖 `hashes[...]`） | 只能靠 TTL/reaper |

- **形态一 · 待清理表（DB 驱动的那一半）**：照抄 `video_service.go:88-131` 的 publish 事务形状
  （`outbox_msgs` 与视频行**同事务**写，"要么都在要么都不在"）。删除事务里（`deleteVideosTx`）
  额外写一条待清理记录（本次删掉的 `play_url`/`cover_url`），`cmd/gc` 逐条回查
  "`videos` 里还有没有行引用它" → 没有才 `os.Remove`。**优势不是"不用扫盘"，是候选集精确**：
  只有被删视频的路径会进来 ⇒ **不会碰到"正在上传、还没发布"的文件**。
- **形态二 · 会话 TTL / reaper（无 DB 引用的那一半）**：被放弃的 `.part` 只能靠它。
  **不选扫盘**的理由：扫盘要区分"孤儿"和"正在传"，而后者在**内存 map** 里，跨进程的 GC 拿不到。
  TTL 是唯一干净的判据，且落点**已经预留**：`chunk_handler.go:22` 的
  `sessionTTL = 24 * time.Hour` **声明着但全项目零引用**（注释"仅为语义占位"），
  同文件还有一行 `// RefreshTTL 阶段7回填`。阶段7 会话迁 Redis 时 TTL 自然生效。

**五个陷阱（都是读代码读出来的，不是推的）**

1. **存进库的是绝对 URL，host 由请求头决定。** `play_url`/`cover_url`/`avatar_url` 存的是
   `http://<Host 头>/static/...`，因为上传接口返回的是 `buildAbsoluteURL(c, urlPath)`：
   `if xf := c.GetHeader("X-Forwarded-Proto"); xf != "" { scheme = xf }` + `c.Request.Host`
   —— **scheme 和 host 都来自请求**，而且这个函数有**三份拷贝**
   （`video/video_handler.go:314`、`account/handler.go:251`、`chunk_handler.go:426`）。
   ⇒ 校验**必须先剥掉 `scheme://host` 再比 path 前缀**。照字面实现"要求 `/static/videos/<自己的 id>/` 前缀"
   会**把每一次合法发布都拒掉**。完整判据三条：能解析出 URL → path 以
   `/static/videos/<自己的 accountID>/` 开头 → 文件名匹配服务器唯一生成的那种形状
   （`randHex(16)` = 32 位小写十六进制）。
2. **`notification.TargetID` 是**多态列**，不能天真按它清。** 含义由 `Type` 决定
   （`notification/entity.go:34`，"阶段5 是 videoID"）。写 `WHERE target_id IN (...)` 会在
   将来 `Type` 变多（评论/关注/私信）时**误删无关通知**。正确：`WHERE type = 'mention' AND target_id IN (...)`，
   并把这条约束写进注释——多态列上的批量删除是经典事故点。
3. **`avatar_url` 有一条不落盘的写入路径。** 除 `UploadAvatar` 外，`UpdateProfile`
   （`account/handler.go:207` → `service.go:154`）接受**任意** `avatar_url` 字符串、不校验不落盘。
   ⇒ 库里这三个 URL 列全是**攻击者可控字符串**，不是可信的本地文件指针。GC 拿它当路径用会被
   `../` 引到任意文件上（和后端零校验的 `PublishVideoRequest` 是同一类问题）。
4. **共享文件：引用检查必须在删除那一刻做。** 不能把"谁是 leader 谁是 duplicate"存进待清理表 ——
   前端判重是**批次内**的，还能事后补选同内容文件复用已发布的 `play_url`（`uploadQueue.ts:308`），
   **服务端不知道谁是 leader**。判据只能是回查 DB 引用。（补充确认：**服务端零 dedup**，
   `chunkSessionStore.hashes` 是**断点续传索引、不是判重** —— 命中的语义是"返回同一个 `upload_id`
   继续写同一个文件"。所以"两行共用一个文件"完全是前端行为，外加一个并发 `init` 竞态放大器。）
5. **`likes_count` 不用动。** 见「已知缺口」#7 下面那段更正：它是 `videos` 行上的反范式列，
   视频行删了就跟着消失，没有残留账目。**实施前再核实一遍**，别照旧注释行事。

**顺带查出来、但不在 GC 范围内的两个问题**（当下就能被利用，记下来等你定）：

- **`.part` 文件被 HTTP 暴露**：`r.Static("/static", "./.run/uploads")`（`router.go:43`）服务
  **整棵树**，未完成的分片文件可按 `/static/videos/<id>/<date>/<hex>.mp4.part` 直接下载。
- **客户端声明的数字决定磁盘分配**：`InitChunkUpload` 一进来就
  `preallocate(session.partPath(), req.FileSize)`（`chunk_handler.go:186`），而 `req.FileSize`
  是**客户端给的**（上限 500MB）。`os.OpenFile` + `f.Truncate` 只移 EOF 不写零，但
  **一个已认证请求就能占 500MB 的盘**，重复发就是填满磁盘。

**落盘路径全量清单**（已查完，实施时直接用。GC 少清一个写入路径 = 永远漏删一类；多清一个 = 删错）

| 家族 | URL（服务端生成） | 磁盘 | 列 |
|---|---|---|---|
| 分片上传 complete | `/static/videos/<accountID>/<YYYYMMDD>/<32hex>.mp4`（`chunk_handler.go:181`） | `.run/uploads/videos/<id>/<date>/` | `play_url` |
| 直传 `UploadVideo` | **形状完全相同**（`video_handler.go:116`） | 同上 | `play_url` |
| 未完成的 `.part` | `/static/videos/<id>/<date>/<32hex>.mp4.part` | 同目录 `.part` | **无 DB 行** |
| 封面 `UploadCover` | `/static/covers/<accountID>/<YYYYMMDD>/<32hex>.<ext>`（`video_handler.go:174`） | `.run/uploads/covers/<id>/<date>/` | `cover_url` |
| 头像 `UploadAvatar` | `/static/avatars/<accountID>/<32hex>.<ext>`（`account/handler.go:198`）**无日期段** | `.run/uploads/avatars/<id>/` | `avatar_url`（另有上面的不落盘路径） |

写盘原语全集（已 grep 全 `internal/`）：`os.MkdirAll` ×4（`chunk_handler.go:158`、
`video_handler.go:98,156`、`account/handler.go:183`）、`c.SaveUploadedFile` ×3
（`video_handler.go:111,169`、`account/handler.go:194`）、`os.OpenFile` ×2
（`chunk_handler.go:208,300`）；`os.Remove` **仅** `chunk_handler.go:203`、
`os.Rename` **仅** `chunk_handler.go:418`。`os.Create` / `os.WriteFile` **0 处**。

另：`.run/uploads/tmp/` 是空目录、无任何源码引用（旧分片实现的残留），可以手工删。

## 备注

- 本机环境：Go 1.25 ✅ / Docker Desktop + WSL2 ✅（注意：Smart App Control 已关，否则拦编译产物）/ MySQL·Redis·RabbitMQ 容器 ✅ / npm 源已换 npmmirror
- 阶段7回填点：chunk 会话迁 Redis、GetDetail 防击穿缓存、限流中间件、token 缓存快路径
- 进阶实验清单：视频转码流水线（阶段9解锁，见任务#14）

### 已知缺口（本轮结束时仍然存在的）

| # | 缺口 | 为什么留着 / 接回来要做什么 |
|---|---|---|
| 1 | **向量那一路整套写在盘上但没接线**：`internal/search/embed.go`（`OllamaEmbedder`）、`internal/search/vector_repo.go`（`VectorStore`）、`service.go` 的 `vectorArm`/`IndexVideo`、`video.VectorIndexer`/`indexAsync`、`videos` 的两个 embedding 列、`ListMissingEmbedding`/`CountMissingEmbedding`/`MarkEmbedded` | 用户明确说"先跳过"。**没有构造点**（`router.go` 给 `NewService` 的 vec/embed 传的都是 `nil`，`NewVideoService` 的 vectorIdx 也是 `nil`），每个文件顶部有横幅写明本轮状态。代码是完整可用的，接回来需要：Redis 换 `redis/redis-stack-server` 镜像（`redis:7-alpine` 没有 RediSearch）、`EnsureIndex`、`ollama pull bge-m3`、一个幂等回填命令。**踩过的坑都记在 `internal/search/entity.go` 的包注释里**（余弦返回的是 distance 不是 similarity、`DIALECT 2` 必需、KNN 的 `LIMIT` 默认 10、先写 Redis 再写标记） |
| 2 | **标签在描述里是冗余的** | 见上面 §① 的引用块。`/feed` 开始返回标签 + FULLTEXT 覆盖到 `video_tags` 之后可以摘掉，摘之前不要动 |
| 3 | `internal/social/service.go` **不是 gofmt-clean** | 阶段6 遗留，不是本轮引入的。**刻意没格式化**：gofmt 会把它的括号续行改写成代码块，注释反而读起来更差。要么手工把那几行注释挪出续行，要么接受它 |
| 4 | 检索**只覆盖 `title + description`** | `video_tags` 不参与 FULLTEXT。所以"按标签找视频"目前只能走标签流 `/tag/:name`（精确），不能走检索（模糊） |
| 5 | 检索索引**没有 `*` 通配符** | 计划里就写了先不加：ngram 已把词切成 bigram，`*` 与 ngram 的交互是版本相关的，且有"搜索词是句末字符时 `*` 失效"的报告。先用裸 `+token`，实测漏检再加 |
| 6 | **删除视频不清磁盘文件**（`.run/uploads` 下的 `.mp4` 和封面 `.jpg` 全留着）。全项目唯一的 `os.Remove` 在 `chunk_handler.go:203`，那是**放弃的分片会话的 `.part`**，和删除无关 | 见下面 §「为什么删视频不删文件」。**不是漏了，是不该随手做**：① 一个文件可能被多条视频行共用；② `play_url` 是**客户端给的**，服务端没有归属凭证；③ MySQL 事务和文件系统之间没有原子性。要做得先解决 ①②，然后按"磁盘上有、`videos` 表无引用"做**异步 GC** |
| 7 | **删除只对 `videos` + `video_tags` 负责**：`likes` / `comments` / `outbox_msgs` 里指向已删视频的行会留下变孤儿 | `video_repo.go:55` 上面那段注释已经自认。`likes` 侧侥幸无害（`/like/listMyLikedVideos` 走 `GetByIDs`，孤儿查不到自然消失），但 `comments` 将来的评论数统计、阶段9 Poller 给已删视频发事件是实实在在的。**和 #6 一起做，方案见下面 §「删除的完整清理 + 文件 GC」** |

> **本条原先写的"做干净要跨模块（`like`/`comment` 各暴露一个 `DeleteByVideoIDs`）"是错的，已删。**
> `internal/video/` 下 17 个文件**全是 `package video`**，like / comment / tag / outbox 同包 ——
> 清理是**同包调用**，比原先估计的小得多。唯一的真跨包是 `notification`，而 `video` 本来就 import 它。
> （同样的错误说法也在 `video_repo.go:64-65` 的原始注释里，已一并改掉。）
>
> 同处原先还写了"`likes_count` 的账目是实实在在留下来的"——**这条未经核实，也删了**。
> `likes_count` 是 `videos` 行上的反范式列（`entity.go:14`），视频行删了它跟着消失；
> 全项目**没有任何地方从 `likes` 表重算它**（feed 直接按该列排序，`feed/repo.go:42-60`）。
> 风险不在"删视频"这个场景，在将来做共享视频/部分删除时。**实施前先核实再决定要不要清 `likes` 行。**
| 8 | `internal/social/service.go` 之外的 `internal/feed/entity.go:25` 注释过时 | `IsLiked bool \`json:"is_liked"\` // 当前用户是否点过赞（阶段4接入，现在恒false）` —— 阶段4 已经真的回填了（`feed/service.go:270` 的批量 `likedMap`），这行注释是假的 |

## 进阶实验待办（按解锁条件排序）

1. **视频转码流水线**（阶段9解锁）：上传完成后发转码消息 → Worker 消费 → ffmpeg 多码率/抽帧/HLS 切片 → 回写 video 表（对齐 B站/YouTube 工业级）
2. **Feed 推拉结合改造**（阶段9解锁）：全局时间线改造成"每用户收件箱 ZSET"；普通用户发布 fan-out 推送进粉丝收件箱，大V（粉丝超阈值）标记为 Pull 源；刷新 = 收件箱 + 实时查大V合并（微博/Twitter 同款方案）
3. **对象存储 + CDN + 预签名直传**（阶段10后最合适）：视频/封面存 OSS/S3，数据库只存 URL；上传走服务端签发的预签名凭证直传对象存储，500MB 流量不再经过 API 进程；播放 URL 走 CDN 边缘节点 + 签名防盗链
