# 07 - Redis 缓存层（封装 / 自愈鉴权 / 防击穿 / 限流）

前置：阶段 6。**性能与稳定性的分水岭**，面试价值最高，写慢点。

## 目标

go-redis v9 封装（nil 安全 + `Key()` 前缀约定 + Lua 脚本）、启动降级与请求级降级、六类缓存落地（token 三件套 / 视频详情 / 视频实体 / 关注流响应 / 全局时间线 ZSET / 热榜桶）、SETNX+Lua 防击穿锁与 singleflight、固定窗口限流中间件。做完后：**kill 掉 Redis，系统所有核心接口照常工作**；`redis-cli` 能看到 `v1:` 前缀的 key；登录接口刷第 11 次会被 429。

## 三级降级哲学在本阶段的落点

00 篇立的三条规矩，本阶段兑现前两条（第三条"按目标降级"在阶段 8/9 展开，本阶段埋的 `UpdatePopularityCache` 就是它的预告）：

1. **启动时降级**：Redis 连不上 → 置 `cache = nil`，日志提示，照常启动（功能整体禁用，服务不退出）
2. **请求级降级**：单个 Redis 命令失败/超时 → 回源 MySQL；缓存操作一律 50ms 超时，绝不拖垮主请求
3. （运行时按目标降级：MQ 发失败 → 只对失败的目标直接写——阶段 9 的点赞/评论降级矩阵）

## 第一步：封装 Client（一切的前提）

`internal/middleware/redis/` 三个文件。**每个方法都做 nil 接收器保护**——这是启动降级能成立的核心机制：

```go
type Client struct {
    rdb       *redis.Client
    keyPrefix string          // 默认 "v1:"，所有 key 统一带版本前缀
}

const defaultKeyPrefix = "v1:"

func NewClient(rdb *redis.Client, keyPrefix string) *Client          // 测试用：直接注入现成 client
func NewFromEnv(cfg *config.RedisConfig) (*Client, error)           // 只建 client 不连接（懒连接）
func (c *Client) Key(format string, args ...any) string             // prefix + fmt.Sprintf(...)
func (c *Client) Ping(ctx context.Context) error                    // nil 接收器 → 返回 error
func (c *Client) Close() error
func IsMiss(err error) bool                                         // err == redis.Nil
```

`Key()` 的 nil 接收器保护有个妙用：`c == nil` 时前缀置空直接拼 key。jwt 中间件的 `check()` 第一行就敢写 `cache.Key("account:%d", id)` 而不判空，靠的就是它。

**nil 策略按方法分两类**（原项目的实际约定，背下来）：

| 类别 | 规律 | 方法 |
|---|---|---|
| 尽力而为：丢了无妨 | `c == nil` → 返回零值 + nil（当"成功"吞掉） | 写类：`SetBytes` 之外的 `ZincrBy`/`ZAdd`/`Expire`/`ZUnionStore`/`ZRemRangeByRank`、尽力读：`ZRevRange`/`ZRevRangeByScore`(nil,nil)、`Exists`(false,nil)、锁：`Lock`→( "",false,nil )、`Unlock`→nil、`IncrementWithExpire`→(0,nil)、`DelByPattern`→nil |
| 结果要当业务依据 | `c == nil` → 返回 error，逼调用方走降级路径 | `GetBytes`/`SetBytes`/`Del`/`MGet`/`ZRangeWithScores`/`Ping` |

> 注意两个细节：① `GetBytes` 在 nil 时返回的是**普通 error**，不是 `redis.Nil`——调用方用 `IsMiss(err)` 区分"真未命中"和"故障/未启用"，故障绝不会被误判成未命中而去疯狂打 DB；② 原项目 service 层仍有 `if cache != nil` 判断，属于双保险（`ListLatest`、`ListByFollowing`、chunk 上传都靠它提前短路）。

文件划分：`redis.go`（连接/Ping/Close/Key/IsMiss/Lock/Unlock/IncrementWithExpire）、`cache.go`（GetBytes/SetBytes/Del/DelByPattern/MGet）、`zset.go`（ZincrBy/ZAdd/ZRemRangeByRank/ZRangeWithScores/Expire/ZUnionStore/Exists/ZRevRange/ZRevRangeByScore）。

