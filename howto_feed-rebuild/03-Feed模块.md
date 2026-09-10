# 03 - Feed 模块（游标分页专题）【全量版】

前置：阶段 2。**分页设计是 Feed 系统的灵魂**，本模块把"时间游标"和"复合游标"两种打穿，热榜分页的 Redis 快照路径在阶段 8。

> **对齐原则（全程适用）**：最终产出 = 原项目全部功能。Feed 实际是 **4+1 个接口**：本阶段做 `listLatest / listLikesCount / listByTag / listByPopularity(DB 兜底路径)`；`listByFollowing` 依赖关注表，**阶段 6 回填**；`listByPopularity` 的 Redis 快照路径**阶段 8 回填**。每个能力都有归属阶段，绝无遗漏。

## 目标

- **最新视频流**（时间游标，软鉴权）
- **按点赞数排序流**（`(likes_count, id)` 双字段复合游标，软鉴权）
- **热度流**（三键游标 `(popularity, create_time, id)` 的 DB 兜底路径；Redis 快照稳定分页阶段 8 回填）
- **标签流**（JOIN tags 两张表，一页拉全，无游标）
- **关注流**（JWT 强鉴权 + `socials` 子查询，阶段 6 回填）
- 统一的 `FeedVideoItem` 响应结构体（全模块通用）
- 批量组装"是否点赞"（`buildFeedVideos` + `BatchGetLiked`，本阶段 `is_liked` 恒 false，阶段 4 变真）
- `SoftJWTAuth` 软鉴权中间件在本模块**正式服役**：匿名可刷、带 token 必须合法

## 接口设计

| 路由 | 鉴权 | 状态 | 请求 → 响应 |
|---|---|---|---|
| POST /feed/listLatest | SoftJWT | 本阶段 | `{limit, latest_time}` → `{video_list, next_time, has_more}` |
| POST /feed/listLikesCount | SoftJWT | 本阶段 | `{limit, likes_count_before?, id_before?}` → `{video_list, next_likes_count_before?, next_id_before?, has_more}` |
| POST /feed/listByPopularity | SoftJWT | DB 兜底本阶段；Redis 快照阶段 8 | `{limit, as_of, offset, latest_id_before?, latest_popularity, latest_before}` → `{video_list, as_of, next_offset, has_more, next_latest_popularity?, next_latest_before?, next_latest_id_before?}` |
| POST /feed/listByTag | SoftJWT | 本阶段 | `{tag_name, limit}` → `{video_list}` |
| POST /feed/listByFollowing | **JWT 强鉴权** | 阶段 6 回填 | `{limit, latest_time}` → `{video_list, next_time, has_more}` |

路由挂载（`internal/http/router.go`，注意是**两个叠加的分组**）：

```go
feedGroup := r.Group("/feed")
feedGroup.Use(jwt.SoftJWTAuth(accountRepository, nil)) // cache 参数阶段 7 回填，先传 nil
{
    feedGroup.POST("/listLatest", feedHandler.ListLatest)
    feedGroup.POST("/listLikesCount", feedHandler.ListLikesCount)
    feedGroup.POST("/listByPopularity", feedHandler.ListByPopularity)
    feedGroup.POST("/listByTag", feedHandler.ListByTag)
}
// 阶段 6 回填：protectedFeedGroup := feedGroup.Group("")，再 Use(JWTAuth)，挂 listByFollowing
// —— listByFollowing 实际跑在 SoftJWTAuth + JWTAuth 两层中间件上，最终由 JWTAuth 强制登录
```

### POST /feed/listLatest（时间游标）

```json
请求:  {"limit": 10, "latest_time": 1735289200000}
// limit 归一化：<=0 或 >50 → 默认 10
// latest_time 是 Unix「毫秒」，handler 里 time.UnixMilli() 还原；0 或缺省 = 第一页

响应:  {"video_list": [FeedVideoItem...],
        "next_time": 1735289199123,        // 本页最后一条 create_time 的 Unix 毫秒，原样传回即下一页
        "has_more": true}                   // len(video_list) == limit
```

### POST /feed/listLikesCount（复合游标，本模块最难）

