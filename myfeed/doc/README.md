# myfeed · 项目文档

> 短视频 Feed 系统（Go + Vue3 全栈）。本文档目录是**项目的完整技术档案**：
> 从功能范围、架构设计、逐阶段实现记录，到全部实测性能数据与疑难问题排查过程。
>
> 与仓库其它文档的分工：
>
> | 文档 | 管什么 |
> |---|---|
> | `doc/`（本目录） | **技术档案**：做了什么、怎么做的、数据是多少 |
> | `../PROGRESS.md` | **开发日志**：逐条改动、当时的决策理由、已知缺口 |
> | `../scripts/bench-notes-*.md` | **压测原始笔记**：单次测量的完整过程与翻车记录 |
> | `../../howto_feed-rebuild/` | **复刻指导手册**：13 篇分阶段教程（含参考答案） |

## 目录

| 文件 | 内容 |
|---|---|
| [01-架构与规模.md](01-架构与规模.md) | 技术栈、系统架构、数据模型、MQ 拓扑、缓存 key 清单、代码规模 |
| [02-阶段回顾.md](02-阶段回顾.md) | 阶段 0~12 + 两个扩展，逐阶段做了什么、遇到什么 |
| [03-性能数据.md](03-性能数据.md) | **全部实测数字**：基线、各缓存点前后对比、MQ 化对比、分发瓶颈、转码、QoE |
| [04-疑难问题排查.md](04-疑难问题排查.md) | 10 个真实 bug 的完整根因分析与修法 |
| [05-简历素材.md](05-简历素材.md) | 可直接引用的条目、需要加限定词的条目、**不能写**的条目 |

---

## 一、这是什么

一个**从零复刻的短视频 Feed 系统**，覆盖 B 站 / 抖音类产品的核心闭环：
投稿 → 分发 → 消费 → 互动 → 通知，外加生产环境的部署与可观测性。

它不是教程跟做，而是一次**实证驱动**的重写：每个性能结论都有前后对比测量，
每次"优化"都要求证明自己真的优化了 —— 项目里保留了三次**优化无效甚至更慢**的实测记录
（见 [03-性能数据.md](03-性能数据.md) §3~§4）。这部分比顺利的部分更有价值。

### 功能范围

| 模块 | 内容 |
|---|---|
| **账号** | 注册 / 登录 / JWT 双 token（access + refresh 单飞续期）/ 改密 / 头像 / 公开主页 |
| **视频** | 直传 + **分片上传**（断点续传、逐片 MD5 校验、**按偏移直写最终文件**）/ 批量投稿（拖文件夹）/ 封面 canvas 抽帧 / 批量删除 / 批量标签 |
| **Feed** | 五种流：最新（时间游标）/ 点赞榜 / 热度榜 / 标签流 / 关注流（复合游标），游客可刷 |
| **互动** | 点赞、评论（@提及）、关注、私信 |
| **检索** | `FULLTEXT + ngram` 模糊检索，三档降级（AND → OR → LIKE 兜底），冻结令牌游标 |
| **通知** | SSE 长连接实时推送 + 通知中心（红点未读、按类型分流跳转） |
| **播放** | 上传质量门禁（读 moov）/ 转码三档 HLS（1080p/720p/480p）/ hls.js + ABR / 手动清晰度 / **QoE 埋点与看板** |
| **运维** | Docker Compose 单栈 + nginx 入口 + 公网部署，可观测性（全链路 SQL 计数、调试面板） |

### 技术栈

**后端**：Go 1.25 · Gin · GORM · MySQL 8（`FULLTEXT + ngram`）· Redis 7 · RabbitMQ（topic + 死信）· ffmpeg
**前端**：Vue 3 · TypeScript · Vite · Pinia · hls.js · SparkMD5
**部署**：Docker 多阶段构建 · Docker Compose · nginx

### 代码规模

| | 文件数 | 行数 |
|---|---|---|
| Go 后端 | 98 | 22,219 |
| Vue3 + TS 前端 | 63 | 13,402 |
| **合计** | **161** | **约 35,600** |

45 个注册 API 路由 · 18 个 `internal/` 包 · 12 个前端页面

---

## 二、系统架构