必须理解的 go-redis v9 基础：
- 命令返回 cmd 对象：`.Result()` / `.Err()` / `.Bytes()` / `.Int64()` 几种取法
- **key 不存在 = `redis.Nil`**（特殊 error，不是普通错误）；封装 `IsMiss(err) = err == redis.Nil`（原项目直接 `==` 比较，能用；更稳的写法是 `errors.Is`，进阶改进里换）
- `NewClient` 不连接（懒连接），要 `Ping` 才确认可达；client 并发安全，全进程单例

两个 Lua 脚本（`redis.NewScript` 会自动走 EVALSHA，脚本对象可包级单例）：

```go
var unlockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
else
  return 0
end`)

var incrementWithExpireScript = redis.NewScript(`
local count = redis.call("INCR", KEYS[1])
if count == 1 then
  redis.call("PEXPIRE", KEYS[1], ARGV[1])
end
return count`)
```

配套方法（都带 nil 保护）：

```go
func (c *Client) Lock(ctx context.Context, key string, ttl time.Duration) (token string, ok bool, err error)
    // crypto/rand 生成 16 字节 hex token；SetNX(key, token, ttl)
func (c *Client) Unlock(ctx context.Context, key string, token string) error     // 跑 unlockScript
func (c *Client) IncrementWithExpire(ctx context.Context, key string, expire time.Duration) (int64, error)
```

为什么解锁要 Lua：GET 判断和 DEL 两步之间锁可能恰好过期易主，"比对+删除"必须原子。为什么计数要 Lua：`INCR` 完再 `EXPIRE` 两步之间进程挂了会留下一个永不过期的计数器；`count == 1` 时才 PEXPIRE，窗口从本窗口第一次请求起算。

## 第二步：接入启动流程（启动时降级）

`cmd/main.go`：DB 失败 `log.Fatalf`（必选依赖），Redis 失败**只记日志置 nil**（可选依赖）：

```go
cache, err := rediscache.NewFromEnv(&cfg.Redis)
if err != nil {
    log.Printf("Redis config error (cache disabled): %v", err)
    cache = nil
} else {
    pingCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
    defer cancel()
    if err := cache.Ping(pingCtx); err != nil {
        log.Printf("Redis not available (cache disabled): %v", err)
        _ = cache.Close()
        cache = nil
    } else {
        defer cache.Close()
        log.Printf("Redis connected (cache enabled)")
    }
}
```

把 `cache`（可能为 nil）一路传给 `SetRouter(sqlDB, cache, rmq)`，由 router 注入各 service/中间件。`cmd/worker/main.go` 是同款套路（阶段 9 用）。

> 为什么 NewFromEnv 几乎不会返回 err：go-redis 是懒连接，构造 client 不碰网络。真正的"连不上"只在 Ping 时暴露——所以 300ms 超时的 Ping 是启动降级的唯一判定点。

## 第三步：50ms 超时纪律（请求级降级）

**所有**主请求路径上的 Redis 调用都包一层短超时 context：

```go
cacheCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
defer cancel()
b, err := cache.GetBytes(cacheCtx, key)
```

Redis 抖一下最多损失 50ms，超时错误和连接错误一样走"回源 DB"分支——这就是请求级降级：**缓存加速，但永远不阻塞、不阻断主请求**。例外：锁释放和事件驱动失效用 `context.Background()`（详见关键设计决策表）。

## 第四步：缓存落地图谱

全项目用到的 Redis key 一览（都有 `v1:` 前缀，表里省略）。先建全表再逐个实现：

| Key | 类型 | Value | TTL | 读 | 写 | 失效 |
|---|---|---|---|---|---|---|
| `account:<id>` | string | 当前 access token | 24h | jwt check | Login/Rename 写；check 回填 | Logout/改密 DEL |
| `account:<id>:refresh` | string | 当前 refresh token | 7d | （原项目无读者） | Login | Logout DEL |
| `refresh:<refreshToken>` | string | accountID 十进制串 | 7d | RefreshAccessToken 快路径 | Login | Logout DEL |
| `video:detail:id=<id>` | string | Video JSON | 5m | GetDetail | 未命中回填 | Delete/UpdatePopularityCache DEL |
| `video:entity:<id>` | string | Video JSON | 1h | GetVideoByIDs MGet | L3 未命中异步回写 | UpdatePopularityCache DEL |
| `feed:listByFollowing:limit=<n>:accountID=<id>:before=<unix秒>` | string | 响应 JSON | 24h | ListByFollowing | 未命中回填 | Follow/Unfollow 按模式 DEL |
| `feed:global_timeline` | ZSET | member=videoID, score=createTime 毫秒 | 无 TTL | ListLatest | 发布事件 ZAdd / 空时重建 | ZRemRangeByRank 只留最近 1000 |
| `hot:video:1m:<yyyyMMddHHmm>` | ZSET | member=videoID, score=增量 | 2h | 阶段 8 | UpdatePopularityCache | 自然过期 |
| `hot:video:merge:1m:<…>` | ZSET | 60 桶聚合快照 | 2m | 阶段 8 | ListByPopularity | 自然过期 |
| `lock:<上面任意缓存key>` | string | 随机锁 token | 详情 2s / 关注流 500ms | — | 防击穿 | Unlock Lua |
| `chunk_upload:<uploadID>` | string | 分片上传会话 JSON | 24h | 分片上传 | init/upload | complete 后 DEL |
| `chunk_upload_hash:<accountID>:<md5>` | string | uploadID | 24h | 秒传/续传 | init | complete 后 DEL |

value 一律 `json.Marshal` 后存字节。`sf:*` 开头的"key"（如 `sf:entity:%d`）**不是 Redis key**，只是进程内 singleflight 的合并键，别误以为是缓存（原项目借用了 `Key()` 拼前缀而已）。

### 自愈鉴权（改造阶段 1 的 check()）

```
1. Get account:<id>（50ms）成功 → 比对 token：不一致 → 401（已撤销）；一致 → 通过
2. Get 失败（未命中、超时、Redis 挂、cache==nil 全算）→ 查 DB account.token 比对（兜底，
   绝不因 Redis 拒绝合法用户）