```json
请求:  {"limit": 10, "likes_count_before": 42, "id_before": 107}
// 两个字段都是「指针」类型：omitted 表示没传；0 是合法值。两者必须成对出现，缺一 → 400
// id_before=0 且 likes_count_before=0 → 视为第一页（原项目如此，不报 400）

响应:  {"video_list": [...],
        "next_likes_count_before": 42,     // 本页最后一条的 likes_count（omitted 当页为空）
        "next_id_before": 107,             // 本页最后一条的 id
        "has_more": true}
```

### POST /feed/listByPopularity（三键游标 + 稳定分页，两套参数并存）

```json
请求:  {"limit": 10,
        "as_of": 0,                        // Redis 快照路径：服务器回传的分钟时间戳，第一页传 0（阶段 8）
        "offset": 0,                       // Redis 快照路径：下一页起点（阶段 8）
        "latest_id_before": 107,           // DB 兜底游标三元组之一（指针）
        "latest_popularity": 35,
        "latest_before": "2026-09-10T12:00:00+08:00"}  // 注意：time.Time 直接 JSON（RFC3339）

响应:  {"video_list": [...],
        "as_of": 1735289160,               // Redis 路径返回快照分钟；DB 兜底路径恒为 0
        "next_offset": 10,                 // Redis 路径 = offset+len(items)；DB 兜底路径恒为 0
        "has_more": true,
        "next_latest_popularity": 35,      // DB 兜底游标三元组，本页最后一条的值
        "next_latest_before": "2026-09-10T11:58:00+08:00",
        "next_latest_id_before": 88}
```

### POST /feed/listByTag（无游标）

```json
请求:  {"tag_name": "go", "limit": 10}   // tag_name 为空 → 400

响应:  {"video_list": [...]}              // 只有 video_list，没有 next_* / has_more
```

### POST /feed/listByFollowing（阶段 6 回填，JWT 强鉴权）

```json
请求:  {"limit": 10, "latest_time": 1735289200}
// 注意：这里的 latest_time 是 Unix「秒」（time.Unix），和 listLatest 的毫秒不是一回事！

响应:  {"video_list": [...], "next_time": 1735289180, "has_more": true}
// next_time 同样是 Unix 秒；没关注任何人 → video_list 为空数组（不是全部视频）
```

### FeedVideoItem / FeedAuthor（全模块通用）

```go
type FeedAuthor struct {
    ID       uint   `json:"id"`
    Username string `json:"username"`
}

type FeedVideoItem struct {
    ID          uint       `json:"id"`
    Author      FeedAuthor `json:"author"`        // 嵌套对象；username 来自 videos 表反范式列，不 join accounts
    Title       string     `json:"title"`
    Description string     `json:"description,omitempty"`
    PlayURL     string     `json:"play_url"`
    CoverURL    string     `json:"cover_url"`
    CreateTime  int64      `json:"create_time"`   // Unix 秒（time.Unix(v)），和 next_time 的毫秒并存！
    LikesCount  int64      `json:"likes_count"`
    IsLiked     bool       `json:"is_liked"`      // viewer 视角；本阶段恒 false，阶段 4 变真
}
```

## 关键设计决策

**Q1：为什么不用 `LIMIT offset,n`？** 深翻页慢（MySQL 要扫过并丢弃 offset 行）；且翻页间隙插入新数据时页会整体位移 → 重复/漏数据。游标（seek）分页 `WHERE create_time < ?` 命中索引直接定位、天然稳定。这也是"不重不漏"不变量的根基：**游标记录的是"位置"，不是"第几页"**。

**Q2：点赞数相同怎么办？（本模块最难的一题）** 单游标 `likes_count < ?` 会把同赞数的一批**整批跳过**。解法：**排序键 + 唯一兜底键**二元组 `(likes_count DESC, id DESC)`，id 唯一 ⇒ 任意两行必分胜负（全序）：

```sql
WHERE (likes_count < ?) OR (likes_count = ? AND id < ?)
ORDER BY likes_count DESC, id DESC
LIMIT ?
```

WHERE 条件是 ORDER BY 的**逐字段镜像**（DESC ⇒ `<`；若是 ASC 则是 `>`）。请把这个模式背下来，所有"可变排序键分页"都是它。三键版（listByPopularity）只是再续一层：`(popularity < ?) OR (popularity = ? AND create_time < ?) OR (popularity = ? AND create_time = ? AND id < ?)`。

