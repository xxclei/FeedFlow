# 09 - RabbitMQ 与 Worker：异步化 + 最终一致性【全量版】

前置：阶段 8。本阶段把"同步写"改造成"发事件 + 独立进程消费"，同时**保留阶段 4/5 写的直写代码当降级路径**——前面没有一步是白写的。

> **对齐原则（全程适用）**：最终产出 = 原项目全部功能。本阶段全量范围：**5 个发布封装 + DLX 死信 + outbox 事务性发件箱 + 4 个独立 Worker 消费者 + 3 条通知队列/SSE 实时推送 + 双进程架构 + 三种姿势的降级**。若某能力依赖后续阶段（本阶段没有），文档会明确标注归属。

## 目标

- `internal/middleware/rabbitmq/`：1 个连接基座 + DLX 封装 + 5 个 topic 链路封装（like / comment / social / popularity / timeline），durable 交换机/队列 + Persistent 消息 + 事件信封（event_id / occurred_at）
- `cmd/worker` 独立进程：连 DB（带重试）→ 连 Redis（可选降级）→ 连 MQ（必选 fatal）→ 声明拓扑 → 4 个 worker 各自独立 channel + Qos(50) + 断线自动重连 + 优雅退出
- 消费端：手动 Ack、进程内指数退避重试 3 次后丢弃毒消息、幂等三板斧
- **outbox 事务性发件箱**：视频发布事务里同步写 `outbox_msgs` 本地消息表，轮询器中继到 MQ，消费端写 `feed:global_timeline`——业务写库和消息投递的最终一致性
- API 进程内的后台 goroutine：outbox 轮询器、timeline 消费者、3 个 NotificationWorker（点赞/评论/关注 → notifications 表 + SSE 实时推送）
- service 层降级矩阵：like/comment 双 flag 按目标降级、social 先 DB 后 MQ、video 走 outbox

## 双进程架构（先看清全貌）

```
cmd/main.go (API 进程)                          cmd/worker (Worker 进程)
┌────────────────────────────────┐              ┌──────────────────────────────┐
│ gin 接口：低延迟响应             │              │ 重活：批量写库，独立伸缩/重启    │
│ ├─ LikeService/CommentService   │──Publish──►  │ ├─ LikeWorker     → MySQL    │
│ ├─ SocialService/VideoService   │   (5 条链路)  │ ├─ CommentWorker  → MySQL    │
│ ├─ StartOutboxPoller  (goroutine)│──outbox中继─►│ ├─ SocialWorker   → MySQL    │
│ ├─ StartConsumer      (goroutine)│◄─consume────│ └─ PopularityWorker → Redis  │
│ └─ NotificationWorker×3(goroutine)│◄─consume──  └──────────────────────────────┘
└────────────────────────────────┘
```

| | cmd/main.go（API） | cmd/worker（Worker） |
|---|---|---|
| MySQL | 必选，失败 fatal | **带重试**连接（1,2,4…封顶 30s ×10 次），仍失败 fatal |
| Redis | 可选，失败置 nil（"cache disabled"） | 可选，失败置 nil（**PopularityWorker 整个禁用**） |
| RabbitMQ | 可选，失败置 nil → 所有 service 拿到 nil MQ → 全线走直写降级 | **必选**：worker 没 MQ 就没有存在意义 → 重试耗尽 `log.Fatalf` |
| 额外职责 | AutoMigrate、路由、限流、SSE、outbox 轮询、timeline 消费、通知消费 | 声明 4 组拓扑、4 个消费者、pprof（`WorkerAddr`） |

关键认知：**不是所有消费者都在 Worker 进程里**。与"实时性"相关的三个后台任务（outbox 轮询、timeline 消费、通知消费）跑在 API 进程的 goroutine 里；纯落库的 4 个 worker 跑在独立进程。照抄这个分工。

## 拓扑总表（先背下来）

| Exchange (topic, durable) | Routing Key | Binding | Queue (durable) | 消费者（进程） | 消费动作 |
|---|---|---|---|---|---|
| `like.events` | `like.like` / `like.unlike` | `like.*` | `like.events` | LikeWorker（worker） | 写 likes 表 + likes_count/popularity ±1 |
| `like.events` | `like.like` | `like.like` | `notification.like` | NotificationWorker（API） | 查视频作者 → 写 notifications + SSE push |
| `comment.events` | `comment.publish` / `comment.delete` | `comment.*` | `comment.events` | CommentWorker（worker） | 写/删评论 + popularity +1（删除不回扣） |
| `comment.events` | `comment.publish` | `comment.publish` | `notification.comment` | NotificationWorker（API） | 通知视频作者 |
| `social.events` | `social.follow` / `social.unfollow` | `social.*` | `social.events` | SocialWorker（worker） | 写 social 表（冗余消费者，1062 幂等） |
| `social.events` | `social.follow` | `social.follow` | `notification.social` | NotificationWorker（API） | 通知被关注人 |
| `video.popularity.events` | `video.popularity.update` | `video.popularity.*` | `video.popularity.events` | PopularityWorker（worker） | `UpdatePopularityCache`（失效详情缓存 + 分钟桶 ZincrBy） |
| `video.timeline.events` | `video.timeline.publish` | `video.timeline.*` | `video.timeline.update.queue` | StartConsumer（API） | ZAdd `feed:global_timeline` + 裁剪到 1000 条 |
| `dlx.events` | `#` | — | `<业务队列>.dlx`（如 `like.events.dlx`） | 无 | 死信存放处，管理台可查 |

