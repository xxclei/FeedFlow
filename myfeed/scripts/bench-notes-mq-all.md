# 阶段9（RabbitMQ + Worker）全部四条链路的 before/after 实测记录

点赞链路另见 `bench-notes-like-mq.md`（那条链路 hay 测不了，方法不一样）。
本文件覆盖 comment / social / timeline-outbox / notification 四条。

**测量方法**：单请求顺序发 N 次，同时开 MySQL general log 做**逐条 SQL 归因**
（不是"总数相减"）。为什么必须逐条归因见第三节。

---

## 一、结论总表

| 操作 | before 请求侧 | **after 请求侧** | after 异步侧 | before 总 | after 总 |
|---|---|---|---|---|---|
| comment publish | 5 | **2** (−60%) | 3 (CommentWorker) + 1 (通知查视频) | 5 | 6 |
| comment delete | 3 | **1** (−67%) | 2 (CommentWorker) | 3 | 3 |
| social follow | 4 | **4** (不变) | 1 (SocialWorker 撞 1062) + 1 (通知 INSERT) | 4 | 6 |
| social unfollow | 4 | **4** (不变) | 1 (SocialWorker DELETE) | 4 | 5 |
| like（另见 like-mq 记录） | 5.50 | **2.00** | 3~4 | — | — |

**一句话**：范式 A（comment/like）把请求路径的 SQL 砍掉 60~67%，代价是同样的
SQL 搬到了 worker；范式 B（social）请求路径**一条都没少**，异步侧反而多了。

**"总 SQL 没降"不是 bug，是 MQ 的本职。** MQ 卖的是"用户等的那段时间变短"，
不是"系统干的活变少"。见 `async-moves-work-not-reduces`。

### 逐请求延迟（顺序单请求，worker 在跑）

| 操作 | n | p50 | p95 | 墙钟 |
|---|---|---|---|---|
| comment publish | 20 | 5.76 ms | 6.35 ms | 3795 ms |
| comment delete | 60 | 4.38 ms | 4.93 ms | 11423 ms |
| social follow/unfollow | 20 | 14.9 ms | 15.9 ms | 4026 ms |

⚠ **这三个绝对值包含 curl 进程启动开销（约 2~4ms），只能横向比，不能当接口真实延迟看。**
social 明显比 comment 慢，因为它是 4 条 SQL + 一次 `SCAN` 式缓存失效（`DelByPattern`）。

---

## 一之二、用户等的那段到底快了多少（before/after 延迟对比）

上面那张表只有 after。before 怎么测的：**把 RabbitMQ 容器停掉** ——
此时 `mysqlEnqueued=false`，service 走**降级直写** `publishMySQLDirect`，
那条路径的 SQL 和改 MQ 之前的同步路径**逐条一致**（实测 general log 确认 = 5 条）。
所以它就是 before，而且不用改一行代码。

| 操作 | before p50 | after p50 | 降幅 | before p95 | after p95 | 降幅 |
|---|---|---|---|---|---|---|
| 点赞 | 15.7 ms | **4.2 ms** | **−73%** | 19.2 ms | 4.9 ms | −74% |
| 评论发布 | 17.95 ms | **5.76 ms** | **−68%** | 20.31 ms | 6.35 ms | −69% |
| 评论删除 | 14.18 ms | **4.38 ms** | **−69%** | 15.12 ms | 4.93 ms | −67% |

**三条都是 −70% 左右，非常一致。**这与 SQL 数的降幅（−60~67%）同向且更大 ——
因为省掉的 SQL 里包含**一个写事务**（`BEGIN`/`COMMIT` + 3 条语句 + fsync），
事务的固定开销比"3 条只读 SELECT"贵得多。

⚠ 两个口径说明：
1. before 的数字**含一次失败的 MQ 发布尝试**（channel 已关，立即返回错误），
   而真正的"历史 before"里没有这次调用 —— 所以这个 before 是**上界**，真实降幅略小于表中数值。
2. 绝对值都被 curl 进程启动开销（2~4ms）抬高。**只看降幅，别看绝对值。**

⚠ social 没有 before/after 延迟对比，因为它的请求路径 SQL **一条没变**（见总表），
停不停 RabbitMQ 都是 4 条，测了也是同一个数。