```
                       公网 IP : 80
                            │
                     ┌──────▼──────┐
                     │    nginx    │   ← 唯一对外发布端口的容器
                     └──┬───┬───┬──┘
         / (SPA dist)    │   │   │  /api/*  → 剥掉 /api 前缀
                         │   │   └──────────────┐
                         │   └── /static/* 原样转发（Range 透传）
                         │                      │
                         │            ┌─────────▼─────────┐
                         │            │  myfeed-api (Go)  │  :8080 仅容器内
                         │            │  ├ HTTP 路由      │
                         │            │  ├ TimelineConsumer│  ← 消费者跑在 API 进程里
                         │            │  ├ NotificationWorker (×3 队列)
                         │            │  ├ OutboxPoller（1 秒一轮）
                         │            │  └ SSEHub         │
                         │            └─────────┬─────────┘
                         │                      │
                         │   ┌──────────────────┼──────────────────┐
                         │   │                  │                  │
                         │ mysql:3306     redis:6379      rabbitmq:5672
                         │   │                  │                  │
                         │   └──────────────────┼──────────────────┘
                         │                      │
                         │            ┌─────────▼─────────┐
                         │            │ myfeed-worker(Go) │  同镜像 + ffmpeg
                         │            │ ├ LikeWorker      │
                         │            │ ├ CommentWorker   │
                         │            │ ├ SocialWorker    │
                         │            │ ├ PopularityWorker│
                         │            │ └ TranscodeWorker │
                         │            └─────────┬─────────┘
                         └──────────────────────┤
                                    共享 volume: uploads
```

**四条硬约束**（都是上云时定的，理由见 [04-疑难问题排查.md](04-疑难问题排查.md)）：

| 约束 | 理由 |
|---|---|
| mysql / redis / rabbitmq **一律 `expose` 不 `ports`** | 开发 compose 把它们映射到宿主机的 3307 / 15672，搬到有公网 IP 的机器上就是弱密码数据库直接对公网开放 |
| 只有 nginx 发布端口 | Go 的 `/healthz`、`/static` 都不直接暴露 |
| api 与 worker **同镜像、同 `WORKDIR`、共享 uploads volume** | 配置、上传根目录、ffmpeg 目录三处都是 CWD 相对路径，不一致时 worker 找不到 API 写的文件，**且失败是静默的** |
| `/static/` 仍由 Go 提供，nginx 只反代 | 保留 `static.go` 那张按扩展名的缓存策略表（`.m3u8→no-cache`、`.ts→immutable`、`.mp4→86400`）。用一次内存拷贝换一份唯一真相 |

---

## 三、Feed 推拉结合设计（Write Fan-out + Big-V Pull）

> **状态：设计完成，代码未实施。** 明确标注在此，不是为了充数 ——
> 它是现有分发架构的**必然下一步**，也是理解当前实现为什么那样写的前提。

### 落地步骤

1. `cmd/worker` 加一个 `InboxFanoutWorker`，消费**已存在**的 `video.timeline.update.queue`
   （与现有 `TimelineConsumer` 共用交换机、各挂一条队列 —— 正是 `topology.go` 里
   "广播副本 vs 竞争消费"那条规则的又一次应用）
2. 消费时查发布者的粉丝列表，`ZADD` 进每个粉丝的 `v1:feed:inbox:<uid>`，
   并对每条收件箱执行 `ZRemRangeByRank` 剪枝
3. 粉丝数超阈值的账号写进 `v1:feed:bigv` 集合，**跳过 fan-out**
4. `/feed/listByFollowing` 改成"读收件箱 + 回查 bigv"两路归并，
   现有的复合游标语义可以直接复用（收件箱本身按 score 有序）

### 为什么需要它

现在的两种流是**两个极端**，都不完整：

| 流 | 当前实现 | 问题 |
|---|---|---|
| `feed/listLatest` 最新流 | 全局时间线 ZSET `v1:feed:global_timeline` + Redis + MySQL 三级缓存 | **无个性化** —— 全体用户共享一个 ZSET |
| `feed/listByFollowing` 关注流 | 读时按关注关系 JOIN 回查（纯 **Pull**） | 粉丝关系一多，每次刷新都要扫关注表；且无法与全局流合并排序 |

推拉结合（微博 / Twitter 同款）要解决的是：**让关注流变成一次 ZSET 读取，同时不给大 V 制造写风暴。**

### 设计

```
用户发布视频
     │
     ▼  publish 事务（与 outbox_msgs 同事务）
  outbox_msgs  ──OutboxPoller（幂等、可重跑）──▶  video.timeline.events
                                                        │
                                          ┌─────────────┴─────────────┐
                                          ▼                           ▼
                              全局时间线 ZSET                 粉丝收件箱 Fan-out
                        v1:feed:global_timeline          对每个粉丝 uid：
                                                          ZADD v1:feed:inbox:<uid>
                                                          （score = create_time）
                                                          └ 大 V（粉丝数 ≥ 阈值）**跳过**
                                                            并加入 bigv 集合
```