3. DB 校验通过且 cache != nil → SetBytes 回填 24h（自愈：Redis 恢复后缓存自动热起来）
```

体会：**缓存加速但不做裁判**——裁判永远是 DB 里的 token 列。鉴权是"fail-closed 到 DB"：宁可慢不可错。

### account：token 三件套（阶段 1 的 service 改造）

| 时机 | 动作 |
|---|---|
| Login | DB 落 token 后写三把：`account:<id>`(24h)、`account:<id>:refresh`(7d)、`refresh:<refreshToken>`→accountID(7d)；写失败只 `log.Printf`，不影响登录 |
| Logout | DEL 三把（`refresh:<token>` 仅当 DB 里 RefreshToken 非空），再删 DB token |
| Rename | DB 换新 token 后 `SetBytes account:<id>` 新 token——**这是正确性问题不是优化**：不写缓存，鉴权读到旧 token，改名返回的新 token 立刻 401 |
| ChangePassword | 内部调 Logout，自然清干净，无需另写 |
| RefreshAccessToken | 快路径：Get `refresh:<token>` → ParseUint 出 id → FindByID → **必须再比对 DB 里的 refresh_token**（缓存可能脏）→ UpdateToken + 回写 `account:<id>`。快路径没走通 → 慢路径：FindAll 全表扫描比对 refresh_token |

慢路径是 O(N) 全表扫——refresh 缓存的意义正是把它变成 O(1) 点查。写缓存永远包 50ms ctx、失败只记日志。

### 缓存旁路 + 防击穿锁（GetDetail / ListByFollowing 同一模式）

```
读缓存（50ms）──命中──► 返回
  │未命中(IsMiss)
  ▼
SETNX lock:<key> 随机token, TTL(详情2s / 关注流500ms)
  │拿到锁                          │没拿到锁
  ▼                               ▼
双重检查缓存（可能别人已回填）      轮询 5次×20ms 读缓存
  │仍无                            │拿到 → 返回
  ▼                               │没拿到 → 兜底自己查 DB