要点：
- exchange/queue **全部 durable**；消息 `DeliveryMode: amqp.Persistent` + `ContentType: application/json` + `Timestamp`
- 每个业务队列声明时带 `args: amqp.Table{"x-dead-letter-exchange": "dlx.events"}`（API 侧 `DeclareTopic` 还会顺带声明 `<queue>.dlx` 死信队列并绑 `#`；worker 侧只带 arg 不声明 DLX——durable 声明幂等，两边参数一致不冲突）
- **同 exchange 不同队列 = 广播副本**（like 队列和 notification.like 队列各拿一份消息）；同队列 = 竞争消费（每条消息只投给一个消费者）

## 消息体（6 个事件 JSON，照抄字段名）

所有事件统一信封：`event_id`（`crypto/rand` 16 字节 → 32 位 hex，排查 + 将来幂等钩子）、`occurred_at`。

```jsonc
// LikeEvent   → like.events（like.like / like.unlike）
{"event_id":"a3f9…32位hex","action":"like","user_id":1,"video_id":2,"occurred_at":"2026-09-10T12:00:00+08:00"}

// CommentEvent → comment.events
// publish：{"event_id":"…","action":"publish","username":"tom","video_id":2,"author_id":1,"content":"哈哈","occurred_at":"…UTC"}
// delete ：{"event_id":"…","action":"delete","comment_id":5,"occurred_at":"…"}     ← 删除只带 comment_id

// SocialEvent → social.events（social.follow / social.unfollow）
{"event_id":"…","action":"follow","follower_id":1,"vlogger_id":2,"occurred_at":"…UTC"}

// PopularityEvent → video.popularity.events
{"event_id":"…","video_id":2,"change":1,"occurred_at":"…UTC"}          // change ±1，发布失败/解析失败都无从补偿，靠幂等

// TimelineEvent → video.timeline.events（video.timeline.publish）
{"event_id":"…","video_id":2,"create_time":1725945600000,"occurred_at":"…"}   // create_time = video.CreateTime.UnixMilli()
```

> 小瑕疵照抄前知会：Like 的 `OccurredAt` 用 `time.Now()`，Comment/Social/Popularity 用 `time.Now().UTC()`——不影响功能，你统一成 UTC 更好。

## 关键设计决策（本项目精华）

**Q1：为什么 API 和 Worker 拆成两个进程？** API 进程的职责是低延迟响应（MQ 发布只是 marshal + 一次 socket 写），重活（DB 批量写、Redis 更新）剥离出去**独立伸缩、独立重启**：worker 崩了 API 照常服务，消息堆在队列里等 worker 回来。`Qos(prefetch=50)` 给每个 worker 限流背压。

**Q2：为什么点赞要发两条消息而不是一条？** 两条链路目标不同：`like.events` → 落 MySQL；`video.popularity.events` → 更新 Redis。目标、消费者、失败域完全不同，**分开才能按目标独立降级**。

**Q3：发布失败怎么办？（降级矩阵，必考——三种姿势并存）**

```
姿势一（like/comment）：双 flag 按目标降级
  likeMQ 发布失败？      → 事务直写 MySQL（阶段 4 写的那段代码原封搬进来！）
  popularityMQ 失败？    → 直调 UpdatePopularityCache 写 Redis（阶段 8 的函数！）
  两个 flag 相互独立，只对失败的目标降级，成功的不动

姿势二（social）：先 DB 后 MQ，MQ 丢了只记日志
  关注必须同步写库成功才返回；socialMQ.Follow 失败 → log 后照常返回 nil
  SocialWorker 只是冗余消费者，丢一条通知无所谓，不值得为它降级

姿势三（video/timeline）：根本不发 MQ —— 走 outbox，最终一致
  事务里同时写 video 和 outbox_msgs，MQ 投递交给轮询器，不存在"发布失败"这个分支
```

**Q4：MQ 会不会重复投递？Worker 怎么幂等？** 会（at-least-once：Ack 之前进程崩溃必重投；outbox 中继在"投递成功、删除失败"的窗口也会重发）。幂等三板斧：
1. **唯一索引**：重复 INSERT 撞 1062 → `LikeIgnoreDuplicate` 返回 created=false / SocialWorker 直接 `errors.As(1062) → return nil`
2. **affected rows 判定**：`DeleteByVideoAndAccount` 返回 deleted bool，**没删到行就不动计数**
3. 解析失败/字段非法的消息直接 Ack 丢弃

配套原则：**先做"会造成副作用"的动作并确认真的发生了，再改计数**；`ChangeLikesCount/ChangePopularity` 用 `GREATEST(expr, 0)` 兜底防负数；timeline 消费端 `ZAdd` 同 member 同 score 天然幂等。

**Q5：消费失败怎么处理？**（注意：原项目已从"Nack requeue 无限重试"进化为**进程内有界重试**）`handleDelivery` 循环最多试 4 次（i=0..3，退避 1s/2s/4s）：仍失败 → 日志"重试 3 次后仍失败, 丢弃" + `Ack(false)` 丢弃；期间 ctx 取消（进程要退出）→ `Nack(false, true)` 放回队列交给下一个消费者。deliveries channel 关闭（连接断开）→ `Run` 返回错误 → 外层重连循环 5 秒后重建 channel。

**Q6：DLX 死信队列现在到底起作用了吗？**（诚实回答）**基础设施一步到位**：`dlx.events` 交换机、每个队列的 `.dlx` 死信队列、`GetRetryCount`（读 x-death header）都写好了；但当前**没有 Nack(requeue=false) 的触发点**，消息进不了 DLX。它是给进阶练习留的插座（见"进阶改进"）。

