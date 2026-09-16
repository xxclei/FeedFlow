# FeedFlow · 短视频内容分发平台

Go + Gin + MySQL + Redis + RabbitMQ + Vue3 全栈实现，覆盖 **投稿上传 → Feed 分发 → 互动 →
检索 → 实时通知 → 多档 HLS 转码播放** 完整链路，并以 Docker Compose + nginx 单栈部署上线公网。

约 **3.6 万行**（Go 22,219 行 / Vue3 + TS 13,402 行）· **45 个 REST API** · **12 个前端页面** · **6 条 MQ 业务链路**

> 代码与文档里的模块名、目录名仍是 `myfeed`（`module myfeed`），项目对外名称是 FeedFlow。

---

## 核心实现

四条，每条都带实测数据。没测过的不写。

### 1. Feed 流：Redis 三级缓存 + 推拉结合

用 **Redis ZSET** 做三级缓存承载 Feed 分发：热榜 ZSET + 冷快照 + MySQL 兜底。视频详情接口
**QPS 提升 338%**，SQL 从 1.00 条/请求降到 0.00 条 —— 数据库开销被完全消掉。

另外设计了推拉结合的分发方案：发布事件写扩散投递到粉丝收件箱，粉丝数超过阈值的头部账号
改成拉取回查，避免 O(粉丝数) 的写放大。

> 缓存部分**已实现并实测**；推拉结合**只有设计，代码没写** —— 方案见
> [设计文档](myfeed/doc/README.md)，为什么不写的理由见 [doc/06 §八](myfeed/doc/06-简历定稿.md)。

### 2. 点赞 / 评论 / 关注：RabbitMQ 异步化

点赞、评论、关注三类写操作全部经 **RabbitMQ** 异步化，用 Outbox 模式保证投递不丢，
由独立的 Worker 进程消费。点赞链路请求侧写延迟**降低 72%**。

但实测下来，**端到端反而升高 18%，系统总 SQL 11 条变 11 条** —— 异步只是把工作从请求路径
搬到 Worker，真正买到的是缩短请求路径和抗下游抖动，不是"少干活"。

评论链路请求侧 SQL 降低 60~67%，关注链路 0%。差别不在用没用 MQ，**在于原本有没有跨表事务**。

### 3. 视频上传、转码与播放

分片上传用 `Truncate` 预分配到最终文件、按偏移直写，传完只做一次 `os.Rename`，
**不需要合并临时文件**，也就没有"最后拼一遍"的窗口期，逐片 MD5 校验支持断点续传。

上传时解析 `moov` 判断要不要转码，转出 1080p / 720p / 480p 三档 HLS，
码率保真度 **93.8% ~ 106.7%**；前端用 hls.js 按带宽自适应切档。

播放质量有前端埋点上报（首帧时间、卡顿、码率、切档次数），后端聚合成 QoE 看板。
实测首帧 p50 **1437 ms**、卡顿率 **2.70%**、码率均值 4585 kbps、切档 0 次
（**样本只有 7 个会话**，p95 已退化成最大值，只能当趋势看）。

### 4. 压测表现

| | 结果 |
|---|---|
| 视频详情缓存 | SQL **1.00 → 0.00 条/请求**，QPS **4944 → 21648（+338%）**，落回"零 SQL"基线 21454 |
| 点赞链路 MQ 异步化 | 请求侧写延迟 **−72%**，但端到端 **+18%**、系统总 SQL **11 = 11 不变** |
| 分布式防击穿锁 | DB 延迟 ≥150ms 时**锁完全失效且比不加锁更慢**（275.0 → 302.8ms） |
| 静态分发能力 | 服务端单流 9.38 Gbps、30 并发 11.53 Gbps，**瓶颈是 12 Mbps 家宽上行** |

锁那行的原因：等待预算 = 轮数 × 步长（5 × 20ms = 100ms）。DB 一旦慢过 100ms，
等锁的请求会在锁释放前全部超时放弃，锁形同不存在，还白花一次 Redis 往返。

**测量口径**：全部为**本机单机** `127.0.0.1`，不是云服务器。QPS / 延迟用 hey（n=8000 / c=80）；
SQL 计数取 `Com_stmt_execute` 而非 `Questions`（后者混进握手与 prepare，会报出 1.84 这种非整数）；
静态分发用 `cmd/staticprobe` 探针 + curl 打 Cloudflare 测速点；码率用 `mp4_probe.py` 解析 mp4 头；
QoE 是 7 个真实会话的埋点聚合。**云服务器上没有任何性能数据** —— 上云买到的是公网可达，
不是更快。完整数据见 [`myfeed/doc/03-性能数据.md`](myfeed/doc/03-性能数据.md)。

---

## 技术栈

**后端** `Go 1.26` `Gin` `GORM` `MySQL 8`（`FULLTEXT + ngram`）`Redis 7`（ZSET / 三级缓存 / 限流 / 防击穿锁）
`RabbitMQ`（topic + 死信 + 广播副本队列）`JWT 双 token` `ffmpeg`

