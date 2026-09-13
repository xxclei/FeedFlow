package video

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

type VideoRepository struct {
	db *gorm.DB
}

func NewVideoRepository(db *gorm.DB) *VideoRepository {
	return &VideoRepository{db: db}
}

func (vr *VideoRepository) CreateVideo(ctx context.Context, video *Video) error {
	return vr.db.WithContext(ctx).Create(video).Error
}

// CreateMsg 写事件信存根（publish 事务里用）
func (vr *VideoRepository) CreateMsg(ctx context.Context, msg *OutboxMsg) error {
	return vr.db.WithContext(ctx).Create(msg).Error
}

// DeleteOwnedVideos 删除 ids 里**确实属于 authorID** 的那些视频，并在同一个事务里
// 清掉它们的 video_tags 关联。返回真正被删掉的 id（按输入顺序）。
//
// 这是本轮对删除的**唯一**实现：单条删除走 len(ids)==1 的特例，
// 不再有两份删除逻辑。
//
// ---------- 为什么返回 id 列表，而不是 RowsAffected ----------
//
// "哪些删掉了、哪些跳过了"必须由**服务端如实报出**，因为它无法从别处推断：
//
//	`WHERE id IN (?) AND author_id = ?` 把"不是我的"和"不存在"合并成了同一种结果 ——
//	两者都不删。而响应里需要区分吗？不需要（从安全角度也不该区分：
//	告诉调用者"这个 id 存在但不是你的"本身就是一次信息泄露）。
//	需要的是**逐个报出被跳过的 id**，让前端把没删掉的那几条留在选中态里。
//
// 所以先用 SelectOwnedIDs 问一遍"哪些真的是我的"，再删那批。
// 不用 RowsAffected 反推，还有第二个理由：它的含义在多表/软删除语义下会随 GORM 版本漂移，
// 而"我查出来的 id 列表"是确定的。
//
// ---------- 事务里的顺序 ----------
//
// 先删 videos、再删 video_tags。反过来的话，如果第二步失败回滚了，
// 标签还在、视频没了 —— 因为整个事务回滚，这其实无所谓。真正的顺序理由是：
// video_tags 的存在性依赖视频（虽然**没有外键约束**，见 tag_entity.go），
// 先删依赖方、再删被依赖方是更保守的写法，将来真加上 FK 也不会需要改这里。
//
// ---------- 这个事务**没有**清理的东西（已知缺口，不是漏写） ----------
//
// likes、comments、outbox_msgs 里指向这些视频的行会留下来变成孤儿，
// 磁盘上的 .mp4 和封面也一样留着（整个删除路径不含任何文件系统调用）。
// 本轮之前的单条删除也一样不清，所以这不是新引入的问题；但它是真实的不完整：
//
//	likes  → 孤儿点赞行会让 /like/listMyLikedVideos 依赖 GetByIDs 过滤掉不存在的视频
//	         （好消息是它确实会过滤 —— 那边按 id 查 videos 再组装，孤儿查不到就自然消失）
//	comments → 孤儿评论会让"评论数"这类将来的统计虚高
//	outbox_msgs → 阶段9 的 Poller 可能会为已删除的视频发一条事件
//	磁盘文件 → 永久泄漏，没有任何东西会回收它
//
// 清干净是**同包调用**，不是跨模块改造：like / comment / tag / outbox 全是 package video
// （internal/video/ 下 17 个文件同包），只有 notification 是真跨包 —— 而本包本来就 import 它。
//
// **完整方案（要两套机制：待清理表 + 会话 TTL；以及五个陷阱，包括库里存的是绝对 URL、
// notification.TargetID 是多态列、avatar_url 有不落盘的写入路径）已存档在
// PROGRESS.md §「删除的完整清理 + 文件 GC」，2026-09-13 决定推迟到阶段9 的 MQ/Worker 一起做。**
//
// 另外**不要**照旧以为 likes_count 会留下坏账：它是 videos 行上的反范式列（entity.go:14），
// 视频行删了它跟着消失。全项目没有任何地方从 likes 表重算它 —— 风险在将来做
// 共享视频/部分删除时才会显现。实施前先核实，别照旧注释行事。
func (vr *VideoRepository) DeleteOwnedVideos(ctx context.Context, ids []uint, authorID uint) ([]uint, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	var owned []uint
	err := vr.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		got, err := selectOwnedIDsTx(tx, ids, authorID)
		if err != nil {
			return err
		}
		// 按**输入顺序**重排：Pluck 出来的顺序是 MySQL 给的，不是调用方给的。
		// 不重排的话删除本身没错，但响应里的 deleted_ids 顺序随机，
		// 前端做乐观更新时对不上（也和搜索那边 GetByIDs 的坑同源）
		owned = orderByInput(got, ids)
		if len(owned) == 0 {
			return nil
		}
		return deleteVideosTx(tx, owned, authorID)
	})
	if err != nil {
		return nil, err
	}
	return owned, nil
}