**Q7：outbox 模式解决什么问题？（全项目最大亮点之一，面试可讲 10 分钟）**
- **双写困境**：发布视频要"写 MySQL + 发 MQ"两件事，先写库再发 MQ，发失败 → 最新流永远缺这条视频；先发 MQ 再写库，写失败 → MQ 里多一条幽灵视频。两步永远不可能原子。
- **outbox 解法**：把"要发的消息"当成一行数据，和业务数据在**同一个事务**里写进本地消息表 `outbox_msgs(status=pending)`——业务落库 ⇔ 消息落库，原子成立。
- **中继**：`StartOutboxPoller` 每秒扫 `status='pending'` 按 create_time 升序取 100 条 → `TimelineMQ.PublishVideo` → 成功才 `Delete` 该行；失败行留在表里下一轮再来。**发布失败不再存在，只有"暂未投出"**。
- **闭环**：消费端 ZAdd 幂等，所以"投递成功、删除失败 → 下一轮重发"也安全。
- **降级语义**：MQ 没起来时启动的 API，轮询器不启动，outbox 行持续积压**但一条不丢**；MQ 恢复后重启 API，积压全部补投——这就是"最终一致性"的可运行形态。

**Q8：NotificationWorker 为什么不直接消费 `like.events` 队列？** 因为同队列是**竞争消费**：它和 LikeWorker 会互相抢消息，每条消息只有一个人拿到，点赞落库和通知就二缺一。所以另开 `notification.like` 等**副本队列**绑到同一个 exchange 上——topic 交换机天然广播，各自 Ack 互不影响。且通知队列只绑正向动作（`like.like`/`comment.publish`/`social.follow`），取关/删评论不通知。

**Q9：channel 归谁？** amqp.Channel **不是线程安全的**。基座 `RabbitMQ` 只持有 `Conn`：API 侧每个 MQ 封装（LikeMQ…TimelineMQ）构造时各开一条 channel；Worker 侧每个 worker 每次重连都新建自己的 channel；timeline 消费者和 3 个通知 worker 也是独立 channel。全项目没有跨 goroutine 共用 channel 的地方。

## 实现提示

### 发布端基座（`internal/middleware/rabbitmq/rabbitMQ.go`）

```go
type RabbitMQ struct{ Conn *amqp.Connection }            // 只管 Connection，Channel 由各组件按需创建

func NewRabbitMQ(cfg *config.RabbitMQConfig) (*RabbitMQ, error)  // 拼 amqp://user:pass@host:port/ → amqp.Dial
func (r *RabbitMQ) Close() error                          // nil 安全
func (r *RabbitMQ) NewChannel() (*amqp.Channel, error)    // 每个封装/每个 worker 各开一条

func DeclareTopic(ch *amqp.Channel, exchange, queue, bindingKey string) error
// ExchangeDeclare(topic, durable) → QueueDeclare(durable, args{"x-dead-letter-exchange": DLXExchange})
// → QueueBind(queue, bindingKey, exchange) → DeclareDLX(ch, queue)（失败仅日志）

func PublishJSON(ctx context.Context, ch *amqp.Channel, exchange, routingKey string, payload any) error
// json.Marshal → PublishWithContext：ContentType "application/json"、DeliveryMode amqp.Persistent、Timestamp time.Now()

func newEventID(n int) (string, error)                    // crypto/rand n 字节 → hex
```

### DLX（`dlx.go`）

```go
const (
    DLXExchange   = "dlx.events"
    MaxRetryCount = 3
)
func DeclareDLX(ch *amqp.Channel, queueName string) error // dlx.events(topic) + <queueName>.dlx 队列，绑定 "#"
func GetRetryCount(d amqp.Delivery) int                   // 从 Headers["x-death"][0]["count"] 读已死信次数
```

### 各链路封装（5 个文件同构，以 likeMQ.go 为模板）

```go
type LikeMQ struct{ ch *amqp.Channel }                    // 独占 channel，发布端互不干扰

const (
    likeExchange   = "like.events"
    likeQueue      = "like.events"                        // 队列名与交换机同名（原项目如此）
    likeBindingKey = "like.*"
    likeLikeRK     = "like.like"
    likeUnlikeRK   = "like.unlike"
)

type LikeEvent struct {
    EventID    string    `json:"event_id"`
    Action     string    `json:"action"`                    // "like" / "unlike"
    UserID     uint      `json:"user_id"`
    VideoID    uint      `json:"video_id"`
    OccurredAt time.Time `json:"occurred_at"`
}

func NewLikeMQ(base *RabbitMQ) (*LikeMQ, error)           // base.NewChannel → DeclareTopic；失败要 ch.Close()
func (l *LikeMQ) Like(ctx context.Context, userID, videoID uint) error
func (l *LikeMQ) Unlike(ctx context.Context, userID, videoID uint) error
// publish 内部：l/l.ch 非空校验 → 参数非 0 校验 → newEventID(16) → PublishJSON
```

其余 4 个照抄常量与事件结构体即可，差异点：CommentMQ 的 `Publish(ctx, username, videoID, authorID, content)` / `Delete(ctx, commentID)` 两个入口共用一条 publish；PopularityMQ 的 `Update(ctx, videoID, change int64)` 多一个 `change==0` 校验；TimelineMQ 的 `PublishVideo(ctx, videoID, createTime)` 把时间转 `UnixMilli()`；SocialMQ 字段是 `follower_id/vlogger_id`。

### Service 降级矩阵（回填到阶段 4/5 的代码里）

```go
mysqlEnqueued, redisEnqueued := false, false
if s.likeMQ != nil {
    if err := s.likeMQ.Like(ctx, like.AccountID, like.VideoID); err == nil { mysqlEnqueued = true }
}
if s.popularityMQ != nil {
    if err := s.popularityMQ.Update(ctx, like.VideoID, 1); err == nil { redisEnqueued = true }
}
if mysqlEnqueued && redisEnqueued { return nil }          // 全部异步成功，接口立即返回
if !mysqlEnqueued { /* 阶段 4 的事务直写，原封不动搬进来 */ }
if !redisEnqueued { UpdatePopularityCache(ctx, s.cache, like.VideoID, 1) }  // 阶段 8 的函数
```

