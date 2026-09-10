# 12 - 私信与 SSE 实时通知 ★ 长连接管理 + 事件广播

前置：阶段 9。私信本身是个"四件套最小练习"（半小时的事）；真正的重头戏是 **SSE 实时通知**：复用阶段 9 已有的 3 条事件流，**不改一行发布端代码**，在 API 进程内再挂 3 个 NotificationWorker，把"点赞/评论/关注"经 SSE 长连接实时推到浏览器。这是全项目唯一住在 API 进程里的 Worker，也是第一次碰长连接。

## 目标

- 私信：`POST /message/send`、`POST /message/list`（双向会话、最近 50 条）
- SSE：`GET /notification/stream` 长连接——连接注册、按用户广播、断连清理、30s keepalive、query token 鉴权
- 通知落库 + 推送：3 个 notification 队列（绑在阶段 9 的 exchange 上）→ NotificationWorker → 写 notifications 表 → hub.Push 实时推送
- 通知中心接口：`/notification/list`、`/notification/markRead`、`/notification/unreadCount`
- 可靠性：每个 notification worker **独立 Channel + 5 秒自动重连循环**；MQ 不可用时整个通知子系统禁用（而非拖垮 API）

## 前置依赖

- **阶段 9**：`like.events` / `comment.events` / `social.events` 三条 topic exchange 和 `LikeEvent`/`CommentEvent`/`SocialEvent` 结构体直接复用，**发布端一行不改**；本阶段只新增队列和消费者
- 阶段 1 的 JWT：`auth.ParseToken`（SSE 用它手动解析 query token）、`jwt.GetAccountID`
- 阶段 0 的 `db.AutoMigrate`：新增 `Message`、`Notification` 两张表
- 不依赖 Redis——通知链路没有缓存

## 接口设计

| 路由 | 鉴权 | 请求 → 响应 |
|---|---|---|
| POST /message/send | JWT | `{to_id, content}` → 直接返回落库后的 Message 对象 |
| POST /message/list | JWT | `{peer_id}` → `{messages: [Message...]}`（与该好友的双向最近 50 条，created_at 倒序）|
| GET /notification/stream | SSE 鉴权 | `?token=JWT` → `text/event-stream`，每条通知一帧 `data: {Notification JSON}\n\n` |
| POST /notification/list | SSE 鉴权 | 无 body → `{notifications: [...]}`（最近 50 条，倒序）|
| POST /notification/markRead | SSE 鉴权 | `{id: 5}` 或 `{}`/空 body → `{message: "ok"}`（带 id 只标一条，不带全标）|
| POST /notification/unreadCount | SSE 鉴权 | 无 body → `{count: 3}` |

Message 对象形状（send 的响应、list 的元素）：

```json
{"id":1,"from_id":1,"to_id":2,"content":"你好","is_read":false,"created_at":"2026-01-01T12:00:00+08:00"}
```

Notification 一帧的 data（与 notifications 表字段一一对应）：

```json
{"id":7,"recipient_id":2,"sender_id":1,"type":"like","target_id":3,"content":"点赞了你的视频","is_read":false,"created_at":"..."}
```

> type 取值：`like` / `comment` / `follow`；target_id 对 like/comment 是视频 ID，对 follow 是关注者的账号 ID。错误响应沿用项目约定 `{error}`。

**鉴权差异要注意**：`/notification/*` 挂的不是 `jwt.JWTAuth`，而是 hub 自己的 `SSERequireAuth`——token 可从 `?token=` 或 `Authorization: Bearer` 两个地方取（浏览器 EventSource 发不了自定义 header，query 是给它的后门）。

## 数据模型

```go
// internal/message/entity.go
type Message struct {
    ID        uint      `gorm:"primaryKey" json:"id"`
    FromID    uint      `gorm:"index:idx_message_from;not null" json:"from_id"`
    ToID      uint      `gorm:"index:idx_message_to;not null" json:"to_id"`
    Content   string    `gorm:"type:text;not null" json:"content"`
    IsRead    bool      `gorm:"default:false" json:"is_read"`
    CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
}

// internal/worker/notificationworker.go（原项目把模型放 worker 包）
type Notification struct {
    ID          uint      `gorm:"primaryKey" json:"id"`
    RecipientID uint      `gorm:"index;not null" json:"recipient_id"` // 收通知的人，读写都以它为条件
    SenderID    uint      `gorm:"not null" json:"sender_id"`          // 触发者，前端拿它查头像昵称
    Type        string    `gorm:"type:varchar(50);not null" json:"type"`
    TargetID    uint      `json:"target_id"`
    Content     string    `gorm:"type:varchar(255)" json:"content"`   // 固定文案，如"点赞了你的视频"
    IsRead      bool      `gorm:"default:false" json:"is_read"`
    CreatedAt   time.Time `gorm:"autoCreateTime" json:"created_at"`
}
```