---

## 二、每一条的逐条 SQL 归因（general log 原文计数）

### 2.1 comment publish ×20（全部 200）

```
20  SELECT * FROM accounts WHERE id = N      ← 请求侧：handler accountService.FindByID（取 username）
20  SELECT * FROM videos   WHERE id = N      ← 请求侧：service IsExist
20  SELECT * FROM videos   WHERE id = N      ← CommentWorker：applyPublish 的 IsExistTx
20  INSERT INTO comments (...)               ← CommentWorker：CreateCommentTx
20  UPDATE videos SET popularity = +1        ← CommentWorker：ChangePopularityTx
20  SELECT * FROM videos   WHERE id = N      ← NotificationWorker：handleComment 的 GetByID
──────────────────────────────────────────────────────────────────
    SELECT videos 合计 60 = 请求 20 + worker 20 + 通知 20   ✓ 三者独立可分辨
```

- **请求侧 = 2/次**（accounts 1 + videos 1）
- **worker 侧 = 3/次**，正好等于原同步事务里那三条（不多一条不少一条）
- **通知侧 = 1/次**，只有一次 `GetByID`（本例作者=评论者，所以查完就 return，没有 INSERT）

### 2.2 comment delete ×60（全部 200）

```
60  SELECT * FROM comments WHERE id = N      ← 请求侧：service GetByID
60  DELETE FROM comments WHERE id = N        ← CommentWorker：DeleteCommentTx
60  UPDATE videos SET popularity = -1        ← CommentWorker：ChangePopularityTx
```

- **请求侧 = 1/次**；**worker 侧 = 2/次**；**通知 = 0**
- 通知为 0 是**设计如此**：`notification.comment` 只绑 `comment.publish`，
  删评论的消息在**交换机层**就被分流掉了（见 topology.go 的 notificationLinks）

### 2.3 social follow ×10 / unfollow ×10（全部 200）

```
40  SELECT * FROM accounts WHERE id = N      ← 2/请求（FindByID follower + vlogger）× 20 请求
20  SELECT count(*) FROM socials ...         ← IsFollowed 预检 × 20
20  INSERT INTO socials (follower,vlogger)   ← 请求侧 10 + SocialWorker 10
20  DELETE FROM socials WHERE ...            ← 请求侧 10 + SocialWorker 10
10  INSERT INTO notifications (...)          ← NotificationWorker，只对 10 次 follow
```

拆开：
- **follow 请求侧 = 4**（2×accounts + COUNT + INSERT）→ 和 before **完全一样**
- **follow 异步侧 = 1**（SocialWorker 的冗余 INSERT 撞 1062）+ **1**（通知 INSERT）= 2
- **unfollow 请求侧 = 4**（2×accounts + COUNT + DELETE）→ 和 before **完全一样**
- **unfollow 异步侧 = 1**（SocialWorker 的冗余 DELETE，0 行）+ **0**（取关不通知）
- `INSERT socials` 日志计数 20 = 请求 10 + worker 10（worker 那条必然 1062 失败，但仍然计入 SQL）

**范式 B 的尴尬如实记录**：同步路径已经写库了，所以每条 follow 消息都必然撞唯一索引，
SocialWorker "几乎什么都不干"，它存在的意义是"把队列吃干净"和给通知链路提供一个触发点。

### 2.4 通知子系统（本阶段唯一全新的一层）

三种通知各验证了一次，`notifications` 表实际落库：

| id | recipient | sender | type | target_id | 说明 |
|---|---|---|---|---|---|
| 1 | 3 | 1 | follow | **1** | TargetID = followerID（点通知跳去关注者主页）|
| 2 | 3 | 1 | like | **279** | TargetID = videoID（跳回被赞视频）|
| 3 | 3 | 1 | comment | **279** | TargetID = videoID |

- `handleFollow` **0 次 DB 查询**（收件人 vlogger_id 就在事件里）→ 日志确认只有 1 条 INSERT
- `handleLike`/`handleComment` 各 **1 次 `SELECT videos`**（要点赞/评论的视频上取 authorID）
- 三种通知都**不给自己发**：作者 == 操作者 → 查完 return，不 INSERT（已实测）

---

## 三、这次踩到的三个测量陷阱（都会让结论完全错误）