注意前置校验（视频存在、是否已点赞）**保留同步**——接口仍要快速失败给用户明确报错，MQ 只承接"确定要做"的写。

### Worker 消费骨架（4 个 worker 同构，写完一个复制改）

```go
type LikeWorker struct {
    ch     *amqp.Channel                 // 自己的消费 channel
    likes  *video.LikeRepository
    videos *video.VideoRepository
    queue  string
}
func NewLikeWorker(ch *amqp.Channel, likes *video.LikeRepository, videos *video.VideoRepository, queue string) *LikeWorker

func (w *LikeWorker) Run(ctx context.Context) error {
    // 参数校验 → deliveries, err := w.ch.Consume(w.queue, "", false /*手动Ack*/, false, false, false, nil)
    for {
        select {
        case <-ctx.Done(): return ctx.Err()
        case d, ok := <-deliveries:
            if !ok { return errors.New("deliveries channel closed") }   // channel 断开 → 返回错误 → 外层重连
            w.handleDelivery(ctx, d)
        }
    }
}

func (w *LikeWorker) handleDelivery(ctx context.Context, d amqp.Delivery) {
    const maxRetries = 3                          // 进程内重试：共试 4 次，退避 1s/2s/4s
    for i := 0; i <= maxRetries; i++ {
        select {
        case <-ctx.Done():
            _ = d.Nack(false, true)               // 退出中：放回队列，别让消息跟着进程陪葬
            return
        default:
        }
        if err := w.process(ctx, d.Body); err != nil {
            if i >= maxRetries {
                log.Printf("like worker: 重试 %d 次后仍失败, 丢弃: %v", maxRetries, err)
                _ = d.Ack(false)                  // 毒消息：丢弃，不能堵死队列
                return
            }
            time.Sleep(time.Duration(1<<uint(i)) * time.Second)
            continue
        }
        _ = d.Ack(false)
        return
    }
}

func (w *LikeWorker) process(ctx context.Context, body []byte) error {
    var evt rabbitmq.LikeEvent
    if err := json.Unmarshal(body, &evt); err != nil { return nil }   // 解析失败＝毒消息，丢弃
    if evt.UserID == 0 || evt.VideoID == 0 { return nil }
    switch evt.Action {
    case "like":   return w.applyLike(ctx, evt.UserID, evt.VideoID)
    case "unlike": return w.applyUnlike(ctx, evt.UserID, evt.VideoID)
    default:       return nil                       // 未知 action 不算失败
    }
}

func (w *LikeWorker) applyLike(ctx context.Context, userID, videoID uint) error {
    ok, err := w.videos.IsExist(ctx, videoID)
    if err != nil { return err }
    if !ok { return nil }                          // 视频已删：业务上无效，不算失败

    created, err := w.likes.LikeIgnoreDuplicate(ctx, &video.Like{VideoID: videoID, AccountID: userID, CreatedAt: time.Now()})
    if err != nil { return err }
    if !created { return nil }                     // 幂等核心：没插进去就什么都不做

    if err := w.videos.ChangeLikesCount(ctx, videoID, 1); err != nil { return err }   // GREATEST(likes_count+1,0)
    return w.videos.ChangePopularity(ctx, videoID, 1)
}
// applyUnlike 同构：DeleteByVideoAndAccount 返回 deleted=false → 直接 return nil，再 ±(-1)
```

另外三个的差异：
- **CommentWorker**：publish 校验 `VideoID/AuthorID/Content(TrimSpace 非空)` → `IsExist` → `CreateComment` → `ChangePopularity(+1)`；delete → `GetByID`，`c == nil` 直接 nil，否则 `DeleteComment`（**不回扣热度**，原样）
- **SocialWorker**：follow → `repo.Follow`，`errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 → return nil`；unfollow → `repo.Unfollow`
- **PopularityWorker**：`VideoID==0 || Change==0 → nil` → `video.UpdatePopularityCache(ctx, w.cache, evt.VideoID, evt.Change)`——该函数错误全吞、永不返回 error，所以这个 worker 的 process 永远成功

### outbox：事务性发件箱（重点，两部分）

**第一部分：业务侧把消息写进同一事务**（`video_service.go` 的 Publish）：

```go
// OutboxMsg（加在 video_entity.go，并进 db.AutoMigrate 列表）
type OutboxMsg struct {
    ID         uint      `gorm:"primaryKey"`
    VideoID    uint      `gorm:"index"`
    EventType  string    `gorm:"type:varchar(50)"`     // "video_published"
    CreateTime time.Time `gorm:"autoCreateTime"`
    Status     string    `gorm:"type:varchar(50);index"` // "pending"
}

err := vs.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
    if err := tx.Create(video).Error; err != nil { return err }
    msg := OutboxMsg{VideoID: video.ID, EventType: "video_published", Status: "pending", CreateTime: video.CreateTime}
    if err := tx.Create(&msg).Error; err != nil { return err }
    // …标签 FirstOrCreate + VideoTag 也在这同一事务里（阶段 3 已写的逻辑合并进来）
    return nil
})
// 注意：这里不调 timelineMQ！发布接口只管落库，投递交给轮询器
```

**第二部分：`outboxworker.go` 的中继轮询 + 消费**：