- 两个方向各一个普通索引：会话查询是 `(from=? and to=?) or (from=? and to=?)`，靠 `idx_message_from`/`idx_message_to` 走索引
- Notification 表的 AutoMigrate 有**两个挂点**：中心 `db.AutoMigrate` 里，以及 `NotificationWorker.Run` 开头（每次重连都幂等建一次，防"表不存在"）
- `Message.IsRead` 在原项目是**死字段**——定义了但没有任何接口写它（见进阶改进）

**通知拓扑（复用 exchange，只加队列）**：

| Exchange（阶段 9 已有） | 新队列 | Binding（精确 routing key，非通配！）| 消费者 |
|---|---|---|---|
| `like.events` | `notification.like` | `like.like` | NotificationWorker-Like（API 进程）|
| `comment.events` | `notification.comment` | `comment.publish` | NotificationWorker-Comment（API 进程）|
| `social.events` | `notification.social` | `social.follow` | NotificationWorker-Social（API 进程）|

一条点赞事件发布后进**两个**队列：`like.events`（Worker 进程落库计数）+ `notification.like`（API 进程发通知）——topic exchange 的一事件多队列扇出，这里第一次真正派上用场。`like.unlike`/`comment.delete`/`social.unfollow` **故意不绑定**：取消动作不产生通知。

## 关键设计决策

**Q1：为什么用 SSE 而不是 WebSocket？** 需求只需要**服务端 → 客户端**单向推送（发私信、点赞这些上行仍是普通 POST 请求）。对比：

| | SSE | WebSocket |
|---|---|---|
| 方向 | 单向（服务端→客户端）| 双向 |
| 协议 | 就是普通 HTTP，无需升级 | 需要 Upgrade 握手 |
| 断线重连 | 浏览器 EventSource 内置自动重连 | 自己写心跳+重连 |
| 代理/网关兼容 | 好（HTTP 语义）| 常被中间件掐 |
| 依赖 | 零（gin 直接写）| 通常引 gorilla/websocket |

什么时候才值得上 WebSocket：打字中状态、已读回执、真正的双向聊天。本项目到不了那个复杂度，SSE 是"够用且最省"的选择。

**Q2：为什么 NotificationWorker 跑在 API 进程，而不是 cmd/worker？** SSEHub 的 `clients map` 在 **API 进程的内存里**，Worker 进程消费完根本推不进来（跨进程推送见难点 3）。代价是通知消费与 API 同生共死；要解耦就得引入 Redis pub/sub 或独立推送网关——进阶改进的事。

**Q3：一个用户开多个标签页怎么办？** 映射是 `map[uint][]chan *Notification`——**一个用户对应一个 channel 切片**，每个标签页各 Subscribe 一条。Push 遍历该用户所有 channel 广播，Unsubscribe 只摘除自己那条，摘空了才 `delete(map, userID)`。

**Q4：客户端离线/消费慢，通知会丢吗？** 推送会丢，数据不会丢。Push 用 `select { case ch <- n: default: }` **非阻塞**发送，缓冲（20）满了直接放弃这个连接——慢客户端不能堵死广播。因为 Notification **先落库再推送**，前端重连后调 `/notification/list` 总能补回来。定位想清楚：**SSE 是尽力而为的实时提醒，DB 才是事实源**。

**Q5：EventSource 发不了 header，怎么鉴权？** `SSERequireAuth` 先取 `c.Query("token")`，为空再退回 `Authorization: Bearer`（兼容 curl/Postman），然后 `auth.ParseToken` → `c.Set("accountID", ...)`，和 `jwt.JWTAuth` 落同一个 key。代价：token 会出现在 URL 里、进访问日志——内网可接受，公网见进阶改进（一次性短时票）。