**Q3：游标怎么"编码"？** 原项目不用 base64 整包，而是**对外两个字段、对内一个结构体**：请求里 `likes_count_before` + `id_before` 两个独立 JSON 字段，handler 校验"成对出现"后打包成 `LikesCountCursor{LikesCount, ID}` 传给 service，repo 拿到 nil 就是第一页。编码解码的全部逻辑就是：响应取**本页最后一条**的两个排序键值，请求原样传回。

**Q4：has_more 怎么算？** `len(videos) == limit`（多取即有）。所以最后一页返回 `len < limit`、has_more=false；恰好整除时最后一页多一次空查询才收尾——可接受。next 游标取**本页最后一条**的字段（不是第一条、不是"当前时间"）。空页时 `next_likes_count_before/next_id_before` 是 nil 指针，配合 `omitempty` 序列化时直接消失。

**Q5：三游标的"第一页"判断为什么不能看 `latest_popularity == 0`？** 因为 popularity 合法值就包含 0（没人互动的视频）。原项目用 `!timeBefore.IsZero() && idBefore > 0` 判断"游标完整提供"。DB 兜底游标在 handler 层校验：`latest_before` 非零或 `latest_id_before` 非空任一出现，则三个字段必须齐全且 id > 0，否则 400。

**Q6：软鉴权（SoftJWTAuth）在这里的作用？** 三条规则（详见亮点 S2）：**没带 header → 直接放行（匿名）；带了但格式/签名/撤销校验不过 → 401 拒绝；合法 → `c.Set("accountID", ...)`**。handler 里 `jwt.GetAccountID(c)` 出错就降级 `viewerAccountID = 0`，**绝不报错**。这个 ID 决定三件事：`is_liked`（阶段 4 起查点赞表，0 直接早退）、缓存 key（阶段 7 只全局缓存匿名流）、关注流的过滤范围（阶段 6）。

**Q7：Author 信息从哪来？** `videos` 表有反范式列 `username`（阶段 2 发布时写入），Feed 组装**零 join** 拿到作者名。代价：改名后历史视频仍显示旧名（原项目接受此不一致）。FeedAuthor 只有 `{id, username}`，没有 avatar——进阶改进可补。

**Q8：listByTag 为什么不做游标分页？** 标签页是小集合场景，原项目一次 `LIMIT n` 拉全（`JOIN video_tags` + `JOIN tags` WHERE `tags.name = ?`，按 `videos.create_time` 倒序）。这是原项目的粗糙点，进阶改进可加游标。

**Q9：limit 边界？** 所有 5 个接口统一：`<=0 或 >50 → 默认 10`，防止一次拉爆。绑定错误统一走 `apierror.ClassifyHTTPStatus(err)`（通常 400）。

## 实现提示

**Repo（`internal/feed/repo.go`，本阶段 5 个方法，返回 `[]*video.Video`）**：

```go
func NewFeedRepository(db *gorm.DB) *FeedRepository

func (r *FeedRepository) ListLatest(ctx context.Context, limit int, latestBefore time.Time) ([]*video.Video, error)
// Order("create_time DESC")；latestBefore.IsZero() 时不加 where（第一页）

func (r *FeedRepository) ListLikesCountWithCursor(ctx context.Context, limit int, cursor *LikesCountCursor) ([]*video.Video, error)
// Order("likes_count DESC, id DESC")；cursor==nil 第一页；否则加复合游标条件（见 Q2 的 SQL）

func (r *FeedRepository) ListByPopularity(ctx context.Context, limit int, popularityBefore int64, timeBefore time.Time, idBefore uint) ([]*video.Video, error)
// Order("popularity DESC, create_time DESC, id DESC")；三键游标条件，
// 只在 !timeBefore.IsZero() && idBefore > 0 时才加（popularity 允许为 0，不能拿它判断第一页）

func (r *FeedRepository) ListByTag(ctx context.Context, tagName string, limit int) ([]*video.Video, error)
// Table("videos").Joins("JOIN video_tags ON video_tags.video_id = videos.id").
//   Joins("JOIN tags ON tags.id = video_tags.tag_id").Where("tags.name = ?").Order("videos.create_time desc")

func (r *FeedRepository) GetByIDs(ctx context.Context, ids []uint) ([]*video.Video, error)
// len(ids)==0 直接返回；本阶段暂无人调用（阶段 7 GetVideoByIDs / 阶段 8 快照路径用），可顺手写好
// 阶段 6 回填：ListByFollowing（socials 子查询，见 06 文档）
```

**Service（`internal/feed/service.go`，本阶段不碰缓存，cache 传 nil）**：