```go
func StartOutboxPoller(db *gorm.DB, tmq *rabbitmq.TimelineMQ)
// tmq == nil → log "Outbox poller disabled" + return（启动时降级：行会积压但不丢）
// go func 死循环：
//   db.Where("status = ?", "pending").Order("create_time ASC").Limit(100).Find(&messages)
//   查询失败或空 → sleep 1s → continue
//   逐条：tmq.PublishVideo(context.Background(), msg.VideoID, msg.CreateTime)   // 轮询器生命周期独立于请求，用 Background
//     成功 → db.Delete(&msg)（删除失败只记日志：大不了重发一次，消费端幂等）
//     失败 → 记日志，行留在表里，下一轮再来

func StartConsumer(tmq *rabbitmq.TimelineMQ, queueName string, redisClient *rediscache.Client, rmq *rabbitmq.RabbitMQ)
// tmq/redis/rmq 为 nil → log + return
// go func 外层死循环（断线重连在这一层）：
//   ch := rmq.NewChannel()                      // 每次重连新建，不与发布者共用
//   ch.Qos(10, 0, false)                        // 时间线消费者 prefetch 10
//   ch.Consume(queueName, "", false, false, false, false, nil)
//   for msg := range msgs:
//     反序列化失败 → log + msg.Ack(false) 丢弃
//     ctx(500ms 超时) → ZAdd("feed:global_timeline", Z{Score: float64(event.CreateTime), Member: videoID})
//       失败 → msg.Nack(false, true) 重新入队 + cancel + continue
//     ZRemRangeByRank(key, 0, -1001)            // 只留最新 1000 条（失败仅日志，仍 Ack）
//     msg.Ack(false)
//   msgs 关闭 = channel 断开 → ch.Close() → log → sleep 5s → 重连
```

### NotificationWorker + SSE 实时推送（API 进程）

```go
// notificationworker.go：通知表模型（AutoMigrate 放在 Run 里，首次启动自动建表）
type Notification struct { ID uint; RecipientID uint `gorm:"index;not null"`; SenderID uint; Type string; TargetID uint; Content string; IsRead bool; CreatedAt time.Time }

type NotificationHub interface{ Push(userID uint, n *Notification) }   // SSEHub 实现它，解耦 consumer 与推送

func NewNotificationWorker(ch *amqp.Channel, db *gorm.DB, queue string, hub NotificationHub) *NotificationWorker
// 注意 process 收整个 amqp.Delivery 而不只是 Body——要按 d.RoutingKey 分发：
//   "like.like"       → 查 videos.author_id；authorID==0 或 ==点赞者 → 忽略（自己点赞不通知自己）
//                       Notification{RecipientID: authorID, SenderID: evt.UserID, Type: "like",    TargetID: evt.VideoID, Content: "点赞了你的视频"}
//   "comment.publish" → 同上；                    Type: "comment", Content: "评论了你的视频"
//   "social.follow"   → 直接收件人=VloggerID；    Type: "follow",  TargetID: evt.FollowerID, Content: "关注了你"
//   其余 routingKey → nil（Ack 丢弃）
// 落库 db.Create(notif) → hub.Push(notif.RecipientID, notif)（Push 满了就丢，绝不阻塞消费）

// ssehub.go：每用户多连接的订阅注册表
func NewSSEHub(db *gorm.DB) *SSEHub                        // clients: map[uint][]chan *Notification + RWMutex
func (h *SSEHub) Push(userID uint, n *Notification)        // RLock + select{case ch<-n: default:} 非阻塞
func (h *SSEHub) Subscribe(userID uint) chan *Notification // buffered 20
func (h *SSEHub) Unsubscribe(userID uint, ch chan *Notification)  // 找到就删 + close
func (h *SSEHub) SSERequireAuth() gin.HandlerFunc          // EventSource 带不了 header：先取 ?token=，再回落 Authorization Bearer
func (h *SSEHub) SSEHandler(c *gin.Context)                // 响应头 text/event-stream/no-cache/keep-alive
                                                           // for select：ctx.Done 返回 | ch 收到 → "data: {json}\n\n" + Flush
                                                           //             | time.After(30s) → ": keepalive\n\n" 保活
func (h *SSEHub) ListHandler(c *gin.Context)               // POST：前 50 条通知，空时返回 []
func (h *SSEHub) MarkReadHandler(c *gin.Context)           // POST {id?}：带 id 单条已读，不带 = 全部已读
func (h *SSEHub) UnreadCountHandler(c *gin.Context)        // POST → {count}
func (h *SSEHub) RegisterRoutes(r *gin.Engine, group *gin.RouterGroup)
// 挂载：GET /notification/stream、POST /notification/list、POST /notification/markRead、POST /notification/unreadCount
```

> 另有一条**不走 MQ** 的同步通知：评论里 `@某人`（`mentionRegex = @(\w+)`）在 `notifyMentions` 里直接查 accounts 表并 INSERT notifications（Type "mention"），失败仅日志。异步成功和降级直写两条路径的收尾都要调它。

### `cmd/worker/main.go`

```go
func connectWithRetry(name string, maxRetries int, fn func() error)
// 指数退避 1<<i 秒、封顶 30s；重试耗尽 → log.Fatalf

func runWorkerWithRetry(ctx context.Context, name string, conn *amqp.Connection, fn func(*amqp.Channel) error)
// for {
//   ctx 取消 → return
//   ch := conn.Channel()（失败 log + 5s 重试）→ ch.Qos(50, 0, false)（失败仅日志）
//   log "started, consuming" → err := fn(ch)
//   fn 出错：ctx 已取消 → ch.Close() + return（正常退出）；否则 log "... 5秒后重连..."
//   ch.Close() → sleep 5s → 下一轮
// }

func main() {
    // godotenv.Load → config.LoadLocalDev(CONFIG_PATH 默认 configs/config.yaml)
    // connectWithRetry("MySQL", 10, db.NewDB…)            必选
    // Redis：NewFromEnv 或 Ping(300ms) 失败 → cache=nil，log "popularity worker disabled"
    // connectWithRetry("RabbitMQ", 10, amqp.Dial…)        必选：没 MQ 的 worker 没有存在意义
    // 临时 topoCh 声明 4 组拓扑（popularity 仅 cache!=nil 时声明）→ topoCh.Close()
    //   declareSocial/Like/Comment/PopularityTopology：ExchangeDeclare(topic,durable)
    //   + QueueDeclare(durable, {"x-dead-letter-exchange": mqrabbit.DLXExchange}) + QueueBind
    // socialRepo / videoRepo / likeRepo / commentRepo
    // ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    // observability.NewPprofServer("Worker", enabled, cfg.ObservabilityConfig.Pprof.WorkerAddr)
    // 4 个 goroutine：
    //   go runWorkerWithRetry(ctx, "LikeWorker", conn, func(ch) error {
    //       return worker.NewLikeWorker(ch, likeRepo, videoRepo, likeQueue).Run(ctx) })
    //   …CommentWorker / SocialWorker 同构；PopularityWorker 仅 cache != nil 时启动
    // <-ctx.Done() → log "Worker shutting down..." → time.Sleep(2 * time.Second) → log "Worker stopped"
    //   （这 2 秒让在途消息处理完；消息本身有手动 Ack + 重投兜底）
}
```

