# 09 - RabbitMQ 异步化 + Worker 进程 ★ 并发正确性终极关卡

前置：阶段 8。把点赞/评论/热度从"同步写"改造成"发事件 + 独立进程消费"，同时**保留阶段 4/5 写的直写代码当降级路径**——前面没有一步是白写的。

## 目标

4 条 topic 事件链路；独立 `cmd/worker` 进程消费（手动 Ack、Qos、幂等）；API 接口"双队列发布 + 按目标降级直写"；Worker 崩溃重启不丢消息、不重复计数。

## 拓扑总表（先背下来）

| Exchange (topic) | Routing Key | Binding | Queue | 消费者 | 消费动作 |
|---|---|---|---|---|---|
| `like.events` | `like.like` / `like.unlike` | `like.*` | `like.events` | LikeWorker | 写 likes 表 + 改计数/热度 |
| `comment.events` | `comment.publish` / `comment.delete` | `comment.*` | `comment.events` | CommentWorker | 写/删评论 + 热度 |
| `social.events` | `social.follow` / `social.unfollow` | `social.*` | `social.events` | SocialWorker | 写 social 表（原项目为冗余消费者） |
| `video.popularity.events` | `video.popularity.update` | `video.popularity.*` | `video.popularity.events` | PopularityWorker | 更新 Redis 热榜 + 失效详情缓存 |

要点：exchange/queue **durable**；消息 **Persistent**；binding 通配符 `like.*` 匹配两种 routing key。

## 关键设计决策（本项目精华）

**Q1：为什么点赞要发两条消息而不是一条？** 因为两条链路的目标不同：`like.events` → 落 MySQL；`video.popularity.events` → 更新 Redis。目标、消费者、失败域完全不同，**分开才能按目标独立降级**。

**Q2：发布失败怎么办？（降级矩阵，必考）**

```
likeMQ 发布失败？  → 降级：事务直写 MySQL（阶段 4 写的那段代码！）
popularityMQ 失败？ → 降级：直调 UpdatePopularityCache 写 Redis（阶段 8 写的函数！）
两个 flag 相互独立，只对失败的目标降级，成功的不动
```

**Q3：MQ 会不会重复投递？Worker 怎么幂等？** 会（at-least-once：Ack 前崩溃必重投）。幂等三板斧：
1. **唯一索引**：重复 INSERT 撞 1062 → `LikeIgnoreDuplicate` 返回 created=false
2. **affected rows 判定**：`DeleteByVideoAndAccount` 返回 deleted bool，**没删到行就不动计数**
3. 解析失败的消息直接 Ack 丢弃（毒消息不能无限 requeue 堵队列）

原则：**先做"会造成副作用"的动作并确认真的发生了，再改计数**。

**Q4：消费失败怎么处理？** 手动模式：成功 `d.Ack(false)`；失败 `d.Nack(false, true)` 重新入队。
⚠️ 原项目的隐患：永久性错误（如数据已不存在引发的约束失败）也会 requeue → **无限重试**。你的改进：Nack(requeue=false) 进 DLX，或计数重试 N 次后丢弃+告警日志。

**Q5：为什么 Worker 是独立进程？** API 进程职责是低延迟响应，重活（DB 批量写）剥离出去**独立伸缩/重启**；Qos(prefetch=50) 控制 Worker 积压。

**Q6：Worker 进程怎么组织？** `cmd/worker/main.go`：连 DB（必选）→ 连 Redis（可选降级）→ 连 MQ（**必选**，Worker 没有 MQ 就没有存在意义 → `log.Fatalf`）→ 声明 4 组拓扑 → `Qos(50)` → 4 个 worker 各一个 goroutine `Run(ctx)` → `errCh` 收第一个退出错误 → `signal.NotifyContext` 优雅退出。

## 实现提示

**发布端**（`internal/middleware/rabbitmq/`，5 个文件）：

```go
type RabbitMQ struct{ conn *amqp.Connection; ch *amqp.Channel }
func NewRabbitMQ(cfg) (*RabbitMQ, error)      // Dial + Channel
func (r *RabbitMQ) DeclareTopic(exchange, queue, bindingKey) error
func (r *RabbitMQ) PublishJSON(ctx, exchange, routingKey string, payload any) error
// Persistent + ContentType application/json

type LikeMQ struct{ *RabbitMQ }                // 嵌入基座
func (l *LikeMQ) Like(ctx, userID, videoID uint) error
// 事件结构体带 EventID(随机hex) Action OccurredAt —— 为排查和将来幂等留钩子
```