```go
type FeedService struct {
    repo     *FeedRepository
    likeRepo *video.LikeRepository   // 阶段 3 就要（BatchGetLiked）
    rediscache *rediscache.Client    // 阶段 7 回填：本阶段恒 nil
    // localcache / requestGroup(singleflight) / cacheTTL 也都是阶段 7 的，现在不写
}
func NewFeedService(repo *FeedRepository, likeRepo *video.LikeRepository, rediscache *rediscache.Client) *FeedService

func (f *FeedService) ListLatest(ctx, limit int, latestBefore time.Time, viewerAccountID uint) (ListLatestResponse, error)
// 阶段 3：直接调 listLatestFromDB。阶段 7 才在前面插入 ZSET 冷热分支

func (f *FeedService) listLatestFromDB(ctx, limit int, latestBefore time.Time, viewerAccountID uint) (ListLatestResponse, error)
// repo 查页 → buildFeedVideos → nextTime = 最后一条 CreateTime.UnixMilli()（毫秒！）
// hasMore = len(videos) == limit

func (f *FeedService) ListLikesCount(ctx, limit int, cursor *LikesCountCursor, viewerAccountID uint) (ListLikesCountResponse, error)
// 空页时 NextLikesCountBefore/NextIDBefore 保持 nil（指针 + omitempty）

func (f *FeedService) ListByPopularity(ctx, limit int, reqAsOf int64, offset int, viewerAccountID uint,
    latestPopularity int64, latestBefore time.Time, latestIDBefore uint) (ListByPopularityResponse, error)
// 阶段 3：跳过 Redis 快照段，直接走最后的 DB 兜底（三游标 repo + buildFeedVideos）；
// AsOf=0、NextOffset=0、回填三个 next_latest_* 指针。阶段 8 在函数开头插入快照路径

func (f *FeedService) ListByTag(ctx, tagName string, limit int, viewerAccountID uint) ([]FeedVideoItem, error)

func (f *FeedService) buildFeedVideos(ctx, videos []*video.Video, viewerAccountID uint) ([]FeedVideoItem, error)
// 1. 收集所有 videoID（一次分配）
// 2. likeRepo.BatchGetLiked(ctx, videoIDs, viewerAccountID) 拿 likedMap
// 3. 按 videos 原顺序组装（CreateTime.Unix() 转秒）
// 全模块复用；阶段 4 只改 BatchGetLiked 内部，这里一行不动
```

**LikeRepository 的口子（`internal/video/like_repo.go`，阶段 3 就建好）**：

```go
func (r *LikeRepository) BatchGetLiked(ctx context.Context, videoIDs []uint, accountID uint) (map[uint]bool, error)
// accountID == 0 或 len(videoIDs) == 0 → 早退返回空 map（匿名用户零查询！）
// 否则 WHERE video_id IN ? AND account_id = ? 查成 map
// 阶段 3 时 likes 表还没有点赞数据 → 天然全 false，无需特判；阶段 4 变真
```

> **警惕 N+1**：每条视频查一次 is_liked 是错误示范，必须先收集 ID 再一次 IN 查询成 map。同理阶段 7 的 `GetVideoByIDs` 也是"批量进、按原序出"。

**Handler（`internal/feed/handler.go`，四件事：绑定 → limit 归一化 → 游标校验 → viewer 降级）**：

```go
// 每个接口共同的骨架：
var req XxxRequest
if err := c.ShouldBindJSON(&req); err != nil { c.JSON(apierror.ClassifyHTTPStatus(err), gin.H{"error": err.Error()}); return }
if req.Limit <= 0 || req.Limit > 50 { req.Limit = 10 }
viewerAccountID, err := jwt.GetAccountID(c)
if err != nil { viewerAccountID = 0 }   // 匿名降级，不报错
// …调 service… 成功后 feedItems.VideoList = nonNilFeedVideoItems(feedItems.VideoList) 再返回

// ListLatest 的游标还原：req.LatestTime > 0 → time.UnixMilli(req.LatestTime)
// ListByFollowing 的游标还原（阶段 6）：req.LatestTime > 0 → time.Unix(req.LatestTime, 0)  ← 单位不同！

// ListLikesCount 的游标校验（伪码）：
//   两个指针都 nil                → 第一页
//   只给了一个                    → 400 "likes_count_before and id_before must be provided together"
//   likes_count_before < 0        → 400
//   id_before == 0 且 likes_count_before != 0 → 400 "invalid cursor: id_before must be > 0"
//   两个都 0                      → 第一页；否则打包 LikesCountCursor

// ListByPopularity 的游标校验：
//   latest_popularity < 0 → 400
//   latest_before 非零 或 latest_id_before 非 nil（任一出现）→ 三元组必须齐全且 id > 0，否则 400
```