### router.go 的接线（API 进程全部 MQ 初始化都在这）

```go
popularityMQ, err := rabbitmq.NewPopularityMQ(rmq)
if err != nil { log.Printf("PopularityMQ init failed (mq disabled): %v", err); popularityMQ = nil }
videoService := video.NewVideoService(videoRepository, cache, popularityMQ)

likeMQ, err := rabbitmq.NewLikeMQ(rmq)
if err != nil { log.Printf("LikeMQ init failed (mq disabled): %v", err); likeMQ = nil }
likeService := video.NewLikeService(likeRepository, videoRepository, cache, likeMQ, popularityMQ)

commentMQ, err := rabbitmq.NewCommentMQ(rmq)   // 同构：失败置 nil → NewCommentService(..., commentMQ, popularityMQ)
socialMQ, err := rabbitmq.NewSocialMQ(rmq)     // 同构：失败置 nil → NewSocialService(..., socialMQ, cache)

timelineMQ, err := rabbitmq.NewTimelineMQ(rmq)
if err != nil { log.Printf("timelineMQ init failed (mq disabled): %v", err); timelineMQ = nil }
worker.StartOutboxPoller(db, timelineMQ)
worker.StartConsumer(timelineMQ, "video.timeline.update.queue", cache, rmq)

// notification：复用业务 exchange，另开 3 个"副本队列"（每个封装共用一条临时 notifCh 声明完即关）
if rmq != nil {
    if notifCh, err := rmq.NewChannel(); err == nil {
        rabbitmq.DeclareTopic(notifCh, "like.events",    "notification.like",    "like.like")
        rabbitmq.DeclareTopic(notifCh, "comment.events", "notification.comment", "comment.publish")
        rabbitmq.DeclareTopic(notifCh, "social.events",  "notification.social",  "social.follow")
        notifCh.Close()
    }
}
sseHub := worker.NewSSEHub(db)
notifGroup := r.Group("/notification")
notifGroup.Use(sseHub.SSERequireAuth())
sseHub.RegisterRoutes(r, notifGroup)
// 再起 3 个 goroutine：每个通知队列一个，内部同样是 NewChannel → NewNotificationWorker(ch, db, queue, hub).Run(ctx)
// → 出错 log + ch.Close() + 5s 重连
```

## 难点清单（本阶段真正难的地方）

1. **最终一致性**：先把"双写困境"推演明白（先库后 MQ / 先 MQ 后库 各会坏在哪），再理解 outbox 三件套——同事务写消息表、轮询中继、消费幂等——如何把"必然失败的两步"变成"必然收敛的轮询"。画时序图：发布事务提交 → 轮询器捞起 → Publish → Delete，每一步崩溃后系统各处于什么状态、靠什么自愈。
2. **消息幂等**：at-least-once 是前提不是缺陷。四个消费动作各自的幂等依据要逐一落到代码：like 靠唯一索引、unlike 靠 affected rows、social 靠 1062、timeline 靠 ZAdd 的 set 语义；再配上 `GREATEST(expr,0)` 防计数为负。验收就是"同一消息重放 3 次，结果不变"。
3. **毒消息处理**：三道防线——解析失败丢弃、字段非法丢弃、重试 3 次后丢弃——每道都要"丢弃 = Ack"而不是 requeue，否则一条坏消息能把队列堵死。区分**永久性错误**（数据没了、约束冲突，重试无意义）和**暂时性错误**（DB 抖动，值得退避重试）。
4. **两 flag 降级矩阵的严密性**：`mysqlEnqueued && redisEnqueued → return` 之后绝不能再走直写，否则一次点赞计数 +2（MQ 消费一次 + 降级直写一次）。改造完旧同步代码后，把旧路径整个删掉而不是注释。
5. **重连与生命周期管理**：谁持有 channel（答案：没有任何人跨 goroutine 共用）、deliveries 关闭怎么感知（`d, ok := <-deliveries` 的 ok）、`runWorkerWithRetry` 在哪一层重连（channel 层，不是连接层）、优雅退出顺序（ctx 停消费 → 在途消息处理完/2 秒 → 关 channel/connection）。
6. **多消费者拓扑**：同 exchange 不同队列 = 广播副本，同队列 = 竞争消费——`like.events` 与 `notification.like` 的区别就是本阶段最容易搞混的一处，配错会导致通知和落库二缺一。

## 亮点（原项目在本阶段的工程亮点）