**Q6：MQ 挂了，SSE 怎么办？（降级）** 原项目很干脆：启动时 `rmq == nil` 就打印 `Notification SSE disabled (MQ not available)`，**3 个 worker 一个都不启动**，通知子系统整体下线；运行中 MQ 断了则靠每个 worker 的 5 秒重连循环自愈。注意分寸：`/notification/list` 等接口读 DB 仍然能用，只是不会再有新通知。通知链路**没有**"降级直写"路径——不像点赞（直写 MySQL 就行），通知不落库就等于不存在，宁可安静地没有实时通知，也不在 API 进程里同步写通知拖慢点赞接口。

**Q7：消费失败怎么办？** NotificationWorker 是阶段 9"进阶改进（毒消息处理）"的现成参考实现：`process` 失败 → 指数退避重试（1s/2s/4s，共 3 次）→ 仍失败 `Ack` 丢弃 + 日志，**绝不无限 requeue**。对比阶段 9 的 LikeWorker 用 `Nack(requeue=true)` 无限重试——两种策略你都已经写过一遍了。

## 实现提示

### 私信（最标准的四件套，先热身）

```go
// repo：两个方法
func (r *Repository) Send(ctx context.Context, m *Message) error      // TrimSpace + 空校验 + Create
func (r *Repository) List(ctx context.Context, userID, peerID uint, limit int) ([]Message, error)
// List 的 WHERE：(from_id = ? AND to_id = ?) OR (from_id = ? AND to_id = ?)，Order("created_at desc")，Limit(50)

// handler：jwt.GetAccountID 取发送者 → ShouldBindJSON → 校验 to_id!=0 && content 非空 → repo
// List 的响应记得：msgs == nil 时置为 []Message{}，否则 JSON 是 null 不是 []
```

### SSEHub（`internal/worker/ssehub.go`）

```go
type SSEHub struct {
    mu      sync.RWMutex                  // 同时保护 map 和 channel 生命周期（见难点 2）
    clients map[uint][]chan *Notification // 用户 → 该用户的所有连接
    db      *gorm.DB                      // 通知中心三个查询接口用
}

func (h *SSEHub) Subscribe(userID uint) chan *Notification            // make(chan, 20) + append
func (h *SSEHub) Unsubscribe(userID uint, ch chan *Notification)      // 摘除 + 空则 delete key + close(ch)
func (h *SSEHub) Push(userID uint, n *Notification)                   // RLock；对每个 ch：select{case ch<-n: default:}
```

`Push` 拿读锁、`Unsubscribe` 拿写锁，所以 **close(ch) 和向 ch 发送永远互斥**——这把 RWMutex 是防 `send on closed channel` panic 的关键，别只当它是 map 的锁。

SSE 专门写一个鉴权中间件（不要复用 `jwt.JWTAuth`，它不认 query token）：

```go
func (h *SSEHub) SSERequireAuth() gin.HandlerFunc {
    // token := c.Query("token")；空则读 Authorization 头剥 "Bearer "
    // 仍为空 → Abort 401 {"error":"missing token"}；ParseToken 失败 → 401 {"error":"invalid token"}
    // c.Set("accountID", claims.AccountID)  ← 与 jwt.GetAccountID 同一个 key
}
```

流式 handler 的骨架：

```go
func (h *SSEHub) SSEHandler(c *gin.Context) {
    // 1) 三个响应头（必须在第一次写之前）：text/event-stream / no-cache / keep-alive，然后 WriteHeader(200)
    // 2) ch := h.Subscribe(userID)；defer h.Unsubscribe(userID, ch)
    // 3) flusher, _ := c.Writer.(http.Flusher)
    for {
        select {
        case <-c.Request.Context().Done():   // 客户端断开（浏览器关页/网络断）
            return
        case n, ok := <-ch:
            if !ok { return }
            b, _ := json.Marshal(n)
            fmt.Fprintf(c.Writer, "data: %s\n\n", b)   // SSE 帧格式：data: + 换行 + 空行
            flusher.Flush()
        case <-time.After(30 * time.Second):  // 30 秒没消息就发一帧注释
            fmt.Fprintf(c.Writer, ": keepalive\n\n")   // 以冒号开头 = SSE 注释行，客户端忽略
            flusher.Flush()
        }
    }
}
```