**路由与依赖注入**：见上文接口设计。构造顺序 `NewFeedRepository(db)` → `NewLikeRepository(db)`（阶段 3 顺手建）→ `NewFeedService(feedRepo, likeRepo, nil)`（cache 阶段 7 传入）→ `NewFeedHandler(feedService)`。

## 难点清单（本阶段真正难的地方）

1. **复合游标 `(likes_count, id)` 的编码解码与排序一致性**——三处必须严丝合缝：`ORDER BY likes_count DESC, id DESC`（全序的"序"）、`WHERE (lc < ?) OR (lc = ? AND id < ?)`（序的"镜像"）、next 游标取**最后一条**的两个值（"位置"的快照）。任何一处用了不同方向/不同字段，立刻出现重或漏。自检方法：拿 `(42,107)` 这个游标手推——`(41,任何id)` 和 `(42,1..106)` 都必须在本页之前，`(42,107)` 本身绝不能再出现。
2. **"没传"和"传 0"的语义区分**：`likes_count_before`/`id_before`/`latest_id_before` 都必须声明为**指针**（`*int64` / `*uint`），值类型无法区分"客户端没带"和"带了 0"，复合游标的成对校验就无从谈起。
3. **三游标的"第一页"判断**：`popularity == 0` 是合法数据不是哨兵值，必须用"时间游标为零值 + id 指针为 nil"判断游标完整性——选哪个字段当哨兵，要看它的**值域**是否与游标语义冲突。
4. **时间单位的四处不一致（照抄原项目，但必须心知肚明）**：`listLatest` 的 latest_time/next_time 是**毫秒**（`time.UnixMilli`）；`listByFollowing` 的 latest_time/next_time 是**秒**（`time.Unix`）；`FeedVideoItem.create_time` 是**秒**；`listByPopularity.latest_before` 干脆是 `time.Time` 直接走 RFC3339 JSON。同一个响应里 create_time（秒）和 next_time（毫秒）并存，前端拼错单位就会"翻页查不到数据"。
5. **is_liked 的批量组装（防 N+1）**：先收集 ID 再一次 IN 查询成 map；`accountID == 0` 必须早退（匿名流的每一次点赞表查询都是白查）。
6. **游标分页的固有弱一致性**：翻页期间某视频被新点赞，likes_count 变了，它可能被"拉回"到后面的页再次出现（或挤出尾部）。这不是 bug，是快照语义的代价——验收时用"新插入数据不影响已翻页"来验证核心不变量即可，点赞漂移在阶段 4 后才可能出现。

## 亮点（原项目在 Feed 模块的工程亮点）

- **S1 游标分页替代 offset**：`WHERE 排序键 < 游标` 走索引 seek，任意深度翻页代价恒定；offset 则 O(偏移量) 且插入/删除会让整页位移（重复、漏读）。游标还天然免疫"第三页刷新时有人发了新视频"这类抖动——游标是位置快照，不是页码。
- **S2 软鉴权设计（SoftJWTAuth）**：一个中间件同时服务两种身份——
  ```
  无 Authorization header → c.Next() 放行（匿名，可无限刷 Feed）
  有 header 但格式错（非 "Bearer xxx"，大小写不敏感比对）/ 签名错 / token 已被顶号·登出·改密撤销
      → 401 Abort（带了就必须合法，绝不静默降级成匿名）
  合法 → c.Set("accountID", ...) / c.Set("username", ...)
  ```
  为什么"坏 token"要 401 而不是当匿名？因为静默降级会把"你的登录已失效"这个事实掩盖掉：前端以为自己登录着，收到的却是匿名内容（is_liked 全 false、看不到关注流），bug 会变成"灵异现象"。**对"缺席的凭证"宽容（fail-open），对"无效的凭证"严格（fail-closed）**——这是 Feed 匿名可刷 + 个性化点赞状态两全的关键。handler 里 `GetAccountID` 失败再降级 0，中间件管"带没带对"，handler 管"用不用得上"，职责分离。