- **outbox 事务性发件箱**：用"消息即数据"的思想消解双写困境，外加"投递成功才删行"的 at-least-once 中继和消费端幂等闭环。停 MQ 不丢数据、恢复后自动补投，是全项目最值得在面试里展开的一段。
- **DLX 基础设施一步到位**：死信交换机/死信队列/x-death 计数工具全部就位，即使触发点留白，拓扑已经是生产形态。
- **降级哲学第二条（运行时按目标降级）的完整落地**：like/comment 的双 flag 矩阵（MySQL、Redis 各自独立降级）、video `UpdatePopularity` 的"MQ 优先、失败直写分钟桶"、social 的"MQ 只是副产品，丢了仅日志"——三种姿势按业务价值选择，而不是一刀切。
- **降级哲学第三条（请求级降级）的延伸**：`UpdatePopularityCache` 全程 50ms 超时 + 错误吞掉，导致 PopularityWorker 的 process 永不失败、永不 requeue；timeline 消费者对 ZAdd 也只给 500ms。
- **事件信封**：每条消息带 `event_id`（随机 hex）+ `occurred_at`，排查时能对上"哪次点击产生了哪条消息"；也为将来的消费端去重表预留了主键。
- **worker 治理**：每 worker 独立 channel + `Qos(50)` 背压 + 断线 5 秒重连 + 启动级指数退避重试（封顶 30s），Redis 挂了只禁用 PopularityWorker 而不是整个进程自杀。
- **SSE 实时通知**：EventSource 鉴权适配（`?token=`）、非阻塞 Push（订阅满了丢弃而不是压垮消费者）、30 秒 keepalive 防代理断连，`NotificationHub` 接口让消费与推送解耦。

## 常见的坑

- Worker 里用 `autoAck=true` → 处理中崩溃消息直接丢
- 忘 `Qos` → MQ 把上千条消息推给一个 Worker，内存爆掉；重连后 channel 是新的，Qos 要在新 channel 上重设（`runWorkerWithRetry` 每轮都设）
- channel 不是线程安全的：**发布端每个 MQ 封装一条 channel，消费端每个 worker 一条**；共用一条必须加锁（进阶改连接池）
- `amqp.Dial` 的 URL 密码里有特殊字符要 `url.QueryEscape`
- 两 flag 逻辑不严密 → 一次点赞计数 +2；或把降级直写写成了"无条件直写" → MQ 恢复后永远双写
- NotificationWorker 若与业务 worker 共用队列名 → 竞争消费丢消息；必须用独立的 `notification.*` 副本队列
- outbox 轮询器里误用了请求 ctx → 请求结束轮询就死；它要用 `context.Background()`
- 投递失败时把 outbox 行删了/改了状态 → 消息永久丢失；**只有 Publish 成功才 Delete**
- `d, ok := <-deliveries` 漏判 ok → channel 断开后死循环空转
- 优雅退出顺序错：先关 connection 再停消费 → 在途消息全部重投
- 轮询查询用了 `Order("create_time ASC")` 却忘了分页 Limit → 大表一次捞爆内存；Limit(100) + 1 秒一轮就是背压
- 时间线消费者忘了 `ZRemRangeByRank` → ZSet 无限增长；裁剪失败只记日志仍 Ack（裁剪是尽力而为，丢裁剪不丢数据）

## 验收清单

- [ ] 双进程分别启动：`go run ./cmd` 与 `go run ./cmd/worker`，日志各就各位（worker 打印 4 个 "started, consuming"）
- [ ] 点赞接口响应 <50ms（MQ 异步），1 秒后 likes 表出现记录、likes_count +1、popularity +1
- [ ] 15672 管理台能看到 5 个业务 exchange、对应 queue、3 个 notification 队列、`dlx.events` 和各 `*.dlx` 队列
- [ ] **kill -9 worker 进程后重启** → 队列里未 Ack 的消息被重新消费，计数最终正确（幂等）
- [ ] 同一条 like 消息手工重投 3 次（管理台 Get Message → Publish）→ 计数只 +1
- [ ] `docker stop rabbitmq` → 点赞接口**仍然 200** 且数据同步落库（双 flag 降级直写）；重启 rabbitmq → 恢复异步
- [ ] 停 Redis 后启动 worker → 只有 PopularityWorker 被禁用，其他三个照常消费
- [ ] 发布视频 → `outbox_msgs` 先出现 pending 行 → ≤1 秒内被删除 → `feed:global_timeline` 出现该 video_id（score = 毫秒时间戳），ZSet 长度始终 ≤1000
- [ ] MQ 不可用时启动 API 期间发布的视频：outbox 行积压；重启 API（MQ 已恢复）后被补投进时间线（最终一致）
- [ ] 用户 B 关注 A → A 的 `GET /notification/stream?token=…` 立刻收到 `data: {"type":"follow",…}`；`/notification/unreadCount` 变 1；`/notification/markRead` 后归零
- [ ] 给自己点赞/评论 → 不产生通知；通知只含正向动作（取关/删评论无）
- [ ] 重复 follow 消息重放 → SocialWorker 撞 1062 静默跳过，social 表不重复
- [ ] 手工往 `like.events` 发一条非法 JSON → worker 无报错、消息被 Ack 丢弃（毒消息第一道防线）
- [ ] 让 worker 处理时杀掉 MySQL → 观察日志 1s/2s/4s 退避重试 3 次后"丢弃"
- [ ] Ctrl+C worker → 日志显示优雅退出，重启后无消息丢失

## 回填清单（本阶段回头改前面阶段的代码，逐条）

**video（阶段 2/3 写的）**
1. `internal/video/video_entity.go` 新增 `OutboxMsg` 结构体，并把它加进 `db.AutoMigrate` 列表
2. `NewVideoService(repo, cache)` → `NewVideoService(repo, cache, popularityMQ)`，存字段 `popularityMQ`
3. `Publish`：单条 `Create(video)` 改成**事务**——`Create(video)` + `Create(OutboxMsg{…"video_published"/"pending"})` + 标签 `FirstOrCreate` + `VideoTag`，全部同一事务；不再直接发 timeline MQ
4. `UpdatePopularity`：`repo.UpdatePopularity`（同步写库）→ `popularityMQ.Update` 成功即 return → 失败则直写 Redis（Del 详情缓存 + `hot:video:1m:<分钟>` ZincrBy + Expire 2h）