### 陷阱 1：API 进程里的 NotificationWorker 会污染"请求侧"读数

第一轮测 comment publish 紧窗口，得到 **3** 而不是分析出的 2，连测三次都是 3。
开 general log 才看清第三条是 `SELECT videos WHERE id = 279`，时间戳比第二条晚 3ms
（正好一个 MQ 往返），来源是 **API 进程里的 NotificationWorker** ——
`notification.comment` 绑 `comment.publish`，消息一发出去消费者立刻拿 `GetByID` 查作者。

**停掉 worker.exe 也没用**：NotificationWorker / TimelineConsumer / OutboxPoller
都跑在 **API 进程**里。想单独量请求路径，只能靠 general log 逐条归因，
"总数相减"必然把异步侧的 SQL 算进请求侧。

（这也是为什么 delete 是 1 而 publish 是 3：`notification.comment` 不绑 `comment.delete`。）

### 陷阱 2：worker 消费只要 ~10ms，会抢在"关日志"之前或之后跑完

跑完请求就 `SET GLOBAL general_log=0` 的话，worker 那一批可能还没消费完 ——
第一次测就只记到 61 条（预期 100）。**必须在请求循环后 sleep 3 秒再关日志**，
让 worker 把这一批排空。sleep 期间的 outbox 轮询噪声用 `grep -v outbox_msgs` 剔掉。

### 陷阱 3：限流桶必须每次请求前清（`stateful-route-breaks-hey` 的本地版）

`comment_write = 10/min`、`social_write = 20/min`、`like_write = 30/min`，按账号计数。

测 comment publish ×20 时没清桶 → 10 个 200 + 10 个 429（**10 个 429 的路径只有 0 条业务 SQL**，
和"发布失败"混在一起会算出一道假数据）。测 social 时更隐蔽：
连打 10 次 follow 只有第 1 次成功，后面 9 次在 `IsFollowed` 预检就 500（只花 3 条 SQL），
得出的是"social 只要 3.1 条 SQL"这种**错误结论**。

**正确做法**：每次请求前 `DEL v1:ratelimit:<桶>:<account_id>`，
并且 social 必须 **follow/unfollow 成对**发（表状态要么先 `DELETE FROM socials` 清干净）。

---

## 四、附带跑通的：outbox → MQ → timeline 首次端到端

启动时发现 **245 条 pending** —— 从阶段 3 起 `outbox_msgs` 一直在写（视频发布时和视频同事务落库），
但**一直没有任何消费者**，积压了 245 条。轮询器起来后 1 秒一轮排空（100 / 100 / 45），
最终 `ZCARD v1:feed:global_timeline = 245`，所有队列归零，`outbox_msgs` 无 pending。

这验证了 outbox 的核心承诺：**积压不丢失，恢复后被一次性补投**。
代价是可见性延迟 —— 那 245 条视频在时间线里"迟到了几个小时"，
但它们的 ZSet score 用的是 **original create_time**，所以排名仍然各自回到正确的位置，
不会一起挤到最前面。

---

## 五、通知 + SSE 端到端验证（全部跑通）

### 5.1 SSE 长连接（`GET /notification/stream?token=...`）

响应头确认：

```
HTTP/1.1 200 OK
Content-Type: text/event-stream
X-Accel-Buffering: no          ← 没有它，nginx 会把流缓冲住，前端一个字都收不到
Transfer-Encoding: chunked
```

实测推送：account 5 挂着 SSE，account 1 点赞它名下的视频，连接上收到：

```
data: {"id":15,"recipient_id":5,"sender_id":1,"type":"like","target_id":279,"content":"点赞了你的视频","is_read":false,"created_at":"2026-09-14T17:20:47.118+08:00"}
```

完整链路一次跑通：**点赞 → MQ → NotificationWorker（API 进程）→ 落库 → SSEHub.Push → 活连接**。

`"id":15` **非 0** 这一点是刻意的验证点：它证明 `save` 里传的是 `n` 本身而不是副本
（`Create` 把自增 ID 回填进了 `n`）。传副本的话这里是 `id:0`，前端拿它调 markRead 会打空。

### 5.2 四个接口