- **S3 阶段 2 就埋好的复合索引**：`idx_videos_create_time`（时间游标）、`idx_videos_likes_count_id`（likes_count DESC + id DESC）、`idx_videos_popularity_time_id`（三键）——索引定义与游标 SQL 一一镜像，数据量大后才不会全表扫描。回看 02 阶段的 Video 实体 gorm tag。
- **S4 反范式 username**：发布时把作者名写进 videos 表，Feed 组装零 join；配合"一次 IN 查询"的 BatchGetLiked，一页 Feed 的 SQL 数是**常数 2**（1 条列表 + 1 条点赞）。
- **S5 单点组装 + 防 null**：所有流共用 `buildFeedVideos` 和 `nonNilFeedVideoItems`，`make([]FeedVideoItem, 0, len)` 保证空页序列化成 `[]` 而非 `null`；阶段 4 改造只动一处。
- **S6 限流的伏笔**：Feed 是匿名可刷的重灾区，阶段 7 回填时可以考虑给它挂 `ratelimit.Limit`（原项目没挂，进阶改进）。

## 常见的坑

- 时间游标单位搞混：listLatest 用**毫秒**（`UnixMilli`/`time.UnixMilli`），listByFollowing 用**秒**（`Unix`/`time.Unix`），照抄时别"顺手统一"成一种——那反而和原项目不兼容；但要在 API 文档里给前端标清楚
- 游标字段用值类型不用指针 → "没传"和"传 0"无法区分 → 成对校验失效、第一页判断错乱
- 第一页判断忘处理 `IsZero()` → 永远查不到第一页；复合游标只传一个字段 → 必须 400，静默放行会数据错乱
- `id_before == 0 && likes_count_before == 0` 原项目**视为第一页**而非 400——别自作主张改语义
- 三游标拿 `popularity == 0` 当"第一页"哨兵 → 没人点赞的视频永远翻不了页
- ORDER BY 与 WHERE 条件方向不一致（DESC 配 `>`）→ 不重不漏瞬间破产
- `video_list` 空时返回 `null` → 前端炸；`make([]FeedVideoItem, 0, len)` + `nonNilFeedVideoItems` 保住 `[]`
- next 游标误取第一条或"当前时间" → 下一页丢数据或永远翻不动
- `buildFeedVideos` 里 `CreateTime.Unix()` 忘了转（直接把 time.Time 塞进 int64 字段编译错；`.UnixMilli()` 用错地方则单位错）
- 绑定错误忘了走 `apierror.ClassifyHTTPStatus`（Feed 各 handler 里它管 400/500 分类）
- listByTag 的 JOIN：`videos` 是复数表名、条件是 `tags.name = ?`（不是别名 `t` 随手写）；发布视频时**没提取标签**（阶段 2 的事务里 `ExtractTags` + `FirstOrCreate` + `video_tags`，若阶段 2 漏了先回补）→ 查出来永远是空
- 匿名请求 viewerID 忘降级（拿到 err 直接 return）→ 匿名用户刷 Feed 全 500

## 验收清单

- [ ] 准备 ≥15 条视频（写个 for 循环调 publish 造数据）
- [ ] listLatest 翻 3 页，收集所有 id：无重复、无遗漏、合计 = 总数；每页的 next_time 传回能翻下一页
- [ ] limit=0 / limit=100 / limit=10 行为：前两个按 10 处理，limit=50 生效
- [ ] 翻页间隙再发布一条新视频 → 已翻过的页**不重复出现**（游标稳定性的意义）
- [ ] 造两条 likes_count 相同的视频，listLikesCount 翻页不跳不重；用 next_likes_count_before + next_id_before 能精确续上
- [ ] listLikesCount：只传 likes_count_before → 400；传负数 → 400；两个都传 0 → 第一页 200
- [ ] 匿名（无 token）访问 4 个接口全部 200，is_liked 全 false
- [ ] 带一个**登出后的旧 token** 访问 → 401（带 token 就必须合法，不降级成匿名）
- [ ] 空库/翻到底：video_list 是 `[]` 不是 `null`，has_more=false，next_* 字段消失（omitempty）
- [ ] listByPopularity（DB 兜底）：不传游标得第一页；回传 next_latest_popularity + next_latest_before + next_latest_id_before 能翻页且不重不漏；造 popularity 相同的视频验证第三层 id 游标生效
- [ ] listByTag：发一个标题含 `#go` 的视频 → `{tag_name:"go"}` 能查到；`tag_name` 为空 → 400
- [ ] 数据量 25 条、limit=10 时：三页分别 10/10/5 条，第三页 has_more=false