// selectOwnedIDsTx 事务版：ids 里哪些是 authorID 的。
//
// 事务版不是形式主义：这里查出来的答案**要被后面的 DELETE 使用**，
// 事务外查到"是我的"不等于事务里"还是我的"（另一个请求可能刚好把它删了）。
// 和 IsExistTx 的理由完全一样 —— 查询逃逸出事务比写入逃逸更隐蔽，结果错但不报错。
func selectOwnedIDsTx(tx *gorm.DB, ids []uint, authorID uint) ([]uint, error) {
	var got []uint
	if err := tx.Model(&Video{}).
		Where("id IN ? AND author_id = ?", ids, authorID).
		Pluck("id", &got).Error; err != nil {
		return nil, err
	}
	return got, nil
}

// deleteVideosTx 删 videos 行 + 清它们的 video_tags 关联。
// 两个语句共用一个 tx，由调用方保证。
func deleteVideosTx(tx *gorm.DB, ids []uint, authorID uint) error {
	// author_id 条件在这里**保留**，不因为 owned 已经是我查出来的就省掉。
	// 理由：service 是"读了再写"，中间有窗口；这一层是最后的把关，
	// 带上它就把窗口关掉了，代价只是一个恒真的条件
	if err := tx.Where("id IN ? AND author_id = ?", ids, authorID).
		Delete(&Video{}).Error; err != nil {
		return err
	}
	// video_tags 按 video_id 清，**不带 authorID** —— 这张表里没有作者这一列，
	// 而且上一条已经保证了这些 video_id 都是本人的
	return tx.Where("video_id IN ?", ids).Delete(&VideoTag{}).Error
}