**消费端骨架**（4 个 worker 同构，写完一个复制改）：

```go
func (w *LikeWorker) Run(ctx context.Context) error {
    deliveries, err := w.ch.Consume(w.queue, "", false /*manual ack*/, false, false, false, nil)
    for {
        select {
        case <-ctx.Done(): return ctx.Err()
        case d, ok := <-deliveries:
            if !ok { return errors.New("deliveries channel closed") }  // 连接断开，进程退出靠重启
            w.handleDelivery(ctx, d)
        }
    }
}
func (w *LikeWorker) applyLike(ctx, userID, videoID) error {
    if !视频存在 { return nil }                      // 业务上无效的消息不算失败
    created, err := w.likes.LikeIgnoreDuplicate(...)
    if err != nil { return err }
    if !created { return nil }                       // 幂等：已存在，什么都不做
    ChangeLikesCount(+1); ChangePopularity(+1)       // gorm.Expr 原子
}
```

**Service 改造**（以 Like 为例，Unlike/Comment 同构）：

```go
mysqlEnqueued, redisEnqueued := false, false
if s.likeMQ != nil { if err := s.likeMQ.Like(ctx, uid, vid); err == nil { mysqlEnqueued = true } }
if s.popularityMQ != nil { if err := s.popularityMQ.Update(ctx, vid, 1); err == nil { redisEnqueued = true } }
if mysqlEnqueued && redisEnqueued { return nil }     // 全部异步成功，接口立即返回
if !mysqlEnqueued   { /* 阶段4的事务直写 */ }
if !redisEnqueued   { UpdatePopularityCache(...) }   // 阶段8的函数
```

## 常见的坑

- Worker 里用 `autoAck=true` → 处理中崩溃消息直接丢
- 忘 `Qos` → MQ 把上千条消息推给一个 Worker 内存爆掉
- channel 不是线程安全的：**发布用一个 channel，每个 Worker 自己持有自己的 channel/或共用需加锁**（原项目 API 端共用一条 ch 做发布、Worker 端共用一条 ch 做消费——量小可行，进阶改连接池）
- `amqp.Dial` 的 URL 密码里有特殊字符要 `url.QueryEscape`
- 改造后忘了删同步路径的重复逻辑 → 一次点赞计数 +2（MQ 消费一次 + 降级又直写一次）；**两个 flag 逻辑必须严密**
- 优雅退出顺序：先 stop 消费（ctx）再 Ack 完在途消息，最后关 channel/connection

## 验收清单

- [ ] 点赞接口响应 <50ms（MQ 异步），1 秒后 likes 表出现记录、计数 +1
- [ ] RabbitMQ 管理台（15672）能看到 4 个 exchange/queue，发点赞时消息涨落
- [ ] **kill -9 worker 进程后重启** → 队列里未 Ack 的消息被重新消费，计数最终正确（幂等）
- [ ] 同一条 like 消息手工重投 3 次（管理台 Get Message）→ 计数只 +1
- [ ] `docker stop rabbitmq` → 点赞接口**仍然 200** 且数据同步落库（降级直写）
- [ ] 停 Redis 时 Worker 只停 PopularityWorker，其他三个照常（启动时降级的组合运用）
- [ ] Ctrl+C worker → 日志显示优雅退出，无消息丢失

## 进阶改进

- 毒消息处理：失败计数 → 超 3 次 Nack(requeue=false) + DLX 队列
- Publisher Confirm（`ch.NotifyPublish`）+ 备份交换器，把"发布成功"做实
- 消费端限速/批量落库（攒 50 条一事务）
- 给评论删除事件补热度 -1（修复阶段 5 发现的瑕疵）

## 原项目对照

- `backend/internal/middleware/rabbitmq/*.go`（基座 + 4 个封装）
- `backend/cmd/worker/main.go`（拓扑声明 + 并发 Run + errCh + 信号退出）
- `backend/internal/worker/*.go`（4 个消费者；重点读 likeworker 的幂等三处判断）
- `backend/internal/video/like_service.go:57-106`（降级矩阵原版）