| 接口 | 结果 |
|---|---|
| `POST /notification/unreadCount` | `{"count":1}` → markRead 后 `{"count":0}` ✓ |
| `POST /notification/list` | 返回完整 JSON，中文 `content` 正确 ✓ |
| `POST /notification/markRead` `{id}` | `is_read` 落库 0 → 1 ✓ |
| `POST /notification/markRead` `{}` | 全部已读 ✓ |
| `GET /notification/stream` 无 token | **401** ✓ |
| `GET /notification/stream` token=garbage | **401** ✓ |

### 5.3 跨用户越权（本项目对原项目的收紧）

`notifications` 的 `MarkRead` 带了 `recipient_id` 条件。实测：用 account 1 的 token
去 markRead account 3 的通知（id=1）→ HTTP 200，但库里 `is_read` **仍然是 0** —— 行没被动过。

⚠ **响应口径偏松**：返回 200 而不是 404/403。`MarkRead` 返回的 `(bool, error)` 里那个 bool
（有没有删到行）在 handler 里被丢掉了。想收紧的话是一行的事，这里照原项目保留。

---

## 六、清理遗留（已完成）

| 对象 | 处理 | 结果 |
|---|---|---|
| `comments` | 删 `bench-*` / `notif-check` 共 6 行 | 剩 3 行（阶段早期的历史数据）|
| `videos` id=279 | `author_id`→1、`popularity`→1、`likes_count`→0 | 已还原 |
| `likes` | 删测试行 | 0 行 |
| `socials` | 清空 | 0 行 |
| `notifications` | 删全部测试通知（15 行）| 0 行 |
| `accounts` | 删测试账号 benchrecv(id=5) | 剩 af(1) / xclei2(3) |
| `outbox_msgs` | 检查 | **0 条 pending** |
| Redis `v1:ratelimit:*` | 全部删除 | 已清空 |
| Redis `v1:feed:global_timeline` | **保留** | 245（真实历史，不是测试数据）|
| 16 条队列（含 5 条 DLX） | 检查 | **全部 0** |

---

## 七、一句话总结

代码层面**五条链路（like / comment / social / timeline-outbox / notification×3）全部完成并跑通**，
`go build` / `go vet` / `go test ./internal/...` 全绿。

量化结论：**范式 A（like/comment）请求路径 SQL 降 60~67%；范式 B（social）一条没降，
异步侧反而多了两条。** 这不是实现差异，是两种降级姿势的必然结果 ——
范式 B 把"关注关系已经落库"当成既成事实，MQ 只用来触发通知，
所以它在请求路径上**什么也没省**，换来的是"关注完刷新一定看得到"。

值不值得，取决于这条写是不是热点：点赞是热点（值得付延迟），关注不是（不值得）。

---

## 八、⚠ 顺带发现的一个真缺陷：MQ 断了**永远不会自己恢复**

测 before 的时候把 RabbitMQ 容器停掉又起来，发现：

```
2026/09/14 17:35:35 [comment] Publish 投递失败, 降级为同步直写: ... "channel/connection is not open"
2026/09/14 17:35:49 [comment] Publish 投递失败, 降级为同步直写: ... "channel/connection is not open"
        ↑ 此时 RabbitMQ 已经恢复 20 秒了，仍然在降级
```

同时消费者在无限重试：

```
2026/09/14 17:35:50 [NotificationWorker] notification.comment 消费中断, 5 秒后重连: "channel/connection is not open"
```

**根因**：整个 `internal/middleware/rabbitmq/` 包里 `amqp.Dial` **只出现一次**（`rabbitmq.go:83`，
启动那一刻）。`RabbitMQ.NewChannel()` 的实现是：

```go
func (r *RabbitMQ) NewChannel() (*amqp.Channel, error) {
	if r == nil || r.Conn == nil { return nil, errors.New("rabbitmq not connected") }
	return r.Conn.Channel()      // ← r.Conn 已经死了，在死连接上开 channel
}
```

所以所有"重连"循环（`runWorkerWithRetry`、`StartConsumer`、`StartNotificationConsumers`、
`consumeNotifications`）**都在空转**：它们每 5 秒调一次 `NewChannel()`，
拿到的一直是 `channel/connection is not open` —— 没人重新 `Dial`。

**为什么这个缺陷特别阴**：

