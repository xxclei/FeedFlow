# 03 - Feed 模块（游标分页专题）

前置：阶段 2。**分页设计是 Feed 系统的灵魂**，本模块先把"时间游标"和"复合游标"两种打穿，热榜分页在阶段 8。

## 目标

最新视频流（时间游标）、按点赞数排序流（双字段复合游标）；统一的响应结构体；批量组装"是否点赞"信息（为阶段 4 铺垫，本阶段先返回 false）。

## 接口设计

**POST /feed/listLatest**（软鉴权）

```json
请求:  {"limit": 10, "latest_time": 0}          // latest_time=0 表示第一页
响应:  {"video_list": [FeedVideoItem...],
        "next_time": 1735289200,                 // 最后一篇的 create_time(Unix秒)，传回即下一页
        "has_more": true}
```

**POST /feed/listLikesCount**（软鉴权）

```json
请求:  {"limit": 10, "likes_count_before": 0, "id_before": 0}   // 两者必须成对出现
响应:  {"video_list": [...],
        "next_likes_count_before": 42, "next_id_before": 107,
        "has_more": true}
```

**FeedVideoItem（前端友好结构，全模块通用）**：

```go
type FeedVideoItem struct {
    ID          uint       `json:"id"`
    Author      FeedAuthor `json:"author"`        // {id, username} 嵌套对象
    Title       string     `json:"title"`
    Description string     `json:"description,omitempty"`
    PlayURL     string     `json:"play_url"`
    CoverURL    string     `json:"cover_url"`
    CreateTime  int64      `json:"create_time"`   // Unix 秒，不是 time.Time
    LikesCount  int64      `json:"likes_count"`
    IsLiked     bool       `json:"is_liked"`      // viewer 视角的点赞状态
}
```

## 关键设计决策

**Q1：为什么不用 `LIMIT offset,n`？** 深翻页慢（扫过 offset 行）；且插入新数据时页会整体位移 → 重复/漏数据。游标（seek）分页 `WHERE create_time < ?` 走索引、天然稳定。

**Q2：点赞数相同怎么办？（本模块最难的一题）** 单游标 `likes_count < ?` 会把同赞数的一批全部跳过。解法：**排序键 + 唯一兜底键**二元组 `(likes_count DESC, id DESC)`，游标条件：

```sql
WHERE (likes_count < ?) OR (likes_count = ? AND id < ?)
ORDER BY likes_count DESC, id DESC
LIMIT ?
```

请把这个模式背下来，所有"可变排序键分页"都是它。

**Q3：has_more 怎么算？** `len(videos) == limit`（多取即有）。响应里的 next 游标取**本页最后一条**的两个字段。

**Q4：软鉴权在这里的作用？** `SoftJWTAuth`：匿名通过 → `viewerAccountID=0`；带合法 token → 拿到 viewerID。这个 ID 决定 `is_liked`（阶段 4 起查点赞表）和缓存 key（阶段 7 只缓存匿名流）。取不到时 handler 里 `viewerAccountID = 0` 降级，**不能报错**。

**Q5：limit 边界？** `<=0 或 >50 → 默认 10`，防止一次拉爆。

## 实现提示

Repo（两方法）：

```go
func ListLatest(ctx, limit int, latestBefore time.Time) ([]*Video, error)
// latestBefore.IsZero() 时不加 where（第一页）
func ListLikesCountWithCursor(ctx, limit int, cursor *LikesCountCursor) ([]*Video, error)
// cursor 为 nil 即第一页
```

Service 的组装函数（全模块复用，阶段 4 会改造它）：

```go
func buildFeedVideos(ctx, videos []*Video, viewerAccountID uint) ([]FeedVideoItem, error)
// 1. 收集所有 videoID
// 2. 一次 IN 查询拿 likedMap（阶段 4 实现；现在返回空 map）
// 3. 按 videos 顺序组装 FeedVideoItem
```

> **警惕 N+1**：每条视频查一次 is_liked 是错误示范，必须先收集 ID 再一次 `WHERE video_id IN ? AND account_id = ?` 查成 map。

Handler 游标校验：`likes_count_before` 和 `id_before` **必须成对**，缺一 → 400；`id_before==0` 视为无游标。

## 常见的坑

- 时间游标用 `time.Time` 直接 JSON 传输 → 时区格式地狱；统一用 **Unix 秒 int64** 传输，service 层 `time.Unix(ts,0)` 还原
- 第一页判断忘处理 `IsZero()` → 永远查不到第一页
- 复合游标只传一个字段 → 数据静默错乱，必须 400 拒绝
- `video_list` 空时返回 `null` → 前端炸；`make([]FeedVideoItem, 0, len)` 保住 `[]`

## 验收清单

- [ ] 准备 ≥15 条视频（可写个 for 循环调 publish 造数据）
- [ ] listLatest 翻 3 页，收集所有 id：无重复、无遗漏、合计 = 总数
- [ ] 翻页间隙再发布一条新视频 → 已翻过的页**不重复出现**（游标稳定性的意义）
- [ ] 造两条 likes_count 相同的视频，listLikesCount 翻页不跳不重
- [ ] 匿名（无 token）访问两个接口均 200

## 进阶改进

- 给 `create_time` 和 `likes_count` 建索引（数据量大后游标查询才走索引）
- 统一分页协议 `{cursor, has_more}` 封装

## 原项目对照

- `backend/internal/feed/entity.go`（所有请求/响应/游标结构体）
- `backend/internal/feed/repo.go:20-50`（两种游标 SQL）
- `backend/internal/feed/service.go:25-141`（ListLatest / ListLikesCount + buildFeedVideos）
- `backend/internal/feed/handler.go`（limit 归一化 + 成对游标校验）
- 阶段 7 会回来在 ListLatest 里加缓存+锁；阶段 6 会加 listByFollowing