// dedupeIDs 保序去重，0 值也丢掉。
//
// 单独一个函数而不是复用 orderByInput：那个的语义是"按 want 重排 got"（两集合求交），
// 拿它当去重用（orderByInput(ids, ids)）虽然碰巧对，但读的人要停下来想三秒
// —— 名字在说一件和调用点无关的事。
//
// 丢 0：id=0 永远不可能是合法行（AUTO_INCREMENT 从 1 起）。
// 前端传 [0] 通常意味着"某个变量没赋上值"，让它变成 skipped_ids 里的一条，
// 比让它进 SQL 更有用。
func dedupeIDs(ids []uint) []uint {
	seen := make(map[uint]bool, len(ids))
	out := make([]uint, 0, len(ids))
	for _, id := range ids {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// orderByInput 把 got 按 want 的顺序重排，并丢掉不在 want 里的元素。
//
// 统一成"调用方给的顺序"是个小但值钱的一致性：批量删除的响应、搜索结果的
// 相关度排序，都依赖"服务端按我给的顺序回答"。两处都栽过同一个跟头
// （`IN ?` 不保证顺序），所以这个 helper 是共用的。
func orderByInput(got []uint, want []uint) []uint {
	if len(got) <= 1 {
		return got
	}
	set := make(map[uint]bool, len(got))
	for _, id := range got {
		set[id] = true
	}
	out := make([]uint, 0, len(got))
	for _, id := range want {
		if set[id] {
			out = append(out, id)
			delete(set, id) // 输入里的重复 id 只保留一次
		}
	}
	return out
}

// ListMissingEmbedding 找出向量该重算的视频：没嵌过的，或嵌的是**旧模型**的。
//
// **本轮没有调用点**（向量那一路先跳过，回填命令没做）。留着它的理由和
// entity.go 里 embedding_model 那两列一样：这是"回填必须是幂等的、且能靠
// SQL 找出待办"的落地形式，是那一整套里最难重新想到的一环。
//
// `embedding_model IS NULL OR embedding_model <> ?` 这个条件就是"换模型 = 全量重嵌"
// 的落地方式 —— 换了模型名，全表都会重新满足这个条件，回填命令自动把整库重算一遍，
// 不需要额外的迁移脚本。
func (vr *VideoRepository) ListMissingEmbedding(ctx context.Context, model string, limit int) ([]Video, error) {
	var videos []Video
	err := vr.db.WithContext(ctx).
		Where("embedding_model IS NULL OR embedding_model <> ?", model).
		Order("id ASC").
		Limit(limit).
		Find(&videos).Error
	return videos, err
}

// CountMissingEmbedding 同上，只要数量（回填命令用它打进度）
func (vr *VideoRepository) CountMissingEmbedding(ctx context.Context, model string) (int64, error) {
	var n int64
	err := vr.db.WithContext(ctx).Model(&Video{}).
		Where("embedding_model IS NULL OR embedding_model <> ?", model).
		Count(&n).Error
	return n, err
}

// MarkEmbedded 记录"这条已经用 model 嵌过了"。
//
// 它和 Redis 里那次写向量**不是一个事务**（一个在 MySQL、一个在 Redis），
// 所以两者的顺序决定了崩溃时的后果：
//
//	先写 Redis 再标记 → 崩溃时是"标记没写、向量在"→ 回填会重算一次（幂等，无害）
//	先标记再写 Redis → 崩溃时是"标记写了、向量没写"→ 回填**不会**补它 → 永久缺失
//
// 所以顺序必须是**先写 Redis，后标记**。回填命令和发布后的异步路径都遵守这一条。
//
// **本轮没有调用点**（它是 search.EmbeddingMarker 的唯一实现，
// 而用它的 search.Service.IndexVideo 本轮不会被调用 —— vectorIdx 传的是 nil）。
// 顺序这条纪律本身是最值得留下的部分：它属于那种"写反了不报错、
// 只在崩溃后表现为永久搜不到一条视频"的规则。
func (vr *VideoRepository) MarkEmbedded(ctx context.Context, id uint, model string) error {
	now := time.Now()
	return vr.db.WithContext(ctx).Model(&Video{}).
		Where("id = ?", id).
		Updates(map[string]any{"embedding_model": model, "embedded_at": now}).Error
}

func (vr *VideoRepository) ListByAuthorID(ctx context.Context, authorID int64) ([]Video, error) {
	var videos []Video
	if err := vr.db.WithContext(ctx).
		Where("author_id = ?", authorID).
		Order("create_time desc").
		Limit(200).
		Find(&videos).Error; err != nil {
		return nil, err
	}
	return videos, nil
}

func (vr *VideoRepository) GetByID(ctx context.Context, id uint) (*Video, error) {
	var video Video
	if err := vr.db.WithContext(ctx).First(&video, id).Error; err != nil {
		return nil, err
	}
	return &video, nil
}

func (vr *VideoRepository) UpdateLikesCount(ctx context.Context, id uint, likesCount int64) error {
	return vr.db.WithContext(ctx).Model(&Video{}).
		Where("id = ?", id).
		Update("likes_count", likesCount).Error
}

// IsExist 把"查无此视频"翻译成 bool（调用方不用碰 gorm.ErrRecordNotFound）
func (vr *VideoRepository) IsExist(ctx context.Context, id uint) (bool, error) {
	return vr.IsExistTx(vr.db.WithContext(ctx), id)
}

// IsExistTx 事务版存在性检查。
//
// 为什么非要 Tx 版：点赞事务里"视频还存在"是它依赖的前提（没有 FK 替我们保证），
// 而 vr.db 上查出来的答案来自**另一条连接**、另一个快照 —— 事务外查到"在"，
// 不等于事务里"在"。查询逃逸出事务比写入逃逸更隐蔽：结果通常是错的，但不报错。
func (vr *VideoRepository) IsExistTx(tx *gorm.DB, id uint) (bool, error) {
	var video Video
	if err := tx.First(&video, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// UpdatePopularity 数据库端原子增减：UPDATE videos SET popularity = popularity + ?
func (vr *VideoRepository) UpdatePopularity(ctx context.Context, id uint, change int64) error {
	return vr.db.WithContext(ctx).Model(&Video{}).
		Where("id = ?", id).
		Update("popularity", gorm.Expr("popularity + ?", change)).Error
}

// ChangeLikesCount 计数增减且不许为负（取消点赞后最低归 0）。
// 非事务版：**只给不在事务里的调用者用**。在 db.Transaction 闭包里必须调用 ...Tx，
// 否则这条 UPDATE 会跑到事务外、在另一条连接上独立提交 —— 事务回滚了它也不回滚。
func (vr *VideoRepository) ChangeLikesCount(ctx context.Context, id uint, change int64) error {
	return vr.ChangeLikesCountTx(vr.db.WithContext(ctx), id, change)
}

// ChangeLikesCountTx 事务版。
//
// 三件事值得单独说：
//
//	① 传的是**增量**（+1 / -1），不是目标值。`GREATEST(likes_count + ?, 0)` 里的 ?
//	   是增量 —— 参数化是为了防注入，不是为了防丢更新，两件事别混。
//	② 为什么能抗并发：这是**单条 SQL**，InnoDB 对同一行的 UPDATE 由行锁串行化，
//	   N 个并发 +1 的结果必定是 +N。"读出来 +1 再写回"和 `SET x = 绝对值` 都丢更新。
//	③ 为什么一律带 GREATEST 而不是只给减法加：对正增量 `GREATEST(x+1, 0)` 恒等于
//	   `x+1`（无操作），所以一律夹不会错；而减法必须夹，否则 0 - 1 会打成负数。
//	   **但 GREATEST 只防负数，防不了"凭空变少"** —— 那个只能靠 RowsAffected 判。
//
// UpdateColumn 而不是 Update：纯改列、不走 GORM 钩子、不碰 UpdatedAt。
func (vr *VideoRepository) ChangeLikesCountTx(tx *gorm.DB, id uint, change int64) error {
	return tx.Model(&Video{}).
		Where("id = ?", id).
		UpdateColumn("likes_count", gorm.Expr("GREATEST(likes_count + ?, 0)", change)).Error
}

// ChangePopularity 热度增减。热度 = 互动信号，和点赞同步 ±1。
// 阶段8 回填：本方法之外还要失效详情缓存 + 写 Redis 热榜分钟桶。
func (vr *VideoRepository) ChangePopularity(ctx context.Context, id uint, change int64) error {
	return vr.ChangePopularityTx(vr.db.WithContext(ctx), id, change)
}

func (vr *VideoRepository) ChangePopularityTx(tx *gorm.DB, id uint, change int64) error {
	return tx.Model(&Video{}).
		Where("id = ?", id).
		UpdateColumn("popularity", gorm.Expr("GREATEST(popularity + ?, 0)", change)).Error
}

// CountByAuthor 该作者的视频总数（getProfile 聚合用，阶段6）
func (vr *VideoRepository) CountByAuthor(ctx context.Context, authorID uint) (int64, error) {
	var count int64
	if err := vr.db.WithContext(ctx).Model(&Video{}).Where("author_id = ?", authorID).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// TotalLikesByAuthor 该作者全部视频的点赞总数（COALESCE：没视频时 SUM 返回 NULL，兜成 0）
func (vr *VideoRepository) TotalLikesByAuthor(ctx context.Context, authorID uint) (int64, error) {
	var total int64
	if err := vr.db.WithContext(ctx).Model(&Video{}).Where("author_id = ?", authorID).Select("COALESCE(SUM(likes_count), 0)").Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}
