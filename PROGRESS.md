# 复刻进度追踪

> 对照 `howto_feed-rebuild/README.md` 的复刻总路线图，完成一个阶段就把状态列改掉。
> 规则：按顺序推进，每个阶段验收清单全过、`git commit` 之后才算完成。

- 当前进度：**阶段 2 已完成，下一阶段 3（Feed 模块）**（2026-09-12）
- 新项目代码：`myfeed/`（后端）+ `myfeed/frontend/`（Vue3 前端，随模块生长）
- 参考答案：`origin-feed-project_example/feedsystem_video_go/`

## 路线图

| # | 阶段 | 文档 | 状态 | 验收要点 |
|---|---|---|---|---|
| 0 | 架构与环境准备 | [00](howto_feed-rebuild/00-架构与环境准备.md) | ✅ 2026-09-10 | `go run ./cmd` 起服务，MySQL 表自动创建 |
| 1 | 账号模块 | [01](howto_feed-rebuild/01-账号模块.md) | ✅ 2026-09-10 | Postman 走完注册→登录→带 token 访问 |
| 2 | 视频模块 | [02](howto_feed-rebuild/02-视频模块.md) | ✅ 2026-09-12 | 直传/分片上传/发布事务/outbox；前端发布页可传可播 |
| 3 | Feed 模块 | [03](howto_feed-rebuild/03-Feed模块.md) | 🔨 进行中 | 两种游标翻页不重不漏 |
| 4 | 点赞模块 | [04](howto_feed-rebuild/04-点赞模块.md) | ⬜ 未开始 | 计数正确，重复点赞被拦截 |
| 5 | 评论模块 | [05](howto_feed-rebuild/05-评论模块.md) | ⬜ 未开始 | 只有作者能删评论 |
| 6 | 关注模块 | [06](howto_feed-rebuild/06-关注模块.md) | ⬜ 未开始 | 关注后关注流出现对方视频 |
| 7 | Redis 缓存 | [07](howto_feed-rebuild/07-Redis缓存.md) | ⬜ 未开始 | 停 Redis 业务不挂；redis-cli 能看到 key |
| 8 | 热榜 | [08](howto_feed-rebuild/08-热榜.md) | ⬜ 未开始 | 翻页榜单不抖；停 Redis 降级 MySQL |
| 9 | RabbitMQ 与 Worker | [09](howto_feed-rebuild/09-RabbitMQ与Worker.md) | ⬜ 未开始 | 点赞秒回异步落库；停 MQ 直写兜底 |
| 10 | Docker 部署 | [10](howto_feed-rebuild/10-Docker部署.md) | ⬜ 未开始 | `docker compose up -d --build` 全部起来 |
| 11 | 前端总装 | [11](howto_feed-rebuild/11-前端.md) | 🔶 随行完成约 30%（骨架/登录/发布页/自动分片已就绪） | 浏览器完整走一遍用户旅程 |
| 12 | 私信与 SSE 实时通知 | [12](howto_feed-rebuild/12-私信与SSE实时通知.md) | ⬜ 未开始 | 两个账号互发私信；点赞触发实时通知 |

## 前端随行记录（DESIGN.md 风格）

- ✅ 骨架：Vite+Vue3+TS、路由守卫、pinia auth store、fetch 封装（401 自动续期单飞）
- ✅ 登录/注册/首页
- ✅ 视频发布页：直传 + **>5MB 自动分片（断点续传）** + 发布 + 作品列表页内播放
- ✅ TagInput 组件：#标签实时高亮框 + 徽章（正则与后端 ExtractTags 一致）
- ⬜ 阶段3+：Feed 滑动流 / 点赞交互 / 评论抽屉 / 热榜页 / SSE通知

## 备注

- 本机环境：Go 1.25 ✅ / Docker Desktop + WSL2 ✅（注意：Smart App Control 已关，否则拦编译产物）/ MySQL·Redis·RabbitMQ 容器 ✅ / npm 源已换 npmmirror
- 阶段7回填点：chunk 会话迁 Redis、GetDetail 防击穿缓存、限流中间件、token 缓存快路径
- 进阶实验清单：视频转码流水线（阶段9解锁，见任务#14）