**前端** `Vue 3` `TypeScript` `Vite` `Pinia` `hls.js` `SparkMD5`

**工程** `Docker`（多阶段构建）`Docker Compose` `nginx` `hey` `MySQL general log` `EXPLAIN`

---

## 架构

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
                         │            │  ├ TimelineConsumer│
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

**四条硬约束**（都是上云时定的）：

| 约束 | 理由 |
|---|---|
| mysql / redis / rabbitmq **一律 `expose` 不 `ports`** | 开发 compose 把它们映射到宿主机 3307 / 15672，搬到有公网 IP 的机器上就是弱密码数据库直接对公网开放 |
| 只有 nginx 发布端口 | Go 的 `/healthz`、`/static` 都不直接暴露 |
| api 与 worker **同镜像、同 `WORKDIR`、共享 uploads volume** | 配置、上传根目录、ffmpeg 目录三处都是 CWD 相对路径，不一致时 worker 找不到 API 写的文件，**且失败是静默的** |
| `/static/` 仍由 Go 提供，nginx 只反代 | 保留 [`static.go`](myfeed/internal/http/static.go) 那张按扩展名的缓存策略表（`.m3u8→no-cache`、`.ts→immutable`、`.mp4→86400`）。用一次内存拷贝换一份唯一真相 |

---

## 快速开始

**1. 起依赖**（MySQL 3307 / Redis 6379 / RabbitMQ 5672 + 管理台 15672）

```bash
docker compose -f myfeed/deploy/docker-compose.deps.yml up -d
```

**2. 起后端**（首次启动会 `AutoMigrate` 建表，并检测全文索引配方是否一致）

```bash
cd myfeed
go run ./cmd            # :8080
go run ./cmd/worker     # 另开一个终端，跑 MQ 消费者 + 转码
```

> `go.mod` 声明 `go 1.26.0`。本机工具链若低于此版本，`GOTOOLCHAIN=auto`（Go 默认）
> 会自动拉取对应版本，首次编译会慢一些。

**3. 起前端**

```bash
cd myfeed/frontend
npm install && npm run dev      # vite dev server，已配 /api 与 /static 代理
```

**4. 配置** —— 开发配置在 [`myfeed/configs/config.yaml`](myfeed/configs/config.yaml)，
默认指向上面那三个依赖容器。**生产配置模板**见
[`configs/config.prod.example.yaml`](myfeed/configs/config.prod.example.yaml)，
真值（含 `JWT_SECRET`）不入库。

> ⚠️ 本机跑测试时注意：`go test ./internal/video/...` 会连**真实数据库**，
> 并可能 `DROP` + `ADD` 全文索引、`OPTIMIZE` 整张 `videos` 表。
> **不要在装有生产数据的机器上跑它。**

---

## 目录导航

| 路径 | 内容 |
|---|---|
| [`myfeed/doc/`](myfeed/doc/) | **技术档案**：架构与规模、阶段回顾、**全部实测性能数据**、疑难问题排查、简历素材 |
| [`PROGRESS.md`](PROGRESS.md) | **开发日志**：逐条改动、当时的决策理由、已知缺口 |
| [`myfeed/scripts/bench-notes-*.md`](myfeed/scripts/) | **压测原始笔记**：单次测量的完整过程与翻车记录 |
| [`howto_feed-rebuild/`](howto_feed-rebuild/) | **复刻指导手册**：13 篇分阶段教程（本项目的实现路线图） |

---

## 未完成的部分

写在这里，是因为**知道边界在哪比假装没有边界更有用**。

| 项 | 状态 |
|---|---|
| **Feed 推拉结合** | **仅设计，代码未实施** —— 前置依赖（Outbox 投递链路、ZSET 原语、游标语义）已全部就位 |
| **过载降级（动态 master / 503）** | 未开始。所以服务端在过载时不会主动缩档，只能靠客户端 ABR 自己扛 |
| **向量检索** | `embed.go` / `vector_repo.go` 完整可用，但 `router.go` 给 `NewService` 传的是 `nil`，**没有构造点**。检索目前只有词法那一路（`FULLTEXT + ngram`） |
| **并发吞吐 / 饱和点** | **完全没测**。现有数据全是顺序单发，所以本文不写"高并发" |
| **云服务器性能数据** | 一条都没有。上云只验证了"能跑通"，没验证"跑得快" |
| **删除视频不清理磁盘文件** | 库里的行删了，`.mp4` 与封面、转码产物都留着。云盘按容量计费，长期会满 |
| **未完成的分片上传残留** | 上传会话在进程内存里、无 TTL。API 重启后 `.part` 文件（每个预分配到 500MB）再无人回收 |

更完整的清单见 [`myfeed/doc/01-架构与规模.md`](myfeed/doc/01-架构与规模.md) §10（10 条已知缺口）
与 [`myfeed/doc/03-性能数据.md`](myfeed/doc/03-性能数据.md) §11（9 项没测的东西）。