通知中心三个 handler（List/MarkRead/UnreadCount）就是普通查询，都记得带 `recipient_id = ?` 限定本人；markRead 的 `{id}` 用 `*uint` 接（区分"没传"和"传 0"），空 body 用 `errors.Is(err, io.EOF)` 放行。最后 `RegisterRoutes(r, group)` 把 4 条路由集中挂上。

### NotificationWorker

```go
type NotificationHub interface { Push(userID uint, n *Notification) }  // 面向接口：worker 不依赖 gin/HTTP
var _ NotificationHub = (*SSEHub)(nil)                                 // 编译期确认 SSEHub 满足

func NewNotificationWorker(ch *amqp.Channel, db *gorm.DB, queue string, hub NotificationHub) *NotificationWorker
func (w *NotificationWorker) Run(ctx context.Context) error
// Run：AutoMigrate(&Notification{}) → Consume(queue, "", false /*手动Ack*/, ...) → select{ctx.Done / deliveries}
// deliveries 关闭（连接断）→ 返回 error，交给外层重连循环

func (w *NotificationWorker) process(ctx context.Context, d amqp.Delivery) error
// switch d.RoutingKey：
//   "like.like"      → 解 LikeEvent{UserID, VideoID} → 查 videos.author_id
//   "comment.publish"→ 解 CommentEvent{AuthorID, VideoID} → 查 videos.author_id
//   "social.follow"  → 解 SocialEvent{FollowerID, VloggerID} → 收件人就是 VloggerID
// 查作者不用 import video 包，匿名 struct 扫一下即可：
//   w.db.WithContext(ctx).Table("videos").Where("id = ?", evt.VideoID).Select("author_id").Scan(&authorID)
// authorID == 0 或 == 事件发起者（自己赞自己不发）→ return nil
// 未识别的 routing key → notif 为 nil → return nil（直接 Ack 丢弃）
// db.Create(notif) → w.hub.Push(notif.RecipientID, notif)
```

### 路由装配（router.go 尾部，本阶段的粘合层）

```go
// ① 用临时 channel 声明 3 组拓扑（复用已有 exchange）：
//    rabbitmq.DeclareTopic(notifCh, "like.events", "notification.like", "like.like")
//    rabbitmq.DeclareTopic(notifCh, "comment.events", "notification.comment", "comment.publish")
//    rabbitmq.DeclareTopic(notifCh, "social.events", "notification.social", "social.follow")
// ② sseHub := worker.NewSSEHub(db)
//    notifGroup := r.Group("/notification"); notifGroup.Use(sseHub.SSERequireAuth())
//    sseHub.RegisterRoutes(r, notifGroup)
// ③ 降级 + 每队列一个 goroutine 重连循环：
if rmq == nil { log.Printf("Notification SSE disabled (MQ not available)") } else {
    for _, q := range []string{"notification.like", "notification.comment", "notification.social"} {
        go func(queue string) {
            for {
                ch, err := rmq.NewChannel()      // 每个 worker 独立 Channel
                if err != nil { log...; time.Sleep(5 * time.Second); continue }
                w := worker.NewNotificationWorker(ch, db, queue, sseHub)
                if err := w.Run(ctx); err != nil { log... }   // Run 退出 = 连接/队列出问题
                ch.Close()                        // 旧 channel 已废，关掉再重建
                time.Sleep(5 * time.Second)
            }
        }(q)
    }
}
```

这里的 ctx 用 `context.Background()`——通知 worker 跟 API 进程同生命周期，不参与 cmd/worker 那套 `signal.NotifyContext` 优雅退出。

## 难点清单

