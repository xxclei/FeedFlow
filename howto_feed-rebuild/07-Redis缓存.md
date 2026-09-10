# 07 - Redis 缓存层（封装 / 自愈鉴权 / 防击穿锁）

前置：阶段 6。**性能与稳定性的分水岭**，面试价值最高，写慢点。

## 目标

go-redis v9 封装（nil 安全降级）、四类缓存落地（token / 视频详情 / 匿名 Feed / 关注流）、SETNX+Lua 分布式锁防击穿。做完后：**kill 掉 Redis，系统所有接口照常工作**。

## 第一步：封装 Client（一切的前提）

`internal/middleware/redis/` 三个文件，**每个方法都做 nil 接收器保护**——这是降级的核心机制：

```go
type Client struct{ rdb *redis.Client }

func (c *Client) GetBytes(ctx context.Context, key string) ([]byte, error) {
    if c == nil || c.rdb == nil { return nil, redis.Nil }   // cache==nil 时表现为"未命中"
    return c.rdb.Get(ctx, key).Bytes()
}
```

> `cache == nil` 时所有方法返回"未命中/成功"，业务代码就**不需要**到处写 `if cache != nil`——降级被吸收在封装层。（原项目 service 层仍有 nil 判断，属于双保险。）

文件划分：`redis.go`（连接/Ping/Close/Lock/Unlock/IsMiss）、`cache.go`（GetBytes/SetBytes/Del）、`zset.go`（ZincrBy/Expire/Exists/ZUnionStore/ZRevRange…）。

必须理解的 go-redis v9 基础：
- 命令返回 cmd 对象：`.Result()` / `.Err()` / `.Bytes()` 三种取法
- **key 不存在 = `redis.Nil`**（特殊 error，不是普通错误）；封装 `IsMiss(err) = errors.Is(err, redis.Nil)`
- `NewClient` 不连接，要 `Ping` 确认；client 并发安全，全进程单例

## 第二步：接入启动流程（启动时降级）

`cmd/main.go`：`NewFromEnv` → **300ms 超时 Ping** → 失败则 `cache = nil` 继续启动。把 `cache`（可能为 nil）一路传给 router。

## 第三步：四类缓存

| Key | 类型 | Value | TTL | 读 | 写/失效 |
|---|---|---|---|---|---|
| `account:<id>` | string | 当前 jwt token | 24h | 鉴权中间件 | login/rename 时写；logout/改密时 DEL |
| `video:detail:id=<id>` | string | Video JSON | 5m | getDetail | 未命中回填；删除/热度变更时 DEL |
| `feed:listLatest:limit=<n>:before=<u>` | string | 响应 JSON | **5s** | 匿名 listLatest | 回填（短 TTL = 接受轻微不一致） |
| `feed:listByFollowing:limit=<n>:accountID=<id>:before=<u>` | string | 响应 JSON | 5s | 登录关注流 | 同上 |

value 一律 `json.Marshal` 后存字节。**所有 Redis 调用包 50ms 超时 context**：

```go
opCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
defer cancel()
```

### 自愈鉴权（改造阶段 1 的 check()）

```
1. Get account:<id> 成功 → 比对 token，不一致 → 401（已撤销）；一致 → 通过
2. Get 失败（含 Redis 挂） → 查 DB account.token 比对（兜底，绝不因 Redis 拒绝登录用户）
3. DB 校验通过且 Redis 可用 → SetBytes 回填（自愈：Redis 恢复后缓存自动热起来）
```

体会：**缓存加速但不做裁判**——裁判永远是 DB 里的 token 列。

### 缓存旁路 + 防击穿锁（getDetail / listLatest 同一模式）

```
读缓存 ──命中──► 返回
  │未命中
  ▼
SETNX lock:<key> 随机token, TTL(详情2s / Feed 0.5s)
  │拿到锁                         │没拿到锁
  ▼                               ▼
双重检查缓存（可能别人已回填）      轮询 5次×20ms 读缓存
  │仍无                            │拿到 → 返回
  ▼                               │没拿到 → 兜底自己查 DB（不回填也行）
查 DB → 回填缓存 → defer Unlock    ▼
                                兜底查 DB 返回
Unlock：Lua 脚本「GET==token 才 DEL」（防误删别人的锁）
```

为什么解锁要 Lua：GET 判断和 DEL 两步之间锁可能恰好过期易主，"比对+删除"必须原子。

```go
var unlockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1])
else return 0 end`)
```

## 关键设计决策

| 问题 | 原项目答案 |
|---|---|
| 为什么 Feed 缓存只缓存匿名流？ | 登录流 is_liked 因人而异；且登录用户 key 空间大、命中率低。登录关注流单独按 uid 缓存 |
| TTL 为什么 5s 这么短？ | Feed 时效性强，5s 已能抗住突发热点流量，一致性损失可接受 |
| 缓存未命中但 DB 也查不到（缓存穿透）怎么办？ | 原项目未处理。进阶：缓存空值短 TTL 或布隆过滤器 |
| 锁没拿到、轮询也超时了怎么办？ | 兜底查 DB——**永远给请求留活路**，锁只用来"减少"回源，不用来"阻止" |
| DEL 缓存用哪个 ctx？ | `context.Background()`（删缓存是尽力而为，不随请求取消）——原项目如此，也可讨论 |

## 常见的坑

- Set 忘传 TTL → 永久 key 内存泄漏
- 判断未命中用 `err != nil` → 把连接错误也当未命中，Redis 挂了会疯狂打 DB；必须区分 `redis.Nil` 和其他错误
- 锁的 TTL > 回源耗时被别人抢走 → 详情锁 2s、Feed 锁 0.5s 是按回源耗时估的
- 轮询用 `time.Sleep` 不看 ctx → 请求取消了还在傻等；select ctx.Done()
- 结构体加字段后旧缓存 JSON 不匹配 → Unmarshal 失败当未命中处理（封装里已这样做了，注意别丢）

## 验收清单

- [ ] `redis-cli -a 123456` 里 `MONITOR` 观察：登录出现 `SET account:1`，鉴权只有 GET
- [ ] getDetail 第二次调用明显变快，TTL 存在（`TTL video:detail:id=1`）
- [ ] **压测 getDetail 缓存过期瞬间**：MySQL 查询日志里只有 1 条 SELECT（锁生效）
- [ ] `docker stop redis` → 全部接口仍 200（鉴权走 DB、缓存走 DB、日志出现降级）
- [ ] `docker start redis` → 无需重启，缓存逐步自动回填（自愈）

## 进阶改进

- GetDetail 里原项目重复读了一次缓存（getCached 吞掉了 error 无法区分未命中/故障）→ 你重构 getCached 返回 `(val, miss bool, err error)` 一次搞定
- 热点 key 加 singleflight（golang.org/x/sync）替代自旋锁，体会两种方案差异

## 原项目对照

- `backend/internal/middleware/redis/{redis,cache,zset}.go`（封装全套 + Lock/Unlock）
- `backend/internal/middleware/jwt/jwt.go:71-112`（自愈 check）
- `backend/internal/video/video_service.go:79-164`（防击穿锁完整版）
- `backend/internal/feed/service.go:25-116`（Feed 缓存版）
- `backend/cmd/main.go:34-50`（启动降级）