## 后续阶段回填清单（到时回来改，现在不做）

- **阶段 4（点赞）**：`BatchGetLiked` 读到真实数据 → `is_liked` 从假变真；观察点赞数变化后 listLikesCount 的游标漂移现象
- **阶段 6（关注）**：新增 `POST /feed/listByFollowing`——repo 加 `ListByFollowing`（`SELECT vlogger_id FROM socials WHERE follower_id = ?` 子查询做 `author_id IN (?)`）、service 加缓存版实现、handler、路由挂 `protectedFeedGroup`（在 feedGroup 之上再叠 `JWTAuth` 强鉴权）。注意它的 latest_time 是**秒**
- **阶段 7（Redis）**：`NewFeedService` 的 cache 参数开始传真 client；`ListLatest` 升级为冷热分离（`feed:global_timeline` ZSET 重建 + 水位线判定 + singleflight 全局锁 + 冷热拼接）；新增 `GetVideoByIDs` 三级缓存（L1 本地 → L2 Redis MGet → L3 MySQL + singleflight）；`ListByFollowing` 加 Redis 缓存 + SETNX 防击穿锁；JWT `check()` 插入 Redis 优先比对
- **阶段 8（热榜）**：`ListByPopularity` 开头插入 Redis 快照路径——60 个分钟桶 `ZUNIONSTORE` 到 `hot:video:merge:1m:<分钟>` 快照 key（TTL 2min），`as_of + offset` 稳定分页，`ZRevRange` 取段后 `GetByIDs` 按榜序重排；快照缺失/Redis 挂掉降级到本阶段写的 DB 三游标路径
- **阶段 9（MQ）**：timeline worker 消费 `video_published` outbox 事件，`ZADD feed:global_timeline`——阶段 7 的 ZSET 才有了持续喂数据的人

## 进阶改进

- 阶段 7 之前 Feed 是裸奔的：可提前给 4 个接口挂 `ratelimit.Limit`（按 IP），Feed 是被刷重灾区
- 统一分页协议 `{cursor, has_more}` 封装，避免每个接口一套 next_* 字段名
- 时间单位全项目统一成毫秒（并与原项目差异写进迁移说明）
- FeedVideoItem 的 Author 补 avatar_url（需要 join/批量查 accounts，权衡 N+1）
- listByTag 补游标分页；`next_time` 与 `create_time` 单位不一致是前端事故高发点，值得在你们自己的项目里修掉

## 原项目对照

- `backend/internal/feed/entity.go`（所有请求/响应/游标结构体；注意 listByPopularity 里 `LatestBefore time.Time` 直接 JSON）
- `backend/internal/feed/repo.go:20-50`（时间游标 + 复合游标两条 SQL）；`repo.go:72-91`（三键游标）；`repo.go:93-103`（GetByIDs）；`repo.go:105-115`（listByTag 双 JOIN）；`repo.go:52-70`（listByFollowing，阶段 6）
- `backend/internal/feed/service.go:292-310`（listLatestFromDB = 本阶段 ListLatest 的全部）；`service.go:313-335`（ListLikesCount）；`service.go:509-533`（ListByPopularity 的 DB 兜底段）；`service.go:535-559`（buildFeedVideos）；`service.go:571-577`（ListByTag）；缓存版 `service.go:149-290`、`338-425`、`427-508` 分别是阶段 7/7/8 回填对象
- `backend/internal/feed/handler.go:19-43, 45-92, 120-169, 178-201`（limit 归一化 + 成对游标校验 + viewer 降级）
- `backend/internal/middleware/jwt/jwt.go:44-68`（SoftJWTAuth）；`113-125`（GetAccountID）
- `backend/internal/video/like_repo.go:65-84`（BatchGetLiked）；`backend/internal/video/video_entity.go:13-15`（三个复合索引）
- `backend/internal/video/video_service.go:64-73`（发布事务提取标签，listByTag 的数据来源）；`backend/internal/db/db.go:28-34`（automigrate 含 Tag/VideoTag）
- `backend/internal/http/router.go:169-185`（feed 分组 + SoftJWTAuth + listByFollowing 叠加强鉴权）