1. **SSE 长连接管理**：handler 从连接建立一直跑到客户端断开才返回——一个连接就是一个 goroutine。响应头必须在首次写入前设好；一旦进入流式输出，这个连接再也不能 `c.JSON`。忘 `Flush()` 的症状很隐蔽：代码不报错，数据永远堵在缓冲区。
2. **用户-连接映射**：`map[uint][]chan` 的增删查都要过同一把 `sync.RWMutex`，且它还要保证 **close 与 send 互斥**（Push 的 RLock vs Unsubscribe 的写锁）。漏掉任何一点就是 panic 或 goroutine 泄漏：一条连接没清理 = 一个卡死的 goroutine + 一个没人读的 channel。
3. **跨进程推送**：阶段 9 所有 Worker 都在独立进程，唯独通知消费必须和 SSEHub 同进程（内存 map 推不进别的进程）。想清楚这个约束，才能理解为什么 topology 声明和 worker 启动都写在 router.go 里；将来 API 多实例部署时，还必须把"同进程"升级为 Redis pub/sub 广播（或粘性会话），否则通知只到挂着连接的那台实例。
4. **扇出但不重复落库**：同一条 like 消息被 LikeWorker（计数）和 NotificationWorker（通知）各消费一次——这是**有意的**两个不同副作用，不是重复消费；幂等边界在各自队列，互不干扰。
5. **毒消息**：通知 worker 有重试预算（3 次指数退避后 Ack 丢弃），别照抄阶段 9 的无限 requeue——通知队列堵死比丢一条提醒严重得多。

## 亮点

- **SSE 替代 WebSocket 的取舍**：用"单向够用 + 零依赖 + 浏览器自带重连"换掉双向协议的全部复杂度，并把代价（query token、无上行通道）明明白白列出来
- **对阶段 9 零侵入**：一条发布端代码不改，只靠 topic exchange 的多 binding 扇出就接通了通知链路——事件驱动架构红利的最直观演示
- **每个 notification worker 独立 Channel + 5 秒自动重连循环**：AMQP channel 不可复用（坏了就废），三条队列互为隔离——一条队列炸了只重连自己，另外两条照常消费；重连是"外层 for + 每轮新建 Channel"而不是在 Run 里修补
- **NotificationHub 接口解耦**：worker 只依赖 `Push(userID, n)` 一个方法，单测可注入 fake hub，将来换 Redis pub/sub 实现不动 worker
- **先落库再推送**：推送丢失永远可由 `/notification/list` 补偿，DB 是唯一事实源
- **30s keepalive 注释行**：专治 Nginx/浏览器对空闲连接的超时掐断

## 常见的坑

- 忘 `flusher.Flush()` → 消息"发出去了"但客户端永远收不到（最常见，没有之一）
- keepalive 间隔拉太长（如 5 分钟）→ 中间代理把空闲连接掐了，客户端还在傻等
- `Push` 写成阻塞发送 `ch <- n` → 一个关着不读的慢客户端把整个广播卡死
- `Unsubscribe` 忘 `close(ch)` → goroutine 泄漏；在 Push 可能正在发送时 close → panic（靠锁的读写互斥避免，见实现提示）
- binding 写成通配 `like.*` → unlike 事件也进通知队列 → "取消点赞"也给用户推一条；必须用精确 key `like.like`
- 忘跳过自己（`authorID == evt.UserID`）→ 自己给自己点赞也收到"点赞了你的视频"
- Notification 表只在 `NotificationWorker.Run` 里建 → MQ 没起来时表不存在，`/notification/list` 直接 SQL 报错（所以中心 AutoMigrate 也要挂上）
- `/message/list` 忘了 `msgs == nil → []Message{}` → 前端拿到 `null` 遍历报错
- `/message/send` 不校验对方存在 → 可以给不存在的 user_id 发私信（原项目就这样，复刻时至少加个 FindByID）
- 挂载 `/notification` 组时用了 `jwt.JWTAuth` → EventSource 连不上（它发不了 header），还纳闷为什么 curl 能通浏览器不能通
- 3 个 notification worker 共用一个 Channel → 一个队列声明失败连累全部（AMQP channel 出错即整体不可用）；照抄 `rmq.NewChannel()` 每队列独立
- 大量通知时 Consume 没设 `Qos` → MQ 无脑全推（原项目这三个 worker 确实没设，量小没事，你要知道这是个待改进点）

## 验收清单

