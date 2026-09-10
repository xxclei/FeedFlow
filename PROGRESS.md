# 复刻进度追踪

> 对照 `howto_feed-rebuild/README.md` 的复刻总路线图，完成一个阶段就把 `[ ]` 改成 `[x]`。
> 规则：按顺序推进，每个阶段验收清单全过、`git commit` 之后才算完成。

- 当前进度：**阶段 0 进行中**（2026-09-10 开始）
- 新项目代码：`myfeed/`
- 参考答案：`origin-feed-project_example/feedsystem_video_go/`

## 路线图

| # | 阶段 | 文档 | 状态 | 验收要点 |
|---|---|---|---|---|
| 0 | 架构与环境准备 | [00](howto_feed-rebuild/00-架构与环境准备.md) | 🔨 进行中 | `go run ./cmd` 起服务，MySQL 表自动创建 |
| 1 | 账号模块 | [01](howto_feed-rebuild/01-账号模块.md) | ⬜ 未开始 | Postman 走完注册→登录→带 token 访问 |
| 2 | 视频模块 | [02](howto_feed-rebuild/02-视频模块.md) | ⬜ 未开始 | 上传 mp4 → 发布 → 播放地址可访问 |
| 3 | Feed 模块 | [03](howto_feed-rebuild/03-Feed模块.md) | ⬜ 未开始 | 两种游标翻页不重不漏 |
| 4 | 点赞模块 | [04](howto_feed-rebuild/04-点赞模块.md) | ⬜ 未开始 | 计数正确，重复点赞被拦截 |
| 5 | 评论模块 | [05](howto_feed-rebuild/05-评论模块.md) | ⬜ 未开始 | 只有作者能删评论 |
| 6 | 关注模块 | [06](howto_feed-rebuild/06-关注模块.md) | ⬜ 未开始 | 关注后关注流出现对方视频 |
| 7 | Redis 缓存 | [07](howto_feed-rebuild/07-Redis缓存.md) | ⬜ 未开始 | 停 Redis 业务不挂；redis-cli 能看到 key |
| 8 | 热榜 | [08](howto_feed-rebuild/08-热榜.md) | ⬜ 未开始 | 翻页榜单不抖；停 Redis 降级 MySQL |
| 9 | RabbitMQ 与 Worker | [09](howto_feed-rebuild/09-RabbitMQ与Worker.md) | ⬜ 未开始 | 点赞秒回异步落库；停 MQ 直写兜底 |
| 10 | Docker 部署 | [10](howto_feed-rebuild/10-Docker部署.md) | ⬜ 未开始 | `docker compose up -d --build` 全部起来 |
| 11 | 前端（可选） | [11](howto_feed-rebuild/11-前端.md) | ⬜ 未开始 | 浏览器完整走一遍用户旅程 |
| 12 | 私信与 SSE 实时通知 | [12](howto_feed-rebuild/12-私信与SSE实时通知.md) | ⬜ 未开始 | 两个账号互发私信；点赞触发实时通知 |

## 阶段 0 验收清单

- [ ] MySQL 环境就绪（Docker compose 或本地安装）
- [ ] `cd myfeed && go run ./cmd` 启动无报错，日志打印端口
- [ ] `show tables` 能看到自动建的表（accounts）
- [ ] `curl http://127.0.0.1:8080/` 返回 404（Gin 已在工作）
- [ ] git commit 本阶段

## 备注

- 本机环境：Go 1.25 ✅ / Redis 已有（C:\Redis）✅ / MySQL ❌ / Docker ❌ / WSL2 ❌
- 已知待修：`internal/config/loadingconfig.go` 的 RabbitMQ yaml tag 写错（`"rabbitmQ "`）