**like（阶段 4 写的）**
5. `NewLikeService(repo, videoRepo, cache)` → `NewLikeService(repo, videoRepo, cache, likeMQ, popularityMQ)`
6. `Like/Unlike`：前置校验保留同步；中间改成双 flag 发 MQ；`!mysqlEnqueued` → 阶段 4 的事务直写**原封搬进 nil 分支**；`!redisEnqueued` → `UpdatePopularityCache(±1)`
7. like repo 为 worker 补两个幂等方法：`LikeIgnoreDuplicate`（撞 1062 返回 created=false）、`DeleteByVideoAndAccount`（返回 deleted bool）；video repo 补 `ChangeLikesCount/ChangePopularity`（`GREATEST(expr, 0)` 防负）

**comment（阶段 5 写的）**
8. `NewCommentService(repo, videoRepo, cache)` → `NewCommentService(repo, videoRepo, cache, commentMQ, popularityMQ)`
9. `Publish`：双 flag + 降级事务（检查视频存在 → `Create(comment)` → popularity +1）；`notifyMentions` 在"异步成功"和"降级"两条路径的收尾都要调用
10. `Delete`：`commentMQ.Delete` 成功即 return；失败/MQ 为 nil → `repo.DeleteComment`（单目标降级；删评论不回扣热度，与原项目一致）

**social（阶段 6 写的）**
11. `NewSocialService(repo, accountrepo, cache)` → `NewSocialService(repo, accountrepo, socialMQ, cache)`（cache 阶段 7 已加）
12. `Follow/Unfollow`：顺序固定为 **先 DB → 失效 following feed 缓存 → 最后发 MQ（失败仅 log）**；不要改成 like 那样的异步——写库必须同步成功才返回

**router（阶段 1 起维护的）**
13. 五个 MQ 封装初始化 + 失败置 nil + 传入对应 service：`NewPopularityMQ` / `NewLikeMQ` / `NewCommentMQ` / `NewSocialMQ` / `NewTimelineMQ`，日志统一 `xxx init failed (mq disabled)`
14. `worker.StartOutboxPoller(db, timelineMQ)` + `worker.StartConsumer(timelineMQ, "video.timeline.update.queue", cache, rmq)`
15. notification：`rmq != nil` 时用临时 channel `DeclareTopic` 声明 3 个副本队列（复用业务 exchange）；`NewSSEHub` + `/notification` 路由组（`SSERequireAuth`）；3 个 goroutine 各自重连循环跑 `NewNotificationWorker`

**cmd**
16. 新建 `cmd/worker/main.go`（与 `cmd/main.go` 平级的第二个 main 包：拓扑声明 + connectWithRetry + runWorkerWithRetry + 信号退出）

## 进阶改进

- **真正启用 DLX**：把 `handleDelivery` 里第 3 次失败的 `Ack` 丢弃改成 `Nack(false, false)`，消息进 `<queue>.dlx`；消费死信队列时用 `GetRetryCount` 对照 `MaxRetryCount` 决定再重试还是落库存档 + 告警
- Publisher Confirm（`ch.NotifyPublish`/`Confirm` 模式）+ 备份交换器，把"发布成功"从"socket 写成功"做实
- 消费端限速/批量落库（攒 50 条一事务）
- 给评论删除事件补 popularity -1（修复原项目瑕疵；注意与"删除幂等"联动）
- outbox 批量投递 + 失败指数退避 + 增加后台清理任务（按 EventType/时间归档）
- 通知模块：多端已读回执、SSE 断线重连补拉（stream + list 组合已是雏形）

## 原项目对照

- `backend/internal/middleware/rabbitmq/rabbitMQ.go`（连接基座 + DeclareTopic + PublishJSON + newEventID）
- `backend/internal/middleware/rabbitmq/dlx.go`（DLXExchange/MaxRetryCount + DeclareDLX + GetRetryCount）
- `backend/internal/middleware/rabbitmq/likeMQ.go / commentMQ.go / socialMQ.go / popularityMQ.go / timelineMQ.go`（5 个封装，常量表 + 事件结构体 + publish）
- `backend/internal/worker/likeworker.go`（重点读 handleDelivery 的重试/丢弃与 applyLike 的幂等三处判断）
- `backend/internal/worker/commentworker.go / socialworker.go / popularityworker.go`（同构消费者；social 看 1062 幂等）
- `backend/internal/worker/outboxworker.go`（StartOutboxPoller 轮询中继 + StartConsumer 时间线消费/重连）
- `backend/internal/worker/notificationworker.go / ssehub.go`（通知消费 + SSE 推送 + 通知 REST 接口）
- `backend/cmd/worker/main.go`（connectWithRetry / runWorkerWithRetry / 4 组拓扑声明 / 信号退出；43-88 行是两个重试函数）
- `backend/cmd/main.go`（API 进程：DB 必选、Redis/MQ 可选降级）
- `backend/internal/http/router.go`（61-130 行五个 MQ 初始化与降级；197-252 行 outbox/timeline/notification/SSE 接线）
- `backend/internal/video/like_service.go`（31-106 行降级矩阵原版）
- `backend/internal/video/comment_service.go`（Publish 双 flag + notifyMentions；Delete 单目标降级）
- `backend/internal/video/video_service.go`（47-77 行 Publish 的 outbox 事务；203-230 行 UpdatePopularity 的 MQ 优先降级）
- `backend/internal/video/video_entity.go`（OutboxMsg 定义）、`backend/internal/video/popularity_cache.go`（UpdatePopularityCache：第三条降级哲学的样本）
- `backend/internal/social/service.go`（23-92 行"先 DB 后 MQ"的姿势二）