查 DB → 回填缓存 → defer Unlock
Unlock：Lua「GET==token 才 DEL」（防误删别人的锁）
```

原项目两处实现并存，细节有差异，照抄时注意：

- **GetDetail**（video_service.go）：先 `getCached()` 闭包（任何 error 都当"没命中"），再**第二次** GetBytes 区分 `IsMiss`（才去拿锁）和其他 error（跳过锁直接回源）——因为 `getCached` 吞掉了错误，需要一次裸读做裁决。轮询用 `select { case <-ctx.Done(): ... case <-time.After(20ms): }`，请求取消能及时退出
- **ListByFollowing**（feed/service.go）：只在 `viewerAccountID != 0 && rediscache != nil` 时启用缓存；key 里 `before` 用**秒**（`latestBefore.Unix()`）；轮询用的是裸 `time.Sleep(20ms)`，不看 ctx——原项目的粗糙点，你复刻时统一成 select 版
- 锁 key 直接 `"lock:" + cacheKey` 前缀拼接；没拿到锁且轮询落空，最后落到统一的 DB 查询 + 回填

为什么锁 TTL 是 2s/500ms：按"一次 DB 回源 + 一次写缓存"的耗时上限估的。太短→回源没完成锁就易主，防击穿失效；太长→拿到锁的goroutine挂了，其他请求白白轮询超时。

### Feed 实体三级缓存 + singleflight（原项目 GetVideoByIDs）

批量取视频的三级架构（关注流/热榜/标签流的底座）：

```go
func (f *FeedService) GetVideoByIDs(ctx context.Context, videoIDs []uint) ([]*video.Video, error)
// L1 本地：patrickmn/go-cache，条目 TTL 5s（实例 cache.New(3s, 5s)，键复用 redis 的完整 key 字符串）
// L2 Redis：MGet 一次批量取未命中的（50ms ctx）；命中条目回写 L1；MGet 整体失败 → 全部降级 L3
// L3 MySQL：每个未命中 id 一个 goroutine，requestGroup.Do("sf:entity:<id>", …) 合并同 id 并发，
//    查到后 json.Marshal → 开新 goroutine 异步 SetBytes 回写 L2（TTL 1h，ctx 用 Background+50ms）
// 最后按传入 id 顺序重排（buildOrderedResult），保证输出顺序 == 输入顺序
```

三个设计点：① 异步回写——回源耗时不算在请求头上；② singleflight 防的是**进程内**同 id 并发回源（和 SETNX 分布式锁互补：一个管单机、一个管集群）；③ L1 TTL 5s 远短于 L2 的 1h，本进程刚写的最多 5 秒不一致，可接受。

### Feed 全局时间线：冷热分离的 ListLatest（原项目 ListLatest）

原项目的"最新流"不是字符串响应缓存，而是 **ZSET 全局时间线**：

```
feed:global_timeline：member=videoID，score=createTime.UnixMilli()
 TimelineMQ 消费者：新视频发布 → ZAdd；ZRemRangeByRank(0,-1001) 只留最近 1000 条（阶段 9 回填增量部分）
 ListLatest 读：
   ZRangeWithScores(key,0,0) 取最老一条 → watermark（水位）
   游标 <= watermark  → 冷数据：MySQL 游标查询（singleflight 合并同游标请求，不回写 ZSET 防污染）
   游标 >  watermark  → 热数据：ZRevRangeByScore(maxScore=游标-1 防重复, limit) 拿 id
                        → GetVideoByIDs（走上面的三级缓存）
                        → 击穿冷热边界（凑不满 limit）→ 冷查询补齐拼接
   ZSET 为空（冷启动/被清）→ singleflight 全局重建：MySQL 捞最新 1000 条 ZAdd 回去 → 递归重查自己
```

注意这个设计的降级闭合：`rediscache == nil` 或 ZSET 读失败 → 直接走纯 MySQL 版（`listLatestFromDB`）；ZSET 为空也有"自举重建"兜底——所以**没有 MQ 它也能工作**（首次请求自己建），阶段 9 的消费者只是让增量的落 ZSET 更实时。复刻时本阶段可先做"空则重建 + 冷热查询"的部分，`feed:global_timeline` 的增量 ZAdd 留到阶段 9。

`buildFeedVideos` 里的 `is_liked` 每次 `BatchGetLiked` 现查 DB——**原项目没有缓存 is_liked**，它是随 viewer 变化的人因数据，实体缓存故意不碰它。

## 第五步：固定窗口限流中间件 ratelimit.Limit

`internal/middleware/ratelimit/ratelimit.go`，基于 `IncrementWithExpire` 的固定窗口计数：

```go
type KeyFunc func(*gin.Context) (string, bool)

func Limit(cache *rediscache.Client, keyPrefix string, maxRequests int64,
    window time.Duration, keyFunc KeyFunc) gin.HandlerFunc