**读取时合并（`/feed/listByFollowing`）**：

```
result = ZREVRANGEBYSCORE( v1:feed:inbox:<我的uid>  )     ← 推来的部分，一次读取
       ∪ 实时回查( 我关注的 ∈ bigv 集合 )                  ← 拉取的部分，只查这几个
       → 归并排序取前 N 条
```

### 核心决策

| # | 决策 | 理由 |
|---|---|---|
| 1 | **大 V 阈值**（粉丝数超阈值不推） | fan-out 的成本是 O(粉丝数)。写扩散的 write amplification 是这套方案唯一的死穴，而它由幂律分布决定 —— 只要把头部切出去，总写入量从 O(用户数 × 平均粉丝数) 降成可控 |
| 2 | **收件箱必须剪枝**（`ZREMRANGEBYRANK` 只留最近 N 条） | 否则每个用户的收件箱无界增长，这是纯 Pull 没有的内存开销；现有 `latest_cache.go` 已经在全局 ZSET 上做过同样的裁剪，可直接复用该模式 |
| 3 | **新关注大 V 不补历史** | 一次关注要写这个人的全部历史 = 又一次写风暴。正确做法是"从现在开始"，历史按需回填 |
| 4 | **fan-out 必须异步**（走 MQ，不在 publish 请求里做） | publish 接口从"写 1 条 ZSET"变成"写 1 + N 条"，放进请求路径会让发布接口的 p99 直接爆炸 |
| 5 | **幂等**（消费端按 `(uid, videoID)` 去重） | RabbitMQ 是 at-least-once。重投会让同一条视频在收件箱里出现两次，而 ZSET 的成员相同、score 相同 → `ZADD` 天然幂等，但**大 V 回查那一路需要额外去重** |

### 为什么它挂在"阶段 9 解锁"

前置条件**已经全部就位**，这是它现在能实施的原因：

- `internal/middleware/rabbitmq/timelineMQ.go` —— 时间线链路已建（交换机 `video.timeline.events`，队列 `video.timeline.update.queue`）
- `internal/worker/outboxworker.go` —— outbox 轮询器已在跑，**实测首次端到端 245 条 pending 分三批 100/100/45 投完，`ZCARD v1:feed:global_timeline = 245`**
- `internal/middleware/redis/zset.go` —— `ZAdd` / `ZRemRangeByRank` / `ZRevRangeByScore` 原语齐全
- 现有四种流的**游标语义在收件箱上依然成立** —— 收件箱本身就是按 score 有序的，时间游标可以直接复用

### 诚实的代价

- **写放大**：一个 10 万粉丝的账号发一条视频 = 10 万次 `ZADD`。阈值调参是这套方案回到现实的关键旋钮。
- **一致性延迟**：收件箱是异步写的，用户发完视频**自己的关注者不会立刻看到**（毫秒~秒级）。这与现有"最新流 ZSET 也是异步 ZAdd"的行为一致，不是新问题。
- **未做压测**：本项目的全部性能数字都在**本机单机**取得，fan-out 的写放大只有在真实粉丝规模下才有意义 —— **没有数据支撑，所以不声称任何性能收益**。

---

## 四、这个项目的测量纪律

理解后续所有数字的前提：

| 项 | 约定 |
|---|---|
| **SQL 计数器** | 一律 `SHOW GLOBAL STATUS LIKE 'Com_stmt_execute'`，**绝不用 `Questions`** —— 后者混进握手与 `Com_stmt_prepare`，实测报出 1.84 / 2.10 这种非整数 |
| **环境** | 全部为**本机 Windows 11 + `127.0.0.1:8080` + Docker 三依赖容器**。**云服务器没有任何实测数字** |
| **归因方式** | 关键结论用 **MySQL general log 逐条归因**，不是总数相减 —— 因为部分消费者跑在 API 进程里，停 `worker.exe` 关不掉 |
| **压测工具** | `hey`（无状态路由）+ 自建一次性 Go 程序（有状态路由，见下） |
| **有状态路由** | `like` / `comment` / `social` **hey 打不了** —— 同一账号连打 8000 次，7999 次走的是"已点赞短路"路径，会得出"优化无效"的完全错误结论 |