- [ ] Postman：A 给 B 发私信 → 200 返回带 `id` 的 Message；B `POST /message/list {"peer_id":A}` 看到这条；A 的视角查 peer_id=B 也能看到（双向）
- [ ] `to_id=0` 或 `content=""` → 400 `{"error":"to_id and content are required"}`；list 空 会话 → `{"messages":[]}` 而非 null
- [ ] `curl -N --no-buffer "http://127.0.0.1:8080/notification/stream?token=<A的JWT>"` 挂住，30 秒后收到 `: keepalive`
- [ ] 不带 token / 带假 token 连 stream → 401 `{"error":"missing token"}` / `{"error":"invalid token"}`
- [ ] A 点赞 B 的视频 → B 的 stream **秒收** `data: {"type":"like","target_id":...}`，且 notifications 表多一行；评论、关注同理（type=comment/follow）
- [ ] A 给自己视频点赞 → 无通知、无落库
- [ ] B 开两个终端各挂一条 stream → 两条都能收到（多连接广播）
- [ ] A 连点 50 次赞 → B 接口不卡、不 panic；stream 缓冲溢出时丢弃但 `/notification/list` 能查全
- [ ] `POST /notification/unreadCount` 的 count 与未读数一致；`markRead {"id":5}` 只标一条，空 body 全标；标别人的通知（id 存在但 recipient 不是你）→ 不生效
- [ ] `docker stop rabbitmq && docker start rabbitmq` → API 日志出现"创建 Channel 失败/…5秒后重连"，MQ 恢复后通知继续到（重启期间的点赞，事后 `/notification/list` 有账）
- [ ] 启动时不配 MQ → 日志打印 `Notification SSE disabled`，API 其余功能正常
- [ ] Ctrl+C 断开 curl → handler 正常返回；压测前后 `go tool pprof` goroutine 数不持续增长（无泄漏）

## 进阶改进

- **一次性短时票**：`POST /notification/ticket` 用 JWT 换 30 秒一次性 ticket，stream 改挂 `?ticket=`——token 不再进访问日志
- **私信实时化**：`/message/send` 落库后 `hub.Push(toID, ...)` 推一条 `type=message` 通知（复用现成 hub，原项目没做——私信目前没有实时提醒）
- **把 IsRead 死字段用起来**：会话已读接口 + 联系人列表未读数；顺便给 message 补上游标分页（现在固定最近 50 条）
- **通知聚合**：30 秒内 N 个赞合并成一条"N 人赞了你的视频"（落库前查重或 Redis 计数窗口）
- **跨实例推送**：API 多副本时，NotificationWorker 移回 worker 进程，经 Redis pub/sub 把通知扇到所有 API 实例的 hub
- **三队列合一**：一个 `notification.all` 队列绑 `notification.*`，对比"隔离性好但 channel 多"的取舍
- **断点续传**：SSE 帧带 `id:` 字段，客户端重连带 `Last-Event-ID`，服务端从 DB 补发
- **背压可观测**：Push 丢弃计数暴露成 metrics；notification 队列积压告警

## 原项目对照

- `backend/internal/message/entity.go` + `handler.go`——注意：这个模块的四件套挤在两个文件里，`Repository`/`Service` 是 handler.go 里的瘦壳（handler 甚至直接摸 `service.repo`）。你按项目约定拆成四个文件，就已经比原项目规范
- `backend/internal/worker/ssehub.go`——hub（Subscribe/Unsubscribe/Push）、SSERequireAuth、SSEHandler（keepalive 在此）、通知中心 3 个 handler、RegisterRoutes
- `backend/internal/worker/notificationworker.go`——Notification 模型、NotificationHub 接口、Run/手动 Ack、process 的 routing key 分派、3 次指数退避后丢弃
- `backend/internal/http/router.go:186-252`——message 分组（186-196）、notification 拓扑声明（207-220）、hub 挂载（221-224）、3 个重连 goroutine + 降级（226-252）
- `backend/cmd/main.go` + `backend/internal/db/db.go`——`db.AutoMigrate` 里同时挂了 `&message.Message{}` 和 `&worker.Notification{}`（原项目让 db 包反向依赖 worker 包来拿模型；你可以把 Notification 挪成独立的 `internal/notification` 四件套，顺便解掉这个别扭的依赖）
- 事件结构体复用：`backend/internal/middleware/rabbitmq/{likeMQ,commentMQ,socialMQ}.go`
- 原项目**前端没有接 SSE**（frontend 里搜不到 EventSource，只有 `/message/send`、`/message/list` 的调用）——浏览器侧验证请自己写 `new EventSource('/notification/stream?token=...')` 或用 curl