// cache==nil / keyFunc==nil / maxRequests<=0 / window<=0 → 直接放行
// keyFunc 取不到主体（如匿名）→ 放行
// INCR 出错 → 放行（fail-open！）
// count > maxRequests → 429 {"error":"too many requests"}（AbortWithStatusJSON）
```

- 限流 key 不走 `Key()` 的 `v1:` 前缀，自己拼 `feedsystem:ratelimit:<keyPrefix>:<subject>`（原项目的不一致点，你可以在复刻里统一掉）
- `KeyByIP`：`c.ClientIP()`（router 里 `SetTrustedProxies(nil)`，拿直连 IP）；`KeyByAccount`：`jwt.GetAccountID(c)`，取不到 → 放行
- **限流是 fail-open**，和鉴权的"fail-closed 到 DB"方向相反：限流器是保护手段，Redis 挂了宁可不限流也不能挡正常用户
- `KeyByAccount` 依赖 `c.Get("accountID")`，所以带账号限流的路由必须**先过 `JWTAuth` 再过 limiter**（gin 里 group `.Use(JWTAuth)` 在前、路由参数里挂 limiter 在后，天然满足）

router 里的五个挂点（参数照抄）：

| keyPrefix | 上限 | 窗口 | 维度 | 挂在 |
|---|---|---|---|---|
| `account_register` | 5 | 1h | IP | POST /account/register |
| `account_login` | 10 | 1min | IP | POST /account/login |
| `like_write` | 30 | 1min | 账号 | /like/like、/like/unlike |
| `comment_write` | 10 | 1min | 账号 | /comment/publish、/comment/delete |
| `social_write` | 20 | 1min | 账号 | /social/follow、/social/unfollow |

## 关键设计决策

| 问题 | 原项目答案 |
|---|---|
| 为什么服务不因 Redis 挂而退出？ | 缓存是加速件不是依赖件。启动降级置 nil + 封装层 nil 保护 + service 双保险，全链路可走 DB |
| 为什么 key 全带 `v1:` 前缀？ | 结构变更时旧缓存整体作废可识别；批量清库/排查也方便。改结构升 `v2:` 即可灰度 |
| 为什么 limitByFollowing 响应缓存 TTL 敢给 24h？ | 有事件失效兜底（关注/取关 → DelByPattern 清该用户全部关注流缓存）；TTL 只是"失效事件丢了"的最终保险 |
| 为什么 listLatest 不缓存响应，而关注流缓存响应？ | 最新流走 ZSET 时间线 + 实体缓存，实体粒度复用率高且 is_liked 每人不同只能现算；关注流结果集小、按人缓存命中直接返回整个响应 |
| 缓存未命中但 DB 也查不到（穿透）怎么办？ | 原项目未处理。进阶：缓存空值短 TTL 或布隆过滤器 |
| 锁没拿到、轮询也超时了怎么办？ | 兜底查 DB——**永远给请求留活路**，锁只用来"减少"回源，不用来"阻止" |
| 限流 INCR 失败怎么办？ | 放行（fail-open）；鉴权相反 fail-closed 到 DB——都是"哪个更不能错就保哪个" |
| DEL 缓存/释放锁用哪个 ctx？ | `context.Background()`（失效和开锁是尽力而为，不随请求取消而中断，请求没了锁也得放掉）；**读**才用请求 ctx+50ms |
| Refresh 快路径为什么还要查 DB 比对 refresh_token？ | 缓存值可能脏/过期错位；缓存做索引，DB 做裁判——和 token 校验同一个哲学 |

## 难点清单（本阶段真正难的地方）

1. **nil 策略的两类划分**：哪些方法吞错、哪些报错，直接决定降级行为。吞错的写方法让"cache==nil"退化成 no-op；报错的读方法让调用方有机会回源。抄错一个，要么 panic（nil map 调用）要么 Redis 挂了疯狂打 DB
2. **未命中 vs 故障的区分**：`redis.Nil` 和其他 error 必须分流——未命中才拿锁回填，故障直接回源不回填（或照常回填但要意识到缓存不可用）。判断用 `err != nil` 一把梭是最常见的灾难
3. **Rename 的缓存一致性**：改名 = 换 token，缓存里 `account:<id>` 必须同步覆盖成新 token。漏写 → 新 token 被旧缓存拒掉，用户被登出，极难排查
4. **锁的正确释放**：token 随机 + Lua 比对删除，防"释放了别人的锁"；`defer Unlock` 用 Background ctx，防止请求提前取消导致锁滞留到 TTL 自然过期（那 2s 内所有请求都在轮询）
5. **回填的并发正确性**：双重检查为什么必须放在拿到锁**之后**——拿锁前的未命中可能已过时，别人可能刚回填完
6. **三级缓存的顺序语义**：GetVideoByIDs 输出必须按输入顺序（MGet 结果、goroutine 完成顺序都乱），`buildOrderedResult` 的 map+遍历重排不能省
7. **冷热边界拼接**：热区查完不足 limit，要用已取最后一条的 create_time 当冷游标补查，两段拼起来不能重不能漏

## 亮点（原项目缓存层的工程亮点）

- **降级吸收在封装层**：nil 接收器保护 + IsMiss 让"Redis 不在"成为一种普通的业务状态而非异常，业务代码的降级路径就是普通的 else 分支
- **50ms 全覆盖**：所有读路径 Redis 调用带超时，Redis 抖动对接口 P99 的影响被钉死在 50ms
- **自愈（self-healing）**：鉴权缓存不靠预热任务，DB 兜底校验通过后顺手回填，Redis 重启后缓存自然热起来，零运维
- **refresh 缓存把全表扫描变点查**：慢路径 O(N) 的存在反衬出快路径的价值——缓存删了也能活，只是慢
- **两代防击穿并存**：SETNX+Lua 分布式锁（跨实例）与 singleflight 进程内合并（GetVideoByIDs/时间线重建）各管一层，面试可对讲两者的适用边界
- **限流 fail-open / 鉴权 fail-closed 的对比设计**：同一个 Redis，两种故障取向，取舍有据
- **原子性强迫症**：解锁比对+删除、计数+过期全部 Lua 化，不留 check-then-act 缝隙
- **Key() 前缀 + 版本号**：一行代码给全库 key 上版本，结构升级不背历史包袱

## 常见的坑

- Set 忘传 TTL → 永久 key 内存泄漏（锁 key 有 TTL 兜底，但业务 key 没有）
- 判断未命中用 `err != nil` → 把连接错误也当未命中，Redis 挂了会疯狂打 DB；必须 `IsMiss(err)` 分流
- `IsMiss` 写成 `errors.Is` 之前先确认 go-redis 版本行为（原项目直接 `==`，redis.Nil 是单例所以成立）
- 锁的 TTL < 回源耗时 → 回源没完成锁过期被别人抢走，第二个人又开始回源；详情 2s、关注流 500ms 是估出来的
- 轮询用 `time.Sleep` 不看 ctx → 请求取消了还在傻等（feed/service.go 原版就这样，复刻时改掉）
- 结构体加字段后旧缓存 JSON 不匹配 → Unmarshal 失败要当未命中处理，不能当错误往外抛
- `cache.Key(...)` 在 `cache == nil` 上调用能活（nil 接收器），但 `cache.GetBytes` 不判 nil 直接调也能活（返回 error）——别因此省掉 service 层的 nil 判断，ListLatest 那种"整个函数换路径"的降级只能靠显式判断
- 限流 key 忘记 `strings.TrimSpace`，或 `KeyByAccount` 挂在 `JWTAuth` 之前 → accountID 永远取不到，限流静默失效
- MGet 结果是 `[]interface{}`，元素要断言 `string` 且判 nil（不存在的 key 对应 nil 元素），直接 `[]byte(str)` 会 panic
- 分片上传那两个 key 是**会话存储不是缓存**——Redis 挂了它们没有 DB 兜底，handler 里直接 503（`chunk upload requires redis`），别套用"降级回源"的思路

## 验收清单

- [ ] `redis-cli -a 123456` 里 `MONITOR` 观察：登录出现 `SET v1:account:1`、`SET v1:refresh:<token>`；鉴权请求只有 GET
- [ ] `TTL v1:video:detail:id=1` ≈ 300；getDetail 第二次明显变快
- [ ] **压测 getDetail 缓存过期瞬间**：MySQL 查询日志里只有 1 条 SELECT（锁生效）
- [ ] logout 后旧 token 立即 401（`v1:account:<id>` 已 DEL，鉴权落到 DB 发现 token 空）
- [ ] refresh 接口第二次刷新走快路径（MONITOR 里只有 `GET v1:refresh:*`，没有全表 SELECT）
- [ ] follow 一个发了视频的人 → 立刻看关注流能看到（`DelByPattern` 失效生效）；`SCAN 0 MATCH 'v1:feed:listByFollowing:*' COUNT 100` 验证清干净
- [ ] 同 IP 第 11 次登录 → 429；停 Redis 后登录限流失效但仍能正常登录（fail-open）
- [ ] **`docker stop redis` → 全部核心接口仍 200**（鉴权走 DB、详情/关注流回源 DB、listLatest 走纯 MySQL 版、日志出现降级）
- [ ] **`docker start redis` → 无需重启**，缓存逐步自动回填（自愈），`v1:` key 重新出现
- [ ] 停 Redis 期间发布的视频，重启 Redis 后 listLatest 正常（空 ZSET 自举重建）

## 回填清单（本阶段要动的所有旧代码，逐条打勾）

以原项目实际代码为准，like/comment **没有** isLiked 缓存——它们对 Redis 的全部触点就是限流中间件和（阶段 8/9 的）热度缓存失效。别发明原项目没有的东西。

**基础设施**

- [ ] `internal/middleware/redis/{redis,cache,zset}.go` 全量封装（含 Lock/Unlock/IncrementWithExpire、IsMiss、Key、DelByPattern、MGet）
- [ ] `internal/middleware/ratelimit/ratelimit.go`（Limit + buildKey + KeyByIP + KeyByAccount）
- [ ] `cmd/main.go`：`NewFromEnv` → 300ms Ping → 失败置 nil 继续启动；`SetRouter(db, cache, rmq)` 加参透传

**account（改阶段 1 的 service/router）**

- [ ] `NewAccountService(repo, cache)` 加参
- [ ] Login：写 `account:<id>`(24h)、`account:<id>:refresh`(7d)、`refresh:<refreshToken>`(7d) 三把
- [ ] Logout：DEL 三把（`refresh:<token>` 仅当 DB RefreshToken 非空）
- [ ] Rename：DB 换 token 后覆盖 `account:<id>` 为新 token
- [ ] RefreshAccessToken：`refresh:<token>` 快路径（命中也要 DB 复核 refresh_token）+ 全表扫描慢路径兜底
- [ ] register 挂 `Limit(cache,"account_register",5,time.Hour,KeyByIP)`；login 挂 `Limit(cache,"account_login",10,time.Minute,KeyByIP)`

**jwt 中间件（改阶段 1 的 check）**

- [ ] `JWTAuth`/`SoftJWTAuth`/`check` 加 cache 参数；check 按"缓存命中比对 → DB 兜底 → 回填自愈"重写，全程 50ms ctx

**video（改阶段 2）**

- [ ] `NewVideoService(repo, cache, …)`，`cacheTTL = 5 * time.Minute`
- [ ] GetDetail：getCached/setCached 闭包 + 二次读区分 miss/故障 + `lock:video:detail:id=<id>` 2s + 双检 + 轮询 5×20ms（ctx 感知）+ 回源回填
- [ ] Delete：成功后 `Del(Background, video:detail:id=<id>)`
- [ ] UpdatePopularity 的缓存部分（DEL detail + `hot:video:1m` 桶 ZincrBy）留到阶段 8 做 `UpdatePopularityCache` 时一并回填，本阶段先不写

**feed（改阶段 3）**

- [ ] `NewFeedService(repo, likeRepo, cache)`：localcache(go-cache, 条目 5s)、`requestGroup singleflight.Group`、`cacheTTL = 24 * time.Hour`
- [ ] ListByFollowing：viewer!=0 且 cache 可用时走"响应缓存 + lock 500ms + 轮询 + 回填"，其余路径纯 DB（key 的 before 用秒）
- [ ] GetVideoByIDs：L1 go-cache → L2 MGet(50ms) → L3 并发+singleflight+异步回写 `video:entity:<id>`(1h)，输出按输入重排
- [ ] ListLatest 冷热分离（可选回填，建议本阶段做"ZSET 读 + 空则 singleflight 重建 + 冷热边界拼接 + rediscache==nil 纯 DB"）：
- [ ] `feed:global_timeline` 的增量 ZAdd/ZRemRangeByRank(保 1000) 属于 TimelineMQ 消费者，阶段 9 回填
- [ ] is_liked 维持 `BatchGetLiked` 现查 DB，不缓存（原项目如此）

**social（改阶段 6）**

- [ ] `NewSocialService(repo, accountRepo, socialMQ, cache)` 加参
- [ ] Follow/Unfollow：DB 成功后 `invalidateFollowingFeedCache(Background, followerID)` → `DelByPattern("feed:listByFollowing:*:accountID=<followerID>:*")`
- [ ] follow/unfollow 路由挂 `Limit(cache,"social_write",20,time.Minute,KeyByAccount)`

**like / comment（改阶段 4/5，量最小）**

- [ ] service 构造函数加 cache 参数（本阶段只为签名对齐，真正用途在阶段 8/9 的 `UpdatePopularityCache` 降级直写）
- [ ] /like/like、/like/unlike 挂 `Limit(cache,"like_write",30,time.Minute,KeyByAccount)`；/comment/publish、/comment/delete 挂 `Limit(cache,"comment_write",10,time.Minute,KeyByAccount)`
- [ ] 确认所有带账号限流的路由都在 `JWTAuth` 之后

**明确不做（原项目有但属于别的阶段/范围）**

- `hot:video:1m`/`hot:video:merge` 的读写逻辑 → 阶段 8
- TimelineMQ 消费者写时间线、Worker 进程的 Redis 降级启动 → 阶段 9
- 分片上传会话（`chunk_upload:*`，会话存储非缓存，且 howto 系列未安排分片上传）→ 可选扩展，看原项目 `chunk_handler.go`

## 进阶改进

- GetDetail 里原项目读了两遍缓存（`getCached` 吞错 + 裸读区分 miss/故障）→ 重构成 `getCached() (val, miss bool, err error)` 三返回值，一遍搞定
- GetDetail 的自旋锁换 singleflight（feed 已在用），一个函数里体会两种方案
- `IsMiss` 改用 `errors.Is(err, redis.Nil)`
- 缓存穿透防护：DB 查空也回填一个空标记（短 TTL）
- is_liked 缓存：`SADD liked:<accountID>` + `SISMEMBER`，点赞/取关时维护——做完对比 BatchGetLiked 的 DB 压力
- ratelimit 的固定窗口换滑动窗口（Lua 有序集合版），体会临界突刺问题
- 统一 ratelimit 的 key 前缀到 `Key()` 体系
- feed 轮询改 ctx 感知 + 抽成公共的 `waitBackfill(cacheCtx, key, n, interval)` 工具，消除 GetDetail/ListByFollowing 的重复

## 原项目对照

- `backend/internal/middleware/redis/redis.go`（Client/NewFromEnv/Key/IsMiss/Lock/Unlock/IncrementWithExpire）
- `backend/internal/middleware/redis/cache.go`（GetBytes/SetBytes/Del/DelByPattern/MGet）
- `backend/internal/middleware/redis/zset.go`（ZSET 工具，阶段 8 大量使用）
- `backend/internal/middleware/ratelimit/ratelimit.go`（限流中间件全量）
- `backend/internal/middleware/jwt/jwt.go:70-111`（自愈 check）
- `backend/internal/account/service.go:45-74`（Rename 写缓存）、`:113-147`（Login 三把）、`:149-174`（Logout 清理）、`:198-244`（Refresh 快慢路径）
- `backend/internal/video/video_service.go:109-194`（GetDetail 防击穿完整版）、`:80-99`（Delete 失效）
- `backend/internal/feed/service.go:36-146`（GetVideoByIDs 三级缓存）、`:149-290`（ListLatest 冷热分离）、`:338-425`（ListByFollowing 响应缓存+锁）
- `backend/internal/social/service.go:94-102`（DelByPattern 失效关注流缓存）
- `backend/cmd/main.go:52-68`（启动降级）、`backend/cmd/worker/main.go:120-133`（worker 同款）
- `backend/internal/worker/outboxworker.go:49-119`（时间线消费者 ZAdd，阶段 9）
- `backend/internal/video/chunk_handler.go`（分片上传会话：非缓存用法，本阶段不展开）
- `backend/internal/video/chunk_handler_test.go:37`（`NewClient(rdb, prefix)` 注入测试的写法）