1. **降级掩盖了它。** 点赞/评论会静默走同步直写，接口 200、功能正常、用户无感。
   唯一的表现是"MQ 里再也没有消息了"，而这一点没有任何告警。
2. **降级本来是临时的，实际变成了永久的。** 降级的设计意图是"熬过 broker 抖动"，
   结果它熬不过去 —— **要么重启进程，要么一直降级到天荒地老**。
3. **命名骗人。** 函数叫 `runWorkerWithRetry`、日志写"5 秒后重连"，
   读代码的人会以为断线重连已经做了。`guide-dont-execute` 的那类坑：
   **名字承诺了一个行为，实现里没有。**

**修法**（本轮没做，先记下）：在 `NewChannel()` 里判 `r.Conn.IsClosed()`，
是就重新 `amqp.Dial`（需要把 dial URL 存进结构体，并在新连接上重新声明拓扑）。
更好的做法是用 `conn.NotifyClose(make(chan *amqp.Error))` 做事件驱动的重连，
而不是在每次用时才检查 —— 后者会让"连接已死"这件事延迟到下一个请求才被发现。

**验证方式**：`docker stop myfeed-rabbitmq && sleep 5 && docker start myfeed-rabbitmq`，
等 30 秒，然后发一条评论 —— 如果 API 日志还在打"降级为同步直写"，说明没重连。
（本轮实测：确实没重连，重启 API 和 worker 两个进程后立刻恢复。）

---

## 九、⚠ 明确**没有**测的东西（别当成测过了）

### 9.1 并发吞吐 / 饱和点 —— 完全没测

本文件所有延迟数字都是**顺序单请求**（curl 一个一个发），不是并发下的表现。
没有 p99@C=50/100/200，没有错误率曲线，没有"撑到多少个并发开始劣化"。

CSV 里 like 那两行的 `qps`（before 61.1 / after 52.2）**不能拿来回答并发问题** ——
`bench-notes-like-mq.md` 里当时就标了口径不同：before 是纯发请求的墙钟，
after 的墙钟含约 0.85 秒**在等落库**。

**为什么当初没测**（三个障碍，将来想补的话还在这）：
1. `like`/`comment`/`social` 都是**有状态一次性**接口，hey 打 8000 次只有 1 次进真实路径
   （见 `stateful-route-breaks-hey`）
2. 限流按账号：like 30/分、comment 10/分、social 20/分 —— 并发一上来全是 429
3. 是写接口，反复打同一个目标会改状态

**要补的话怎么做**：造 **N 个账号 × M 个视频**得到 N×M 个"第一次"操作
（注册接口限 5 次/小时/IP，账号得直接插库），然后 C 并发去打；
before 仍然用"停 RabbitMQ 切降级"那招，不用改代码。
现有 `.run/likebench/main.go` **是单线程的**（无 `go func`/`WaitGroup`），得改造成并发版。

### 9.2 一个必须先说清楚的前提（否则会测出个平局然后误判）

**MQ 不会提升吞吐上限。** 它把工作从请求路径搬到 worker，系统总的 SQL **不动**
（like 链路实测 341 → 341）。跑满时瓶颈还是同一个 MySQL。

所以"并发能力提升"的准确表述**不是 QPS 变高**，而是：
- 单请求**占用 DB 连接的时长**变短（5 条 SQL + 一个写事务 → 2 条只读），
  同一个连接池下能同时撑住更多请求
- 并发下的 **p99 / 错误率**更好

**该测的指标是这两个，不是 QPS 上限。** 直接测 QPS 上限大概率得到"没变化"，
然后被误读成"MQ 没用"。这正是 `bench-must-target-instrumented-route` 的同类坑：
指标选错，结论反的。

### 9.3 其他没测的

- **断线重连缺陷**（第八节）只做了手动验证，**没有写自动化测试**
- **DLX 死信队列**：5 条队列都建了 `.dlx`，但**从未制造过一条死信**去验证它真的会进 DLX
- **`processed_events` 去重**：CommentWorker / NotificationWorker 的重复插入问题
  只在注释里记了"已知不修"，**没有实测过重复投递会产生什么**
- **prefetch 的取值**（消费端 10、worker 端 50）是照原项目抄的，**没有压测验证过**
